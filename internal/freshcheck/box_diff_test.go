package freshcheck

// box_diff_test.go: W2.3 框内差值算法单测
//   5 个场景:
//     1. 正常: in=1.0, out=0.5, pos=0.4 → box_loss=0.1
//     2. 报损: in=1.0, out=0.5(含 0.2 spoiled), pos=0.3 → box_loss=0.2
//     3. 偷损: in=1.0, out=0.3, pos=0.2 → box_loss=0.5 (异常)
//     4. 多负: out > in → box_loss 负数 (异常)
//     5. NeedsInvestigate 阈值判断

import (
	"testing"
)

func TestComputeBoxDiff_Normal(t *testing.T) {
	now := newTime(t, 12)
	from := newTime(t, 0)
	events := []*PoolEvent{
		{ItemNo: "ITEM1", EventKind: PoolEventIn, EventTime: newTime(t, 1), WeightKg: 1.0, Confidence: ConfidenceHigh},
		{ItemNo: "ITEM1", EventKind: PoolEventOut, EventTime: newTime(t, 11), WeightKg: 0.5,
			Confidence: ConfidenceHigh, OutDestination: strPtr(DestSoldOut)},
	}
	posSales := 0.4
	r := ComputeBoxDiff("0001", "POOL001", 1.0, from, now, events, posSales)
	if !floatNear(r.InTotalKg, 1.0, 1e-9) {
		t.Errorf("InTotalKg = %f, want 1.0", r.InTotalKg)
	}
	if !floatNear(r.OutTotalKg, 0.5, 1e-9) {
		t.Errorf("OutTotalKg = %f, want 0.5", r.OutTotalKg)
	}
	if !floatNear(r.PosTotalKg, 0.4, 1e-9) {
		t.Errorf("PosTotalKg = %f, want 0.4", r.PosTotalKg)
	}
	// box_loss = 1.0 - 0.5 - 0.4 = 0.1
	if !floatNear(r.BoxLossKg, 0.1, 1e-9) {
		t.Errorf("BoxLossKg = %f, want 0.1", r.BoxLossKg)
	}
	if !floatNear(r.BoxLossAmt, 0.1, 1e-9) {
		t.Errorf("BoxLossAmt = %f, want 0.1 (unit_price=1.0)", r.BoxLossAmt)
	}
}

func TestComputeBoxDiff_WithSpoiled(t *testing.T) {
	now := newTime(t, 12)
	from := newTime(t, 0)
	events := []*PoolEvent{
		{ItemNo: "ITEM1", EventKind: PoolEventIn, EventTime: newTime(t, 1), WeightKg: 1.0, Confidence: ConfidenceHigh},
		{ItemNo: "ITEM1", EventKind: PoolEventOut, EventTime: newTime(t, 11), WeightKg: 0.3,
			Confidence: ConfidenceHigh, OutDestination: strPtr(DestSpoiled), PieceCount: 200}, // 0.2kg spoiled
	}
	posSales := 0.3
	r := ComputeBoxDiff("0001", "POOL001", 1.0, from, now, events, posSales)
	// out=0.3 (业务记录剩余), spoiled=0.2 (独立)
	// box_loss = 1.0 - 0.3 - 0.3 = 0.4
	if !floatNear(r.OutTotalKg, 0.3, 1e-9) {
		t.Errorf("OutTotalKg = %f, want 0.3", r.OutTotalKg)
	}
	if !floatNear(r.OutSpoiledKg, 0.2, 1e-9) {
		t.Errorf("OutSpoiledKg = %f, want 0.2", r.OutSpoiledKg)
	}
	if !floatNear(r.BoxLossKg, 0.4, 1e-9) {
		t.Errorf("BoxLossKg = %f, want 0.4", r.BoxLossKg)
	}
}

func TestComputeBoxDiff_LargeLoss(t *testing.T) {
	// 偷损场景: in 多, out 少, pos 更少
	now := newTime(t, 12)
	from := newTime(t, 0)
	events := []*PoolEvent{
		{ItemNo: "ITEM1", EventKind: PoolEventIn, EventTime: newTime(t, 1), WeightKg: 2.0, Confidence: ConfidenceHigh},
		{ItemNo: "ITEM1", EventKind: PoolEventOut, EventTime: newTime(t, 11), WeightKg: 0.5,
			Confidence: ConfidenceHigh, OutDestination: strPtr(DestSoldOut)},
	}
	posSales := 0.3
	r := ComputeBoxDiff("0001", "POOL001", 1.0, from, now, events, posSales)
	// box_loss = 2.0 - 0.5 - 0.3 = 1.2 (大, 应调查)
	if !floatNear(r.BoxLossKg, 1.2, 1e-9) {
		t.Errorf("BoxLossKg = %f, want 1.2", r.BoxLossKg)
	}
	if !r.NeedsInvestigate {
		t.Error("1.2 kg 损失应触发调查 (默认阈值 0.5)")
	}
}

func TestComputeBoxDiff_NegativeLoss(t *testing.T) {
	// 异常: out > in (理论不应发生, 但代码要保护)
	now := newTime(t, 12)
	from := newTime(t, 0)
	events := []*PoolEvent{
		{ItemNo: "ITEM1", EventKind: PoolEventIn, EventTime: newTime(t, 1), WeightKg: 0.3, Confidence: ConfidenceHigh},
		{ItemNo: "ITEM1", EventKind: PoolEventOut, EventTime: newTime(t, 11), WeightKg: 1.0,
			Confidence: ConfidenceHigh, OutDestination: strPtr(DestSoldOut)},
	}
	posSales := 0.0
	r := ComputeBoxDiff("0001", "POOL001", 1.0, from, now, events, posSales)
	// box_loss = 0.3 - 1.0 - 0.0 = -0.7 (负, abs > 0.5, 触发调查)
	if !floatNear(r.BoxLossKg, -0.7, 1e-9) {
		t.Errorf("BoxLossKg = %f, want -0.7", r.BoxLossKg)
	}
	// abs(0.7) > 0.5 默认阈值, 触发调查
	if !r.NeedsInvestigate {
		t.Error("abs(0.7) > 0.5 默认阈值, 应触发调查")
	}
}

func TestComputeBoxDiff_EmptyEvents(t *testing.T) {
	now := newTime(t, 12)
	from := newTime(t, 0)
	r := ComputeBoxDiff("0001", "POOL001", 1.0, from, now, nil, 0.0)
	if r.InTotalKg != 0 || r.OutTotalKg != 0 || r.PosTotalKg != 0 {
		t.Errorf("empty events: in=%f out=%f pos=%f, want 0/0/0", r.InTotalKg, r.OutTotalKg, r.PosTotalKg)
	}
	if r.BoxLossKg != 0 {
		t.Errorf("empty box_loss = %f, want 0", r.BoxLossKg)
	}
}

func TestComputeBoxDiff_OutOfWindowEvents(t *testing.T) {
	now := newTime(t, 12)
	from := newTime(t, 0)
	events := []*PoolEvent{
		{ItemNo: "ITEM1", EventKind: PoolEventIn, EventTime: newTime(t, 23), WeightKg: 1.0, Confidence: ConfidenceHigh}, // 窗口外
		{ItemNo: "ITEM1", EventKind: PoolEventOut, EventTime: newTime(t, 11), WeightKg: 0.5, Confidence: ConfidenceHigh},
	}
	posSales := 0.0
	r := ComputeBoxDiff("0001", "POOL001", 1.0, from, now, events, posSales)
	// in 窗口外不计入, 只有 out 0.5
	// box_loss = 0 - 0.5 - 0 = -0.5
	if !floatNear(r.InTotalKg, 0.0, 1e-9) {
		t.Errorf("InTotalKg = %f, want 0 (窗口外不计入)", r.InTotalKg)
	}
	if !floatNear(r.OutTotalKg, 0.5, 1e-9) {
		t.Errorf("OutTotalKg = %f, want 0.5", r.OutTotalKg)
	}
	if !floatNear(r.BoxLossKg, -0.5, 1e-9) {
		t.Errorf("BoxLossKg = %f, want -0.5", r.BoxLossKg)
	}
}
