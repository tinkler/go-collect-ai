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

	// 企微
	WeComCorpID        string
	WeComAgentID       string
	WeComAgentSecret   string
	WeComCallbackToken string
	WeComCallbackAES   string
	WeComFloorChatID   string
	WeComOfficeChatID  string
}
