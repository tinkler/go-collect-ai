package freshcheck

// downgrade_test.go: W4.1 跨筐降级 单测
//
// 覆盖场景:
//   - 1 框 A 降级到 1 框 B, 5kg 虚拟入 B
//   - 同 SKU 多次降级累加 in_weight
//   - 已有 item 累加, confidence 提升到 1.0
//   - 新 SKU 跨筐降级 (目标 pool 之前无此 SKU) 新建 item
//   - 不同 pool 间独立处理

import (
	"testing"
	"time"
)

// applyDowngradeVirtualIn W4.1 helper: 给目标 pool 加虚拟入 (从 downgrades map)
//   downgrades: pool_code -> []*PoolEvent (out_destination=downgrade, downgrade_to=pool_code)
//   items: 目标 pool 当前 items 列表 (会被修改)
//   返回: 修改后的 items (新建/累加 item)
//
//   这是 buildAllocateInput 里跨筐降级处理的纯函数提炼, 便于单测
func applyDowngradeVirtualIn(poolCode string, downgrades map[string][]*PoolEvent, items []*PoolSegmentItem) []*PoolSegmentItem {
	itemByNo := map[string]*PoolSegmentItem{}
	for _, it := range items {
		itemByNo[it.ItemNo] = it
	}
	for _, dg := range downgrades[poolCode] {
		existing, ok := itemByNo[dg.ItemNo]
		if !ok {
			psi := &PoolSegmentItem{
				ItemNo:           dg.ItemNo,
				ItemName:         "",
				InWeightKg:       dg.WeightKg,
				OutWeightKg:      0,
				SpoiledWeightKg:  0,
				ConfidenceFactor: 1.0,
			}
			items = append(items, psi)
			itemByNo[dg.ItemNo] = psi
		} else {
			existing.InWeightKg += dg.WeightKg
			if existing.ConfidenceFactor < 1.0 {
				existing.ConfidenceFactor = 1.0
			}
		}
	}
	return items
}

func mkDowngrade(itemNo string, fromPool string, weightKg float64) *PoolEvent {
	return &PoolEvent{
		BranchNo:       "TEST",
		PoolCode:       fromPool,
		ItemNo:         itemNo,
		EventKind:      PoolEventOut,
		EventTime:      time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		WeightKg:       weightKg,
		OutDestination: stringPtr(DestDowngrade),
		DowngradeTo:    stringPtr(""),
		Operator:       "u_floor",
		Confidence:     "high",
	}
}

func stringPtr(s string) *string { return &s }

func TestDowngrade_VirtualIn_NewItem(t *testing.T) {
	// POOL_HIGH 降级 5kg SKU 到 POOL_LOW, POOL_LOW 之前无此 SKU
	downgrades := map[string][]*PoolEvent{
		"POOL_LOW": {mkDowngrade("NEW_SKU", "POOL_HIGH", 5.0)},
	}
	items := []*PoolSegmentItem{} // POOL_LOW 之前空

	out := applyDowngradeVirtualIn("POOL_LOW", downgrades, items)
	if len(out) != 1 {
		t.Fatalf("items = %d, want 1 (新建虚拟入)", len(out))
	}
	if out[0].ItemNo != "NEW_SKU" {
		t.Errorf("ItemNo = %s, want NEW_SKU", out[0].ItemNo)
	}
	if out[0].InWeightKg != 5.0 {
		t.Errorf("InWeightKg = %f, want 5.0", out[0].InWeightKg)
	}
	if out[0].ConfidenceFactor != 1.0 {
		t.Errorf("ConfidenceFactor = %f, want 1.0 (high)", out[0].ConfidenceFactor)
	}
	if out[0].OutWeightKg != 0 {
		t.Errorf("OutWeightKg = %f, want 0 (虚拟入无 out)", out[0].OutWeightKg)
	}
}

func TestDowngrade_VirtualIn_ExistingItem(t *testing.T) {
	// POOL_HIGH 降级 3kg SKU 到 POOL_LOW, POOL_LOW 已有此 SKU (in=10, confidence=0.5)
	downgrades := map[string][]*PoolEvent{
		"POOL_LOW": {mkDowngrade("EXIST_SKU", "POOL_HIGH", 3.0)},
	}
	items := []*PoolSegmentItem{
		{ItemNo: "EXIST_SKU", InWeightKg: 10.0, ConfidenceFactor: 0.5},
	}
	out := applyDowngradeVirtualIn("POOL_LOW", downgrades, items)
	if len(out) != 1 {
		t.Fatalf("items = %d, want 1 (累加, 不新建)", len(out))
	}
	if out[0].InWeightKg != 13.0 {
		t.Errorf("InWeightKg = %f, want 13.0 (10 + 3)", out[0].InWeightKg)
	}
	if out[0].ConfidenceFactor != 1.0 {
		t.Errorf("ConfidenceFactor = %f, want 1.0 (虚拟入提升到 high)", out[0].ConfidenceFactor)
	}
}

func TestDowngrade_VirtualIn_MultipleSameTarget(t *testing.T) {
	// 2 次降级到同一个 POOL_LOW, 同 SKU: 累加 in_weight
	downgrades := map[string][]*PoolEvent{
		"POOL_LOW": {
			mkDowngrade("SKU1", "POOL_HIGH", 2.0),
			mkDowngrade("SKU1", "POOL_HIGH", 3.0),
		},
	}
	items := []*PoolSegmentItem{}
	out := applyDowngradeVirtualIn("POOL_LOW", downgrades, items)
	if len(out) != 1 {
		t.Fatalf("items = %d, want 1 (同 SKU 累加)", len(out))
	}
	if out[0].InWeightKg != 5.0 {
		t.Errorf("InWeightKg = %f, want 5.0 (2+3)", out[0].InWeightKg)
	}
}

func TestDowngrade_VirtualIn_MixedItems(t *testing.T) {
	// POOL_LOW 已有 2 个 SKU, downgrade 加 1 个新的 + 累加 1 个旧的
	downgrades := map[string][]*PoolEvent{
		"POOL_LOW": {
			mkDowngrade("EXIST_SKU", "POOL_HIGH", 1.5),  // 累加
			mkDowngrade("NEW_SKU", "POOL_HIGH", 4.0),    // 新建
		},
	}
	items := []*PoolSegmentItem{
		{ItemNo: "EXIST_SKU", InWeightKg: 10.0, ConfidenceFactor: 1.0},
		{ItemNo: "OTHER_SKU", InWeightKg: 5.0, ConfidenceFactor: 1.0}, // 不被 downgrade 碰
	}
	out := applyDowngradeVirtualIn("POOL_LOW", downgrades, items)
	if len(out) != 3 {
		t.Fatalf("items = %d, want 3 (2 旧 + 1 新)", len(out))
	}
	got := map[string]float64{}
	conf := map[string]float64{}
	for _, it := range out {
		got[it.ItemNo] = it.InWeightKg
		conf[it.ItemNo] = it.ConfidenceFactor
	}
	if got["EXIST_SKU"] != 11.5 {
		t.Errorf("EXIST_SKU InWeightKg = %f, want 11.5 (10+1.5)", got["EXIST_SKU"])
	}
	if got["NEW_SKU"] != 4.0 {
		t.Errorf("NEW_SKU InWeightKg = %f, want 4.0", got["NEW_SKU"])
	}
	if got["OTHER_SKU"] != 5.0 {
		t.Errorf("OTHER_SKU InWeightKg = %f, want 5.0 (未触碰)", got["OTHER_SKU"])
	}
	if conf["NEW_SKU"] != 1.0 {
		t.Errorf("NEW_SKU confidence = %f, want 1.0", conf["NEW_SKU"])
	}
}

func TestDowngrade_VirtualIn_NoMatchingPool(t *testing.T) {
	// POOL_X 没在 downgrades map 里 → items 不变
	downgrades := map[string][]*PoolEvent{
		"POOL_OTHER": {mkDowngrade("SKU1", "POOL_HIGH", 5.0)},
	}
	items := []*PoolSegmentItem{
		{ItemNo: "SKU1", InWeightKg: 10.0, ConfidenceFactor: 0.5},
	}
	out := applyDowngradeVirtualIn("POOL_X", downgrades, items)
	if len(out) != 1 {
		t.Fatalf("items = %d, want 1 (不变)", len(out))
	}
	if out[0].InWeightKg != 10.0 {
		t.Errorf("InWeightKg = %f, want 10.0 (未触碰)", out[0].InWeightKg)
	}
	if out[0].ConfidenceFactor != 0.5 {
		t.Errorf("ConfidenceFactor = %f, want 0.5 (保持原值)", out[0].ConfidenceFactor)
	}
}

func TestDowngrade_VirtualIn_ConfidenceNotDowngraded(t *testing.T) {
	// 已有 item confidence=1.0, 虚拟入不会降低 (应保持 1.0)
	downgrades := map[string][]*PoolEvent{
		"POOL_LOW": {mkDowngrade("EXIST_SKU", "POOL_HIGH", 2.0)},
	}
	items := []*PoolSegmentItem{
		{ItemNo: "EXIST_SKU", InWeightKg: 5.0, ConfidenceFactor: 1.0},
	}
	out := applyDowngradeVirtualIn("POOL_LOW", downgrades, items)
	if out[0].ConfidenceFactor != 1.0 {
		t.Errorf("ConfidenceFactor = %f, want 1.0 (保持 high)", out[0].ConfidenceFactor)
	}
	if out[0].InWeightKg != 7.0 {
		t.Errorf("InWeightKg = %f, want 7.0 (5+2)", out[0].InWeightKg)
	}
}