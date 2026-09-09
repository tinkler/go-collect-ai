package freshcheck

// reconcile_test.go: W3.6 双轨互验 + 框内对账 单测
//
// 覆盖:
//   - updateAllocDeviation 算法: 偏差 > 5% 标 needs_review
//   - buildBoxRecon: 框亏计算 + 汇总
//   - PoolAdjustableByItem helper: BackflushResult 聚合

import (
	"context"
	"testing"
	"time"
)

func TestReconcile_DeviationAboveThreshold(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownTestStore(t, s)
	svc := &Service{Store: s}
	ctx := context.Background()

	// alloc 跟 backflush 差 6 (> 5% 阈值, threshold 触发)
	periodID := int64(99999701)
	poolCode := "TEST_POOL"
	segStart := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	// 写 alloc 行
	dbAlloc := &Alloc{
		PeriodID: periodID, PoolCode: poolCode, SegmentStart: segStart, SegmentEnd: segStart.Add(24 * time.Hour),
		ItemNo: "TEST_DIFF1", AllocQty: 10.0, AllocAmt: 10.0,
	}
	if err := s.InsertAlloc(ctx, dbAlloc); err != nil {
		t.Fatalf("InsertAlloc: %v", err)
	}
	defer func() { _, _ = s.pool.Exec(ctx, `DELETE FROM freshcheck_alloc WHERE period_id = $1`, periodID) }()

	alloc := &AllocateResult{
		Allocs: []*AllocItem{
			{PoolCode: poolCode, SegmentStart: segStart, ItemNo: "TEST_DIFF1", AllocQty: 10.0, AllocAmt: 10.0},
		},
	}
	bf := &BackflushResult{
		Items: []*BackflushItem{
			{ItemNo: "TEST_DIFF1", PoolAdjustable: 4.0}, // 差 6, dev_rate = 6/10 = 60%
		},
	}
	poolSegs := []*PoolSegment{}
	if err := svc.RunReconciliation(ctx, SettleRequest{BranchNo: "TEST", PeriodID: periodID}, alloc, bf, poolSegs); err != nil {
		t.Fatalf("RunReconciliation: %v", err)
	}

	// 查 alloc 行, 验证 deviation 已写入
	got, err := s.GetAllocByKey(ctx, periodID, poolCode, segStart, "TEST_DIFF1")
	if err != nil {
		t.Fatalf("GetAllocByKey: %v", err)
	}
	if got.DeviationQty != 6.0 {
		t.Errorf("DeviationQty = %f, want 6.0", got.DeviationQty)
	}
	if got.DeviationRate == nil {
		t.Fatal("DeviationRate should be set")
	}
	if *got.DeviationRate < 0.5 {
		t.Errorf("DeviationRate = %f, want >= 0.5 (60%% 偏差)", *got.DeviationRate)
	}
	if !got.NeedsReview {
		t.Error("NeedsReview should be true (偏差 > 5%)")
	}
}

func TestReconcile_DeviationBelowThreshold(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownTestStore(t, s)
	svc := &Service{Store: s}
	ctx := context.Background()

	// alloc 跟 backflush 几乎相等 (差 0.5, dev_rate = 5%, 等于阈值不触发)
	periodID := int64(99999702)
	poolCode := "TEST_POOL_OK"
	segStart := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	dbAlloc := &Alloc{
		PeriodID: periodID, PoolCode: poolCode, SegmentStart: segStart, SegmentEnd: segStart.Add(24 * time.Hour),
		ItemNo: "TEST_DIFF2", AllocQty: 10.0, AllocAmt: 10.0,
	}
	if err := s.InsertAlloc(ctx, dbAlloc); err != nil {
		t.Fatalf("InsertAlloc: %v", err)
	}
	defer func() { _, _ = s.pool.Exec(ctx, `DELETE FROM freshcheck_alloc WHERE period_id = $1`, periodID) }()

	alloc := &AllocateResult{
		Allocs: []*AllocItem{
			{PoolCode: poolCode, SegmentStart: segStart, ItemNo: "TEST_DIFF2", AllocQty: 10.0, AllocAmt: 10.0},
		},
	}
	bf := &BackflushResult{
		Items: []*BackflushItem{
			{ItemNo: "TEST_DIFF2", PoolAdjustable: 9.5}, // 差 0.5, dev_rate = 5% (边界, 不触发)
		},
	}
	poolSegs := []*PoolSegment{}
	if err := svc.RunReconciliation(ctx, SettleRequest{BranchNo: "TEST", PeriodID: periodID}, alloc, bf, poolSegs); err != nil {
		t.Fatalf("RunReconciliation: %v", err)
	}
	got, _ := s.GetAllocByKey(ctx, periodID, poolCode, segStart, "TEST_DIFF2")
	if got.NeedsReview {
		t.Error("NeedsReview should be false (5% 边界, 不超 5%)")
	}
}

func TestReconcile_BoxRecon_WithBoxLoss(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownTestStore(t, s)
	svc := &Service{Store: s}
	ctx := context.Background()

	periodID := int64(99999703)
	segStart := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	seg := &PoolSegment{
		PoolCode:     "TEST_BOX",
		SegmentStart: segStart,
		SegmentEnd:   segStart.Add(24 * time.Hour),
		PoolPosQty:   5.0, // POS 5
		Items: []*PoolSegmentItem{
			{ItemNo: "A", InWeightKg: 10.0, OutWeightKg: 3.0, SpoiledWeightKg: 1.0, ConfidenceFactor: 1.0}, // 净 in=10-3-1=6
		},
	}
	bf := &BackflushResult{}

	if err := svc.RunReconciliation(ctx,
		SettleRequest{BranchNo: "TEST", PeriodID: periodID, Operator: "u_test"},
		&AllocateResult{}, bf, []*PoolSegment{seg}); err != nil {
		t.Fatalf("RunReconciliation: %v", err)
	}
	defer func() { _, _ = s.pool.Exec(ctx, `DELETE FROM freshcheck_box_recon WHERE period_id = $1`, periodID) }()

	// box_loss = in_total(10) - out_total(3) - pos(5) = 2
	got, err := s.GetBoxReconByKey(ctx, periodID, "TEST_BOX", segStart)
	if err != nil {
		t.Fatalf("GetBoxReconByKey: %v", err)
	}
	if got.BoxLossKg != 2.0 {
		t.Errorf("BoxLossKg = %f, want 2.0 (10-3-5)", got.BoxLossKg)
	}
	if !got.NeedsInvestigate {
		t.Error("NeedsInvestigate should be true (box_loss > 0)")
	}
	if got.ResponsibleUser != "u_test" {
		t.Errorf("ResponsibleUser = %q, want u_test", got.ResponsibleUser)
	}
}

func TestReconcile_BoxRecon_NoBoxLoss(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownTestStore(t, s)
	svc := &Service{Store: s}
	ctx := context.Background()

	periodID := int64(99999704)
	segStart := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	seg := &PoolSegment{
		PoolCode:     "TEST_BOX_OK",
		SegmentStart: segStart,
		SegmentEnd:   segStart.Add(24 * time.Hour),
		PoolPosQty:   5.0,
		Items: []*PoolSegmentItem{
			{ItemNo: "A", InWeightKg: 8.0, OutWeightKg: 3.0, SpoiledWeightKg: 0.0, ConfidenceFactor: 1.0}, // 净 5
		},
	}
	bf := &BackflushResult{}
	if err := svc.RunReconciliation(ctx,
		SettleRequest{BranchNo: "TEST", PeriodID: periodID, Operator: "u_test"},
		&AllocateResult{}, bf, []*PoolSegment{seg}); err != nil {
		t.Fatalf("RunReconciliation: %v", err)
	}
	defer func() { _, _ = s.pool.Exec(ctx, `DELETE FROM freshcheck_box_recon WHERE period_id = $1`, periodID) }()

	got, _ := s.GetBoxReconByKey(ctx, periodID, "TEST_BOX_OK", segStart)
	if got.BoxLossKg != 0.0 {
		t.Errorf("BoxLossKg = %f, want 0 (8-3-5)", got.BoxLossKg)
	}
	if got.NeedsInvestigate {
		t.Error("NeedsInvestigate should be false (box_loss=0)")
	}
}

func TestPoolAdjustableByItem(t *testing.T) {
	bf := &BackflushResult{
		Items: []*BackflushItem{
			{ItemNo: "A", PoolAdjustable: 5.0},
			{ItemNo: "B", PoolAdjustable: 3.0},
			{ItemNo: "C", PoolAdjustable: 0.0},
		},
	}
	m := bf.PoolAdjustableByItem()
	if m["A"] != 5.0 || m["B"] != 3.0 || m["C"] != 0.0 {
		t.Errorf("map = %+v, want A=5, B=3, C=0", m)
	}
	if len(m) != 3 {
		t.Errorf("len = %d, want 3", len(m))
	}
}
