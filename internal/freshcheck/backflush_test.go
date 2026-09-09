package freshcheck

// backflush_test.go: W3.2 鍊掓尋 + 鎶ユ崯鍓ョ 鍗曞厓娴嬭瘯 (鏃?PG, 绾嚱鏁?
//
// 瑕嗙洊鐭╅樀:
//   - 鍊掓尋: 鍩烘湰 / begin=0 / end=0 / 閫€璐?(璧?normal_sale 鍑忔墸) / 璐?backflush / end 缂?/ 缂烘崯鑰楃巼
//   - 鎶ユ崯: fast / slow / shelf_life 鎴柇 / 缂烘崯鑰楃巼
//   - daysBetween: < 24h / 澶氭棩

import (
	"testing"
	"time"
)

func newRFC3339(t *testing.T, d string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, d)
	if err != nil {
		t.Fatalf("parse %q: %v", d, err)
	}
	return v
}

func skuLeafFast() *SkuMap {
	return &SkuMap{
		BranchNo: "TEST", ItemNo: "TESTLEAF1", ItemName: "spinach",
		FreshCategory: CategoryLeaf, TurnoverClass: TurnoverFast, ShelfLifeDays: 5,
		IsActive: true,
	}
}
func skuRootSlow() *SkuMap {
	return &SkuMap{
		BranchNo: "TEST", ItemNo: "TESTROOT1", ItemName: "potato",
		FreshCategory: CategoryRoot, TurnoverClass: TurnoverSlow, ShelfLifeDays: 30,
		IsActive: true,
	}
}

func TestComputeBackflush_Basic(t *testing.T) {
	// fast 鍛ㄨ彍: begin=10, purchase=50, sale=45, end=12 鈫?backflush=3
	// fast: loss = 50 脳 0.05 脳 min(5, 7) = 50 脳 0.05 脳 5 = 12.5
	// pool_adj = max(3 - 12.5, 0) = 0
	in := BackflushInput{
		BranchNo:   "TEST",
		PeriodID:   202609,
		WindowFrom: newRFC3339(t, "2026-09-01T00:00:00Z"),
		WindowTo:   newRFC3339(t, "2026-09-08T00:00:00Z"),
		BeginByItem:  map[string]float64{"TESTLEAF1": 10},
		EndByItem:    map[string]float64{"TESTLEAF1": 12},
		PurchaseByItem: map[string]float64{"TESTLEAF1": 50},
		NormalSaleByItem: map[string]float64{"TESTLEAF1": 45},
		SKUs: []*SkuMap{skuLeafFast()},
		LossRateByKey: map[string]float64{
			"leaf:fast:natural": 0.05,
		},
	}
	res, err := ComputeBackflush(in)
	if err != nil {
		t.Fatalf("ComputeBackflush: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("Items = %d, want 1", len(res.Items))
	}
	it := res.Items[0]
	if it.Backflush != 3 {
		t.Errorf("Backflush = %f, want 3", it.Backflush)
	}
	if it.ExpectedLoss != 12.5 {
		t.Errorf("ExpectedLoss = %f, want 12.5", it.ExpectedLoss)
	}
	if it.PoolAdjustable != 0 {
		t.Errorf("PoolAdjustable = %f, want 0 (3 < 12.5)", it.PoolAdjustable)
	}
}

func TestComputeBackflush_BeginZero(t *testing.T) {
	// 棣栨湡: begin=0, purchase=100, sale=80, end=15 鈫?backflush=5
	// loss = 100 脳 0.05 脳 5 = 25, pool_adj = 0
	in := BackflushInput{
		BranchNo:   "TEST",
		PeriodID:   202609,
		WindowFrom: newRFC3339(t, "2026-09-01T00:00:00Z"),
		WindowTo:   newRFC3339(t, "2026-09-08T00:00:00Z"),
		BeginByItem:  map[string]float64{},
		EndByItem:    map[string]float64{"TESTLEAF1": 15},
		PurchaseByItem: map[string]float64{"TESTLEAF1": 100},
		NormalSaleByItem: map[string]float64{"TESTLEAF1": 80},
		SKUs: []*SkuMap{skuLeafFast()},
		LossRateByKey: map[string]float64{
			"leaf:fast:natural": 0.05,
		},
	}
	res, _ := ComputeBackflush(in)
	it := res.Items[0]
	if it.BeginQty != 0 {
		t.Errorf("BeginQty = %f, want 0", it.BeginQty)
	}
	if it.Backflush != 5 {
		t.Errorf("Backflush = %f, want 5 (0+100-80-15)", it.Backflush)
	}
}

func TestComputeBackflush_EndZero(t *testing.T) {
	// 鏈熸湯鍏ㄥ崠鍏? begin=10, purchase=20, sale=25, end=0 鈫?backflush=5
	in := BackflushInput{
		BranchNo:   "TEST",
		PeriodID:   202609,
		WindowFrom: newRFC3339(t, "2026-09-01T00:00:00Z"),
		WindowTo:   newRFC3339(t, "2026-09-08T00:00:00Z"),
		BeginByItem:  map[string]float64{"TESTLEAF1": 10},
		EndByItem:    map[string]float64{"TESTLEAF1": 0},
		PurchaseByItem: map[string]float64{"TESTLEAF1": 20},
		NormalSaleByItem: map[string]float64{"TESTLEAF1": 25},
		SKUs: []*SkuMap{skuLeafFast()},
		LossRateByKey: map[string]float64{"leaf:fast:natural": 0.05},
	}
	res, _ := ComputeBackflush(in)
	it := res.Items[0]
	if it.EndQty != 0 {
		t.Errorf("EndQty = %f, want 0", it.EndQty)
	}
	if it.Backflush != 5 {
		t.Errorf("Backflush = %f, want 5 (10+20-25-0)", it.Backflush)
	}
}

func TestComputeBackflush_RefundHandled(t *testing.T) {
	// 閫€璐ц蛋 normal_sale 鍑忔墸 (璋冪敤鏂瑰凡澶勭悊 is_refund=0)
	// 鍗?30 閫€ 5 鈫?normal_sale=25
	// begin=5, purchase=30, sale=25, end=8 鈫?backflush=2
	in := BackflushInput{
		BranchNo:   "TEST",
		PeriodID:   202609,
		WindowFrom: newRFC3339(t, "2026-09-01T00:00:00Z"),
		WindowTo:   newRFC3339(t, "2026-09-08T00:00:00Z"),
		BeginByItem:  map[string]float64{"TESTLEAF1": 5},
		EndByItem:    map[string]float64{"TESTLEAF1": 8},
		PurchaseByItem: map[string]float64{"TESTLEAF1": 30},
		NormalSaleByItem: map[string]float64{"TESTLEAF1": 25}, // 30 - 5
		SKUs: []*SkuMap{skuLeafFast()},
		LossRateByKey: map[string]float64{"leaf:fast:natural": 0.05},
	}
	res, _ := ComputeBackflush(in)
	if res.Items[0].Backflush != 2 {
		t.Errorf("Backflush = %f, want 2 (5+30-25-8)", res.Items[0].Backflush)
	}
}

func TestComputeBackflush_NegativeBackflush(t *testing.T) {
	// 閿€鍞紓甯? 鍗栧緱姣旇繘璐ц繕澶? backflush < 0
	// begin=2, purchase=10, sale=15, end=0 鈫?backflush=-3
	// pool_adj = max(-3 - loss, 0) = 0
	in := BackflushInput{
		BranchNo:   "TEST",
		PeriodID:   202609,
		WindowFrom: newRFC3339(t, "2026-09-01T00:00:00Z"),
		WindowTo:   newRFC3339(t, "2026-09-08T00:00:00Z"),
		BeginByItem:  map[string]float64{"TESTLEAF1": 2},
		EndByItem:    map[string]float64{"TESTLEAF1": 0},
		PurchaseByItem: map[string]float64{"TESTLEAF1": 10},
		NormalSaleByItem: map[string]float64{"TESTLEAF1": 15},
		SKUs: []*SkuMap{skuLeafFast()},
		LossRateByKey: map[string]float64{"leaf:fast:natural": 0.05},
	}
	res, _ := ComputeBackflush(in)
	if res.Items[0].Backflush != -3 {
		t.Errorf("Backflush = %f, want -3", res.Items[0].Backflush)
	}
	if res.Items[0].PoolAdjustable != 0 {
		t.Errorf("PoolAdjustable = %f, want 0 (璐?backflush)", res.Items[0].PoolAdjustable)
	}
}

func TestComputeBackflush_MissingEnd(t *testing.T) {
	// C8 闃绘柇: end_qty 娌″綍, 鏍?missing_end=true, 浣?backflush 浠嶅彲绠?(end 瑙嗕负 0)
	in := BackflushInput{
		BranchNo:   "TEST",
		PeriodID:   202609,
		WindowFrom: newRFC3339(t, "2026-09-01T00:00:00Z"),
		WindowTo:   newRFC3339(t, "2026-09-08T00:00:00Z"),
		BeginByItem:  map[string]float64{"TESTLEAF1": 5},
		EndByItem:    map[string]float64{}, // 娌″綍
		PurchaseByItem: map[string]float64{"TESTLEAF1": 20},
		NormalSaleByItem: map[string]float64{"TESTLEAF1": 15},
		SKUs: []*SkuMap{skuLeafFast()},
		LossRateByKey: map[string]float64{"leaf:fast:natural": 0.05},
	}
	res, _ := ComputeBackflush(in)
	it := res.Items[0]
	if !it.MissingEnd {
		t.Error("MissingEnd should be true")
	}
	if it.EndQty != 0 {
		t.Errorf("EndQty = %f, want 0 (缂鸿 0)", it.EndQty)
	}
	if it.Backflush != 10 {
		t.Errorf("Backflush = %f, want 10 (5+20-15-0)", it.Backflush)
	}
	if res.Summary.MissingEndCount != 1 {
		t.Errorf("MissingEndCount = %d, want 1", res.Summary.MissingEndCount)
	}
}

func TestComputeBackflush_MissingLossRate(t *testing.T) {
	// 缂烘崯鑰楃巼: missing_loss_rate=true, loss=0 (淇濆畧), pool_adj=backflush
	in := BackflushInput{
		BranchNo:   "TEST",
		PeriodID:   202609,
		WindowFrom: newRFC3339(t, "2026-09-01T00:00:00Z"),
		WindowTo:   newRFC3339(t, "2026-09-08T00:00:00Z"),
		BeginByItem:  map[string]float64{"TESTLEAF1": 5},
		EndByItem:    map[string]float64{"TESTLEAF1": 3},
		PurchaseByItem: map[string]float64{"TESTLEAF1": 20},
		NormalSaleByItem: map[string]float64{"TESTLEAF1": 15},
		SKUs: []*SkuMap{skuLeafFast()},
		LossRateByKey: map[string]float64{}, // 娌￠厤鎹熻€楃巼
	}
	res, _ := ComputeBackflush(in)
	it := res.Items[0]
	if !it.MissingLossRate {
		t.Error("MissingLossRate should be true")
	}
	if it.ExpectedLoss != 0 {
		t.Errorf("ExpectedLoss = %f, want 0 (缂烘崯鐜?", it.ExpectedLoss)
	}
	if it.Backflush != 7 {
		t.Errorf("Backflush = %f, want 7 (5+20-15-3)", it.Backflush)
	}
	// pool_adj = 7 - 0 = 7
	if it.PoolAdjustable != 7 {
		t.Errorf("PoolAdjustable = %f, want 7 (鏃犳崯鑰楀垎鎽?", it.PoolAdjustable)
	}
}

func TestComputeLoss_FastShelfLifeCut(t *testing.T) {
	// fast: shelf_life=5, days=7 鈫?鍙?min(5, 7) = 5
	// loss = 100 脳 0.05 脳 5 = 25
	sku := skuLeafFast()
	got := computeLoss(sku, 0, 100, 0, 7, 0.05, true)
	if got != 25 {
		t.Errorf("fast loss = %f, want 25", got)
	}
	// shelf_life 10 days 3 鈫?min(10, 3) = 3
	sku.ShelfLifeDays = 10
	got = computeLoss(sku, 0, 100, 0, 3, 0.05, true)
	if got != 15 {
		t.Errorf("fast loss shelf_life>days = %f, want 15 (100脳0.05脳3)", got)
	}
}

func TestComputeLoss_SlowAvgStock(t *testing.T) {
	// slow: avg_stock = (begin+end)/2, days=30, rate=0.01
	// loss = ((50+30)/2) 脳 0.01 脳 30 = 40 脳 0.3 = 12
	sku := skuRootSlow()
	got := computeLoss(sku, 50, 100, 30, 30, 0.01, true)
	if got != 12 {
		t.Errorf("slow loss = %f, want 12", got)
	}
}

func TestDaysBetween(t *testing.T) {
	from := newRFC3339(t, "2026-09-01T00:00:00Z")
	// half day -> 1
	to := newRFC3339(t, "2026-09-01T12:00:00Z")
	if d := daysBetween(from, to); d != 1 {
		t.Errorf("half day = %d, want 1 (rounded up)", d)
	}
	// 7 days
	to = newRFC3339(t, "2026-09-08T00:00:00Z")
	if d := daysBetween(from, to); d != 7 {
		t.Errorf("7 days = %d, want 7", d)
	}
	// reverse -> 0
	if d := daysBetween(to, from); d != 0 {
		t.Errorf("reverse = %d, want 0", d)
	}
}

func TestComputeBackflush_Summary(t *testing.T) {
	// 澶?SKU 姹囨€绘祴璇? 2 涓?fast + 1 涓?slow
	in := BackflushInput{
		BranchNo:   "TEST",
		PeriodID:   202609,
		WindowFrom: newRFC3339(t, "2026-09-01T00:00:00Z"),
		WindowTo:   newRFC3339(t, "2026-09-08T00:00:00Z"),
		BeginByItem:  map[string]float64{"TESTLEAF1": 5, "TESTLEAF2": 3, "TESTROOT1": 20},
		EndByItem:    map[string]float64{"TESTLEAF1": 3, "TESTLEAF2": 2, "TESTROOT1": 15},
		PurchaseByItem: map[string]float64{"TESTLEAF1": 20, "TESTLEAF2": 10, "TESTROOT1": 30},
		NormalSaleByItem: map[string]float64{"TESTLEAF1": 15, "TESTLEAF2": 8, "TESTROOT1": 25},
		SKUs: []*SkuMap{skuLeafFast(),
			{BranchNo: "TEST", ItemNo: "TESTLEAF2", FreshCategory: CategoryLeaf, TurnoverClass: TurnoverFast, ShelfLifeDays: 5, IsActive: true},
			skuRootSlow(),
		},
		LossRateByKey: map[string]float64{
			"leaf:fast:natural":  0.05,
			"root:slow:natural":  0.01,
		},
	}
	res, err := ComputeBackflush(in)
	if err != nil {
		t.Fatalf("ComputeBackflush: %v", err)
	}
	if res.Summary.SKUsCount != 3 {
		t.Errorf("SKUsCount = %d, want 3", res.Summary.SKUsCount)
	}
	if res.WindowDays != 7 {
		t.Errorf("WindowDays = %d, want 7", res.WindowDays)
	}
	// 3 SKU 鐨?backflush 閮?>= 0
	if res.Summary.TotalBackflush <= 0 {
		t.Errorf("TotalBackflush = %f, want > 0", res.Summary.TotalBackflush)
	}
	// fast loss should be > slow loss in this fixture
	fastLoss := 0.0
	slowLoss := 0.0
	for _, it := range res.Items {
		if it.TurnoverClass == TurnoverFast {
			fastLoss += it.ExpectedLoss
		} else if it.TurnoverClass == TurnoverSlow {
			slowLoss = it.ExpectedLoss
		}
	}
	if fastLoss <= slowLoss {
		t.Errorf("fast loss (%f) should > slow loss (%f) for this fixture", fastLoss, slowLoss)
	}
}
