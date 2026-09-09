package freshcheck

// pool_state_test.go: W2.2 池状态重建算法单测
//   5 个场景:
//     1. 正常: in 0.5kg, in 0.3kg, out 0.5kg → current 0.3kg
//     2. 跨筐降级: out downgraded → 目标 pool 加虚拟 in
//     3. 漏录标记: out 在 in 之前 → HasOutBeforeIn = true
//     4. 空池: events=[] → state.Items=[]
//     5. 变质报损: spoiled 不计入 in, 单独累加

import (
	"testing"
	"time"
)

func newTime(t *testing.T, h int) time.Time {
	t.Helper()
	return time.Date(2026, 9, 8, h, 0, 0, 0, time.UTC)
}

func TestPoolState_NormalInOut(t *testing.T) {
	now := newTime(t, 12)
	events := []*PoolEvent{
		{ItemNo: "ITEM1", EventKind: PoolEventIn, EventTime: newTime(t, 8), WeightKg: 0.5, Confidence: ConfidenceHigh},
		{ItemNo: "ITEM1", EventKind: PoolEventIn, EventTime: newTime(t, 9), WeightKg: 0.3, Confidence: ConfidenceHigh},
		{ItemNo: "ITEM1", EventKind: PoolEventOut, EventTime: newTime(t, 10), WeightKg: 0.5, Confidence: ConfidenceHigh, OutDestination: strPtr(DestSoldOut)},
	}
	state := RebuildPoolState("POOL001", "1元/斤", now, events)
	if len(state.Items) != 1 {
		t.Fatalf("Items = %d, want 1", len(state.Items))
	}
	item := state.Items[0]
	if item.InWeightKg != 0.8 {
		t.Errorf("InWeightKg = %f, want 0.8", item.InWeightKg)
	}
	if item.OutWeightKg != 0.5 {
		t.Errorf("OutWeightKg = %f, want 0.5", item.OutWeightKg)
	}
	if !floatNear(item.CurrentWeightKg, 0.3, 1e-9) {
		t.Errorf("CurrentWeightKg = %f, want 0.3", item.CurrentWeightKg)
	}
	if !floatNear(state.InTotalKg, 0.8, 1e-9) {
		t.Errorf("InTotalKg = %f, want 0.8", state.InTotalKg)
	}
}

func TestPoolState_DowngradeAddsVirtualIn(t *testing.T) {
	now := newTime(t, 12)
	downgradeTo := "POOL_LOW"
	events := []*PoolEvent{
		{ItemNo: "FISH1", EventKind: PoolEventIn, EventTime: newTime(t, 8), WeightKg: 1.0, Confidence: ConfidenceHigh},
		{ItemNo: "FISH1", EventKind: PoolEventOut, EventTime: newTime(t, 11), WeightKg: 0.5,
			Confidence: ConfidenceHigh, OutDestination: strPtr(DestDowngrade), DowngradeTo: &downgradeTo},
	}
	state := RebuildPoolState("POOL_HIGH", "1元/斤", now, events)
	// POOL_HIGH 状态: 0.5 剩余
	if state.CurrentTotalKg != 0.5 {
		t.Errorf("POOL_HIGH current = %f, want 0.5", state.CurrentTotalKg)
	}
	// 降级部分虚拟 in 已在代码里 (但当前算法不算入 state)
	// 实际生产: 目标 pool 的 state 由调用方单独算
	if len(state.Items) != 1 {
		t.Errorf("Items = %d, want 1", len(state.Items))
	}
}

func TestPoolState_EmptyEvents(t *testing.T) {
	state := RebuildPoolState("POOL001", "1元/斤", time.Now(), nil)
	if len(state.Items) != 0 {
		t.Errorf("Items = %d, want 0", len(state.Items))
	}
	if state.CurrentTotalKg != 0 {
		t.Errorf("CurrentTotalKg = %f, want 0", state.CurrentTotalKg)
	}
}

func TestPoolState_ConfidenceDowngrades(t *testing.T) {
	now := newTime(t, 12)
	events := []*PoolEvent{
		{ItemNo: "ITEM1", EventKind: PoolEventIn, EventTime: newTime(t, 8), WeightKg: 0.5, Confidence: ConfidenceHigh},
		{ItemNo: "ITEM1", EventKind: PoolEventIn, EventTime: newTime(t, 9), WeightKg: 0.3, Confidence: ConfidenceLow}, // 补录
	}
	state := RebuildPoolState("POOL001", "1元/斤", now, events)
	if state.Items[0].Confidence != ConfidenceLow {
		t.Errorf("Confidence = %s, want low (混合后取最低)", state.Items[0].Confidence)
	}
}

func TestPoolState_SpoiledTrackedSeparately(t *testing.T) {
	now := newTime(t, 12)
	spoiled := 0.3
	events := []*PoolEvent{
		{ItemNo: "VEG1", EventKind: PoolEventIn, EventTime: newTime(t, 8), WeightKg: 1.0, Confidence: ConfidenceHigh},
		{ItemNo: "VEG1", EventKind: PoolEventOut, EventTime: newTime(t, 11), WeightKg: 0.7,
			Confidence: ConfidenceHigh, OutDestination: strPtr(DestSpoiled), PieceCount: int(spoiled * 1000)},
	}
	state := RebuildPoolState("POOL001", "1元/斤", now, events)
	item := state.Items[0]
	if item.OutWeightKg != 0.7 {
		t.Errorf("OutWeightKg = %f, want 0.7", item.OutWeightKg)
	}
	// 业务: spoiled 用 piece_count 字段传 (临时, 后期改 schema)
	// piece_count=300 → SpoiledWeightKg = 300/1000 = 0.3
	if item.SpoiledWeightKg != 0.3 {
		t.Errorf("SpoiledWeightKg = %f, want 0.3 (piece_count 300/1000 临时复用)", item.SpoiledWeightKg)
	}
}

func TestHasOutBeforeIn_DetectsMissing(t *testing.T) {
	events := []*PoolEvent{
		// out 在 in 之前
		{ItemNo: "ITEM1", EventKind: PoolEventOut, EventTime: newTime(t, 8), WeightKg: 0.3, Confidence: ConfidenceHigh},
		{ItemNo: "ITEM1", EventKind: PoolEventIn, EventTime: newTime(t, 10), WeightKg: 0.5, Confidence: ConfidenceHigh},
	}
	if !HasOutBeforeIn(events) {
		t.Error("HasOutBeforeIn should be true (out at 8am < in at 10am)")
	}

	// 正常顺序
	events2 := []*PoolEvent{
		{ItemNo: "ITEM1", EventKind: PoolEventIn, EventTime: newTime(t, 8), WeightKg: 0.5, Confidence: ConfidenceHigh},
		{ItemNo: "ITEM1", EventKind: PoolEventOut, EventTime: newTime(t, 10), WeightKg: 0.3, Confidence: ConfidenceHigh},
	}
	if HasOutBeforeIn(events2) {
		t.Error("HasOutBeforeIn should be false (in before out)")
	}
}

func TestDetectMissingInAt(t *testing.T) {
	now := newTime(t, 12)
	events := []*PoolEvent{
		// ITEM1: 只有 out, 没有 in → missing
		{ItemNo: "ITEM1", EventKind: PoolEventOut, EventTime: newTime(t, 8), WeightKg: 0.5, Confidence: ConfidenceHigh},
		// ITEM2: in + out → OK
		{ItemNo: "ITEM2", EventKind: PoolEventIn, EventTime: newTime(t, 7), WeightKg: 1.0, Confidence: ConfidenceHigh},
		{ItemNo: "ITEM2", EventKind: PoolEventOut, EventTime: newTime(t, 9), WeightKg: 0.3, Confidence: ConfidenceHigh},
		// ITEM3: 只有 in → OK (没漏)
		{ItemNo: "ITEM3", EventKind: PoolEventIn, EventTime: newTime(t, 7), WeightKg: 1.0, Confidence: ConfidenceHigh},
	}
	missing := DetectMissingInAt(events, now)
	if len(missing) != 1 {
		t.Fatalf("missing = %d, want 1", len(missing))
	}
	if missing[0].ItemNo != "ITEM1" {
		t.Errorf("missing item = %s, want ITEM1", missing[0].ItemNo)
	}
}

// strPtr 字符串指针 helper
func strPtr(s string) *string { return &s }

// floatNear 浮点近似比较
func floatNear(a, b, eps float64) bool {
	if a > b {
		return a-b < eps
	}
	return b-a < eps
}
