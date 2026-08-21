package handler

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tinkler/collect-ai/internal/model"
	"github.com/tinkler/collect-ai/internal/parser"
	"github.com/tinkler/collect-ai/internal/parser/agent"
	"github.com/tinkler/collect-ai/internal/parser/matcher"
	"github.com/tinkler/collect-ai/internal/store"
)

// Handler 持有依赖
type Handler struct {
	UploadDir     string
	PublicBase    string
	MaxUpload     int64 // bytes
	Parser        *parser.Parser
	Agent         *agent.Client
	Sessions      *store.SessionRepo
	Templates     *store.TemplateRepo
	FuzzyDistance int // rematch 用 (旧接口保留)
	// 兜底值 (per-template 没配时, 用这几个)
	DefaultOcrModel  string
	DefaultLlmModel  string
	DefaultUseLlm    bool
	DefaultFuzzyDist int
}

// resolveTemplateConfig 根据 template_id 查 PG, 合并兜底值
//   - 优先顺序: 显式 override (customPrompt) > template.X > handler 兜底 (env)
//   - template 不存在 / template_id 为空 → 直接用兜底
//   - use_llm / fuzzy_distance 是 nullable (DB NULL / Go nil) → 区分"未配"和"显式 false / 0"
func (h *Handler) resolveTemplateConfig(ctx context.Context, templateID, customPrompt string) (effectivePrompt, effectiveOcrModel, effectiveLlmModel string, useLlm bool, fuzzyDist int, tpl *model.Template) {
	effectiveOcrModel = h.DefaultOcrModel
	effectiveLlmModel = h.DefaultLlmModel
	useLlm = h.DefaultUseLlm
	fuzzyDist = h.DefaultFuzzyDist
	if templateID == "" {
		return
	}
	t, err := h.Templates.GetByID(ctx, templateID)
	if err != nil || t == nil {
		return // 查不到也别报错, 走兜底
	}
	tpl = t
	if t.OcrModel != "" {
		effectiveOcrModel = t.OcrModel
	}
	if t.LlmModel != "" {
		effectiveLlmModel = t.LlmModel
	}
	if t.UseLlm != nil {
		useLlm = *t.UseLlm
	}
	if t.FuzzyDistance != nil {
		fuzzyDist = *t.FuzzyDistance
	}
	if customPrompt == "" {
		customPrompt = t.LlmPrompt
	}
	effectivePrompt = customPrompt
	return
}

// ============== Health ==============

func (h *Handler) Health(c *gin.Context) {
	c.JSON(200, gin.H{"status": "ok", "ts": time.Now().Unix()})
}

// ============== Suppliers ==============

func (h *Handler) ListSuppliers(c *gin.Context) {
	if err := h.Agent.Ping(); err != nil {
		c.JSON(503, gin.H{"error": "agent 不可达: " + err.Error()})
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit == 0 {
		limit = 20000
	}
	list, err := h.Agent.GetDistinctSuppliers(limit)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"suppliers": list, "count": len(list)})
}

// ============== Templates ==============

// ListTemplates 飞书端: 拉某供应商的模板 (默认 + purchase only)
func (h *Handler) ListTemplates(c *gin.Context) {
	supplier := c.Query("supplier")
	onlyDefault := c.Query("default") == "1" || c.Query("default") == "true"
	mode := c.Query("mode") // "purchase" | "inventory" | "" (auto)
	purchaseOnly := c.Query("purchase") == "1" || c.Query("purchase") == "true" || mode == "purchase"
	if mode == "" {
		// 飞书端默认只看 purchase
		purchaseOnly = true
	}
	list, err := h.Templates.ListForSupplier(c.Request.Context(), supplier, mode, onlyDefault, purchaseOnly)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"templates": list, "count": len(list)})
}

// SyncTemplates C# 端调用: 整体覆盖同步
func (h *Handler) SyncTemplates(c *gin.Context) {
	var req struct {
		Templates []model.Template `json:"templates"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	if err := h.Templates.UpsertAll(c.Request.Context(), req.Templates); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"synced": len(req.Templates)})
}

// ListAllTemplates C# 端管理界面用
func (h *Handler) ListAllTemplates(c *gin.Context) {
	list, err := h.Templates.ListAll(c.Request.Context())
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"templates": list, "count": len(list)})
}

// ============== Parse (不存库) ==============

func (h *Handler) Parse(c *gin.Context) {
	supplier := c.Query("supplier")
	mode := c.DefaultQuery("mode", "purchase")
	if supplier == "" {
		c.JSON(400, gin.H{"error": "supplier 必填 (query ?supplier=xxx)"})
		return
	}
	customPrompt := c.Query("prompt")
	templateID := c.Query("template_id")

	effectivePrompt, effectiveOcrModel, effectiveLlmModel, useLlm, fuzzyDist, _ :=
		h.resolveTemplateConfig(c.Request.Context(), templateID, customPrompt)

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(400, gin.H{"error": "未收到 file: " + err.Error()})
		return
	}
	defer file.Close()
	imgBytes, err := io.ReadAll(file)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	rows, lines, _, err := h.Parser.ParseImageBytes(c.Request.Context(), imgBytes, header.Filename,
		supplier, mode, effectivePrompt, effectiveOcrModel, effectiveLlmModel, useLlm, fuzzyDist)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{
		"supplier":       supplier,
		"mode":           mode,
		"ocr_lines":      len(lines),
		"ocr_model":      effectiveOcrModel,
		"llm_model":      effectiveLlmModel,
		"use_llm":        useLlm,
		"fuzzy_distance": fuzzyDist,
		"rows":           rows,
	})
}

// Rematch 用现有 rows (来自 OCR 解析/历史) + 新 supplier 重新跑 SkuMatcher
// 不调 OCR / LLM, 只换 SKU 库重新匹配
// body: { "rows": [{ "row_id": 1, "raw_barcode": "...", "raw_name": "...", "raw_qty": "..." }], "mode": "purchase" }
// query: ?supplier=xxx (必填)
func (h *Handler) Rematch(c *gin.Context) {
	supplier := c.Query("supplier")
	if supplier == "" {
		c.JSON(400, gin.H{"error": "supplier 必填 (query ?supplier=xxx)"})
		return
	}
	mode := c.DefaultQuery("mode", "purchase")

	var req struct {
		Rows []model.SkuRow `json:"rows"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	if len(req.Rows) == 0 {
		c.JSON(400, gin.H{"error": "rows 不能为空"})
		return
	}

	// 加载新 supplier 的 SKU
	skus, err := h.Agent.LoadSupplierSkus(supplier, 5000)
	if err != nil {
		c.JSON(500, gin.H{"error": "加载 SKU 失败: " + err.Error()})
		return
	}

	// 重新匹配
	m := matcher.New(skus, h.FuzzyDistance)
	out := make([]model.SkuRow, 0, len(req.Rows))
	for i, r := range req.Rows {
		// 转成 ParsedOcrRow
		parsed := model.ParsedOcrRow{
			Barcode: r.RawBarcode,
			Name:    r.RawName,
			QtyRaw:  r.RawQty,
			Qty:     r.Qty,
		}
		matched := m.Match(parsed, i+1)
		// 保留原 row_id, 防止前端索引错位
		matched.RowID = r.RowID
		matched.IsDeleted = r.IsDeleted
		matched.UnitPrice = r.UnitPrice
		// 用户已改 matched_* 字段: 如果新 supplier 下应该重置, 但保留 UnitPrice
		// (只重置 matched_*, 用户的 qty 修改保留)
		// 这里直接用 m.Match 的结果 (覆盖), 用户的 qty 改通过 PUT 后续保存
		if matched.Qty == nil {
			matched.Qty = r.Qty
		}
		out = append(out, matched)
	}

	// 盘点模式: 重算 StockDiff
	if mode == string(model.ModeInventory) {
		for i := range out {
			if out[i].StockQty != nil && out[i].Qty != nil {
				diff := float64(*out[i].Qty) - *out[i].StockQty
				out[i].StockDiff = &diff
				out[i].StockMismatch = diff != 0
				if out[i].StockMismatch && out[i].Status == "OK" {
					out[i].Status = "盘存差异"
				}
			}
		}
	}

	c.JSON(200, gin.H{
		"supplier":      supplier,
		"mode":          mode,
		"sku_count":     len(skus),
		"rows":          out,
		"rematched":     len(out),
		"skipped":       0,
	})
}

// ============== Sessions ==============

// CreateSession multipart 收图 + 存库
func (h *Handler) CreateSession(c *gin.Context) {
	supplier := c.Query("supplier")
	mode := c.DefaultQuery("mode", "purchase")
	templateID := c.Query("template_id")
	templateName := c.Query("template_name")
	customPrompt := c.Query("prompt")
	note := c.Query("note")
	source := c.DefaultQuery("source", "feishu")

	if supplier == "" {
		c.JSON(400, gin.H{"error": "supplier 必填"})
		return
	}

	effectivePrompt, effectiveOcrModel, effectiveLlmModel, useLlm, fuzzyDist, _ :=
		h.resolveTemplateConfig(c.Request.Context(), templateID, customPrompt)

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(400, gin.H{"error": "未收到 file: " + err.Error()})
		return
	}
	defer file.Close()
	imgBytes, err := io.ReadAll(file)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	rows, _, _, err := h.Parser.ParseImageBytes(c.Request.Context(), imgBytes, header.Filename,
		supplier, mode, effectivePrompt, effectiveOcrModel, effectiveLlmModel, useLlm, fuzzyDist)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	// 保存图片到 uploads/
	id := uuid.NewString()
	ext := filepath.Ext(header.Filename)
	if ext == "" {
		ext = ".jpg"
	}
	relPath := filepath.Join(id[:2], id+ext)
	absDir := filepath.Join(h.UploadDir, id[:2])
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		c.JSON(500, gin.H{"error": "创建上传目录失败: " + err.Error()})
		return
	}
	absPath := filepath.Join(absDir, id+ext)
	if err := os.WriteFile(absPath, imgBytes, 0o644); err != nil {
		c.JSON(500, gin.H{"error": "保存图片失败: " + err.Error()})
		return
	}
	imageURL := ""
	if h.PublicBase != "" {
		imageURL = fmt.Sprintf("%s/uploads/%s/%s", h.PublicBase, id[:2], id+ext)
	}

	s := &model.Session{
		ID:           id,
		SupplierName: supplier,
		TemplateID:   templateID,
		TemplateName: templateName,
		Mode:         model.TemplateMode(mode),
		ImagePath:    relPath,
		ImageURL:     imageURL,
		Source:       source,
		Note:         note,
		Rows:         rows,
	}
	if err := h.Sessions.Create(c.Request.Context(), s); err != nil {
		c.JSON(500, gin.H{"error": "存库失败: " + err.Error()})
		return
	}
	// 回填 row_id
	if saved, err := h.Sessions.Get(c.Request.Context(), id); err == nil && saved != nil {
		s.Rows = saved.Rows
	}
	c.JSON(200, s)
}

func (h *Handler) ListSessions(c *gin.Context) {
	supplier := c.Query("supplier")
	fromStr := c.Query("from")
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit == 0 {
		limit = 50
	}
	var from time.Time
	if fromStr != "" {
		if t, err := time.Parse("2006-01-02", fromStr); err == nil {
			from = t
		}
	}
	list, err := h.Sessions.ListSummaries(c.Request.Context(), supplier, from, limit)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"sessions": list, "count": len(list)})
}

func (h *Handler) GetSession(c *gin.Context) {
	id := c.Param("id")
	s, err := h.Sessions.Get(c.Request.Context(), id)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	if s == nil {
		c.JSON(404, gin.H{"error": "not found"})
		return
	}
	c.JSON(200, s)
}

func (h *Handler) DeleteSession(c *gin.Context) {
	id := c.Param("id")
	if err := h.Sessions.DeleteSession(c.Request.Context(), id); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"deleted": id})
}

// ============== Row 操作 ==============

func (h *Handler) UpdateRow(c *gin.Context) {
	id := c.Param("id")
	rowIDStr := c.Param("rowId")
	rowID, err := strconv.ParseInt(rowIDStr, 10, 64)
	if err != nil {
		c.JSON(400, gin.H{"error": "rowId 不合法"})
		return
	}
	var patch map[string]any
	if err := c.ShouldBindJSON(&patch); err != nil {
		c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	if err := h.Sessions.UpdateRow(c.Request.Context(), id, rowID, patch); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	// 回查返回最新行
	s, _ := h.Sessions.Get(c.Request.Context(), id)
	if s != nil {
		for _, r := range s.Rows {
			if r.RowID == rowID {
				c.JSON(200, r)
				return
			}
		}
	}
	c.JSON(200, gin.H{"updated": true})
}

func (h *Handler) DeleteRow(c *gin.Context) {
	id := c.Param("id")
	rowIDStr := c.Param("rowId")
	rowID, err := strconv.ParseInt(rowIDStr, 10, 64)
	if err != nil {
		c.JSON(400, gin.H{"error": "rowId 不合法"})
		return
	}
	if err := h.Sessions.DeleteRow(c.Request.Context(), id, rowID); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"deleted": rowID})
}

// ============== 导出 ==============

// ExportSession 采购模式 TXT (排除 is_deleted + is_new)
func (h *Handler) ExportSession(c *gin.Context) {
	id := c.Param("id")
	s, err := h.Sessions.Get(c.Request.Context(), id)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	if s == nil {
		c.JSON(404, gin.H{"error": "not found"})
		return
	}
	if s.Mode == model.ModeInventory {
		// 盘点模式: 检查差异
		var mism int
		for _, r := range s.Rows {
			if r.StockMismatch {
				mism++
			}
		}
		if mism > 0 {
			c.JSON(409, gin.H{
				"error":             "盘点模式有差异行, 请先确认",
				"stock_mismatch_count": mism,
			})
			return
		}
	}

	var sb stringBuilder
	// 头: 注释行
	sb.WriteString("# session_id: " + s.ID + "\n")
	sb.WriteString("# supplier: " + s.SupplierName + "\n")
	sb.WriteString("# created_at: " + s.CreatedAt.Format("2006-01-02 15:04:05") + "\n")
	if s.Note != "" {
		sb.WriteString("# note: " + s.Note + "\n")
	}
	exported := 0
	skipped := 0
	for _, r := range s.Rows {
		if r.IsDeleted {
			skipped++
			continue
		}
		if r.IsNew {
			skipped++
			continue
		}
		var qtyStr string
		if r.Qty != nil {
			qtyStr = strconv.Itoa(*r.Qty)
		}
		var priceStr string
		if r.UnitPrice != nil {
			priceStr = strconv.FormatFloat(*r.UnitPrice, 'f', 2, 64)
		}
		sb.WriteString(r.MatchedBarcode + "\t" + qtyStr + "\t" + priceStr + "\r\n")
		exported++
	}
	sb.WriteString(fmt.Sprintf("# exported: %d  skipped: %d\r\n", exported, skipped))

	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=collect-ai_%s.txt", s.ID))
	c.String(200, sb.String())
}

// stringBuilder 简化的字符串拼接 (避免 import strings)
type stringBuilder struct {
	buf []byte
}

func (s *stringBuilder) WriteString(str string) {
	s.buf = append(s.buf, str...)
}
func (s *stringBuilder) String() string { return string(s.buf) }
