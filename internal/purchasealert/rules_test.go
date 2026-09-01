package purchasealert

import (
	"context"
	"testing"
	"time"

	"github.com/tinkler/collect-ai/internal/model"
)

// ============================================================
// 测试 fixture
// ============================================================

func sess() *model.Session {
	return &model.Session{
		ID:           "test-sess-1",
		SupplierName: "汇一",
		Mode:         model.ModePurchase,
	}
}

func row(supplier, name string, rowID int64) *model.SkuRow {
	return &model.SkuRow{
		RowID:       rowID,
		Seq:         1,
		MatchedSupp: supplier,
		MatchedName: name,
	}
}

func ctx() RuleContext {
	return RuleContext{
		SupplierPolicies: map[string][]PolicyKV{
			"汇一": {
				{Key: "is_self_procure", Val: true},
				{Key: "has_duitou", Val: true},
			},
			"榄菊": {
				{Key: "allow_return", Val: false},
				{Key: "block_entry", Val: true},
			},
			"普通供应商": {
				{Key: "is_self_procure", Val: false},
			},
		},
		Holidays: []Holiday{},
		Now:      time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
	}
}

// ============================================================
// BlockEntry
// ============================================================

func TestBlockEntry_Fires(t *testing.T) {
	r := BlockEntryRule{}
	got := r.Apply(context.Background(), sess(), row("榄菊", "蚊香", 1), ctx())
	if len(got) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(got))
	}
	if got[0].Rule != "block_entry" || got[0].Severity != "block" {
		t.Errorf("unexpected alert: %+v", got[0])
	}
	if got[0].RowID != 1 {
		t.Errorf("row_id = %d, want 1", got[0].RowID)
	}
}

func TestBlockEntry_NoFire(t *testing.T) {
	r := BlockEntryRule{}
	got := r.Apply(context.Background(), sess(), row("汇一", "可口可乐", 1), ctx())
	if len(got) != 0 {
		t.Errorf("汇一未限入场,应不触发, got %d", len(got))
	}
}

func TestBlockEntry_NoSupplier(t *testing.T) {
	r := BlockEntryRule{}
	got := r.Apply(context.Background(), sess(), row("", "无名", 1), ctx())
	if len(got) != 0 {
		t.Errorf("无供应商应不触发, got %d", len(got))
	}
}

// ============================================================
// NoReturn
// ============================================================

func TestNoReturn_Fires(t *testing.T) {
	r := NoReturnRule{}
	got := r.Apply(context.Background(), sess(), row("榄菊", "蚊香", 1), ctx())
	if len(got) != 1 || got[0].Rule != "no_return" || got[0].Severity != "warn" {
		t.Errorf("unexpected: %+v", got)
	}
}

func TestNoReturn_NoFire(t *testing.T) {
	r := NoReturnRule{}
	got := r.Apply(context.Background(), sess(), row("汇一", "可口可乐", 1), ctx())
	if len(got) != 0 {
		t.Errorf("汇一 allow_return 默认(无 policy)=true, 应不触发, got %d", len(got))
	}
}

// ============================================================
// Offseason
// ============================================================

func TestOffseason_FiresInWrongSeason(t *testing.T) {
	r := OffseasonRule{}
	// 9 月(秋), 期望 "冰品" 触发
	got := r.Apply(context.Background(), sess(), row("普通供应商", "冰品大全", 1), ctx())
	if len(got) != 1 || got[0].Rule != "offseason" || got[0].Severity != "info" {
		t.Errorf("秋季命中冰品应触发, got %+v", got)
	}
}

func TestOffseason_NoFireInRightSeason(t *testing.T) {
	r := OffseasonRule{}
	c := ctx()
	// 改到 7 月(夏)
	c.Now = time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	got := r.Apply(context.Background(), sess(), row("普通供应商", "冰品大全", 1), c)
	if len(got) != 0 {
		t.Errorf("夏季命中冰品应不触发, got %+v", got)
	}
}

func TestOffseason_FiresWinterProductInSummer(t *testing.T) {
	r := OffseasonRule{}
	c := ctx()
	c.Now = time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC) // 夏
	got := r.Apply(context.Background(), sess(), row("普通供应商", "暖手宝", 1), c)
	if len(got) != 1 {
		t.Errorf("夏季补暖手宝应触发, got %d", len(got))
	}
}

// ============================================================
// HolidayLead
// ============================================================

func TestHolidayLead_Fires(t *testing.T) {
	r := HolidayLeadRule{}
	c := ctx()
	// 9/1 出发, 9/8 中秋 + lead_days=7
	c.Holidays = []Holiday{
		{Date: time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC), Type: "holiday", Name: "中秋节", LeadDays: 7},
	}
	got := r.Apply(context.Background(), sess(), nil, c)
	if len(got) != 1 || got[0].Rule != "holiday_lead" {
		t.Errorf("中秋前 7 天应触发, got %+v", got)
	}
	if got[0].RowID != 0 {
		t.Errorf("holiday_lead 应是 session 级别, got row_id=%d", got[0].RowID)
	}
}

func TestHolidayLead_NoFire_FarFuture(t *testing.T) {
	r := HolidayLeadRule{}
	c := ctx()
	c.Holidays = []Holiday{
		// 60 天后, lead_days=7, 超过 lead 窗口不触发
		{Date: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Type: "holiday", Name: "元旦", LeadDays: 7},
	}
	got := r.Apply(context.Background(), sess(), nil, c)
	if len(got) != 0 {
		t.Errorf("60 天后的节日不在 lead 窗口,应不触发, got %+v", got)
	}
}

func TestHolidayLead_NoFire_Past(t *testing.T) {
	r := HolidayLeadRule{}
	c := ctx()
	c.Holidays = []Holiday{
		{Date: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Type: "holiday", Name: "建军节", LeadDays: 7},
	}
	got := r.Apply(context.Background(), sess(), nil, c)
	if len(got) != 0 {
		t.Errorf("过去的节日应不触发, got %+v", got)
	}
}

func TestHolidayLead_PickNearest(t *testing.T) {
	r := HolidayLeadRule{}
	c := ctx()
	c.Holidays = []Holiday{
		{Date: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC), Type: "holiday", Name: "A", LeadDays: 7},
		{Date: time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC), Type: "holiday", Name: "B", LeadDays: 7},
	}
	got := r.Apply(context.Background(), sess(), nil, c)
	if len(got) != 1 {
		t.Fatalf("应选最近的节日, got %d alerts", len(got))
	}
	if got[0].Message == "" || !contains(got[0].Message, "A") {
		// 9/1 + 4 days = 9/5 (A, 距 4 天 < lead_days=7)
		// 9/6 (B) 距 5 天 < 7, 但比 A 远
		// 应选 A
		if !contains(got[0].Message, "A") {
			t.Errorf("应选最近的 'A', got: %q", got[0].Message)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (s == sub ||
		indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// ============================================================
// currentSeason
// ============================================================

func TestCurrentSeason(t *testing.T) {
	cases := []struct {
		month time.Month
		want  string
	}{
		{time.January, "winter"},
		{time.March, "spring"},
		{time.June, "summer"},
		{time.September, "autumn"},
		{time.December, "winter"},
	}
	for _, tc := range cases {
		got := currentSeason(time.Date(2026, tc.month, 1, 0, 0, 0, 0, time.UTC))
		if got != tc.want {
			t.Errorf("month %s = %q, want %q", tc.month, got, tc.want)
		}
	}
}
