// Package restock 商超陈列补货模块
//
// 重构后 (2026-09-02): 只保留 H5 驱动的陈列补货,不再推企微群。
//
// 职责:
//   - 每天 3 次 cron 拉 cube 销售窗口,写入 display_suggest
//   - 员工在 H5 看到补货列表,点 DONE/SHORT
//   - SHORT 时锁 short_state + 自动生成 need_purchase 采购单
//
// 存储: 复用 collect-ai 现有 PG 实例(同 schema)
// 依赖: collect-ai 现有 agent.Client / pgxpool.Pool
package restock

import "time"

// ============== 任务反馈 kind (前端用) ==============

const (
	FeedbackShort = "short" // 缺货
	FeedbackDone  = "done"  // 已完成
)

// ============== Period 时段 (三次 tick 各对应一段销售窗口) ==============

const (
	PeriodEve  = "eve"  // 07:00 tick, 拉 昨日 20:30 ~ 今 07:00
	PeriodMorn = "morn" // 12:00 tick, 拉 今 07:00 ~ 12:00
	PeriodAft  = "aft"  // 20:30 tick, 拉 今 12:00 ~ 20:30
	PeriodManual = "manual" // 手动触发, 拉 最近 1h
)

// ============== Tick status ==============

const (
	TickStatusOK    = "ok"
	TickStatusError = "error"
)

// ============== NeedPurchase status ==============

const (
	NeedStatusPending   = "pending"
	NeedStatusSent      = "sent_to_supplier"
	NeedStatusReceived  = "received"
	NeedStatusCancelled = "cancelled"
)

// ============== Trigger kind ==============

// 短补触发的采购单
const TriggerDisplayShort = "display_short"

// ============== DisplaySuggest 陈列补货建议 ==============

// DisplaySuggest 每店每商品每天 1 行
//   - suggest_qty 3 次 tick 累加
//   - 员工点 DONE → suggest_qty=0
//   - 员工点 SHORT → suggest_qty 通过 NeedPurchase 持续覆盖
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

// ============== ShortState 全局短补状态 ==============

// ShortState 每店每商品 1 行,跨天持续
//   - 员工点 SHORT → is_short=TRUE 锁 ONCE
//   - 员工点 DONE → is_short=FALSE 解锁
type ShortState struct {
	BranchNo  string     `json:"branch_no"`
	ItemNo    string     `json:"item_no"`
	IsShort   bool       `json:"is_short"`
	ShortAt   *time.Time `json:"short_at,omitempty"`
	ShortUser string     `json:"short_user,omitempty"`
}

// ============== PurchasePlan 采购计划单 ==============

// PurchasePlan (旧名 NeedPurchase) 采购计划单条目
//   - 员工点 SHORT 时 upsert(pending)
//   - 员工点 DONE 时 close(pending → cancelled)
//   - supplier 导出后 → sent_to_supplier
type PurchasePlan struct {
	ID            int64      `json:"id"`
	BranchNo      string     `json:"branch_no"`
	ItemNo        string     `json:"item_no"`
	ItemName      string     `json:"item_name,omitempty"`
	Barcode       string     `json:"barcode,omitempty"`
	SupplierName  string     `json:"supplier_name,omitempty"`
	SuggestQty    int        `json:"suggest_qty"`
	TriggerKind   string     `json:"trigger_kind"`
	TriggerTaskID string     `json:"trigger_task_id,omitempty"`
	Status        string     `json:"status"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	ExportedAt    *time.Time `json:"exported_at,omitempty"`
}

// ============== TickLog tick 执行日志 ==============

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

// ============== H5TaskItem H5 任务列表返回结构 ==============

// H5TaskItem 合并 display_suggest + short_state + purchase_plan
//   前端根据 is_short 决定按钮显示 (只显 DONE / 或 SHORT+DONE)
type H5TaskItem struct {
	ItemNo       string `json:"item_no"`
	ItemName     string `json:"item_name"`
	BranchNo     string `json:"branch_no"`
	SuggestQty   int    `json:"suggest_qty"`
	InvSnapshot  int    `json:"inv_snapshot"`
	IsShort      bool   `json:"is_short"`
	ShortAt      string `json:"short_at,omitempty"`
	ShortUser    string `json:"short_user,omitempty"`
	NeedQty      int    `json:"need_qty"`
	NeedStatus   string `json:"need_status,omitempty"`
	LastPeriod   string `json:"last_period"`
	LastSaleAt   string `json:"last_sale_at,omitempty"`
	LastUpdateAt string `json:"last_update_at"`
	// 注入 (item cube 内存字典)
	ItemClsno   string `json:"item_clsno,omitempty"`
	ItemClsname string `json:"item_clsname,omitempty"`
	Unit        string `json:"unit,omitempty"`
}

// ============== SkuSnapshot 单 SKU 快照(给 cube 用) ==============

type SkuSnapshot struct {
	BranchNo       string
	ItemNo         string
	ItemName       string
	Barcode        string
	SupplierName   string
	Stock          int
	YesterdaySales int
	SevenDayAvg    int
	ThirtyDayAvg   int
	HasPromo7d     bool
}

// ============== RestockConfig 精简后 ==============

// RestockConfig restock 模块精简配置
//   重构后: 删 ROP / 加权 / 节流 / LLM / office / floor 概念
//   保留: BranchNo + 3 次 cron + cube name + retry/max_push
type RestockConfig struct {
	BranchNo string

	// cron 表达式 (3 次 tick + 手动)
	CronEve  string // 默认 "0 0 7 * * *"
	CronMorn string // 默认 "0 0 12 * * *"
	CronAft  string // 默认 "0 30 20 * * *"

	// cube 配置
	CubeName    string // 默认 "display_restock_window"
	RetryMax    int    // 默认 3 (5s/20s/45s 指数退避)
	MaxPerTick  int    // 默认 30

	// 企微智能机器人 (长连接模式) — H5 反馈兜底通道, 暂不推群但保留
	WeComBotID     string
	WeComBotSecret string
	WeComWSURL     string // 默认 wss://openws.work.weixin.qq.com
	WeComBindFile  string // 默认 ./wecom_bindings.yaml
}
