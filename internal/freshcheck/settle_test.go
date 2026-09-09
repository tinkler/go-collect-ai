package freshcheck

// settle_test.go: W3.4 6 步编排 单元测试 (helper 覆盖, 端到端在 W3.9 验收)
//
// 覆盖:
//   - determineWindow: track_code 决定窗口长度
//   - idempotencyKey: 稳定 hash
//   - SortPoolsByPrice: 价高先
//   - allocItemToDB: 字段桥接
//   - summarizeSettlement: 汇总算术

import (
	"testing"
	"time"
)

func TestDetermineWindow(t *testing.T) {
	end := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		track    string
		wantDays int
	}{
		{"leaf-weekly", 7},
		{"", 7}, // 默认 7
		{"root-monthly", 30},
		{"frozen-monthly", 30},
		{"meat-biweekly", 14},
		{"aquatic-biweekly", 14},
	}
	for _, c := range cases {
		t.Run(c.track, func(t *testing.T) {
			_, _, days := determineWindow(SettleRequest{
				TrackCode: c.track,
				PeriodEnd: end,
			})
			if days != c.wantDays {
				t.Errorf("days = %d, want %d (track=%s)", days, c.wantDays, c.track)
			}
		})
	}
}

func TestIdempotencyKey_Stable(t *testing.T) {
	k1 := idempotencyKey("0001", "leaf-weekly", 202609, "SKU1", "u_owner")
	k2 := idempotencyKey("0001", "leaf-weekly", 202609, "SKU1", "u_owner")
	if k1 != k2 {
		t.Errorf("idempotency key should be stable: %s != %s", k1, k2)
	}
	// 改 item 应当不同
	k3 := idempotencyKey("0001", "leaf-weekly", 202609, "SKU2", "u_owner")
	if k1 == k3 {
		t.Error("diff item should give diff key")
	}
	// 改 operator 也不同
	k4 := idempotencyKey("0001", "leaf-weekly", 202609, "SKU1", "u_manager")
	if k1 == k4 {
		t.Error("diff operator should give diff key")
	}
	if len(k1) != 40 {
		// sha1 hex = 40 chars
		t.Errorf("key length = %d, want 40 (sha1 hex)", len(k1))
	}
}

func TestSortPoolsByPrice(t *testing.T) {
	pools := []*PoolCode{
		{PoolCode: "L", UnitPrice: 0.5},
		{PoolCode: "H", UnitPrice: 2.0},
		{PoolCode: "M", UnitPrice: 1.0},
	}
	SortPoolsByPrice(pools)
	if pools[0].PoolCode != "H" {
		t.Errorf("pools[0] = %s, want H (价高先)", pools[0].PoolCode)
	}
	if pools[1].PoolCode != "M" {
		t.Errorf("pools[1] = %s, want M", pools[1].PoolCode)
	}
	if pools[2].PoolCode != "L" {
		t.Errorf("pools[2] = %s, want L", pools[2].PoolCode)
	}
}

func TestAllocItemToDB(t *testing.T) {
	a := &AllocItem{
		PoolCode:     "P1",
		SegmentStart: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		SegmentEnd:   time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC),
		ItemNo:       "SKU1",
		AllocQty:     5.0,
		AllocAmt:     7.5,
		Confidence:   0.8,
		NeedsReview:  true,
	}
	db := allocItemToDB(a, 202609)
	if db.PeriodID != 202609 {
		t.Errorf("PeriodID = %d, want 202609", db.PeriodID)
	}
	if db.PoolCode != "P1" || db.ItemNo != "SKU1" {
		t.Errorf("PoolCode/ItemNo mismatch: %s/%s", db.PoolCode, db.ItemNo)
	}
	if db.AllocQty != 5.0 {
		t.Errorf("AllocQty = %f, want 5.0", db.AllocQty)
	}
	if db.ConfidenceFactor != 0.8 {
		t.Errorf("ConfidenceFactor = %f, want 0.8", db.ConfidenceFactor)
	}
	if !db.NeedsReview {
		t.Error("NeedsReview should be true")
	}
}

func TestSummarizeSettlement(t *testing.T) {
	// 2 个 SKU, 一个 OK 一个 ConservationFail
	sts := []*Settlement{
		{ItemNo: "SKU1", NormalSaleAmt: 100, PoolAllocAmt: 50, LossQty: 1, AvgCost: 5, BoxLossQty: 0, GrossProfit: 30, ConservationOK: true},
		{ItemNo: "SKU2", NormalSaleAmt: 200, PoolAllocAmt: 30, LossQty: 0, AvgCost: 5, BoxLossQty: 0, GrossProfit: 50, ConservationOK: false},
	}
	s := summarizeSettlement(sts, &BackflushResult{})
	if s.ItemsCount != 2 {
		t.Errorf("ItemsCount = %d, want 2", s.ItemsCount)
	}
	if s.TotalNormalSaleAmt != 300 {
		t.Errorf("TotalNormalSaleAmt = %f, want 300", s.TotalNormalSaleAmt)
	}
	if s.TotalPoolAllocAmt != 80 {
		t.Errorf("TotalPoolAllocAmt = %f, want 80", s.TotalPoolAllocAmt)
	}
	if s.TotalLossAmt != 5 { // 1 × 5
		t.Errorf("TotalLossAmt = %f, want 5", s.TotalLossAmt)
	}
	if s.TotalGrossProfit != 80 {
		t.Errorf("TotalGrossProfit = %f, want 80", s.TotalGrossProfit)
	}
	// gross_profit_rate = 80 / (300+80) = 0.2105
	expected := 80.0 / 380.0
	if absFloat(s.GrossProfitRate-expected) > 0.001 {
		t.Errorf("GrossProfitRate = %f, want %f", s.GrossProfitRate, expected)
	}
}
