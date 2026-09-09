package freshcheck

// downgrade_pg_test.go: W4.1 跨筐降级 PG 集成测试
//
// 验证:
//   - Store.ListDowngradeEventsInWindow: 按 target pool 分组
//   - buildAllocateInput 集成: pool_event 写入 + 跑 buildAllocateInput
//     + 验证目标 pool 有虚拟入
//
// 依赖: 真实 PG (FRESHCHECK_TEST_PG_DSN)

import (
	"context"
	"testing"
	"time"
)

func teardownDowngradeEvents(t *testing.T, s *Store) {
	t.Helper()
	_, _ = s.pool.Exec(context.Background(), `DELETE FROM freshcheck_pool_event WHERE branch_no = 'TEST' AND pool_code IN ('POOL_HIGH','POOL_LOW','POOL_OTHER')`)
	_, _ = s.pool.Exec(context.Background(), `DELETE FROM freshcheck_sku_map WHERE branch_no = 'TEST' AND item_no IN ('SKU1','SKU2','SKU3')`)
	_, _ = s.pool.Exec(context.Background(), `DELETE FROM freshcheck_pool_code WHERE branch_no = 'TEST' AND pool_code IN ('POOL_LOW','POOL_OTHER')`)
}

// ============== 1. ListDowngradeEventsInWindow 端到端 ==============

func TestListDowngradeEventsInWindow_GroupsByTarget(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownDowngradeEvents(t, s)
	ctx := context.Background()

	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)

	// 写入 5 个事件 (3 主测试 + 2 干扰项), 各时间戳不同避免 UNIQUE 冲突
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO freshcheck_pool_event (branch_no, pool_code, item_no, event_kind, event_time,
			weight_kg, piece_count, out_destination, downgrade_to, operator, confidence, source)
		VALUES
		  ('TEST', 'POOL_HIGH', 'SKU1', 'out', $1, 3.0, 0, 'downgrade', 'POOL_LOW', 'u_floor', 'high', 'h5'),
		  ('TEST', 'POOL_HIGH', 'SKU2', 'out', $2, 5.0, 0, 'downgrade', 'POOL_LOW', 'u_floor', 'high', 'h5'),
		  ('TEST', 'POOL_HIGH', 'SKU3', 'out', $3, 2.0, 0, 'downgrade', 'POOL_OTHER', 'u_floor', 'high', 'h5'),
		  ('TEST', 'POOL_HIGH', 'SKU1', 'out', $4, 1.0, 0, 'sold_out', NULL, 'u_floor', 'high', 'h5'),
		  ('TEST', 'POOL_HIGH', 'SKU1', 'out', $5, 2.0, 0, 'downgrade', '', 'u_floor', 'high', 'h5')
	`, from.Add(2*time.Hour), from.Add(2*time.Hour+5*time.Minute), from.Add(2*time.Hour+10*time.Minute), from.Add(2*time.Hour+15*time.Minute), from.Add(2*time.Hour+20*time.Minute)); err != nil {
		t.Fatalf("INSERT pool_event: %v", err)
	}

	got, err := s.ListDowngradeEventsInWindow(ctx, "TEST", from, to)
	if err != nil {
		t.Fatalf("ListDowngradeEventsInWindow: %v", err)
	}
	if len(got["POOL_LOW"]) != 2 {
		t.Errorf("POOL_LOW count = %d, want 2 (SKU1 + SKU2)", len(got["POOL_LOW"]))
	}
	if len(got["POOL_OTHER"]) != 1 {
		t.Errorf("POOL_OTHER count = %d, want 1 (SKU3)", len(got["POOL_OTHER"]))
	}
	// 干扰项确认
	for _, list := range got {
		for _, ev := range list {
			if ev.ItemNo == "SKU_BAD_EMPTY" {
				t.Error("downgrade_to=空 的事件应被过滤")
			}
		}
	}
}

// ============== 2. buildAllocateInput 端到端 (W4.1 集成) ==============

func TestBuildAllocateInput_DowngradeVirtualIn(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownDowngradeEvents(t, s)
	ctx := context.Background()

	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)

	// seed: POOL_LOW 和 POOL_OTHER 两个 pool, 各 1 SKU
	_, _ = s.pool.Exec(ctx, `
		INSERT INTO freshcheck_pool_code (branch_no, pool_code, pool_name, pricing_mode, unit_price, priority_rank, is_active)
		VALUES ('TEST', 'POOL_LOW', 'low', 'weight', 0.5, 1, TRUE),
		       ('TEST', 'POOL_OTHER', 'other', 'weight', 0.3, 2, TRUE)
	`)
	_, _ = s.pool.Exec(ctx, `
		INSERT INTO freshcheck_sku_map (branch_no, item_no, item_name, fresh_category, turnover_class, shelf_life_days, is_active)
		VALUES ('TEST', 'SKU1', 'cabbage', 'leaf', 'fast', 5, TRUE)
	`)
	_, _ = s.pool.Exec(ctx, `
		INSERT INTO freshcheck_pool_event (branch_no, pool_code, item_no, event_kind, event_time,
			weight_kg, piece_count, out_destination, downgrade_to, operator, confidence, source)
		VALUES ('TEST', 'POOL_HIGH', 'SKU1', 'out', $1, 5.0, 0, 'downgrade', 'POOL_LOW', 'u_floor', 'high', 'h5')
	`, from.Add(2*time.Hour))

	// 构造 input, 调 buildAllocateInput
	pools := []*PoolCode{
		{PoolCode: "POOL_LOW", PoolName: "low", UnitPrice: 0.5, PriorityRank: 1, IsActive: true},
		{PoolCode: "POOL_OTHER", PoolName: "other", UnitPrice: 0.3, PriorityRank: 2, IsActive: true},
	}
	bf := &BackflushResult{Items: []*BackflushItem{
		{ItemNo: "SKU1", PoolAdjustable: 10.0},
	}}
	poolSales := map[string]*PoolSalesRow{
		"POOL_LOW":   {PoolCode: "POOL_LOW", Qty: 0, Amt: 0},
		"POOL_OTHER": {PoolCode: "POOL_OTHER", Qty: 0, Amt: 0},
	}
	svc := &Service{Store: s}
	in := buildAllocateInput("TEST", 1, bf, pools, poolSales, from, to, svc)

	// 找 POOL_LOW 的 segment, 验证 SKU1 有 5kg 虚拟入
	var poolLowSeg *PoolSegment
	for _, seg := range in.PoolSegments {
		if seg.PoolCode == "POOL_LOW" {
			poolLowSeg = seg
			break
		}
	}
	if poolLowSeg == nil {
		t.Fatal("POOL_LOW segment missing")
	}
	var item1 *PoolSegmentItem
	for _, it := range poolLowSeg.Items {
		if it.ItemNo == "SKU1" {
			item1 = it
			break
		}
	}
	if item1 == nil {
		t.Fatal("SKU1 在 POOL_LOW 应有虚拟入")
	}
	if item1.InWeightKg != 5.0 {
		t.Errorf("InWeightKg = %f, want 5.0 (downgrade 5kg 虚拟入)", item1.InWeightKg)
	}
	if item1.OutWeightKg != 0 {
		t.Errorf("OutWeightKg = %f, want 0", item1.OutWeightKg)
	}
	if item1.ConfidenceFactor != 1.0 {
		t.Errorf("ConfidenceFactor = %f, want 1.0", item1.ConfidenceFactor)
	}
}