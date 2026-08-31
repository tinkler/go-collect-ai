// Package restock 商超智能补货模块
//
// 职责: 定时从 cube-agent-server 拉数据 → 触发补货规则 → 推企业微信双群 → 接收员工反馈
// 存储: 复用 collect-ai 现有 PG 实例(同 schema),不创建独立数据库
// 依赖: collect-ai 现有 agent.Client / bigmodel.LlmClient / pgxpool.Pool / business.Registry
package restock

import "time"

// TaskStatus task 状态机
//   open     待补货(open 唯一约束: 同 branch+item 只 1 行)
//   acked    员工点"已补货"(临时状态,等库存增加自动 close)
//   short    员工点"缺货"(同时写 need_purchase,等供应商配送后 close)
//   closed   关闭(库存增加入库 / 人工关闭 / 过期)
const (
	TaskStatusOpen   = "open"
	TaskStatusAcked  = "acked"
	TaskStatusShort  = "short"
	TaskStatusClosed = "closed"
)

// FeedbackType 员工反馈
const (
	FeedbackDone  = "DONE"
	FeedbackShort = "SHORT"
)

// Priority 补货优先级
const (
	PriorityP0 = "P0" // 半天内断货
	PriorityP1 = "P1" // 1.5 天
	PriorityP2 = "P2" // 3 天
	PriorityP3 = "P3" // 预防性
)

// TriggerKind need_purchase 触发原因
const (
	TriggerShortFeedback = "short_feedback" // 员工反馈缺货
	TriggerBelowSafety   = "below_safety"   // 库存低于安全库存
	TriggerLlmJudge      = "llm_judge"      // LLM 判定
)

// NeedStatus need_purchase 状态
const (
	NeedStatusPending   = "pending"
	NeedStatusSent      = "sent_to_supplier"
	NeedStatusReceived  = "received"
	NeedStatusCancelled = "cancelled"
)

// Task 补货任务表的一行
type Task struct {
	TaskID         string     `json:"task_id"`
	BranchNo       string     `json:"branch_no"`
	ItemNo         string     `json:"item_no"`
	ItemName       string     `json:"item_name"`
	SupplierName   string     `json:"supplier_name"`
	CurrentStock   int        `json:"current_stock"`
	SafetyStock    int        `json:"safety_stock"`
	YesterdaySales int        `json:"yesterday_sales"`
	SuggestQty     int        `json:"suggest_qty"`
	Reason         string     `json:"reason"`
	Priority       string     `json:"priority"`
	Status         string     `json:"status"`
	FirstPushAt    *time.Time `json:"first_push_at,omitempty"`
	LastPushAt     *time.Time `json:"last_push_at,omitempty"`
	LastUpdateAt   time.Time  `json:"last_update_at"`
	ClosedAt       *time.Time `json:"closed_at,omitempty"`
	ClosedReason   string     `json:"closed_reason,omitempty"`
	PushCount      int        `json:"push_count"`
}

// Feedback 员工反馈
type Feedback struct {
	ID           int64     `json:"id"`
	TaskID       string    `json:"task_id"`
	FeedbackType string    `json:"feedback_type"`
	FeedbackUser string    `json:"feedback_user"`
	FeedbackTime time.Time `json:"feedback_time"`
}

// NeedPurchase 采购计划单条目
type NeedPurchase struct {
	ID             int64      `json:"id"`
	BranchNo       string     `json:"branch_no"`
	ItemNo         string     `json:"item_no"`
	ItemName       string     `json:"item_name"`
	Barcode        string     `json:"barcode"`
	SupplierName   string     `json:"supplier_name"`
	SuggestQty     int        `json:"suggest_qty"`
	TriggerKind    string     `json:"trigger_kind"`
	TriggerTaskID  string     `json:"trigger_task_id"`
	Status         string     `json:"status"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	ExportedAt     *time.Time `json:"exported_at,omitempty"`
}

// SalesWatch 销售观测窗口(供 R2/R2b 判定)
type SalesWatch struct {
	BranchNo    string    `json:"branch_no"`
	ItemNo      string    `json:"item_no"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	SaleQnty    int       `json:"sale_qnty"`
}

// SupplierReliability 供应商供应能力(供 LLM 调整补货量)
type SupplierReliability struct {
	SupplierName string    `json:"supplier_name"`
	ItemNo       string    `json:"item_no"`
	RequestedQty float64   `json:"requested_qty"`
	SuppliedQty  float64   `json:"supplied_qty"`
	FillRate     float64   `json:"fill_rate"` // 0~1, 默认 1.0
	AvgLeadDays  float64   `json:"avg_lead_days"`
	LastOrderAt  *time.Time `json:"last_order_at,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// SkuSnapshot 单 SKU 当前快照(从 cube 拉数据后组装)
type SkuSnapshot struct {
	BranchNo       string `json:"branch_no"`
	ItemNo         string `json:"item_no"`
	ItemName       string `json:"item_name"`
	Barcode        string `json:"barcode"`
	SupplierName   string `json:"supplier_name"`
	Stock          int    `json:"stock"`
	YesterdaySales int    `json:"yesterday_sales"`
	SevenDayAvg    int    `json:"seven_day_avg"`
	ThirtyDayAvg   int    `json:"thirty_day_avg"`
	HasPromo7d     bool   `json:"has_promo_7d"`
}

// RestockConfig restock 模块独立配置
type RestockConfig struct {
	BranchNo  string

	// cron
	HourlyCron      string
	AggregateCron   string
	LlmPlanCron     string
	MaxPushPerTick  int

	// 触发 / 水位
	ROPFactor       float64 // ROP = daily_avg × 此值
	OUTDays         int     // 补货目标 = daily_avg × 此值
	OUTPromoBoost   float64 // 促销期再 × 此值
	SafetyMin       int     // ROP 最小缓冲
	WYesterday      float64
	WSevenDay       float64
	WThirtyDay      float64

	// 节流
	FloorMinIntervalMin  int
	OfficeP0MinMin       int
	OfficeP1MinMin       int
	OfficeP2MinMin       int

	// 静默升级
	EscalateP2ToP1Hours int
	EscalateP1ToP0Hours int

	// cube 名(允许 env 覆盖;空 = 用默认)
	CubeSales     string
	CubeInventory string
	CubePromotion string

	// LLM 批量
	LLMModel       string
	LLMEnabled     bool
	LLMPlanEnabled bool
	LLMPlanCacheHrs int

	// 企微智能机器人(长连接模式)
	WeComBotID     string
	WeComBotSecret string
	WeComWSURL     string // 默认 wss://openws.work.weixin.qq.com
	WeComBindFile  string // 默认 ./wecom_bindings.json

	// 陈列补货新版 (2026-08-30 新增,逐步替代旧 HourlyTick)
	DisplayRestockCronEve  string // 默认 "0 0 7 * * *",07:00 tick(跨天窗口)
	DisplayRestockCronMorn string // 默认 "0 0 12 * * *",12:00 tick
	DisplayRestockCronAft  string // 默认 "0 30 20 * * *",20:30 tick
	DisplayRestockCubeName string // 默认 "display_restock_window"
	DisplayRestockRetryMax int    // 默认 3(指数退避 5s/15s/45s)
	DisplayRestockMaxPush  int    // 默认 30(每次 tick 最多推 N 条)
}

// ============== 陈列补货新版 (2026-08-30 新增,逐步替代旧 Task 体系) ==============

// Period 时段标识(三次 tick 各自对应一段销售窗口)
const (
	PeriodEve  = "eve"  // 7:00 tick,拉 昨日 20:30 ~ 今 07:00(10.5h,跨天)
	PeriodMorn = "morn" // 12:00 tick,拉 今 07:00 ~ 12:00(5h)
	PeriodAft  = "aft"  // 20:30 tick,拉 今 12:00 ~ 20:30(8.5h)
)

// TickStatus tick 执行结果
const (
	TickStatusOK    = "ok"
	TickStatusError = "error"
)

// TriggerDisplayShort 短补触发枚举(新,2026-08-30)
//   need_purchase 由 display_suggest.suggest_qty 驱动 + 持续覆盖
//   区别于旧的 short_feedback / below_safety / llm_judge
const TriggerDisplayShort = "display_short"

// DisplaySuggest 每日陈列补充建议
//   每店每商品每天 1 行,三次 tick 累加
//   员工点完成 → SuggestQty=0 + IsShort=FALSE(由 ShortState 翻)
//   员工点缺货 → IsShort=TRUE(由 ShortState 翻),need_purchase 立即 upsert
//   tick 解除 short → ShortState.is_short=FALSE,但 need_purchase 保持 pending(等员工点完成清 0)
type DisplaySuggest struct {
	BranchNo     string     `json:"branch_no"`
	ItemNo       string     `json:"item_no"`
	ItemName     string     `json:"item_name,omitempty"`
	PeriodDate   time.Time  `json:"period_date"`
	SuggestQty   int        `json:"suggest_qty"`
	InvSnapshot  int        `json:"inv_snapshot"`
	LastPeriod   string     `json:"last_period,omitempty"`
	LastSaleAt   *time.Time `json:"last_sale_at,omitempty"`
	LastUpdateAt time.Time  `json:"last_update_at"`
}
// WindowSaleRow 定义在 cube_query.go (2026-08-31: SaleQty 改 float64)

// ShortState 全局短补状态
//   每店每商品 1 行,跨天持续
//   ONCE 锁定:员工点缺货后 IsShort=TRUE,后续 SHORT 按钮被前端禁用 + 后端静默 ACK
//   直到员工点完成 → IsShort=FALSE,允许下一轮短补
type ShortState struct {
	BranchNo  string     `json:"branch_no"`
	ItemNo    string     `json:"item_no"`
	IsShort   bool       `json:"is_short"`
	ShortAt   *time.Time `json:"short_at,omitempty"`
	ShortUser string     `json:"short_user,omitempty"`
}

// TickLog tick 执行日志(用于错误恢复 + 审计)
//   启动时扫描 status='error' 的记录告警
//   7 天后由定时清理脚本 DELETE
type TickLog struct {
	ID         int64     `json:"id"`
	BranchNo   string    `json:"branch_no"`
	Period     string    `json:"period"`
	TickAt     time.Time `json:"tick_at"`
	WindowFrom time.Time `json:"window_from"`
	WindowTo   time.Time `json:"window_to"`
	Status     string    `json:"status"`
	ErrorMsg   string    `json:"error_msg,omitempty"`
	ItemsCount int       `json:"items_count"`
	CreatedAt  time.Time `json:"created_at"`
}

// H5TaskItem H5 任务列表返回结构(2026-08-30 新版)
//   合并 display_suggest + short_state + need_purchase 三个数据源
//   前端根据 IsShort 决定按钮显示(只显 DONE / 或 SHORT+DONE)
type H5TaskItem struct {
	ItemNo       string `json:"item_no"`
	ItemName     string `json:"item_name"`
	BranchNo     string `json:"branch_no"`
	SuggestQty   int    `json:"suggest_qty"`
	InvSnapshot  int    `json:"inv_snapshot"`
	IsShort      bool    `json:"is_short"`     // 短补中(true 时前端隐藏 SHORT 按钮)
	ShortAt      string  `json:"short_at,omitempty"`
	ShortUser    string  `json:"short_user,omitempty"`
	NeedQty      int     `json:"need_qty"`      // need_purchase.suggest_qty(0 表示无采购单)
	NeedStatus   string  `json:"need_status,omitempty"`
	LastPeriod   string  `json:"last_period"`
	PeriodDate   string  `json:"period_date"`
	LastUpdateAt string  `json:"last_update_at"`
}
