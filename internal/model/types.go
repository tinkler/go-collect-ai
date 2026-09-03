package model

import (
	"encoding/json"
	"time"
)

// ============== OCR ==============

// OcrWordBlock BigModel words_result 单条
//
// BigModel OCR API 实际返回的字段是嵌套的:
//   {
//     "words": "蒙牛纯牛奶",
//     "location": {"top": 120, "left": 200, "width": 80, "height": 32},
//     "probability": {"average": 0.99, "min": 0.99, "variance": 0}
//   }
//
// 之前 (Phase A 之前) JSON tag 用的是扁平 top/left,导致坐标全是 0,
// ParseOcrResponse 把所有 block 合并成 1 行. 2026-09-02 e2e 修.
//
// 字段直接放 OcrWordBlock 里 (Top/Left/Width/Height/Average), 用 UnmarshalJSON
// 从嵌套对象 location.* 解出来
type OcrWordBlock struct {
	Words   string  `json:"words"`
	Top     int     `json:"top"`
	Left    int     `json:"left"`
	Width   int     `json:"width"`
	Height  int     `json:"height"`
	Average float64 `json:"average,omitempty"`
}

// UnmarshalJSON 把 BigModel 嵌套的 location 字段展开到 OcrWordBlock 扁平字段
//   调用方 lines.Blocks[i].Top 仍然可用,JSON 序列化保持原样
func (b *OcrWordBlock) UnmarshalJSON(data []byte) error {
	type alias OcrWordBlock // 避免递归调 UnmarshalJSON
	var raw struct {
		*alias
		Location *struct {
			Top    int `json:"top"`
			Left   int `json:"left"`
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"location"`
		Probability *struct {
			Average float64 `json:"average"`
			Min     float64 `json:"min"`
			Variance float64 `json:"variance"`
		} `json:"probability"`
	}
	raw.alias = (*alias)(b)
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.Location != nil {
		b.Top = raw.Location.Top
		b.Left = raw.Location.Left
		b.Width = raw.Location.Width
		b.Height = raw.Location.Height
	}
	if raw.Probability != nil {
		b.Average = raw.Probability.Average
	}
	return nil
}

// OcrLine 按 top 分行聚合
type OcrLine struct {
	Top    int             `json:"top"`
	Blocks []OcrWordBlock `json:"blocks"`
}

// ParsedOcrRow 启发式或 LLM 解析的 1 行
type ParsedOcrRow struct {
	Barcode string `json:"barcode,omitempty"`
	Name    string `json:"name,omitempty"`
	QtyRaw  string `json:"qty_raw,omitempty"`
	Qty     *int   `json:"qty,omitempty"`
}

// ============== 匹配 ==============

// SkuRecord 来自 agent 的商品
type SkuRecord struct {
	Barcode      string  `json:"barcode"`
	Name         string  `json:"name"`
	MainSuppId   string  `json:"main_supp_id"`
	MainSuppName string  `json:"main_supp_name"`
	SrcSheet     string  `json:"src_sheet"`
	StockQty     *float64 `json:"stock_qty,omitempty"`
}

// SkuRow 表格行 (最终展示)
type SkuRow struct {
	RowID         int64    `json:"row_id"`        // DB id, PUT/DELETE 用
	Seq           int      `json:"seq"`
	RawBarcode    string   `json:"raw_barcode"`
	RawName       string   `json:"raw_name"`
	RawQty        string   `json:"raw_qty"`
	MatchedBarcode string  `json:"matched_barcode"`
	MatchedName   string   `json:"matched_name"`
	MatchedSupp   string   `json:"matched_supp"`
	MatchedSrc    string   `json:"matched_src"`
	Qty           *int     `json:"qty"`
	UnitPrice     *float64 `json:"unit_price,omitempty"`
	Status        string   `json:"status"`
	IsNew         bool     `json:"is_new"`
	IsDeleted     bool     `json:"is_deleted,omitempty"`
	StockQty      *float64 `json:"stock_qty,omitempty"`
	StockDiff     *float64 `json:"stock_diff,omitempty"`
	StockMismatch bool     `json:"stock_mismatch,omitempty"`
	// 采购计划参考(2026-08-28 加入,按识别 SKU 反查 restock_need_purchase)
	// 仅在 GetSession 响应里填充,不入库
	PlanItemNo   string `json:"plan_item_no,omitempty"`
	PlanItemName string `json:"plan_item_name,omitempty"`
	PlanBarcode  string `json:"plan_barcode,omitempty"`
	PlanQty      *int   `json:"plan_qty,omitempty"`
}

// ============== 模板 (Phase A 已删除,2026-09-02) ==============
//   - Template 表已删 (见 docs/ocr-purchase-skill-architecture.md §三)
//   - 替代: per-supplier supplier_parse_strategy 表 (一户一条特定策略 + LLM 拼 hints)
//   - ModeInventory 已删 (盘点单不再解析)

// Mode 解析模式 (Phase A 之后只保留 purchase,ModeInventory 已废)
type Mode string

const (
	ModePurchase Mode = "purchase"
)

// ============== 供应商特定解析策略 (Phase A, 2026-09-02) ==============

// Strategy 一家供应商的特定 OCR 解析策略(0 或 1 条,无则走通用 skill 路径)
//   - body + llm_prompt_overlay 是 LLM 友好的自由文本,split_hint 是机器友好 JSON
//   - edit_count 累计人工修正次数,达到 3 触发 optimize-parse-strategy skill (Phase B)
//   - generic_apply_count 累计通用解析次数,达到 5 触发自动建策略 (Phase B)
type Strategy struct {
	SupplierName        string         `json:"supplier_name"`
	IsHandwrite         bool           `json:"is_handwrite"`
	Enabled             bool           `json:"enabled"`
	Body                string         `json:"body"`
	SkuHints            map[string]any `json:"sku_hints"`
	LlmPromptOverlay    string         `json:"llm_prompt_overlay"`
	StrategyVersion     int            `json:"strategy_version"`
	GenericApplyCount   int            `json:"generic_apply_count"`
	EditCount           int            `json:"edit_count"`
	CreatedAt           time.Time      `json:"created_at"`
	LastEditedAt        *time.Time     `json:"last_edited_at,omitempty"`
	LastAutoOptimizedAt *time.Time     `json:"last_auto_optimized_at,omitempty"`
	LastAppliedAt       *time.Time     `json:"last_applied_at,omitempty"`
	Note                string         `json:"note,omitempty"`
}

// ============== 会话 ==============

type Session struct {
	ID           string    `json:"id"`
	SupplierName string    `json:"supplier_name"`
	// 2026-09-02 Phase A: 删 TemplateID / TemplateName,改为 StrategyVersion
	//   0 = 走通用 skill 解析;>0 = 走了该 supplier 的特定 strategy
	StrategyVersion int    `json:"strategy_version"`
	Mode            Mode   `json:"mode"`
	ImagePath       string `json:"image_path"`  // 兼容: 多图时为第一张
	ImageURL        string `json:"image_url"`   // 兼容: 多图时为第一张
	ImagePaths      []string `json:"image_paths"`  // 多图: 相对路径数组
	ImageURLs       []string `json:"image_urls"`   // 多图: 完整 URL 数组
	Source          string `json:"source"`         // csharp / feishu / wecom_h5
	Note            string `json:"note,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Rows            []SkuRow  `json:"rows,omitempty"`
	// 2026-09-01 W3.2: 智能提醒 (purchasealert 规则引擎产出)
	Alerts       []AlertItem `json:"alerts,omitempty"`
}

// AlertItem 采购订单智能提醒 (W3.2)
//   跟 purchasealert.Alert 同字段,放 model 包避免循环依赖
type AlertItem struct {
	AlertID  int64      `json:"alert_id"`
	RowID    int64      `json:"row_id"`
	Rule     string     `json:"rule"`
	Severity string     `json:"severity"`
	Message  string     `json:"message"`
	AckedAt  *time.Time `json:"acked_at,omitempty"`
	AckedBy  string     `json:"acked_by,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

type SessionSummary struct {
	ID             string    `json:"id"`
	SupplierName   string    `json:"supplier_name"`
	StrategyVersion int      `json:"strategy_version"`
	Mode           Mode      `json:"mode"`
	RowCount       int       `json:"row_count"`
	ImageCount     int       `json:"image_count"`  // 图片张数(2026-08-28 多图)
	Source         string    `json:"source"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}
