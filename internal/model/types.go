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
	UseGlmOcr       bool         `json:"use_glm_ocr"`
	// OcrModel BigModel OCR tool_type: hand_write (手写) / layout_parsing (印刷) / "" (用 env 默认)
	OcrModel string `json:"ocr_model"`
	// LlmModel BigModel LLM model: glm-4-flash / glm-4-plus / "" (用 env 默认)
	LlmModel string `json:"llm_model"`
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
	ImagePath    string    `json:"image_path"`
	ImageURL     string    `json:"image_url"`
	Source       string    `json:"source"` // csharp / feishu
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
	Source       string       `json:"source"`
	CreatedAt    time.Time    `json:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at"`
}
