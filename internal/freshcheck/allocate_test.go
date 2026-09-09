package freshcheck

// allocate_test.go: W3.3 多筐串联分摊 单测 (纯函数, 无 PG/cube)
//
// 覆盖场景:
//   1. 1 框 1 SKU (基本, 主算法)
//   2. 1 框 N SKU (主算法, 权重按框内差值)
//   3. N 框 1 SKU (串联, 高价框先分, 扣 remaining)
//   4. 物理约束: 单 SKU 倒挤不够分, 多次重分配
//   5. 主→备退化: 框内差值=0 (全出), 退到倒挤回退
//   6. Summary C1 饱和度: |total_alloc - total_pos| / total_pos
//   7. 跨框约束: total_alloc 不超过所有 SKU 剩余总和

import (
	"testing"
	"time"
)

// mkPool 建特价码
func mkPool(code string, price float64, rank int) *PoolCode {
	return &PoolCode{BranchNo: "TEST", PoolCode: code, PoolName: code, PricingMode: "weight", UnitPrice: price, PriorityRank: rank, IsActive: true}
}

// mkSeg 建 pool 段 (单 SKU, 简化版)
func mkSeg(poolCode string, posQty float64, items []*PoolSegmentItem) *PoolSegment {
	return &PoolSegment{
		PoolCode:     poolCode,
		SegmentStart: t0(),
		SegmentEnd:   t0().Add(23 * time.Hour),
		PoolPosQty:   posQty,
		PoolPosAmt:   posQty * 1.0, // 简化为 1.0
		Items:        items,
	}
}

// mkItem 建段内 SKU 状态
func mkItem(itemNo, name string, in, out, spoiled, conf float64) *PoolSegmentItem {
	return &PoolSegmentItem{
		ItemNo:           itemNo,
		ItemName:         name,
		InWeightKg:       in,
		OutWeightKg:      out,
		SpoiledWeightKg:  spoiled,
		ConfidenceFactor: conf,
	}
}

// mkBackflush 建 backflush item (只填 PoolAdjustable)
func mkBackflush(itemNo string, poolAdj float64) *BackflushItem {
	return &BackflushItem{ItemNo: itemNo, PoolAdjustable: poolAdj}
}

// t0 helper: 2026-09-08 基准
func t0() time.Time { return time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC) }

func TestAllocate_1Pool1SKU_BoxDiff(t *testing.T) {
	// 1 框 1 SKU, 框内差值 = in - out - spoiled = 10 - 2 - 1 = 7
	// pos_qty = 7 → 分摊 7
	pool := mkPool("POOL1", 1.0, 0)
	seg := mkSeg("POOL1", 7.0, []*PoolSegmentItem{
		mkItem("SKU1", "cabbage", 10, 2, 1, 1.0),
	})
	bf := []*BackflushItem{mkBackflush("SKU1", 20.0)} // 远大于 pos
	res, err := ComputeAllocate(AllocateInput{
		BranchNo: "TEST", PeriodID: 1,
		Pools: []*PoolCode{pool}, PoolSegments: []*PoolSegment{seg},
		BackflushItems: bf,
	})
	if err != nil {
		t.Fatalf("ComputeAllocate: %v", err)
	}
	if len(res.Allocs) != 1 {
		t.Fatalf("Allocs = %d, want 1", len(res.Allocs))
	}
	a := res.Allocs[0]
	if a.ItemNo != "SKU1" {
		t.Errorf("ItemNo = %s, want SKU1", a.ItemNo)
	}
	if a.AllocQty != 7.0 {
		t.Errorf("AllocQty = %f, want 7 (主算法精确分摊)", a.AllocQty)
	}
	if a.Method != "box_diff" {
		t.Errorf("Method = %s, want box_diff", a.Method)
	}
}

func TestAllocate_1PoolNSKU_BoxDiff(t *testing.T) {
	// 1 框 3 SKU, 各有不同框内差值
	// weights: SKU1=8, SKU2=4, SKU3=2 (sum=14)
	// pos_qty=14 → 按权重分: SKU1=8, SKU2=4, SKU3=2
	pool := mkPool("POOL1", 1.0, 0)
	seg := mkSeg("POOL1", 14.0, []*PoolSegmentItem{
		mkItem("SKU1", "spinach", 10, 2, 0, 1.0), // diff=8
		mkItem("SKU2", "lettuce", 6, 2, 0, 1.0),  // diff=4
		mkItem("SKU3", "tomato", 3, 1, 0, 1.0),   // diff=2
	})
	bf := []*BackflushItem{
		mkBackflush("SKU1", 50), mkBackflush("SKU2", 50), mkBackflush("SKU3", 50),
	}
	res, err := ComputeAllocate(AllocateInput{
		BranchNo: "TEST", PeriodID: 1,
		Pools: []*PoolCode{pool}, PoolSegments: []*PoolSegment{seg},
		BackflushItems: bf,
	})
	if err != nil {
		t.Fatalf("ComputeAllocate: %v", err)
	}
	got := map[string]float64{}
	for _, a := range res.Allocs {
		got[a.ItemNo] = a.AllocQty
	}
	if got["SKU1"] != 8.0 {
		t.Errorf("SKU1 alloc = %f, want 8 (8/14 × 14)", got["SKU1"])
	}
	if got["SKU2"] != 4.0 {
		t.Errorf("SKU2 alloc = %f, want 4 (4/14 × 14)", got["SKU2"])
	}
	if got["SKU3"] != 2.0 {
		t.Errorf("SKU3 alloc = %f, want 2 (2/14 × 14)", got["SKU3"])
	}
}

func TestAllocate_NPool1SKU_Cascade(t *testing.T) {
	// 2 框同 SKU, 高价框 (POOL1 1元) 先分, 剩余给 POOL2 0.5元
	// SKU1 pool_adjustable = 5
	// POOL1 pos=4, weight=4 → alloc=4, remaining=1
	// POOL2 pos=2, weight=1 (因为 remaining 变 1) → alloc=1, remaining=0
	// 总分摊 = 4 + 1 = 5 = pool_adjustable ✓
	pool1 := mkPool("POOL1", 1.0, 0) // 高价
	pool2 := mkPool("POOL2", 0.5, 1) // 低价
	seg1 := mkSeg("POOL1", 4.0, []*PoolSegmentItem{
		mkItem("SKU1", "spinach", 6, 2, 0, 1.0), // diff=4
	})
	seg2 := mkSeg("POOL2", 2.0, []*PoolSegmentItem{
		mkItem("SKU1", "spinach", 3, 1, 0, 1.0), // diff=2
	})
	bf := []*BackflushItem{mkBackflush("SKU1", 5.0)}
	res, err := ComputeAllocate(AllocateInput{
		BranchNo: "TEST", PeriodID: 1,
		Pools: []*PoolCode{pool1, pool2}, // 高价先
		PoolSegments: []*PoolSegment{seg1, seg2},
		BackflushItems: bf,
	})
	if err != nil {
		t.Fatalf("ComputeAllocate: %v", err)
	}
	if len(res.Allocs) != 2 {
		t.Fatalf("Allocs = %d, want 2", len(res.Allocs))
	}
	if res.Allocs[0].PoolCode != "POOL1" || res.Allocs[0].AllocQty != 4.0 {
		t.Errorf("POOL1 alloc = %f, want 4 (高分摊)", res.Allocs[0].AllocQty)
	}
	if res.Allocs[1].PoolCode != "POOL2" || res.Allocs[1].AllocQty != 1.0 {
		t.Errorf("POOL2 alloc = %f, want 1 (剩余 1)", res.Allocs[1].AllocQty)
	}
	if res.Summary.TotalAllocQty != 5.0 {
		t.Errorf("TotalAllocQty = %f, want 5 (= pool_adjustable)", res.Summary.TotalAllocQty)
	}
}

func TestAllocate_PhysicalConstraint_Redistribute(t *testing.T) {
	// 1 框 2 SKU, 但 pool_adjustable 远小于 pos_qty
	// pos_qty=10, weights: SKU1=8, SKU2=2
	// SKU1 pool_adj=3, SKU2 pool_adj=2
	// 初步: SKU1=8, SKU2=2 → 触发约束, SKU1 excess=5
	// 重分配: SKU2 alone, 但 SKU2 没满 (2<2 → 已满, 不会再分)
	// 结果: SKU1=3, SKU2=2, excess=5 浪费
	pool := mkPool("POOL1", 1.0, 0)
	seg := mkSeg("POOL1", 10.0, []*PoolSegmentItem{
		mkItem("SKU1", "spinach", 12, 4, 0, 1.0), // diff=8
		mkItem("SKU2", "lettuce", 4, 2, 0, 1.0),  // diff=2
	})
	bf := []*BackflushItem{
		mkBackflush("SKU1", 3.0), // 远小于 weight 8
		mkBackflush("SKU2", 2.0),
	}
	res, err := ComputeAllocate(AllocateInput{
		BranchNo: "TEST", PeriodID: 1,
		Pools: []*PoolCode{pool}, PoolSegments: []*PoolSegment{seg},
		BackflushItems: bf,
		MaxIterations: 5,
	})
	if err != nil {
		t.Fatalf("ComputeAllocate: %v", err)
	}
	got := map[string]float64{}
	for _, a := range res.Allocs {
		got[a.ItemNo] = a.AllocQty
	}
	// SKU1 截顶到 3
	if got["SKU1"] != 3.0 {
		t.Errorf("SKU1 alloc = %f, want 3 (受 pool_adj 截顶)", got["SKU1"])
	}
	// SKU2 已满 (2=2), 不再接收 excess
	if got["SKU2"] != 2.0 {
		t.Errorf("SKU2 alloc = %f, want 2 (已满)", got["SKU2"])
	}
	if res.Summary.TotalAllocQty != 5.0 {
		t.Errorf("TotalAllocQty = %f, want 5 (= 3+2)", res.Summary.TotalAllocQty)
	}
}

func TestAllocate_BackflushFallback(t *testing.T) {
	// 框内差值=0 (全出), 退化到倒挤回退
	// pos_qty=3, weights 主=0 → 退到备
	// 备算法 weights: SKU1.remaining=5, conf=1.0 → weight=5
	// alloc: 3 × 5/5 = 3
	pool := mkPool("POOL1", 1.0, 0)
	seg := mkSeg("POOL1", 3.0, []*PoolSegmentItem{
		mkItem("SKU1", "spinach", 5, 5, 0, 1.0), // in=out=5, diff=0
	})
	bf := []*BackflushItem{mkBackflush("SKU1", 5.0)}
	res, err := ComputeAllocate(AllocateInput{
		BranchNo: "TEST", PeriodID: 1,
		Pools: []*PoolCode{pool}, PoolSegments: []*PoolSegment{seg},
		BackflushItems: bf,
	})
	if err != nil {
		t.Fatalf("ComputeAllocate: %v", err)
	}
	if len(res.Allocs) != 1 {
		t.Fatalf("Allocs = %d, want 1", len(res.Allocs))
	}
	a := res.Allocs[0]
	if a.Method != "backflush_fallback" {
		t.Errorf("Method = %s, want backflush_fallback (主退化)", a.Method)
	}
	if a.AllocQty != 3.0 {
		t.Errorf("AllocQty = %f, want 3 (倒挤回退 5≥3, 全分)", a.AllocQty)
	}
}

func TestAllocate_SummaryC1(t *testing.T) {
	// C1 饱和度: 完美匹配 = 0%, meets_c1=true
	pool := mkPool("POOL1", 1.0, 0)
	seg := mkSeg("POOL1", 10.0, []*PoolSegmentItem{
		mkItem("SKU1", "spinach", 12, 2, 0, 1.0), // diff=10
	})
	bf := []*BackflushItem{mkBackflush("SKU1", 20.0)} // 远大于 10
	res, _ := ComputeAllocate(AllocateInput{
		BranchNo: "TEST", PeriodID: 1,
		Pools: []*PoolCode{pool}, PoolSegments: []*PoolSegment{seg},
		BackflushItems: bf,
	})
	if res.Summary.TotalAllocQty != 10.0 {
		t.Errorf("TotalAllocQty = %f, want 10", res.Summary.TotalAllocQty)
	}
	if res.Summary.TotalPosQty != 10.0 {
		t.Errorf("TotalPosQty = %f, want 10", res.Summary.TotalPosQty)
	}
	if res.Summary.SaturationPct != 0 {
		t.Errorf("SaturationPct = %f, want 0 (完美匹配)", res.Summary.SaturationPct)
	}
	if !res.Summary.MeetsC1 {
		t.Error("MeetsC1 should be true (0% <= 15%)")
	}
}

func TestAllocate_TotalNotExceedBackflush(t *testing.T) {
	// 跨框约束: 多框多 SKU 总分摊 ≤ pool_adjustable 总和
	pool1 := mkPool("POOL1", 1.0, 0)
	pool2 := mkPool("POOL2", 0.5, 1)
	seg1 := mkSeg("POOL1", 8.0, []*PoolSegmentItem{
		mkItem("SKU1", "spinach", 10, 2, 0, 1.0),
	})
	seg2 := mkSeg("POOL2", 3.0, []*PoolSegmentItem{
		mkItem("SKU1", "spinach", 5, 1, 0, 1.0),
	})
	bf := []*BackflushItem{mkBackflush("SKU1", 6.0)} // 总可分 6
	res, _ := ComputeAllocate(AllocateInput{
		BranchNo: "TEST", PeriodID: 1,
		Pools: []*PoolCode{pool1, pool2},
		PoolSegments: []*PoolSegment{seg1, seg2},
		BackflushItems: bf,
	})
	// 框 1 diff=8, 但 SKU1 adj=6, 应该分 6 然后 remaining=0
	// 框 2 拿不到 (remaining=0), alloc=0
	if res.Summary.TotalAllocQty > 6.0 {
		t.Errorf("TotalAllocQty = %f, should <= 6 (pool_adj)", res.Summary.TotalAllocQty)
	}
	// 验证 SKU1 在 POOL1 分了 6
	var po1Alloc float64
	for _, a := range res.Allocs {
		if a.PoolCode == "POOL1" && a.ItemNo == "SKU1" {
			po1Alloc = a.AllocQty
		}
	}
	if po1Alloc != 6.0 {
		t.Errorf("POOL1+SKU1 alloc = %f, want 6 (全部)", po1Alloc)
	}
}

func TestAllocate_InputValidation(t *testing.T) {
	// Pools 跟 PoolSegments 数量不匹配 → 错
	_, err := ComputeAllocate(AllocateInput{
		BranchNo:     "TEST",
		Pools:        []*PoolCode{mkPool("P1", 1.0, 0)},
		PoolSegments: []*PoolSegment{}, // 空
	})
	if err == nil {
		t.Error("应返数量不匹配错")
	}
}
