package freshcheck

// http_h5.go: W2.1b H5 / Admin HTTP 端点 (池事件 + 池状态 + 框内差值)
//
// 端点清单 (按 docs/freshcheck-h5-contract.md §1.1 + §1.3):
//   POST /pool/events/in         录入一次入框 (H5 录单用)
//   POST /pool/events/out        录入出框 (含漏录检测, 早晨检查用)
//   GET  /pool/events            查某日某框事件流
//   GET  /pool/state             重建某时刻某框在框集合
//   GET  /pool/diff              算框内差值 (in - out - pos)
//   POST /period/stock           提交期末盘点
//   GET  /period/stock/template  下载 Excel 模板
//
// RBAC:
//   pool:write (入框/出框/盘点) + settle:read (查) + 管理员/auditor

import (
	"errors"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// ============== POST /pool/events/in ==============

// HTTPRecordPoolEventIn 录入一次入框
//   body: PoolInRequest (types.go 定义)
//   权限: freshcheck:pool:write
//   响应: PoolInResponse (含 pool_state_after 重建)
func (s *Store) HTTPRecordPoolEventIn(c *gin.Context) {
	var req PoolInRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	branchNo := c.DefaultQuery("branch_no", "0001")
	operator := operatorFromCtx(c)
	if operator == "" {
		operator = req.Operator
	}
	confidence := ConfidenceHigh // 实时录入; 补录走另一端点 (W3 加)

	// req.EventTime 是 *time.Time, nil = now
	var eventTime time.Time
	if req.EventTime != nil {
		eventTime = *req.EventTime
	}

	ev, err := s.RecordPoolEventIn(c.Request.Context(),
		branchNo, req.PoolCode, req.ItemNo,
		req.WeightKg, req.PieceCount, operator, eventTime, confidence, req.Note)
	if err != nil {
		status := 500
		if err == ErrDuplicateKey {
			status = 409
		} else if err == ErrInvalidInput {
			status = 400
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	// 重建当前在框集合 (响应需要)
	// ListPoolEventsByPool 是 [from, to) 区间,刚 insert 的 event 自身需要包含,
	// 所以 to 加 1ms 容差,避免 rebuilt state 漏掉当前 event
	from := ev.EventTime.Add(-24 * time.Hour)
	to := ev.EventTime.Add(time.Millisecond)
	events, _ := s.ListPoolEventsByPool(c.Request.Context(), branchNo, ev.PoolCode, from, to)
	state := RebuildPoolState(ev.PoolCode, "", ev.EventTime, events)

	c.JSON(201, PoolInResponse{
		ID:         ev.ID,
		EventTime:  ev.EventTime,
		RecordedAt: ev.RecordedAt,
		Confidence: ev.Confidence,
		PoolStateAfter: PoolState{
			PoolCode: state.PoolCode,
			At:       state.At,
			Items:    state.Items,
			InTotalKg: state.InTotalKg, OutTotalKg: state.OutTotalKg, CurrentTotalKg: state.CurrentTotalKg,
		},
	})
}

// ============== POST /pool/events/out ==============

// HTTPRecordPoolEventOut 录入出框 (含漏录检测)
//   body: PoolOutRequest (多 SKU)
//   权限: freshcheck:pool:write
//   响应: PoolOutResponse (含 missing_in_records 漏录清单)
func (s *Store) HTTPRecordPoolEventOut(c *gin.Context) {
	var req PoolOutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	branchNo := c.DefaultQuery("branch_no", "0001")
	operator := operatorFromCtx(c)
	if operator == "" {
		operator = req.Operator
	}

	created, missing, err := s.RecordPoolEventOut(c.Request.Context(),
		branchNo, req.PoolCode, req.Items, operator, req.EventTime)
	if err != nil {
		status := 500
		if err == ErrDuplicateKey {
			status = 409
		} else if err == ErrInvalidInput {
			status = 400
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	// 算 box_diff (有入框事件就可以算, 简单展示当前状态)
	from := req.EventTime.Add(-24 * time.Hour)
	events, _ := s.ListPoolEventsByPool(c.Request.Context(), branchNo, req.PoolCode, from, req.EventTime)
	boxLoss := 0.0
	for _, ev := range events {
		if ev.EventKind == PoolEventIn {
			boxLoss += ev.WeightKg
		}
	}
	for _, it := range req.Items {
		boxLoss -= it.WeightKg
		if it.OutDestination == DestSpoiled {
			boxLoss -= float64(it.SpoiledWeightKg)
		}
	}

	needsLowConf := len(missing) > 0
	resp := PoolOutResponse{
		EventsCreated:      created,
		MissingInRecords:   missing,
		BoxDiffKg:          boxLoss,
		NeedsLowConfidence: needsLowConf,
	}
	c.JSON(200, resp)
}

// ============== GET /pool/events ==============

// HTTPListPoolEvents 列某日某框事件
//   query: branch_no, pool_code, date (YYYY-MM-DD, 默认今天)
//   权限: freshcheck:settle:read
func (s *Store) HTTPListPoolEvents(c *gin.Context) {
	branchNo := c.DefaultQuery("branch_no", "0001")
	poolCode := c.Query("pool_code")
	if poolCode == "" {
		c.JSON(400, gin.H{"error": "pool_code required"})
		return
	}
	dateStr := c.Query("date")
	if dateStr == "" {
		dateStr = time.Now().Format("2006-01-02")
	}
	date, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		c.JSON(400, gin.H{"error": "date format YYYY-MM-DD"})
		return
	}
	from := date
	to := date.Add(24 * time.Hour)

	events, err := s.ListPoolEventsByPool(c.Request.Context(), branchNo, poolCode, from, to)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{
		"events":    events,
		"count":     len(events),
		"branch_no": branchNo,
		"pool_code": poolCode,
		"date":      dateStr,
	})
}

// ============== GET /pool/state ==============

// HTTPGetPoolState 重建某时刻某框在框集合
//   query: branch_no, pool_code, at (RFC3339, 默认 now)
//   权限: freshcheck:settle:read
func (s *Store) HTTPGetPoolState(c *gin.Context) {
	branchNo := c.DefaultQuery("branch_no", "0001")
	poolCode := c.Query("pool_code")
	if poolCode == "" {
		c.JSON(400, gin.H{"error": "pool_code required"})
		return
	}
	atStr := c.Query("at")
	var at time.Time
	if atStr == "" {
		at = time.Now()
	} else {
		var err error
		at, err = time.Parse(time.RFC3339, atStr)
		if err != nil {
			c.JSON(400, gin.H{"error": "at format RFC3339"})
			return
		}
	}

	// 拉窗口 [at-24h, at] 事件
	from := at.Add(-24 * time.Hour)
	events, err := s.ListPoolEventsByPool(c.Request.Context(), branchNo, poolCode, from, at)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	// 查 pool name
	pool, _ := s.GetPoolCode(c.Request.Context(), branchNo, poolCode)
	poolName := ""
	if pool != nil {
		poolName = pool.PoolName
	}

	state := RebuildPoolState(poolCode, poolName, at, events)
	c.JSON(200, state)
}

// ============== GET /pool/diff ==============

// HTTPGetPoolDiff 算框内差值
//   query: branch_no, pool_code, from, to, pos_sales_kg
//   业务: 暂由前端传 pos_sales_kg (W3 接 cube 自动拉)
//   权限: freshcheck:settle:read
func (s *Store) HTTPGetPoolDiff(c *gin.Context) {
	branchNo := c.DefaultQuery("branch_no", "0001")
	poolCode := c.Query("pool_code")
	if poolCode == "" {
		c.JSON(400, gin.H{"error": "pool_code required"})
		return
	}
	fromStr := c.DefaultQuery("from", time.Now().AddDate(0, 0, -7).Format("2006-01-02"))
	toStr := c.DefaultQuery("to", time.Now().Format("2006-01-02"))
	from, err := time.Parse("2006-01-02", fromStr)
	if err != nil {
		c.JSON(400, gin.H{"error": "from format YYYY-MM-DD"})
		return
	}
	to, err := time.Parse("2006-01-02", toStr)
	if err != nil {
		c.JSON(400, gin.H{"error": "to format YYYY-MM-DD"})
		return
	}
	posSalesKg, _ := strconv.ParseFloat(c.DefaultQuery("pos_sales_kg", "0"), 64)

	pool, _ := s.GetPoolCode(c.Request.Context(), branchNo, poolCode)
	unitPrice := 0.0
	if pool != nil {
		unitPrice = pool.UnitPrice
	}

	fromTime := from
	toTime := to.Add(24 * time.Hour)
	events, _ := s.ListPoolEventsByPool(c.Request.Context(), branchNo, poolCode, fromTime, toTime)

	r := ComputeBoxDiff(branchNo, poolCode, unitPrice, fromTime, toTime, events, posSalesKg)
	c.JSON(200, r)
}

// ============== W3.1 周期盘点 CRUD 端点 ==============
//
//   POST   /freshcheck/period/stock              创建盘点行
//   GET    /freshcheck/period/stock              列某期盘点 (query: period_id)
//   GET    /freshcheck/period/stock/coverage     算 C8 覆盖率 (query: period_id)
//   GET    /freshcheck/period/stock/:id          单条查
//   PUT    /freshcheck/period/stock/:id          更新 (只改 item_name/qty/unit/note)
//   DELETE /freshcheck/period/stock/:id          删
//
//   权限:
//     - 读 (GET):  freshcheck:settle:read
//     - 写 (POST/PUT/DELETE): freshcheck:pool:write  (实地员工录, 跟 W2 池事件一致)
//
//   强校验 (C8):
//     - item_no ∉ freshcheck_pool_code.pool_code (即不是特价码)

// HTTPCreatePeriodStock POST /freshcheck/period/stock
func (s *Store) HTTPCreatePeriodStock(c *gin.Context) {
	var req PeriodStockCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	branchNo := c.DefaultQuery("branch_no", "0001")
	operator := operatorFromCtx(c)
	if operator == "" {
		operator = req.Operator
	}

	ps := &PeriodStock{
		BranchNo:  branchNo,
		PeriodID:  req.PeriodID,
		ItemNo:    req.ItemNo,
		ItemName:  req.ItemName,
		Qty:       req.Qty,
		Unit:      req.Unit,
		StockTime: req.StockTime,
		Operator:  operator,
		Note:      req.Note,
	}
	if err := s.CreatePeriodStock(c.Request.Context(), ps); err != nil {
		status := 500
		switch {
		case errors.Is(err, ErrDuplicateKey):
			status = 409
		case errors.Is(err, ErrInvalidInput):
			status = 400
		case errors.Is(err, ErrNotFound):
			status = 404
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(201, ps)
}

// HTTPListPeriodStocks GET /freshcheck/period/stock?period_id=...
func (s *Store) HTTPListPeriodStocks(c *gin.Context) {
	branchNo := c.DefaultQuery("branch_no", "0001")
	periodIDStr := c.Query("period_id")
	if periodIDStr == "" {
		c.JSON(400, gin.H{"error": "period_id required"})
		return
	}
	periodID, err := strconv.ParseInt(periodIDStr, 10, 64)
	if err != nil || periodID <= 0 {
		c.JSON(400, gin.H{"error": "period_id must be positive int"})
		return
	}
	items, err := s.ListPeriodStocksByPeriod(c.Request.Context(), branchNo, periodID)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{
		"branch_no": branchNo,
		"period_id": periodID,
		"count":     len(items),
		"items":     items,
	})
}

// HTTPCheckPeriodStockCoverage GET /freshcheck/period/stock/coverage?period_id=...
func (s *Store) HTTPCheckPeriodStockCoverage(c *gin.Context) {
	branchNo := c.DefaultQuery("branch_no", "0001")
	periodIDStr := c.Query("period_id")
	if periodIDStr == "" {
		c.JSON(400, gin.H{"error": "period_id required"})
		return
	}
	periodID, err := strconv.ParseInt(periodIDStr, 10, 64)
	if err != nil || periodID <= 0 {
		c.JSON(400, gin.H{"error": "period_id must be positive int"})
		return
	}
	cov, err := s.CheckPeriodStockCoverage(c.Request.Context(), branchNo, periodID)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, cov)
}

// HTTPGetPeriodStock GET /freshcheck/period/stock/:id
func (s *Store) HTTPGetPeriodStock(c *gin.Context) {
	branchNo := c.DefaultQuery("branch_no", "0001")
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(400, gin.H{"error": "id must be positive int"})
		return
	}
	ps, err := s.GetPeriodStock(c.Request.Context(), branchNo, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			c.JSON(404, gin.H{"error": "period stock not found"})
			return
		}
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, ps)
}

// HTTPUpdatePeriodStock PUT /freshcheck/period/stock/:id
func (s *Store) HTTPUpdatePeriodStock(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(400, gin.H{"error": "id must be positive int"})
		return
	}
	var req PeriodStockUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	if err := s.UpdatePeriodStock(c.Request.Context(), id, req.ItemName, req.Qty, req.Unit, req.Note); err != nil {
		status := 500
		switch {
		case errors.Is(err, ErrInvalidInput):
			status = 400
		case errors.Is(err, ErrNotFound):
			status = 404
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"ok": true, "id": id})
}

// HTTPDeletePeriodStock DELETE /freshcheck/period/stock/:id
func (s *Store) HTTPDeletePeriodStock(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(400, gin.H{"error": "id must be positive int"})
		return
	}
	if err := s.DeletePeriodStock(c.Request.Context(), id); err != nil {
		if errors.Is(err, ErrNotFound) {
			c.JSON(404, gin.H{"error": "period stock not found"})
			return
		}
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"ok": true, "id": id})
}
