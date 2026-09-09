// Package freshcheck 生鲜免日盘管理子系统 (2026-09-09 W1.1)
//
// 需求: docs/生鲜免日盘管理扩展子系统设计需求文档.md v1.0
// 设计: docs/freshcheck-architecture.md
// 数据: docs/freshcheck-data-model.md
// 结算: docs/freshcheck-settlement.md
//
// 核心业务:
//   - 共享特价码归因断裂修复 (1元/0.5元特价码)
//   - 周期倒挤 + 特价池事件流 + 预设损耗率 → 单品毛利核算
//   - 多节奏结算 (周结/月结/不定时结混合)
//
// 模块边界 (AGENTS.md §12.1 强制):
//   - 不直连思迅 HB POS, 全部走 cube-agent-server
//   - 不写思迅主库, 只在 collect-ai 同 schema 写派生
//   - 不同步销售/采购/库存数据, 按需 cube 聚合查询
//
// 周期结算: 6 步流程 + 8 校验 (C1-C8), 见 settlement.md
// 业务阈值: 全部走 freshcheck_threshold PG 表, 不硬编码 Go
package freshcheck

import "time"

// ============== 1. 枚举常量 (PG CHECK 约束同步) ==============

// FreshCategory 生鲜子类 (跟 t_bd_item_cls.item_clsname 静态映射)
const (
	CategoryLeaf    = "leaf"    // 叶菜
	CategoryRoot    = "root"    // 根茎
	CategoryAquatic = "aquatic" // 水产
	CategoryMeat    = "meat"    // 肉类
	CategoryFrozen  = "frozen"  // 冻品
)

// AllCategories 所有生鲜子类 (用于建账 seed)
var AllCategories = []string{CategoryLeaf, CategoryRoot, CategoryAquatic, CategoryMeat, CategoryFrozen}

// TurnoverClass 快/慢周转 (决定损耗模型)
const (
	TurnoverFast = "fast" // 快周转 (叶菜/鲜肉/水产)
	TurnoverSlow = "slow" // 慢周转 (根茎/冻品)
)

// LossType 损耗类型
const (
	LossNatural   = "natural"   // 自然损耗
	LossSpoilage  = "spoilage"  // 变质
	LossProcess   = "process"   // 加工
)

// PoolEventKind 池事件类型
const (
	PoolEventIn  = "in"  // 入框
	PoolEventOut = "out" // 出框
)

// OutDestination 出框去向 (4 选 1)
const (
	DestSoldOut      = "sold_out"       // 售罄离框
	DestSpoiled      = "spoiled"        // 变质报损
	DestReturnShelf  = "return_to_shelf" // 撤回正常货架
	DestDowngrade    = "downgrade"      // 降级转框
)

// AllOutDestinations 所有出框去向
var AllOutDestinations = []string{DestSoldOut, DestSpoiled, DestReturnShelf, DestDowngrade}

// Confidence 置信度
const (
	ConfidenceHigh = "high" // 实时录入
	ConfidenceLow  = "low"  // 事后补录 / 漏录兜底
)

// PricingMode 特价码计价模式
const (
	PricingWeight = "weight" // 称重
	PricingPiece  = "piece"  // 计件
)

// EventSource 事件来源
const (
	SourceH5     = "h5"     // H5 录入
	SourceAdmin  = "admin"  // admin 后台
	SourceImport = "import" // Excel 导入
)

// SettlementStatus 结算状态
const (
	SettleFinalized     = "finalized"     // 已结算
	SettleRecalculating = "recalculating" // 重算中
	SettleOverridden    = "overridden"    // 管理员手工改过
	SettleVoided        = "voided"        // 废止
)

// AlertSeverity 告警严重度
const (
	SeverityInfo  = "info"
	SeverityWarn  = "warn"
	SeverityBlock = "block"
)

// AlertStatus 告警状态
const (
	AlertOpen      = "open"
	AlertOverridden = "overridden"
	AlertFixed     = "fixed"
	AlertIgnored   = "ignored"
)

// LossCalibrateAction 损耗率校准处置
const (
	CalibPending      = "pending"
	CalibUpdatePreset = "update_preset"
	CalibInvestigate  = "investigate"
	CalibDiscard      = "discard"
)

// SnapAction 配置快照操作类型
const (
	SnapCreate = "create"
	SnapUpdate = "update"
	SnapDelete = "delete"
)

// StockFreq 盘点频率
const (
	StockFreqDaily   = "daily"
	StockFreqWeekly  = "weekly"
	StockFreqMonthly = "monthly"
	StockFreqNone    = "none"
)

// ============== 2. 业务阈值 key (写 freshcheck_threshold 表) ==============

const (
	ThrC1PoolSaturationPct      = "c1_pool_saturation_pct"       // 池内饱和度容差 (%)
	ThrC2LeakageMultWeekly       = "c2_leakage_multiplier_weekly" // 周结池外泄漏倍数
	ThrC2LeakageMultLong        = "c2_leakage_multiplier_long"   // 月结/长窗口泄漏倍数
	ThrC6PeriodLockLeaf         = "c6_period_lock_leaf_days"     // 叶菜周期锁
	ThrC6PeriodLockRoot         = "c6_period_lock_root_days"     // 根茎周期锁
	ThrC6PeriodLockAquatic      = "c6_period_lock_aquatic_days"  // 水产周期锁
	ThrC6PeriodLockMeat         = "c6_period_lock_meat_days"     // 肉类周期锁
	ThrC6PeriodLockFrozen       = "c6_period_lock_frozen_days"   // 冻品周期锁
	// 2026-09-09: 移除 ThrC7SyncDiffPct (零同步架构下 C7 不适用)
	ThrC8PeriodStockCoverage    = "c8_period_stock_coverage_pct" // 盘点覆盖率
	ThrLossCalibrateDeviation   = "loss_calibrate_deviation_pct" // 校准偏差阈值
	ThrLowConfidenceWeight      = "low_confidence_weight"        // 低置信度分摊权重
)

// ============== 3. 配置表 struct ==============

// SkuMap 生鲜 SKU 映射 (freshcheck_sku_map)
//   - 思迅 t_bd_item_info.item_no → 生鲜子类 + 关联特价码
type SkuMap struct {
	ID              int64     `json:"id"`
	BranchNo        string    `json:"branch_no"`
	ItemNo          string    `json:"item_no"`
	ItemName        string    `json:"item_name"`
	FreshCategory   string    `json:"fresh_category"`   // 'leaf' | 'root' | ...
	TurnoverClass   string    `json:"turnover_class"`   // 'fast' | 'slow'
	ShelfLifeDays   int       `json:"shelf_life_days"`  // 商品生命期
	DefaultPoolCode *string   `json:"default_pool_code,omitempty"` // 关联特价码 (NULL=不入池)
	IsActive        bool      `json:"is_active"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// PoolCode 特价码定义 (freshcheck_pool_code)
//   - "1元/斤" / "0.5元/斤" / "1元2件" 等
//   - priority_rank=0 是最高优先级 (= 最高单价先入)
type PoolCode struct {
	ID           int64     `json:"id"`
	BranchNo     string    `json:"branch_no"`
	PoolCode     string    `json:"pool_code"`     // 思迅货号 (特价码)
	PoolName     string    `json:"pool_name"`     // 业务名
	PricingMode  string    `json:"pricing_mode"`  // 'weight' | 'piece'
	UnitPrice    float64   `json:"unit_price"`    // 元/斤 或 元/件
	PieceWeight  *float64  `json:"piece_weight,omitempty"` // 计件模式: 每件折重 kg
	PriorityRank int       `json:"priority_rank"` // 0=最高
	IsActive     bool      `json:"is_active"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// LossRate 损耗率规则 (freshcheck_loss_rate)
//   - effective_to IS NULL = 当前生效
//   - 同一 (category, turnover, type) 多条: 取 effective_from 最大者
type LossRate struct {
	ID               int64      `json:"id"`
	FreshCategory    string     `json:"fresh_category"`
	TurnoverClass    string     `json:"turnover_class"`
	LossType         string     `json:"loss_type"`
	DailyRate        float64    `json:"daily_rate"`         // e.g. 0.050000 = 5%/天
	EffectiveFrom    time.Time  `json:"effective_from"`
	EffectiveTo      *time.Time `json:"effective_to,omitempty"`
	IsCalibrated     bool       `json:"is_calibrated"`
	LastCalibrateAt  *time.Time `json:"last_calibrate_at,omitempty"`
	LastCalibrateBy  string     `json:"last_calibrate_by"`
	CreatedAt        time.Time  `json:"created_at"`
}

// Threshold 业务阈值 (freshcheck_threshold)
type Threshold struct {
	Key         string    `json:"key"`
	Value       float64   `json:"value"`
	Unit        string    `json:"unit"`
	Description string    `json:"description"`
	UpdatedAt   time.Time `json:"updated_at"`
	UpdatedBy   string    `json:"updated_by"`
}

// CategoryTrack 品类结算轨道 (freshcheck_category_track)
type CategoryTrack struct {
	ID                 int64      `json:"id"`
	FreshCategory      string     `json:"fresh_category"`
	BranchNo           string     `json:"branch_no"`
	TrackCode          string     `json:"track_code"` // 'leaf-weekly' | 'root-monthly' | ...
	PeriodLockDays     int        `json:"period_lock_days"`
	StockFreq          string     `json:"stock_freq"`
	RequireStock       bool       `json:"require_stock"`
	LastSettleAt       *time.Time `json:"last_settle_at,omitempty"`
	NextSettleDeadline *time.Time `json:"next_settle_deadline,omitempty"`
	IsActive           bool       `json:"is_active"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// ============== 4. 人工录入 struct (事实表) ==============

// PoolEvent 特价池事件 (freshcheck_pool_event)
//   - 同一 (branch, pool, item, kind, event_time) 不可重复 (UNIQUE)
//   - 兜底: 补录事件 confidence=low
type PoolEvent struct {
	ID              int64     `json:"id"`
	BranchNo        string    `json:"branch_no"`
	PoolCode        string    `json:"pool_code"`
	ItemNo          string    `json:"item_no"`
	EventKind       string    `json:"event_kind"`        // 'in' | 'out'
	EventTime       time.Time `json:"event_time"`        // 业务事件时间
	RecordedAt      time.Time `json:"recorded_at"`       // 录库时间 (只读)
	WeightKg        float64   `json:"weight_kg"`
	PieceCount      int       `json:"piece_count"`
	OutDestination  *string   `json:"out_destination,omitempty"`  // 出框时填
	DowngradeTo     *string   `json:"downgrade_to,omitempty"`     // 降级转框时填
	Operator        string    `json:"operator"`
	Confidence      string    `json:"confidence"` // 'high' | 'low'
	Source          string    `json:"source"`     // 'h5' | 'admin' | 'import'
	Note            string    `json:"note"`
}

// PeriodStockInput 周期盘点录入请求体
type PeriodStockInput struct {
	ItemNo   string  `json:"item_no"`
	ItemName string  `json:"item_name"`
	Qty      float64 `json:"qty"`
	Unit     string  `json:"unit"`
	Operator string  `json:"operator"`
	Note     string  `json:"note"`
}

// PeriodStock 周期盘点 (freshcheck_period_stock)
type PeriodStock struct {
	ID         int64     `json:"id"`
	BranchNo   string    `json:"branch_no"`
	PeriodID   int64     `json:"period_id"`
	ItemNo     string    `json:"item_no"`
	ItemName   string    `json:"item_name"`
	Qty        float64   `json:"qty"`
	Unit       string    `json:"unit"`
	StockTime  time.Time `json:"stock_time"`
	Operator   string    `json:"operator"`
	Confidence string    `json:"confidence"`
	Source     string    `json:"source"`
	Note       string    `json:"note"`
	CreatedAt  time.Time `json:"created_at"`
}

// ============== 5. 派生 struct (结算结果) ==============

// Settlement 周期结算结果 (freshcheck_settlement) — R1 周期单品毛利表
//   守恒: (normal + alloc + loss + box_loss) == (begin + purchase - end)
type Settlement struct {
	ID              int64     `json:"id"`
	BranchNo        string    `json:"branch_no"`
	PeriodID        int64     `json:"period_id"`
	TrackCode       string    `json:"track_code"`
	FreshCategory   string    `json:"fresh_category"`
	ItemNo          string    `json:"item_no"`
	ItemName        string    `json:"item_name"`
	// 流量
	BeginQty        float64   `json:"begin_qty"`
	PurchaseQty     float64   `json:"purchase_qty"`
	NormalSaleQty   float64   `json:"normal_sale_qty"`
	NormalSaleAmt   float64   `json:"normal_sale_amt"`
	PoolAllocQty    float64   `json:"pool_alloc_qty"`
	PoolAllocAmt    float64   `json:"pool_alloc_amt"`
	EndQty          float64   `json:"end_qty"`
	BackflushQty    float64   `json:"backflush_qty"`
	LossQty         float64   `json:"loss_qty"`
	BoxLossQty      float64   `json:"box_loss_qty"`
	// 钱
	AvgCost         float64   `json:"avg_cost"`
	SaleCost        float64   `json:"sale_cost"`
	TotalRevenue    float64   `json:"total_revenue"`
	GrossProfit     float64   `json:"gross_profit"`
	GrossProfitRate *float64  `json:"gross_profit_rate,omitempty"`
	// 标记
	Confidence      string    `json:"confidence"`
	IsOverridden    bool      `json:"is_overridden"`
	OverrideReason  string    `json:"override_reason"`
	// 窗口
	WindowStart     time.Time `json:"window_start"`
	WindowEnd       time.Time `json:"window_end"`
	WindowDays      int       `json:"window_days"`
	// 状态
	Status          string    `json:"status"`
	SettledAt       time.Time `json:"settled_at"`
	SettledBy       string    `json:"settled_by"`
	// 守恒
	ConservationOK  bool      `json:"conservation_ok"`
	ConservationMsg string    `json:"conservation_msg"`
	// 幂等
	IdempotencyKey  string    `json:"idempotency_key"`
}

// Alloc 归因分摊明细 (freshcheck_alloc) — R2 特价归因明细
//   每 SKU 每特价框每时段 1 行
type Alloc struct {
	ID                 int64     `json:"id"`
	PeriodID           int64     `json:"period_id"`
	PoolCode           string    `json:"pool_code"`
	SegmentStart       time.Time `json:"segment_start"`
	SegmentEnd         time.Time `json:"segment_end"`
	ItemNo             string    `json:"item_no"`
	// 输入
	InWeightKg         float64   `json:"in_weight_kg"`
	OutWeightKg        float64   `json:"out_weight_kg"`
	SpoiledWeightKg    float64   `json:"spoiled_weight_kg"`
	WeightDiffKg       float64   `json:"weight_diff_kg"`
	PoolPosQty         float64   `json:"pool_pos_qty"`
	PoolPosAmt         float64   `json:"pool_pos_amt"`
	// 权重
	ShareWeight        float64   `json:"share_weight"`
	ConfidenceFactor   float64   `json:"confidence_factor"`
	// 产出
	AllocQty           float64   `json:"alloc_qty"`
	AllocAmt           float64   `json:"alloc_amt"`
	// 双轨偏差
	BackflushAllocQty  float64   `json:"backflush_alloc_qty"`
	DeviationQty       float64   `json:"deviation_qty"`
	DeviationRate      *float64  `json:"deviation_rate,omitempty"`
	NeedsReview        bool      `json:"needs_review"`
	CreatedAt          time.Time `json:"created_at"`
}

// BoxRecon 框内对账 (freshcheck_box_recon) — R3 框内对账
type BoxRecon struct {
	ID                int64      `json:"id"`
	PeriodID          int64      `json:"period_id"`
	BranchNo          string     `json:"branch_no"`
	PoolCode          string     `json:"pool_code"`
	SegmentStart      time.Time  `json:"segment_start"`
	SegmentEnd        time.Time  `json:"segment_end"`
	InTotalKg         float64    `json:"in_total_kg"`
	OutTotalKg        float64    `json:"out_total_kg"`
	OutSoldOutKg      float64    `json:"out_sold_out_kg"`
	OutSpoiledKg      float64    `json:"out_spoiled_out_kg"`  // 注: JSON 字段名保留历史, DB 列名 out_spoiled_kg
	OutReturnKg       float64    `json:"out_return_kg"`
	OutDowngradeKg    float64    `json:"out_downgrade_kg"`
	PosTotalKg        float64    `json:"pos_total_kg"`
	BoxLossKg         float64    `json:"box_loss_kg"`
	BoxLossAmt        float64    `json:"box_loss_amt"`
	ResponsibleUser   string     `json:"responsible_user"`
	ResponsibleAt     *time.Time `json:"responsible_at,omitempty"`
	NeedsInvestigate  bool       `json:"needs_investigate"`
	InvestigateNote   string     `json:"investigate_note"`
	CreatedAt         time.Time  `json:"created_at"`
}

// Alert 校验告警 (freshcheck_alert) — R5 异常清单
type Alert struct {
	ID             int64      `json:"id"`
	BranchNo       string     `json:"branch_no"`
	PeriodID       *int64     `json:"period_id,omitempty"`
	RuleCode       string     `json:"rule_code"`   // 'C1'..'C8' | 'LOSS_CALIB' | ...
	Severity       string     `json:"severity"`    // 'info' | 'warn' | 'block'
	EntityType     string     `json:"entity_type"` // 'pool' | 'item' | 'category' | 'sync'
	EntityID       string     `json:"entity_id"`
	Message        string     `json:"message"`
	Payload        string     `json:"payload"` // JSONB 序列化为 string (handler 层 map[string]any)
	Status         string     `json:"status"`
	OverrideBy     string     `json:"override_by"`
	OverrideReason string     `json:"override_reason"`
	OverrideAt     *time.Time `json:"override_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// LossCalibrate 损耗率校准历史 (freshcheck_loss_calibrate)
type LossCalibrate struct {
	ID                  int64     `json:"id"`
	BranchNo            string    `json:"branch_no"`
	FreshCategory       string    `json:"fresh_category"`
	TurnoverClass       string    `json:"turnover_class"`
	PeriodWindowStart   time.Time `json:"period_window_start"`
	PeriodWindowEnd     time.Time `json:"period_window_end"`
	MeasuredLossQty     float64   `json:"measured_loss_qty"`
	MeasuredThroughput  float64   `json:"measured_throughput"`
	MeasuredRate        float64   `json:"measured_rate"`
	PresetRate          float64   `json:"preset_rate"`
	DeviationPct        float64   `json:"deviation_pct"`
	Action              string    `json:"action"`
	ActionBy            string    `json:"action_by"`
	ActionAt            *time.Time `json:"action_at,omitempty"`
	ActionNote          string    `json:"action_note"`
	CreatedAt           time.Time `json:"created_at"`
}

// ConfigSnap 配置变更快照 (freshcheck_config_snap) — 审计
type ConfigSnap struct {
	ID           int64      `json:"id"`
	TableName    string     `json:"table_name"`
	RowPK        string     `json:"row_pk"`
	Action       string     `json:"action"` // 'create' | 'update' | 'delete'
	OldValue     string     `json:"old_value"` // JSONB
	NewValue     string     `json:"new_value"` // JSONB
	ChangedBy    string     `json:"changed_by"`
	ChangedAt    time.Time  `json:"changed_at"`
	RollbackTo   *time.Time `json:"rollback_to,omitempty"`
	RollbackBy   string     `json:"rollback_by"`
}

// ============== 6. 业务响应 struct (HTTP API 用) ==============

// PoolStateItem 池状态重建 (GET /pool/state) 单 SKU
//   算法用 (内部累加字段) + API response 字段
type PoolStateItem struct {
	ItemNo          string    `json:"item_no"`
	ItemName        string    `json:"item_name"`
	InWeightKg      float64   `json:"in_weight_kg"`
	OutWeightKg     float64   `json:"out_weight_kg"`
	SpoiledWeightKg float64   `json:"spoiled_weight_kg"` // 业务独立: 报损量不计 in/out
	CurrentWeightKg float64   `json:"current_weight_kg"`
	Confidence      string    `json:"confidence"`
	LastEventTime   time.Time `json:"last_event_time"`
	HasOpenIn       bool      `json:"has_open_in"`  // 漏录标记
	HasOpenOut      bool      `json:"has_open_out"` // 漏录标记
}

// PoolState 池状态 (任意时刻)
type PoolState struct {
	PoolCode        string          `json:"pool_code"`
	PoolName        string          `json:"pool_name"`
	At              time.Time       `json:"at"`
	Items           []PoolStateItem `json:"items"`
	InTotalKg       float64         `json:"in_total_kg"`
	OutTotalKg      float64         `json:"out_total_kg"`
	SpoiledTotalKg  float64         `json:"spoiled_total_kg"`
	CurrentTotalKg  float64         `json:"current_total_kg"`
	MissingInCount  int             `json:"missing_in_count"`
}

// SettleRequest 触发结算请求
type SettleRequest struct {
	BranchNo       string             `json:"branch_no"`
	TrackCode      string             `json:"track_code"`
	PeriodEnd      time.Time          `json:"period_end"`
	PeriodStock    []PeriodStockInput `json:"period_stock"`
	Operator       string             `json:"operator"`
	Override       bool               `json:"override"`
	OverrideReason string             `json:"override_reason"`
}

// SettleResult 结算结果汇总
type SettleResult struct {
	PeriodID     int64               `json:"period_id"`
	TrackCode    string              `json:"track_code"`
	WindowStart  time.Time           `json:"window_start"`
	WindowEnd    time.Time           `json:"window_end"`
	WindowDays   int                 `json:"window_days"`
	Summary      SettleSummary       `json:"summary"`
	Alerts       []Alert             `json:"alerts"`
	ConservationOK bool              `json:"conservation_ok"`
}

// SettleSummary 结算汇总
type SettleSummary struct {
	ItemsCount         int     `json:"items_count"`
	TotalPurchaseQty   float64 `json:"total_purchase_qty"`
	TotalNormalSaleAmt float64 `json:"total_normal_sale_amt"`
	TotalPoolAllocAmt  float64 `json:"total_pool_alloc_amt"`
	TotalLossAmt       float64 `json:"total_loss_amt"`
	TotalBoxLossAmt    float64 `json:"total_box_loss_amt"`
	TotalGrossProfit   float64 `json:"total_gross_profit"`
	GrossProfitRate    float64 `json:"gross_profit_rate"`
}

// PoolInRequest 入框事件请求
type PoolInRequest struct {
	PoolCode   string    `json:"pool_code"`
	ItemNo     string    `json:"item_no"`
	EventTime  *time.Time `json:"event_time,omitempty"` // 缺省=now
	WeightKg   float64   `json:"weight_kg"`
	PieceCount int       `json:"piece_count"`
	Operator   string    `json:"operator"`
	Source     string    `json:"source"`
	Note       string    `json:"note"`
}

// PoolOutRequest 出框事件请求 (含兜底补录)
type PoolOutRequest struct {
	PoolCode  string `json:"pool_code"`
	EventTime time.Time `json:"event_time"` // 早晨检查时刻
	Items     []PoolOutItem `json:"items"`
	Operator  string `json:"operator"`
}

// PoolOutItem 出框单 SKU
type PoolOutItem struct {
	ItemNo           string  `json:"item_no"`
	WeightKg         float64 `json:"weight_kg"`           // 剩余
	OutDestination   string  `json:"out_destination"`
	SpoiledWeightKg  float64 `json:"spoiled_weight_kg"`   // 仅 destination=spoiled 时填
	DowngradeTo      string  `json:"downgrade_to"`        // 仅 destination=downgrade 时填
}

// PoolInResponse 入框响应
type PoolInResponse struct {
	ID                int64     `json:"id"`
	EventTime         time.Time `json:"event_time"`
	RecordedAt        time.Time `json:"recorded_at"`
	Confidence        string    `json:"confidence"`
	PoolStateAfter    PoolState `json:"pool_state_after"`
}

// PoolOutResponse 出框响应
type PoolOutResponse struct {
	EventsCreated        int          `json:"events_created"`
	MissingInRecords     []MissingRec `json:"missing_in_records"`
	BoxDiffKg            float64      `json:"box_diff_kg"`
	NeedsLowConfidence   bool         `json:"needs_low_confidence"`
}

// MissingRec 漏录兜底
type MissingRec struct {
	ItemNo             string    `json:"item_no"`
	SuggestedInWeight  float64   `json:"suggested_in_weight"`
	SuggestedInTime    time.Time `json:"suggested_in_time"`
}

// ============== W3.1 周期盘点 Request/Response ==============

// PeriodStockCreateRequest 创建周期盘点行
//   强校验:
//     - period_id > 0
//     - item_no 非特价码 (∉ freshcheck_pool_code.pool_code, C8)
//     - qty >= 0
//     - operator 非空
//   业务:
//     - stock_time 缺省=now
//     - source 缺省='h5' (前端录入)
//     - confidence 缺省='high', 补录走另一端点 (W3.4 加)
type PeriodStockCreateRequest struct {
	PeriodID  int64     `json:"period_id"`
	ItemNo    string    `json:"item_no"`
	ItemName  string    `json:"item_name"`
	Qty       float64   `json:"qty"`
	Unit      string    `json:"unit"`
	StockTime time.Time `json:"stock_time"` // RFC3339, 零值=now
	Operator  string    `json:"operator"`
	Note      string    `json:"note"`
}

// PeriodStockUpdateRequest 更新周期盘点 (只能改这些字段, 不能改 period_id/item_no)
//   业务: 期末盘点错了, 补录/重盘
type PeriodStockUpdateRequest struct {
	ItemName string  `json:"item_name"`
	Qty      float64 `json:"qty"`
	Unit     string  `json:"unit"`
	Note     string  `json:"note"`
}

// PeriodStockCoverage 盘点覆盖率 (C8 校验)
//   coverage_pct = (已盘 sku 数 / 应盘 sku 数) * 100
//   业务: 覆盖率 < 100 触发告警 (c8_period_stock_coverage_pct 阈值, 默认 100)
type PeriodStockCoverage struct {
	PeriodID     int64   `json:"period_id"`
	Counted      int     `json:"counted"`      // 已盘 SKU 数
	Total        int     `json:"total"`        // 应盘 SKU 数 (sku_map.is_active=TRUE)
	Missing      []string `json:"missing"`     // 未盘 SKU item_no 列表
	CoveragePct  float64 `json:"coverage_pct"`
	MeetsC8      bool    `json:"meets_c8"`     // coverage_pct >= 阈值
}

// ============== W3.2 倒挤 + 报损剥离 ==============

// BackflushItem 单 SKU 倒挤结果 (W3.2 Step 3 + Step 4)
//   公式: backflush = begin + purchase - normal_sale - end
//         loss       = fast: purchase × daily × min(life, days)
//                      slow: avg_stock × daily × days
//         pool_adj   = max(backflush - loss, 0)  // 可归因到特价池的量
//
//   业务:
//     - normal_sale 已扣退货 (sales_with_refund.is_refund=0 净销售)
//     - end_qty 缺则该 SKU 跳过 (C8 阻断在更高层处理, 这里只标 missing_end=true)
//     - negative backflush 提示销售异常, 不阻断 (W3.5 校验告警)
type BackflushItem struct {
	ItemNo         string  `json:"item_no"`
	ItemName       string  `json:"item_name"`
	FreshCategory  string  `json:"fresh_category"`
	TurnoverClass  string  `json:"turnover_class"`
	ShelfLifeDays  int     `json:"shelf_life_days"`
	BeginQty       float64 `json:"begin_qty"`
	PurchaseQty    float64 `json:"purchase_qty"`
	NormalSaleQty  float64 `json:"normal_sale_qty"`
	EndQty         float64 `json:"end_qty"`
	Backflush      float64 `json:"backflush"`       // 倒挤量 (可为负)
	ExpectedLoss   float64 `json:"expected_loss"`   // 期望损耗
	PoolAdjustable float64 `json:"pool_adjustable"` // max(backflush - loss, 0)
	MissingEnd     bool    `json:"missing_end"`     // 缺期末盘点 (C8 阻断)
	MissingLossRate bool   `json:"missing_loss_rate"` // 缺损耗率配置
	WindowDays     int     `json:"window_days"`
}

// BackflushResult 整期倒挤结果
type BackflushResult struct {
	BranchNo   string           `json:"branch_no"`
	PeriodID   int64            `json:"period_id"`
	WindowFrom time.Time        `json:"window_from"`
	WindowTo   time.Time        `json:"window_to"`
	WindowDays int              `json:"window_days"`
	Items      []*BackflushItem `json:"items"`
	Summary    BackflushSummary `json:"summary"`
}

// BackflushSummary 整期汇总
type BackflushSummary struct {
	SKUsCount         int     `json:"skus_count"`
	TotalBackflush    float64 `json:"total_backflush"`
	TotalExpectedLoss float64 `json:"total_expected_loss"`
	TotalPoolAdjustable float64 `json:"total_pool_adjustable"`
	MissingEndCount   int     `json:"missing_end_count"`     // C8 阻断计数
	MissingLossCount  int     `json:"missing_loss_rate_count"`
}


