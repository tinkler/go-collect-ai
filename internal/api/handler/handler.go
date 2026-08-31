package handler

import (
	"context"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tinkler/collect-ai/internal/auth"
	"github.com/tinkler/collect-ai/internal/business"
	"github.com/tinkler/collect-ai/internal/model"
	"github.com/tinkler/collect-ai/internal/parser"
	"github.com/tinkler/collect-ai/internal/parser/agent"
	"github.com/tinkler/collect-ai/internal/parser/matcher"
	"github.com/tinkler/collect-ai/internal/restock"
	"github.com/tinkler/collect-ai/internal/store"
)

// Handler 持有依赖
type Handler struct {
	UploadDir     string
	PublicBase    string
	MaxUpload     int64 // bytes
	Parser        *parser.Parser
	Agent         *agent.Client
	BusinessReg   *business.Registry // 业务字段映射(products / suppliers 跨数据源)
	Sessions      *store.SessionRepo
	Templates     *store.TemplateRepo
	RestockSvc    *restock.Service // 2026-08-28: 采购收货单附加 plan_qty
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

// ListSuppliers 拉所有 distinct 供应商(业务字段名)
//   ?datasource=erp|hbpos  不传则用当前 agent client 的数据源
//   返回:{"suppliers": [...], "count": N, "datasource": "..."}
func (h *Handler) ListSuppliers(c *gin.Context) {
	if err := h.Agent.Ping(); err != nil {
		c.JSON(503, gin.H{"error": "agent 不可达: " + err.Error()})
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit == 0 {
		limit = 20000
	}
	// 数据源启动后即固定(2026-08-31),不再接受 ?datasource= 覆盖
	ds := h.Agent.GetDataSource()
	if h.BusinessReg == nil {
		c.JSON(500, gin.H{"error": "business registry not configured"})
		return
	}
	ent, ok := h.BusinessReg.Get("suppliers")
	if !ok {
		c.JSON(500, gin.H{"error": "suppliers entity not found"})
		return
	}
	src, ok := ent.Sources[ds]
	if !ok {
		c.JSON(400, gin.H{"error": "suppliers entity has no mapping for datasource " + ds})
		return
	}
	if src.Cube == "" {
		c.JSON(400, gin.H{"error": "suppliers " + ds + " has no cube"})
		return
	}
	supplierNameRef := src.FieldRefs["supplier_name"]
	if supplierNameRef == "" {
		c.JSON(400, gin.H{"error": "supplier_name field not mapped for " + ds})
		return
	}
	measures := []string{}
	if ds == "erp" {
		if r, ok := src.FieldRefs["stock_qty"]; ok && r != "" {
			measures = []string{r}
		}
	} else {
		measures = []string{"suppliers.count"}
	}

	rows, err := h.Agent.Execute(src.Cube, measures, []string{supplierNameRef}, nil, []string{"sup_only"}, limit)
	if err != nil {
		c.JSON(500, gin.H{"error": "agent query: " + err.Error()})
		return
	}
	bizRows, err := h.BusinessReg.ToBusinessResponse("suppliers", ds, rows, []string{"supplier_name"})
	if err != nil {
		c.JSON(500, gin.H{"error": "translate response: " + err.Error()})
		return
	}
	set := make(map[string]struct{})
	for _, br := range bizRows {
		if s, ok := br["supplier_name"].(string); ok {
			s = strings.TrimSpace(s)
			if s != "" {
				set[s] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sortStrings(out)
	c.JSON(200, gin.H{
		"suppliers":  out,
		"count":      len(out),
		"datasource": ds,
	})
}

// sortStrings 内部 helper (避免引入 sort 包循环依赖)
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// ListSuppliersByBrand GET /api/v1/suppliers/by-brand?brand=xxx&datasource=xxx&limit=N
//   按品牌(产品名 contains brand)反查供应商, 按 product_count 降序
//   业务字段名 → 物理字段名由 business registry 翻译
//   返回:
//     {
//       "brand": "蒙牛",
//       "datasource": "erp",
//       "suppliers": [{"supplier_name": "汇一", "product_count": 47}, ...],
//       "count": 2
//     }
func (h *Handler) ListSuppliersByBrand(c *gin.Context) {
	brand := strings.TrimSpace(c.Query("brand"))
	if brand == "" {
		c.JSON(400, gin.H{"error": "brand 必填 (query ?brand=蒙牛)"})
		return
	}
	if err := h.Agent.Ping(); err != nil {
		c.JSON(503, gin.H{"error": "agent 不可达: " + err.Error()})
		return
	}
	if h.BusinessReg == nil {
		c.JSON(500, gin.H{"error": "business registry not configured"})
		return
	}
	// 数据源启动后即固定(2026-08-31),不再接受 ?datasource= 覆盖
	ds := h.Agent.GetDataSource()
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit == 0 {
		limit = 50000
	}

	ent, ok := h.BusinessReg.Get("products")
	if !ok {
		c.JSON(500, gin.H{"error": "products entity not found"})
		return
	}
	src, ok := ent.Sources[ds]
	if !ok {
		c.JSON(400, gin.H{"error": "products has no mapping for datasource " + ds})
		return
	}
	if src.Cube == "" {
		c.JSON(400, gin.H{"error": "products " + ds + " has no cube"})
		return
	}

	productNameRef := src.FieldRefs["product_name"]
	supplierNameRef := src.FieldRefs["supplier_name"]
	if supplierNameRef == "" {
		c.JSON(400, gin.H{"error": "supplier_name not mapped for " + ds})
		return
	}
	if productNameRef == "" {
		c.JSON(400, gin.H{"error": "product_name not mapped for " + ds})
		return
	}

	// 业务字段 → 物理 measures/dimensions
	//   dimensions 用 product_name + supplier_name (按 product 粒度取, 再 distinct supplier)
	measures := []string{}
	dimensions := []string{}
	for _, bf := range []string{"product_name", "supplier_name"} {
		ref, ok := src.FieldRefs[bf]
		if !ok || ref == "" {
			continue
		}
		if ent.Fields[bf].Type == business.FieldTypeMeasure {
			measures = append(measures, ref)
		} else {
			dimensions = append(dimensions, ref)
		}
	}
	if len(dimensions) == 0 {
		c.JSON(400, gin.H{"error": "no dimensions resolved for brand query (datasource " + ds + ")"})
		return
	}

	// 按 product_name contains XXX 过滤
	//   注意: agent 数据里 brand 字段经常是空的,所以"按品牌反查"实际语义是
	//         "商品名包含这个关键词的产品归属于哪些供应商"
	//   t_bd_item_info cube SQL 已加 supcust_flag='1' 过滤,排除客户
	filters := []map[string]any{
		{"member": productNameRef, "operator": "contains", "values": []string{brand}},
	}

	rows, err := h.Agent.Execute(src.Cube, measures, dimensions, filters, []string{"sup_only"}, limit)
	if err != nil {
		c.JSON(500, gin.H{"error": "agent query: " + err.Error()})
		return
	}

	// 内存聚合: supplier -> distinct products -> count
	type supplierAgg struct {
		Products map[string]struct{}
		Count    int
	}
	agg := make(map[string]*supplierAgg)
	for _, r := range rows {
		supplier := asAnyString(r[supplierNameRef])
		if supplier == "" {
			continue
		}
		product := asAnyString(r[productNameRef])
		a, ok := agg[supplier]
		if !ok {
			a = &supplierAgg{Products: make(map[string]struct{})}
			agg[supplier] = a
		}
		if product != "" {
			if _, exists := a.Products[product]; !exists {
				a.Products[product] = struct{}{}
				a.Count++
			}
		}
	}

	// 转 list, 按 product_count 降序
	out := make([]gin.H, 0, len(agg))
	for name, a := range agg {
		out = append(out, gin.H{
			"supplier_name": name,
			"product_count": a.Count,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		ci, _ := out[i]["product_count"].(int)
		cj, _ := out[j]["product_count"].(int)
		if ci != cj {
			return ci > cj
		}
		return out[i]["supplier_name"].(string) < out[j]["supplier_name"].(string)
	})

	c.JSON(200, gin.H{
		"brand":      brand,
		"datasource": ds,
		"suppliers":  out,
		"count":      len(out),
	})
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

	// 加载新 supplier 的 SKU(走业务层,business mapping 翻译)
	skus, err := h.loadSupplierSkusBiz(supplier, 5000)
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

// CreateSession multipart 收图(支持 1 张或多张) + 存库
//   多图: form-data 用 files[] 重复提交, 或 files (单字段多文件)
//   单图兼容: 仍可用 file 字段(向后兼容飞书 H5)
//   2026-08-28 加入多图支持
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

	// 收集所有文件(多图): 优先 files[] 数组, 其次 files 多文件, 最后单图 file
	type uploaded struct {
		header *multipart.FileHeader
		bytes  []byte
	}
	var uploads []uploaded

	// 强制 parse multipart(不调的话, MultipartForm 为 nil, 拿不到 files map)
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		// parse 失败, 退化到 file 字段
		log.Printf("[CreateSession] ParseMultipartForm err: %v, fallback to file field", err)
	}

	if files := c.Request.MultipartForm; files != nil && files.File != nil {
		if fhs, ok := files.File["files[]"]; ok && len(fhs) > 0 {
			for _, fh := range fhs {
				f, err := fh.Open()
				if err != nil {
					c.JSON(400, gin.H{"error": "打开文件失败: " + err.Error()})
					return
				}
				bytes, err := io.ReadAll(f)
				f.Close()
				if err != nil {
					c.JSON(500, gin.H{"error": err.Error()})
					return
				}
				uploads = append(uploads, uploaded{fh, bytes})
			}
		} else if fhs, ok := files.File["files"]; ok && len(fhs) > 0 {
			for _, fh := range fhs {
				f, err := fh.Open()
				if err != nil {
					c.JSON(400, gin.H{"error": "打开文件失败: " + err.Error()})
					return
				}
				bytes, err := io.ReadAll(f)
				f.Close()
				if err != nil {
					c.JSON(500, gin.H{"error": err.Error()})
					return
				}
				uploads = append(uploads, uploaded{fh, bytes})
			}
		}
	}
	if len(uploads) == 0 {
		// 单图兼容: 飞书 H5 / 旧调用方可能只发 file
		file, header, err := c.Request.FormFile("file")
		if err != nil {
			log.Printf("[CreateSession] 没有 file/files, 收到字段: %v", c.Request.MultipartForm)
			c.JSON(400, gin.H{"error": "未收到 file/files: " + err.Error()})
			return
		}
		defer file.Close()
		bytes, err := io.ReadAll(file)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		uploads = append(uploads, uploaded{header, bytes})
	}

	if len(uploads) == 0 {
		c.JSON(400, gin.H{"error": "未收到任何图片"})
		return
	}

	// 预先创建 session id, 用来组织多图目录
	id := uuid.NewString()
	bucket := id[:2]
	absDir := filepath.Join(h.UploadDir, bucket)
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		c.JSON(500, gin.H{"error": "创建上传目录失败: " + err.Error()})
		return
	}

	// 逐张图 OCR + 匹配, 合并 rows
	var allRows []model.SkuRow
	var imagePaths, imageURLs []string
	for idx, u := range uploads {
		ext := filepath.Ext(u.header.Filename)
		if ext == "" {
			ext = ".jpg"
		}
		// 多图文件命名: <id>_<idx>.<ext>
		fileName := fmt.Sprintf("%s_%d%s", id, idx, ext)
		relPath := filepath.Join(bucket, fileName)
		absPath := filepath.Join(absDir, fileName)
		if err := os.WriteFile(absPath, u.bytes, 0o644); err != nil {
			c.JSON(500, gin.H{"error": "保存图片失败: " + err.Error()})
			return
		}
		imagePaths = append(imagePaths, relPath)
		if h.PublicBase != "" {
			imageURLs = append(imageURLs, fmt.Sprintf("%s/uploads/%s/%s", h.PublicBase, bucket, fileName))
		} else {
			imageURLs = append(imageURLs, fmt.Sprintf("/uploads/%s/%s", bucket, fileName))
		}

		rows, _, _, err := h.Parser.ParseImageBytes(c.Request.Context(), u.bytes, u.header.Filename,
			supplier, mode, effectivePrompt, effectiveOcrModel, effectiveLlmModel, useLlm, fuzzyDist)
		if err != nil {
			c.JSON(500, gin.H{"error": fmt.Sprintf("OCR 第 %d 张失败: %s", idx+1, err.Error())})
			return
		}
		// 续接 seq (从已有 max 续)
		baseSeq := len(allRows)
		for i := range rows {
			rows[i].Seq = baseSeq + i + 1
		}
		allRows = append(allRows, rows...)
	}

	// 单图兼容字段(取第一张)
	imagePath := ""
	imageURL := ""
	if len(imagePaths) > 0 {
		imagePath = imagePaths[0]
	}
	if len(imageURLs) > 0 {
		imageURL = imageURLs[0]
	}

	s := &model.Session{
		ID:           id,
		SupplierName: supplier,
		TemplateID:   templateID,
		TemplateName: templateName,
		Mode:         model.TemplateMode(mode),
		ImagePath:    imagePath,
		ImageURL:     imageURL,
		ImagePaths:   imagePaths,
		ImageURLs:    imageURLs,
		Source:       source,
		Note:         note,
		Rows:         allRows,
	}
	if err := h.Sessions.Create(c.Request.Context(), s); err != nil {
		c.JSON(500, gin.H{"error": "存库失败: " + err.Error()})
		return
	}
	// 回填 row_id
	if saved, err := h.Sessions.Get(c.Request.Context(), id); err == nil && saved != nil {
		s.Rows = saved.Rows
	}
	// 采购模式 + 有 restock 服务: 附加 plan_qty
	if s.Mode == model.ModePurchase && h.RestockSvc != nil {
		_ = h.RestockSvc.AttachPlanQtyToRows(c.Request.Context(), s.SupplierName, s.Rows)
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
	// 采购模式 + 有 restock 服务: 附加 plan_qty (2026-08-28)
	if s.Mode == model.ModePurchase && h.RestockSvc != nil {
		_ = h.RestockSvc.AttachPlanQtyToRows(c.Request.Context(), s.SupplierName, s.Rows)
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

// ============== 数据源 (2026-08-31 彻底移除) ==============
//   /api/v1/datasource 路由删除,GetDataSource / SetDataSource 函数删除
//   数据源启动后即固定(从 .env / cfg),不再有任何运行时 API
//   前端不需要知道当前数据源,也不允许切换

// ============== 业务层(业务字段 ↔ 物理字段 翻译) ==============

// loadSupplierSkusBiz 用 business mapping 翻译
//   前端/parser 给业务字段名(supplier_name, barcode, ...)
//   内部翻译为物理字段,调 agent,响应再翻回业务字段名
//   返回 []model.SkuRecord 业务字段模型(供 SkuMatcher 用)
func (h *Handler) loadSupplierSkusBiz(supplierKeyword string, limit int) ([]model.SkuRecord, error) {
	if h.BusinessReg == nil {
		return nil, fmt.Errorf("business registry not configured")
	}
	ds := h.Agent.GetDataSource()
	ent, ok := h.BusinessReg.Get("products")
	if !ok {
		return nil, fmt.Errorf("products entity not found")
	}
	src, ok := ent.Sources[ds]
	if !ok {
		return nil, fmt.Errorf("products entity has no mapping for datasource %s", ds)
	}
	// 业务字段清单(只取该 ds 支持的)
	bizFields := []string{"barcode", "product_name", "supplier_id", "supplier_name", "category", "brand", "stock_qty"}
	// 物理字段清单
	measures := []string{}
	dimensions := []string{}
	for _, bf := range bizFields {
		ref, ok := src.FieldRefs[bf]
		if !ok || ref == "" {
			continue
		}
		if ent.Fields[bf].Type == business.FieldTypeMeasure {
			measures = append(measures, ref)
		} else {
			dimensions = append(dimensions, ref)
		}
	}
	// supplier_name filter
	supplierNameRef := src.FieldRefs["supplier_name"]
	if supplierNameRef == "" {
		return nil, fmt.Errorf("supplier_name not mapped for datasource %s", ds)
	}
	// 多关键词
	keywords := splitAndTrim(supplierKeyword, ";,\n\r\t ")
	if len(keywords) == 0 {
		return nil, fmt.Errorf("supplier keyword empty")
	}
	seen := make(map[string]struct{})
	var merged []model.SkuRecord
	for _, kw := range keywords {
		filters := []map[string]any{
			{"member": supplierNameRef, "operator": "contains", "values": []string{kw}},
		}
		rows, err := h.Agent.Execute(src.Cube, measures, dimensions, filters, []string{"sup_only"}, limit)
		if err != nil {
			return nil, err
		}
		// 翻回业务字段名
		bizRows, err := h.BusinessReg.ToBusinessResponse("products", ds, rows, bizFields)
		if err != nil {
			return nil, err
		}
		for _, br := range bizRows {
			r := mapToSkuRecord(br)
			if r.Barcode == "" && r.Name == "" {
				continue
			}
			key := ""
			if r.Barcode != "" {
				key = "bc:" + r.Barcode
			} else {
				key = "sn:" + r.MainSuppName + "|" + r.Name
			}
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				merged = append(merged, r)
			}
		}
	}
	return merged, nil
}

// mapToSkuRecord 业务字段 map → model.SkuRecord
func mapToSkuRecord(br map[string]any) model.SkuRecord {
	r := model.SkuRecord{
		Barcode:      asAnyString(br["barcode"]),
		Name:         asAnyString(br["product_name"]),
		MainSuppId:   asAnyString(br["supplier_id"]),
		MainSuppName: asAnyString(br["supplier_name"]),
		// SrcSheet 暂存 category(原有 model 字段复用)
		SrcSheet: asAnyString(br["category"]),
		StockQty: asAnyFloat(br["stock_qty"]),
	}
	return r
}

func asAnyString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func asAnyFloat(v any) *float64 {
	if v == nil {
		return nil
	}
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case float32:
		f = float64(x)
	case int:
		f = float64(x)
	case int64:
		f = float64(x)
	case string:
		if _, err := fmt.Sscanf(x, "%f", &f); err != nil {
			return nil
		}
	default:
		if _, err := fmt.Sscanf(fmt.Sprintf("%v", v), "%f", &f); err != nil {
			return nil
		}
	}
	return &f
}

// splitAndTrim 按多个分隔符切字符串并去重 trim
func splitAndTrim(s string, seps string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		for _, s := range seps {
			if r == s {
				return true
			}
		}
		return false
	})
	seen := make(map[string]struct{})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// ============== 业务 API(供前端直接调用) ==============

// SearchProducts GET /api/v1/products/search?supplier=xxx&limit=100
//   数据源启动后即固定(2026-08-31),不再接受 ?datasource= 覆盖
//   业务字段:barcode/product_name/supplier_id/supplier_name/category/brand/stock_qty
//   业务字段查询(前端直接调)
//   业务字段:barcode/product_name/supplier_id/supplier_name/category/brand/stock_qty
//
// 2026-09-01 权限隔离: stock_qty 字段需 inventory:view perm
//   - 无 perm → stock_qty 既不入 query measures 也不入 response
//   - meta.inv_viewable=false → 前端显"无权限"而不是"无库存字段"
func (h *Handler) SearchProducts(c *gin.Context) {
	if err := h.Agent.Ping(); err != nil {
		c.JSON(503, gin.H{"error": "agent 不可达: " + err.Error()})
		return
	}
	if h.BusinessReg == nil {
		c.JSON(500, gin.H{"error": "business registry not configured"})
		return
	}
	// 数据源启动后即固定(2026-08-31),不再接受 ?datasource= 覆盖
	ds := h.Agent.GetDataSource()
	supplier := c.Query("supplier")
	barcode := strings.TrimSpace(c.Query("barcode")) // 2026-08-31: 扫码查商品
	itemNo  := strings.TrimSpace(c.Query("item_no"))  // 别名, 跟 barcode 等价 (HBPoS 用 item_no 当 barcode)
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit == 0 {
		limit = 100
	}

	// 业务字段 → 物理 query
	ent, ok := h.BusinessReg.Get("products")
	if !ok {
		c.JSON(500, gin.H{"error": "products entity not found"})
		return
	}
	src, ok := ent.Sources[ds]
	if !ok {
		c.JSON(400, gin.H{"error": "products entity has no mapping for datasource " + ds})
		return
	}
	if src.Cube == "" {
		c.JSON(400, gin.H{"error": "products " + ds + " has no cube"})
		return
	}

	// 2026-09-01: 权限隔离 — 无 inventory:view 则不查不返回 stock_qty
	invViewable := auth.HasPerm(auth.RoleFromCtx(c), "inventory:view")

	// 默认拉所有可用业务字段
	bizFields := []string{"barcode", "product_name", "supplier_id", "supplier_name", "category", "brand", "stock_qty", "price"}
	if !invViewable {
		// 过滤掉 stock_qty: 既不进 query measures 也不进 response
		filtered := bizFields[:0]
		for _, bf := range bizFields {
			if bf != "stock_qty" {
				filtered = append(filtered, bf)
			}
		}
		bizFields = filtered
	}
	measures := []string{}
	dimensions := []string{}
	for _, bf := range bizFields {
		ref, ok := src.FieldRefs[bf]
		if !ok || ref == "" {
			continue
		}
		if ent.Fields[bf].Type == business.FieldTypeMeasure {
			measures = append(measures, ref)
		} else {
			dimensions = append(dimensions, ref)
		}
	}
	filters := []map[string]any{}
	if supplier != "" {
		supplierNameRef := src.FieldRefs["supplier_name"]
		if supplierNameRef != "" {
			filters = append(filters, map[string]any{
				"member": supplierNameRef, "operator": "contains", "values": []string{supplier},
			})
		}
	}
	// 2026-08-31: barcode / item_no 过滤 (扫码查商品, 返回 1 条精准结果)
	barcodeQuery := barcode
	if barcodeQuery == "" {
		barcodeQuery = itemNo
	}
	if barcodeQuery != "" {
		barcodeRef := src.FieldRefs["barcode"]
		if barcodeRef != "" {
			filters = append(filters, map[string]any{
				"member": barcodeRef, "operator": "equals", "values": []string{barcodeQuery},
			})
			// 精准查询: 限定 1 条
			if limit > 1 {
				limit = 1
			}
		}
	}

	rows, err := h.Agent.Execute(src.Cube, measures, dimensions, filters, []string{"sup_only"}, limit)
	if err != nil {
		c.JSON(500, gin.H{"error": "agent query: " + err.Error()})
		return
	}
	bizRows, err := h.BusinessReg.ToBusinessResponse("products", ds, rows, bizFields)
	if err != nil {
		c.JSON(500, gin.H{"error": "translate response: " + err.Error()})
		return
	}
	c.JSON(200, gin.H{
		"products":   bizRows,
		"count":      len(bizRows),
		"datasource": ds,
		"cube":       src.Cube,
		"meta": gin.H{
			"inv_viewable": invViewable,
		},
	})
}
