package model

import "time"

// ============== OCR ==============

// OcrWordBlock BigModel words_result 单条
type OcrWordBlock struct {
	Words   string `json:"words"`
	Top     int    `json:"top"`
	Left    int    `json:"left"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Average float64 `json:"average,omitempty"`
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

// ============== 模板 ==============

type TemplateMode string

const (
	ModeInventory TemplateMode = "inventory"
	ModePurchase  TemplateMode = "purchase"
)

type Template struct {
	ID              string       `json:"id"`
	Name            string       `json:"name"`
	SupplierName    string       `json:"supplier_name"`
	Mode            TemplateMode `json:"mode"`
	LlmPrompt       string       `json:"llm_prompt"`
	// OcrModel BigModel OCR tool_type: hand_write (手写) / layout_parsing (印刷) / "" (用 env 默认)
	OcrModel string `json:"ocr_model"`
	// LlmModel BigModel LLM model: glm-4-flash / glm-4-plus / "" (用 env 默认)
	LlmModel string `json:"llm_model"`
	// UseLlm 走 LLM 解析 (true) / 纯启发式 (false) / nil=用 env 默认
	//   指针是为了区分"未设置"和"显式 false"
	UseLlm *bool `json:"use_llm,omitempty"`
	// FuzzyDistance SkuMatcher 模糊匹配 Levenshtein 距离, nil=用 env 默认
	//   0 合法 (禁用模糊匹配), 所以必须用指针
	FuzzyDistance *int `json:"fuzzy_distance,omitempty"`
	HeaderKeywords  []string     `json:"header_keywords"`
	FooterKeywords  []string     `json:"footer_keywords"`
	SubtitleKeywords []string    `json:"subtitle_keywords"`
	IsDefault       bool         `json:"is_default"`
	UpdatedAt       time.Time    `json:"updated_at"`
	Note            string       `json:"note,omitempty"`
}

// ============== 会话 ==============

type Session struct {
	ID           string    `json:"id"`
	SupplierName string    `json:"supplier_name"`
	TemplateID   string    `json:"template_id"`
	TemplateName string    `json:"template_name"`
	Mode         TemplateMode `json:"mode"`
	ImagePath    string    `json:"image_path"`     // 兼容: 多图时为第一张
	ImageURL     string    `json:"image_url"`      // 兼容: 多图时为第一张
	ImagePaths   []string  `json:"image_paths"`    // 多图: 相对路径数组
	ImageURLs    []string  `json:"image_urls"`     // 多图: 完整 URL 数组
	Source       string    `json:"source"`         // csharp / feishu / wecom_h5
	Note         string    `json:"note,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Rows         []SkuRow  `json:"rows,omitempty"`
}

type SessionSummary struct {
	ID           string       `json:"id"`
	SupplierName string       `json:"supplier_name"`
	TemplateName string       `json:"template_name"`
	Mode         TemplateMode `json:"mode"`
	RowCount     int          `json:"row_count"`
	ImageCount   int          `json:"image_count"`  // 图片张数(2026-08-28 多图)
	Source       string       `json:"source"`
	CreatedAt    time.Time    `json:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at"`
}
