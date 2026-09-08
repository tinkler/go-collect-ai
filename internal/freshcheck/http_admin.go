package freshcheck

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// ============== HTTP Admin 路由 (W1.4) ==============
//
// 端点清单 (跟 docs/freshcheck-h5-contract.md 1.5 节对齐):
//   GET    /config/sku-map             列所有 / 按子类过滤
//   GET    /config/sku-map/:item_no    单条查
//   PUT    /config/sku-map/:item_no    upsert
//   GET    /config/pool-codes          列所有 (按 unit_price DESC)
//   GET    /config/pool-codes/:pool    单条查
//   PUT    /config/pool-codes/:pool    upsert
//   GET    /config/loss-rates          列 (按子类过滤)
//   PUT    /config/loss-rates          upsert (新版本, 自动 close 旧版)
//   GET    /config/thresholds          列所有 KV
//   PUT    /config/thresholds/:key     改单个值
//   GET    /config/category-tracks     列所有轨道
//   PUT    /config/category-tracks/:fc  upsert (按 fresh_category)
//
// 权限: 全部走 freshcheck:config:write
// 鉴权: 走 collect-ai auth.RequirePerm 中间件 (路由层)
// 审计: Store 内部 WriteConfigSnap 自动写

// operatorFromCtx 拿操作用户 (从 gin context 提取)
//   跟 collect-ai auth.UserFromCtx 保持一致风格
func operatorFromCtx(c *gin.Context) string {
	if v, ok := c.Get("user_id"); ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	if v, ok := c.Get("username"); ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return "system"
}

// ============== 1. SKU Map ==============

// ListSkuMap GET /api/v1/freshcheck/config/sku-map?fresh_category=leaf
func (s *Store) HTTPListSkuMap(c *gin.Context) {
	branchNo := c.DefaultQuery("branch_no", "0001")
	freshCategory := c.Query("fresh_category")

	var items []*SkuMap
	var err error
	if freshCategory != "" {
		items, err = s.ListSkuMapByCategory(c.Request.Context(), branchNo, freshCategory)
	} else {
		items, err = s.ListAllSkuMap(c.Request.Context(), branchNo)
	}
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"items": items, "count": len(items), "branch_no": branchNo})
}

// GetSkuMap GET /api/v1/freshcheck/config/sku-map/:item_no
func (s *Store) HTTPGetSkuMap(c *gin.Context) {
	branchNo := c.DefaultQuery("branch_no", "0001")
	itemNo := c.Param("item_no")
	m, err := s.GetSkuMap(c.Request.Context(), branchNo, itemNo)
	if err == ErrNotFound {
		c.JSON(404, gin.H{"error": "not found"})
		return
	}
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, m)
}

// UpsertSkuMap PUT /api/v1/freshcheck/config/sku-map/:item_no
func (s *Store) HTTPUpsertSkuMap(c *gin.Context) {
	branchNo := c.DefaultQuery("branch_no", "0001")
	itemNo := c.Param("item_no")
	var req SkuMap
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	req.BranchNo = branchNo
	req.ItemNo = itemNo
	// 字段校验
	if req.FreshCategory == "" || (req.FreshCategory != "leaf" && req.FreshCategory != "root" &&
		req.FreshCategory != "aquatic" && req.FreshCategory != "meat" && req.FreshCategory != "frozen") {
		c.JSON(400, gin.H{"error": "fresh_category 必须 leaf/root/aquatic/meat/frozen"})
		return
	}
	if req.TurnoverClass != "fast" && req.TurnoverClass != "slow" {
		c.JSON(400, gin.H{"error": "turnover_class 必须 fast/slow"})
		return
	}
	if req.ShelfLifeDays < 0 {
		c.JSON(400, gin.H{"error": "shelf_life_days 必须 >= 0"})
		return
	}
	if err := s.UpsertSkuMap(c.Request.Context(), &req); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"ok": true, "item_no": itemNo})
}

// ============== 2. Pool Code ==============

// ListPoolCodes GET /api/v1/freshcheck/config/pool-codes
func (s *Store) HTTPListPoolCodes(c *gin.Context) {
	branchNo := c.DefaultQuery("branch_no", "0001")
	items, err := s.ListPoolCodes(c.Request.Context(), branchNo)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"items": items, "count": len(items), "branch_no": branchNo})
}

// GetPoolCode GET /api/v1/freshcheck/config/pool-codes/:pool
func (s *Store) HTTPGetPoolCode(c *gin.Context) {
	branchNo := c.DefaultQuery("branch_no", "0001")
	poolCode := c.Param("pool")
	p, err := s.GetPoolCode(c.Request.Context(), branchNo, poolCode)
	if err == ErrNotFound {
		c.JSON(404, gin.H{"error": "not found"})
		return
	}
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, p)
}

// UpsertPoolCode PUT /api/v1/freshcheck/config/pool-codes/:pool
func (s *Store) HTTPUpsertPoolCode(c *gin.Context) {
	branchNo := c.DefaultQuery("branch_no", "0001")
	poolCode := c.Param("pool")
	var req PoolCode
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	req.BranchNo = branchNo
	req.PoolCode = poolCode
	if req.PricingMode != "weight" && req.PricingMode != "piece" {
		c.JSON(400, gin.H{"error": "pricing_mode 必须 weight/piece"})
		return
	}
	if req.UnitPrice <= 0 {
		c.JSON(400, gin.H{"error": "unit_price 必须 > 0"})
		return
	}
	if err := s.UpsertPoolCode(c.Request.Context(), &req); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"ok": true, "pool_code": poolCode})
}

// ============== 3. Loss Rate ==============

// ListLossRates GET /api/v1/freshcheck/config/loss-rates?fresh_category=leaf&turnover_class=fast
func (s *Store) HTTPListLossRates(c *gin.Context) {
	freshCategory := c.Query("fresh_category")
	turnoverClass := c.Query("turnover_class")
	items, err := s.ListLossRates(c.Request.Context(), freshCategory, turnoverClass)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"items": items, "count": len(items)})
}

// UpsertLossRate PUT /api/v1/freshcheck/config/loss-rates
//   body: { fresh_category, turnover_class, loss_type, daily_rate, effective_from? }
func (s *Store) HTTPUpsertLossRate(c *gin.Context) {
	var req LossRate
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	if req.EffectiveFrom.IsZero() {
		req.EffectiveFrom = time.Now()
	}
	if req.DailyRate < 0 || req.DailyRate > 1 {
		c.JSON(400, gin.H{"error": "daily_rate 应在 [0, 1] 之间"})
		return
	}
	if err := s.UpsertLossRate(c.Request.Context(), &req); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"ok": true, "effective_from": req.EffectiveFrom})
}

// ============== 4. Threshold ==============

// ListThresholds GET /api/v1/freshcheck/config/thresholds
func (s *Store) HTTPListThresholds(c *gin.Context) {
	items, err := s.ListThresholds(c.Request.Context())
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"items": items, "count": len(items)})
}

// UpdateThreshold PUT /api/v1/freshcheck/config/thresholds/:key
//   body: { value: 18.0 }
func (s *Store) HTTPUpdateThreshold(c *gin.Context) {
	key := c.Param("key")
	var req struct {
		Value float64 `json:"value"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	operator := operatorFromCtx(c)
	if err := s.UpdateThreshold(c.Request.Context(), key, req.Value, operator); err != nil {
		if err == ErrNotFound {
			c.JSON(404, gin.H{"error": "threshold key 不存在"})
			return
		}
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"ok": true, "key": key, "value": req.Value})
}

// ============== 5. Category Track ==============

// ListCategoryTracks GET /api/v1/freshcheck/config/category-tracks
func (s *Store) HTTPListCategoryTracks(c *gin.Context) {
	branchNo := c.DefaultQuery("branch_no", "0001")
	items, err := s.ListTracks(c.Request.Context(), branchNo)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"items": items, "count": len(items), "branch_no": branchNo})
}

// UpsertCategoryTrack PUT /api/v1/freshcheck/config/category-tracks/:fc
//   :fc = fresh_category (leaf/root/aquatic/meat/frozen)
func (s *Store) HTTPUpsertCategoryTrack(c *gin.Context) {
	branchNo := c.DefaultQuery("branch_no", "0001")
	fc := c.Param("fc")
	var req CategoryTrack
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	req.BranchNo = branchNo
	req.FreshCategory = fc
	if req.PeriodLockDays <= 0 {
		c.JSON(400, gin.H{"error": "period_lock_days 必须 > 0"})
		return
	}
	if req.StockFreq != "daily" && req.StockFreq != "weekly" && req.StockFreq != "monthly" && req.StockFreq != "none" {
		c.JSON(400, gin.H{"error": "stock_freq 必须 daily/weekly/monthly/none"})
		return
	}
	if err := s.UpsertCategoryTrack(c.Request.Context(), &req); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"ok": true, "fresh_category": fc})
}

// ============== 健康检查 ==============

// HTTPHealth GET /api/v1/freshcheck/health
func (s *Store) HTTPHealth(c *gin.Context) {
	missing, err := s.VerifyTablesExist(c.Request.Context())
	if err != nil {
		c.JSON(503, gin.H{"error": "pg error: " + err.Error()})
		return
	}
	if len(missing) > 0 {
		c.JSON(503, gin.H{
			"error":   "missing tables",
			"missing": missing,
		})
		return
	}
	c.JSON(200, gin.H{
		"ok":      true,
		"tables":  AllFreshcheckTables,
		"count":   len(AllFreshcheckTables),
		"service": "freshcheck",
	})
}

// ============== helper ==============

// StatusIntToStr (防止 w1.4 之后的代码引用, 保留兼容)
func statusIntToStr(code int) string {
	return strconv.Itoa(code)
}

// guardAgainst 防止意外 nil deref
var _ = http.StatusOK
