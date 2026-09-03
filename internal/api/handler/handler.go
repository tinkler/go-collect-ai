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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tinkler/collect-ai/internal/agent"
	"github.com/tinkler/collect-ai/internal/agent/skill"
	"github.com/tinkler/collect-ai/internal/auth"
	"github.com/tinkler/collect-ai/internal/business"
	"github.com/tinkler/collect-ai/internal/model"
	"github.com/tinkler/collect-ai/internal/parser"
	parseragent "github.com/tinkler/collect-ai/internal/parser/agent"
	"github.com/tinkler/collect-ai/internal/parser/matcher"
	"github.com/tinkler/collect-ai/internal/purchasealert"
	"github.com/tinkler/collect-ai/internal/restock"
	"github.com/tinkler/collect-ai/internal/store"
)

// Handler 持有依赖
//
// Phase A (2026-09-02): Parser → Orchestrator, TemplateRepo 移除, parse_session.template_id 字段去掉
//   - 取代原半硬编码 + template 覆盖老路
//   - 详见 docs/ocr-purchase-skill-architecture.md
type Handler struct {
	UploadDir   string
	PublicBase  string
	MaxUpload   int64 // bytes
	Orchestrator *parser.Orchestrator // Phase A: 新增, OCR + Strategy + LLM 编排
	Agent       *parseragent.Client   // 仅用于 Ping / GetDataSource (2026-09-02: cube 调用已收编到 BizExecutor)
	BizExecutor *business.Executor    // 2026-09-02: cube 业务字段调用入口
	BusinessReg *business.Registry    // 业务字段映射(products / suppliers 跨数据源)
	Pool        *pgxpool.Pool         // W4.1: GetAnalysisStatus 轻量查询
	Sessions    *store.SessionRepo
	Strategies  *store.StrategyRepo   // Phase A: 新增, per-supplier 特定解析策略
	SkillStore  *skill.Store          // Phase A: 新增, 读 skills/ocr-purchase/SKILL.md
	CashRepo    *store.CashBalanceRepo
	PayRepo     *store.SupplierPaymentRepo
	RestockSvc  *restock.Service
	AlertSvc    *purchasealert.Service
	AgentRunner *agent.Runner
	// Phase B+ (2026-09-03): 删 DefaultOcrModel/DefaultLlmModel 字段 (VLM 内部固定 glm-4v)
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
	if h.BizExecutor == nil {
		c.JSON(500, gin.H{"error": "business executor not configured"})
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit == 0 {
		limit = 20000
	}
	// 2026-09-02: 重复 Executor.DistinctSuppliers 收编
	out, err := h.BizExecutor.DistinctSuppliers(limit)
	if err != nil {
		c.JSON(500, gin.H{"error": "list suppliers: " + err.Error()})
		return
	}
	sortStrings(out)
	c.JSON(200, gin.H{
		"suppliers":  out,
		"count":      len(out),
		"datasource": h.Agent.GetDataSource(),
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
	if h.BizExecutor == nil {
		c.JSON(500, gin.H{"error": "business executor not configured"})
		return
	}
	// 数据源启动后即固定(2026-08-31),不再接受 ?datasource= 覆盖
	ds := h.Agent.GetDataSource()
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit == 0 {
		limit = 50000
	}

	// 2026-09-02: 翻译部分收编到 Executor.SearchProductsByBrand
	rows, err := h.BizExecutor.SearchProductsByBrand(brand, limit)
	if err != nil {
		c.JSON(500, gin.H{"error": "agent query: " + err.Error()})
		return
	}

	// 内存聚合: supplier -> distinct products -> count (handler 业务层,不动)
	type supplierAgg struct {
		Products map[string]struct{}
		Count    int
	}
	agg := make(map[string]*supplierAgg)
	for _, r := range rows {
		supplier := asAnyString(r["supplier_name"])
		if supplier == "" {
			continue
		}
		product := asAnyString(r["product_name"])
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

// ============== Parse (不存库) ==============

func (h *Handler) Parse(c *gin.Context) {
	supplier := c.Query("supplier")
	if supplier == "" {
		c.JSON(400, gin.H{"error": "supplier 必填 (query ?supplier=xxx)"})
		return
	}

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

	// Phase B+ (2026-09-03): VLM-only 模式, 不再传 ocr/llm model (Orchestrator 内部固定 glm-4v)
	res, err := h.Orchestrator.Parse(c.Request.Context(), imgBytes, header.Filename,
		supplier)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{
		"supplier":         supplier,
		"strategy_version": res.StrategyVersion,
		"is_handwrite":     res.IsHandwrite,
		"is_generic":       res.IsGeneric,
		"rows":             res.Rows,
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
	// Phase A: mode 参数已废,固定走 purchase 模式
	_ = c.DefaultQuery("mode", "purchase")

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

	// 重新匹配 (Phase A: FuzzyDistance 字段已删, 用 0 兜底)
	m := matcher.New(skus, 0)
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

	// Phase A (2026-09-02): 盘点模式已下线, 不再重算 StockDiff
	// 参见 docs/ocr-purchase-skill-architecture.md §三

	c.JSON(200, gin.H{
		"supplier":      supplier,
		"sku_count":     len(skus),
		"rows":          out,
		"rematched":     len(out),
		"skipped":       0,
	})
}

// ============== Sessions ==============

// AppendImages 追加图片到已有 session (W4.1 重复图去重)
//   流程:
//     1) 收图 (files[] / files / file) — 跟 CreateSession 一样
//     2) 算每张图 sha256, 调 Sessions.AppendImages (内部判重)
//     3) 重复的 hash → 跳过, 不解析, 不入 rows
//     4) 新的 → 调 Orchestrator.Parse, 续接 seq + image_index
//     5) 触发异步策略分析 (analysis_status 重置 pending → running → done)
//   前端调用: 已经有一个 session, 用户点"添加图片"+"提交识别" → POST /sessions/:id/images
//   Response 包含:
//     - added_rows: 新加的行 (含新 row_id)
//     - skipped_hashes: 已存在的 hash (UI 可提示"该图已识别过, 已跳过")
//     - analysis_status: 当前状态 (前端轮询)
func (h *Handler) AppendImages(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(400, gin.H{"error": "session id 必填"})
		return
	}

	// 1) 先确认 session 存在
	existing, err := h.Sessions.Get(c.Request.Context(), id)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	if existing == nil {
		c.JSON(404, gin.H{"error": "session not found"})
		return
	}
	supplier := existing.SupplierName

	// 2) 收图 (复用 CreateSession 一样的多图逻辑)
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		log.Printf("[AppendImages] ParseMultipartForm err: %v", err)
	}

	type uploaded struct {
		header *multipart.FileHeader
		bytes  []byte
	}
	var uploads []uploaded

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
		file, header, err := c.Request.FormFile("file")
		if err != nil {
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

	// 3) 算 hash + 落盘新文件 + 准备 candidate
	bucket := id[:2]
	absDir := filepath.Join(h.UploadDir, bucket)
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		c.JSON(500, gin.H{"error": "创建上传目录失败: " + err.Error()})
		return
	}
	candidates := make([]store.ImageCandidate, 0, len(uploads))
	for _, u := range uploads {
		hash := store.HashImageBytes(u.bytes)
		// 用 hash 前 8 位 + 原 ext 做文件名 (避免重复, 也方便人查)
		ext := filepath.Ext(u.header.Filename)
		if ext == "" {
			ext = ".jpg"
		}
		// 真正落盘在判重之后 (AppendImages 返回 skipped 时省一次写)
		// 这里先全部写, 简化逻辑
		fileName := fmt.Sprintf("%s_%s%s", id, hash[:8], ext)
		_ = filepath.Join(bucket, fileName) // 暂存引用 (后续 v2 用, 现在只用 hash 判重)
		absPath := filepath.Join(absDir, fileName)
		if _, err := os.Stat(absPath); os.IsNotExist(err) {
			if err := os.WriteFile(absPath, u.bytes, 0o644); err != nil {
				c.JSON(500, gin.H{"error": "保存图片失败: " + err.Error()})
				return
			}
		}
		candidates = append(candidates, store.ImageCandidate{
			Hash:     hash,
			FileName: u.header.Filename,
			ImgBytes: u.bytes,
		})
	}

	// 4) 调 AppendImages (内部判重 + 解析新图 + 续接 seq)
	addedRows, skippedHashes, newHashes, err := h.Sessions.AppendImages(
		c.Request.Context(), id, candidates,
		func(hash, fileName string, imgBytes []byte) ([]model.SkuRow, error) {
			res, err := h.Orchestrator.Parse(c.Request.Context(), imgBytes, fileName, supplier)
			if err != nil {
				return nil, err
			}
			return res.Rows, nil
		},
	)
	if err != nil {
		c.JSON(500, gin.H{"error": "append 失败: " + err.Error()})
		return
	}

	// 5) 触发异步策略分析 (append 后必重跑)
	if h.AlertSvc != nil {
		h.AlertSvc.StartAnalysisAsync(id, h.Sessions.Get)
	}

	// 6) 回填 row_id
	for i := range addedRows {
		if saved, err := h.Sessions.Get(c.Request.Context(), id); err == nil && saved != nil {
			// 找刚加的行 (按 Seq + ImageIndex)
			for _, r := range saved.Rows {
				if r.Seq == addedRows[i].Seq && r.ImageIndex == addedRows[i].ImageIndex && addedRows[i].RowID == 0 {
					addedRows[i].RowID = r.RowID
					break
				}
			}
		}
	}

	c.JSON(200, gin.H{
		"session_id":      id,
		"added_rows":      addedRows,
		"added_count":     len(addedRows),
		"skipped_hashes":  skippedHashes,
		"skipped_count":   len(skippedHashes),
		"new_hashes":      newHashes,
		"new_count":       len(newHashes),
		"analysis_status": "pending", // 后台分析中
	})
}

// GetAnalysisStatus 轻量状态查询 (W4.1 轮询用)
//   替代方案: 也可直接用 GET /sessions/:id 拿 analysis_status
//   但轮询时不需要拉全部 rows, 这个端点更轻
func (h *Handler) GetAnalysisStatus(c *gin.Context) {
	id := c.Param("id")
	var status, errMsg string
	var at *time.Time
	var alertCount int
	err := h.Pool.QueryRow(c.Request.Context(), `
		SELECT analysis_status, analysis_at, analysis_error
		FROM parse_session WHERE id = $1
	`, id).Scan(&status, &at, &errMsg)
	if err == pgx.ErrNoRows {
		c.JSON(404, gin.H{"error": "not found"})
		return
	}
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	_ = h.Pool.QueryRow(c.Request.Context(), `
		SELECT COUNT(*) FROM purchase_session_alert WHERE session_id = $1
	`, id).Scan(&alertCount)
	c.JSON(200, gin.H{
		"session_id":      id,
		"analysis_status": status,
		"analysis_at":     at,
		"analysis_error":  errMsg,
		"alert_count":     alertCount,
	})
}



// CreateSession multipart 收图(支持 1 张或多张) + 存库
//   多图: form-data 用 files[] 重复提交, 或 files (单字段多文件)
//   单图兼容: 仍可用 file 字段(向后兼容飞书 H5)
//   2026-08-28 加入多图支持
//
// Phase A (2026-09-02): 删 template_id / template_name / prompt 参数, 改用 Orchestrator.Parse
//   - 内部根据 supplier 查 supplier_parse_strategy 自动选 generic / specific / handwrite 路径
//   - 解析成功后记 strategy_version 到 parse_session (0 = 通用, >0 = 特定 strategy 版本)
func (h *Handler) CreateSession(c *gin.Context) {
	supplier := c.Query("supplier")
	note := c.Query("note")
	source := c.DefaultQuery("source", "feishu")

	if supplier == "" {
		c.JSON(400, gin.H{"error": "supplier 必填"})
		return
	}

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
	s := &model.Session{
		ID:           id,
		SupplierName: supplier,
		Mode:         model.ModePurchase, // Phase A: 固定 purchase
	}
	var allRows []model.SkuRow
	var imagePaths, imageURLs, imageHashes []string
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
		// W4.1: 算每张图 hash, 用于后续 append 去重
		imageHashes = append(imageHashes, store.HashImageBytes(u.bytes))

		// Phase A: 改用 Orchestrator (template_id / customPrompt 都不再需要, 走 strategy.body)
		// Phase B+ (2026-09-03): VLM-only 模式
		res, err := h.Orchestrator.Parse(c.Request.Context(), u.bytes, u.header.Filename,
			supplier)
		if err != nil {
			c.JSON(500, gin.H{"error": fmt.Sprintf("VLM 第 %d 张失败: %s", idx+1, err.Error())})
			return
		}
		// 续接 seq (从已有 max 续)
		baseSeq := len(allRows)
		rows := res.Rows
		for i := range rows {
			rows[i].Seq = baseSeq + i + 1
			// W4.1: 标记本行属于第几张图 (0-based)
			rows[i].ImageIndex = idx
		}
		allRows = append(allRows, rows...)
		// 记录本次解析的 strategy_version (多图时取第一张的版本号)
		if idx == 0 {
			s.StrategyVersion = res.StrategyVersion
		}
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

	// 填充 s 剩余字段
	s.ImagePath = imagePath
	s.ImageURL = imageURL
	s.ImagePaths = imagePaths
	s.ImageURLs = imageURLs
	s.ImageHashes = imageHashes
	s.Source = source
	s.Note = note
	s.Rows = allRows
	s.AnalysisStatus = "pending" // W4.1: 立即 pending, 后台跑分析
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
	// W4.1: 异步触发策略分析 (立即返回, 不等 LLM)
	if s.Mode == model.ModePurchase && h.AlertSvc != nil {
		h.AlertSvc.StartAnalysisAsync(id, h.Sessions.Get)
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
	// W4.1: 异步分析 — 不再同步 Apply, 直接读 alerts
	//   analysis_status='done' 时返回 alerts + summary
	//   analysis_status='pending'/'running'/'failed' 时 alerts 可能是空或旧值, 前端按 status 处理
	if s.Mode == model.ModePurchase && h.AlertSvc != nil {
		ctx := c.Request.Context()
		existing, _ := h.AlertSvc.ListAlertsBySession(ctx, id)
		// W4.1: 拆分 row-specific alerts (表格行内 icon) + session-level summary (图片卡片下)
		rowAlerts, summary := splitAlertsByScope(existing)
		s.Alerts = convertAlertsToModel(rowAlerts)
		s.Summary = convertAlertsToModel(summary)
	}
	c.JSON(200, s)
}

// splitAlertsByScope 拆分 row-specific vs session-level alerts (W4.1)
//   row_id > 0 → row-specific (表格行内 icon)
//   row_id = 0 → session-level (总结栏)
func splitAlertsByScope(in []purchasealert.Alert) (rowAlerts []purchasealert.Alert, summary []purchasealert.Alert) {
	for _, a := range in {
		if a.RowID == 0 {
			summary = append(summary, a)
		} else {
			rowAlerts = append(rowAlerts, a)
		}
	}
	return
}

// convertAlertsToModel purchasealert.Alert → model.AlertItem (避免 handler 依赖 purchasealert.Alert 类型)
// W4: 现金日报 endpoint
func (h *Handler) SetCashBalance(c *gin.Context) {
	var body struct {
		Date   string  `json:"date" binding:"required"`
		Amount float64 `json:"amount" binding:"required"`
		Source string  `json:"source"`
		Note   string  `json:"note"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	date, err := time.Parse("2006-01-02", body.Date)
	if err != nil {
		c.JSON(400, gin.H{"error": "date 格式错误 YYYY-MM-DD"})
		return
	}
	if body.Source == "" {
		body.Source = "manual"
	}
	if err := h.CashRepo.Upsert(c.Request.Context(), date, body.Amount, body.Source, body.Note, ""); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"date": body.Date, "amount": body.Amount, "source": body.Source, "note": body.Note})
}

func (h *Handler) GetCashBalance(c *gin.Context) {
	dateStr := c.Query("date")
	daysStr := c.DefaultQuery("days", "")
	if dateStr != "" {
		date, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			c.JSON(400, gin.H{"error": "date 格式错误"})
			return
		}
		cb, err := h.CashRepo.GetByDate(c.Request.Context(), date)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		if cb == nil {
			c.JSON(404, gin.H{"error": "not found"})
			return
		}
		c.JSON(200, cb)
		return
	}
	if daysStr != "" {
		days := 7
		_, _ = fmt.Sscanf(daysStr, "%d", &days)
		out, err := h.CashRepo.GetLatest(c.Request.Context(), days)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"balances": out, "count": len(out)})
		return
	}
	// default: today
	date := time.Now().UTC().Truncate(24 * time.Hour)
	cb, err := h.CashRepo.GetByDate(c.Request.Context(), date)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	if cb == nil {
		c.JSON(200, gin.H{"date": date.Format("2006-01-02"), "amount": 0, "source": "none", "note": "尚未录入"})
		return
	}
	c.JSON(200, cb)
}

// W2.5: H5 端触发 Agent 跑一轮 (复用 Runner.Run)
//   Body: { "user_id": "u1", "session_id": "s1", "message": "汇一是自采" }
//   Response: { "reply": "...", "tool_calls": [...] }
//   LLM 不可用时返降级提示 (200 OK, 不报错)
func (h *Handler) AgentChat(c *gin.Context) {
	if h.AgentRunner == nil {
		c.JSON(503, gin.H{"error": "agent runner 未配置"})
		return
	}
	var body struct {
		UserID    string `json:"user_id"`
		SessionID string `json:"session_id"`
		Message   string `json:"message" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	if strings.TrimSpace(body.Message) == "" {
		c.JSON(400, gin.H{"error": "message 必填"})
		return
	}
	if strings.TrimSpace(body.UserID) == "" {
		body.UserID = "u_" + c.ClientIP()
	}
	if strings.TrimSpace(body.SessionID) == "" {
		body.SessionID = "sess_" + body.UserID + "_" + time.Now().Format("20060102150405")
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()

	if !h.AgentRunner.Enabled() {
		c.JSON(200, gin.H{
			"reply":      "智能助理暂未配置 (需 COLLECTAI_LLM_API_KEY),无法回复。",
			"tool_calls": []string{},
			"enabled":    false,
		})
		return
	}

	events, err := h.AgentRunner.Run(ctx, body.UserID, body.SessionID, body.Message)
	if err != nil {
		log.Printf("[handler.AgentChat] runner.Run err: %v", err)
		c.JSON(200, gin.H{
			"reply":   "我没听懂,换个说法试试",
			"enabled": true,
			"error":   err.Error(),
		})
		return
	}

	var reply strings.Builder
	toolCalls := []string{}
	chunks := 0
	for ev := range events {
		if ev == nil || ev.Raw == nil {
			continue
		}
		// 抽文本 chunk
		if ev.Raw.Response != nil {
			for _, ch := range ev.Raw.Response.Choices {
				if ch.Delta.Content != "" {
					reply.WriteString(ch.Delta.Content)
					chunks++
				} else if ch.Message.Content != "" {
					reply.WriteString(ch.Message.Content)
					chunks++
				}
			}
		}
		// 抽 tool calls
		if ev.Raw.Response != nil {
			for _, ch := range ev.Raw.Response.Choices {
				if len(ch.Message.ToolCalls) > 0 {
					for _, tc := range ch.Message.ToolCalls {
						if tc.Function.Name != "" {
							toolCalls = append(toolCalls, tc.Function.Name)
						}
					}
				}
			}
		}
	}
	msg := strings.TrimSpace(reply.String())
	if msg == "" {
		msg = "我没听懂,换个说法试试"
	}
	c.JSON(200, gin.H{
		"reply":      msg,
		"tool_calls": toolCalls,
		"chunks":     chunks,
		"enabled":    true,
	})
}

func (h *Handler) ListPendingPayments(c *gin.Context) {
	if h.PayRepo == nil {
		c.JSON(503, gin.H{"error": "payment repo 未配置"})
		return
	}
	limit := 50
	if v := c.Query("limit"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	out, err := h.PayRepo.ListPending(c.Request.Context(), limit)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"suggestions": out, "count": len(out)})
}

func convertAlertsToModel(in []purchasealert.Alert) []model.AlertItem {
	if len(in) == 0 {
		return nil
	}
	out := make([]model.AlertItem, 0, len(in))
	for _, a := range in {
		it := model.AlertItem{
			AlertID:  a.AlertID,
			RowID:    a.RowID,
			Rule:     a.Rule,
			Severity: a.Severity,
			Message:  a.Message,
			AckedBy:  a.AckedBy,
			CreatedAt: a.CreatedAt,
		}
		if !a.AckedAt.IsZero() {
			t := a.AckedAt
			it.AckedAt = &t
		}
		out = append(out, it)
	}
	return out
}

func (h *Handler) DeleteSession(c *gin.Context) {
	id := c.Param("id")
	if err := h.Sessions.DeleteSession(c.Request.Context(), id); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"deleted": id})
}

// ============== Strategy (Phase A, 2026-09-02) ==============
//   per-supplier 特定解析策略, 取代旧 template
//   Phase A: 3 个端点 (GET / PUT / POST optimize) 全部可用
//     - GET  /suppliers/:name/strategy        查 (没有返 404)
//     - PUT  /suppliers/:name/strategy        改 (覆盖 body/overlay/hints/handwrite/enabled)
//     - POST /suppliers/:name/strategy/optimize 触发 LLM 优化 (Phase A: 占位,Phase B 实现)

// GetStrategy 查某 supplier 的 strategy
//   不存在 → 404 + {"exists": false} (前端可据此显示"未建"提示)
func (h *Handler) GetStrategy(c *gin.Context) {
	name := c.Param("name")
	if name == "" {
		c.JSON(400, gin.H{"error": "name 必填"})
		return
	}
	s, err := h.Strategies.GetBySupplier(c.Request.Context(), name)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	if s == nil {
		c.JSON(404, gin.H{"exists": false, "supplier": name})
		return
	}
	c.JSON(200, gin.H{"exists": true, "strategy": s})
}

// UpsertStrategy 覆盖式改 strategy (运营手动纠错用)
//   body: 完整 model.Strategy JSON (含 supplier_name 必填)
//   行为: Upsert,version 由调用方管理 (建议 +1)
//   注意: 不并发安全(Phase A 单调用方),Phase B 改乐观锁
func (h *Handler) UpsertStrategy(c *gin.Context) {
	name := c.Param("name")
	var s model.Strategy
	if err := c.ShouldBindJSON(&s); err != nil {
		c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	if s.SupplierName == "" {
		s.SupplierName = name
	}
	if s.SupplierName != name {
		c.JSON(400, gin.H{"error": "URL name 与 JSON supplier_name 不一致"})
		return
	}
	if err := h.Strategies.Upsert(c.Request.Context(), &s); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"upserted": true, "strategy": s})
}

// OptimizeStrategy 触发 LLM 优化 (Phase A: 占位,Phase B 接入 optimize-parse-strategy skill)
//   行为:
//     - 现在: 立即触发通用流程 + 记 last_auto_optimized_at
//     - Phase B: 调 runner.Run 让 LLM 读 diff + 调 invoke_skill("optimize-parse-strategy")
//   前端可手动触发或等自动阈值 (edit_count >= 3)
func (h *Handler) OptimizeStrategy(c *gin.Context) {
	name := c.Param("name")
	if name == "" {
		c.JSON(400, gin.H{"error": "name 必填"})
		return
	}
	// Phase A 占位: 只重置 edit_count,Phase B 接入完整 LLM 优化流程
	// (Phase B: 拿最近 N 次 session 的 LLM 解析结果 + 人工修正 diff, 调 optimize-parse-strategy skill)
	if err := h.Strategies.ResetEditCount(c.Request.Context(), name); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{
		"optimized":    false,
		"phase":        "A",
		"note":         "Phase A 仅重置 edit_count,Phase B 接入完整 LLM 优化",
		"supplier":     name,
	})
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
	// Phase A (2026-09-02): 盘点模式已下线, ExportSession 不再检查 StockMismatch
	// 参见 docs/ocr-purchase-skill-architecture.md §三

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
//
// 2026-09-02 重构: 翻译/调用/翻回全部收编到 Executor.SearchProducts
//   handler 只剩"业务字段 map → model.SkuRecord"和"按 keyword 去重"2 步
func (h *Handler) loadSupplierSkusBiz(supplierKeyword string, limit int) ([]model.SkuRecord, error) {
	if h.BizExecutor == nil {
		return nil, fmt.Errorf("business executor not configured")
	}
	// 多关键词去重 → 拆多次调
	keywords := splitAndTrim(supplierKeyword, ";,\n\r\t ")
	if len(keywords) == 0 {
		return nil, fmt.Errorf("supplier keyword empty")
	}
	seen := make(map[string]struct{})
	var merged []model.SkuRecord
	for _, kw := range keywords {
		// 2026-09-02: 用 Executor.SearchProducts 走 Registry 翻译
		//   注意: 原代码每个 keyword 单独 limit,合并去重
		//   简化: 用每个 keyword 调一次,跟原行为一致
		bizRows, err := h.BizExecutor.SearchProducts(kw, limit)
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
//   业务字段:barcode/product_name/supplier_id/supplier_name/category/brand/stock_qty/unit
//   业务字段查询(前端直接调)
//
// 权限隔离 (2026-09-01 库存 + 2026-09-03 供应商 + 单位):
//   - stock_qty       需 inventory:view perm (无 → 不查不返回)
//   - supplier_id /
//     supplier_name   需 supplier:view  perm (无 → 不查不返回)
//   - unit            不做权限控制(所有用户都能看商品计量单位)
//   - meta.inv_viewable / meta.supplier_viewable 告诉前端哪些字段被权限过滤
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
	// 2026-09-03: 加 supplier:view 隔离 (跟 restock 模块 permSupplierView 一致)
	role := auth.RoleFromCtx(c)
	invViewable := auth.HasPerm(role, "inventory:view")
	supplierViewable := auth.HasPerm(role, "supplier:view")

	// 默认拉所有可用业务字段
	// 2026-09-03: 加 unit 字段 (mapping.go:332 已定义 → t_bd_item_info.unit_no)
	bizFields := []string{"barcode", "product_name", "supplier_id", "supplier_name", "category", "brand", "stock_qty", "unit", "price"}
	if !invViewable || !supplierViewable {
		// 过滤掉无 perm 的敏感字段: 既不进 query measures/dimensions 也不进 response
		filtered := bizFields[:0]
		for _, bf := range bizFields {
			if !invViewable && bf == "stock_qty" {
				continue
			}
			if !supplierViewable && (bf == "supplier_id" || bf == "supplier_name") {
				continue
			}
			filtered = append(filtered, bf)
		}
		bizFields = filtered
	}

	// 2026-09-02: 翻译/Execute/翻回收编到 Executor.Query
	//   handler 只负责"业务字段 filter 拼装 + permission 切字段"
	bizFilters := []business.BusinessFilter{}
	if supplier != "" {
		bizFilters = append(bizFilters, business.BusinessFilter{
			Field: "supplier_name", Op: "contains", Values: []any{supplier},
		})
	}
	// 2026-08-31: barcode / item_no 过滤 (扫码查商品, 返回 1 条精准结果)
	barcodeQuery := barcode
	if barcodeQuery == "" {
		barcodeQuery = itemNo
	}
	if barcodeQuery != "" {
		bizFilters = append(bizFilters, business.BusinessFilter{
			Field: "barcode", Op: "equals", Values: []any{barcodeQuery},
		})
		// 精准查询: 限定 1 条
		if limit > 1 {
			limit = 1
		}
	}

	bizRows, err := h.BizExecutor.Query("products", bizFields, bizFilters, limit)
	if err != nil {
		c.JSON(500, gin.H{"error": "query: " + err.Error()})
		return
	}
	c.JSON(200, gin.H{
		"products":   bizRows,
		"count":      len(bizRows),
		"datasource": ds,
		"cube":       h.BizExecutor.CubeOf("products"),
		"meta": gin.H{
			"inv_viewable":      invViewable,
			"supplier_viewable": supplierViewable,
		},
	})
}
