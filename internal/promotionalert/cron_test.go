package promotionalert

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ============================================================
// mock sender
// ============================================================

type mockSender struct {
	mu    sync.Mutex
	sent  []sentMsg
	failN int
}

type sentMsg struct {
	ChatID string
	Text   string
}

func (s *mockSender) SendText(_ context.Context, chatID, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failN > 0 {
		s.failN--
		return fmt.Errorf("mock network error")
	}
	s.sent = append(s.sent, sentMsg{ChatID: chatID, Text: text})
	return nil
}

func (s *mockSender) Sent() []sentMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sentMsg, len(s.sent))
	copy(out, s.sent)
	return out
}

// ============================================================
// RunOnce 单测
// ============================================================

func TestRunOnce_FindsExpiring(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()

	// 清理旧测试数据
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `DELETE FROM promotion_fee WHERE supplier_name LIKE 't-%'`)

	// 准备 3 条: 1 已过期, 1 3 天后到期, 1 10 天后到期
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	insertFee(t, pool, "t-sup-A", "堆头", 5000, "2026-08-01", "2026-08-31", now) // 已过期
	insertFee(t, pool, "t-sup-B", "堆头", 3000, "2026-09-01", "2026-09-04", now) // 3 天后
	insertFee(t, pool, "t-sup-C", "端架", 2000, "2026-09-01", "2026-09-11", now) // 10 天后 (超 7 天窗口)

	svc := NewService(pool, "office-chat")
	svc.SetNow(func() time.Time { return now })
	svc.DaysAhead = 7

	grouped, err := svc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// 期望: B 在(3 天), A 不在(已过期), C 不在(>7 天)
	if _, ok := grouped["t-sup-A"]; ok {
		t.Errorf("已过期不应在结果中")
	}
	if _, ok := grouped["t-sup-C"]; ok {
		t.Errorf(">7 天不应在结果中")
	}
	bFees, ok := grouped["t-sup-B"]
	if !ok {
		t.Fatalf("t-sup-B 应在结果中, got: %v", grouped)
	}
	if len(bFees) != 1 {
		t.Errorf("t-sup-B 应 1 条, got %d", len(bFees))
	}
	if bFees[0].DaysLeft != 3 {
		t.Errorf("DaysLeft = %d, want 3", bFees[0].DaysLeft)
	}
}

func TestRunOnce_EmptyWhenNoExpiring(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `DELETE FROM promotion_fee WHERE supplier_name LIKE 't-%'`)

	svc := NewService(pool, "office-chat")
	svc.SetNow(func() time.Time { return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) })
	svc.DaysAhead = 7

	grouped, err := svc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(grouped) != 0 {
		t.Errorf("空数据应无结果, got %d 供应商", len(grouped))
	}
}

// ============================================================
// Push 单测
// ============================================================

func TestPush_MultiSupplier_Grouped(t *testing.T) {
	svc := NewService(nil, "office-chat")
	sender := &mockSender{}
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	grouped := map[string][]ExpiringFee{
		"t-sup-A": {
			{FeeID: 1, Supplier: "t-sup-A", Kind: "堆头", Amount: 5000, PeriodEnd: now.AddDate(0, 0, 3), DaysLeft: 3},
		},
		"t-sup-B": {
			{FeeID: 2, Supplier: "t-sup-B", Kind: "端架", Amount: 2000, PeriodEnd: now.AddDate(0, 0, 5), DaysLeft: 5},
		},
	}
	if err := svc.Push(context.Background(), sender, grouped); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got := len(sender.Sent()); got != 2 {
		t.Errorf("应发 2 条(按 supplier 分组), got %d", got)
	}
}

func TestPush_Empty_NoOp(t *testing.T) {
	svc := NewService(nil, "office-chat")
	sender := &mockSender{}
	if err := svc.Push(context.Background(), sender, map[string][]ExpiringFee{}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got := len(sender.Sent()); got != 0 {
		t.Errorf("空 grouped 应不推, got %d", got)
	}
}

func TestPush_NoChatID_NoOp(t *testing.T) {
	svc := NewService(nil, "") // 空 ChatID
	sender := &mockSender{}
	grouped := map[string][]ExpiringFee{
		"t-sup-A": {{Supplier: "t-sup-A", Kind: "堆头", Amount: 5000, PeriodEnd: time.Now(), DaysLeft: 3}},
	}
	if err := svc.Push(context.Background(), sender, grouped); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got := len(sender.Sent()); got != 0 {
		t.Errorf("空 ChatID 应不推, got %d", got)
	}
}

func TestPush_MaxMsgs_5(t *testing.T) {
	svc := NewService(nil, "office-chat")
	sender := &mockSender{}
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	grouped := make(map[string][]ExpiringFee)
	for i := 0; i < 10; i++ {
		sup := fmt.Sprintf("t-sup-%02d", i)
		grouped[sup] = []ExpiringFee{
			{Supplier: sup, Kind: "堆头", Amount: 1000, PeriodEnd: now.AddDate(0, 0, 3), DaysLeft: 3},
		}
	}
	if err := svc.Push(context.Background(), sender, grouped); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got := len(sender.Sent()); got != 5 {
		t.Errorf("max 5 条, got %d", got)
	}
}

// ============================================================
// FormatExpiringMsg 单测
// ============================================================

func TestFormatMsg_SingleFee(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	fees := []ExpiringFee{
		{Supplier: "t-sup", Kind: "堆头", Amount: 5000, PeriodEnd: now.AddDate(0, 0, 3), DaysLeft: 3},
	}
	got := formatExpiringMsg("t-sup", fees)
	if got == "" {
		t.Fatal("msg empty")
	}
	// 期望包含 supplier + kind + amount + days
	for _, sub := range []string{"t-sup", "堆头", "5000", "3", "09-04"} {
		if !contains(got, sub) {
			t.Errorf("msg %q 缺 %q", got, sub)
		}
	}
}

func TestFormatMsg_MultiFee(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	fees := []ExpiringFee{
		{Supplier: "t-sup", Kind: "堆头", Amount: 5000, PeriodEnd: now.AddDate(0, 0, 3), DaysLeft: 3},
		{Supplier: "t-sup", Kind: "端架", Amount: 2000, PeriodEnd: now.AddDate(0, 0, 5), DaysLeft: 5},
	}
	got := formatExpiringMsg("t-sup", fees)
	if !contains(got, "2 笔") {
		t.Errorf("msg 应含 '2 笔': %q", got)
	}
	if !contains(got, "合计") {
		t.Errorf("msg 应含 '合计': %q", got)
	}
	if !contains(got, "3 天后") {
		t.Errorf("msg 应含 '3 天后' (min days): %q", got)
	}
}

// ============================================================
// helpers
// ============================================================

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func insertFee(t *testing.T, pool *pgxpool.Pool, sup, kind string, amount float64, start, end string, _ time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO promotion_fee (supplier_name, kind, amount, period_start, period_end, source, note)
		VALUES ($1, $2, $3, $4::date, $5::date, 'test', 't-test')
	`, sup, kind, amount, start, end)
	if err != nil {
		t.Fatalf("insertFee: %v", err)
	}
}
