package supplierpayment

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
	mu   sync.Mutex
	sent []sentMsg
}

type sentMsg struct {
	ChatID string
	Text   string
}

func (s *mockSender) SendText(_ context.Context, chatID, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
// testhelper (跟 promotionalert 几乎一致, 简化复用)
// ============================================================

func testPool(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	dsn := testDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("PG 不可达: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("PG ping 失败: %v", err)
	}
	if err := migrateW4Tables(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	// setup 前清场 (上次测试残留)
	clearTestData(ctx, pool)
	cleanup := func() {
		cleanCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM supplier_payment_suggestion WHERE supplier_name LIKE 't-%'`)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM supplier_forecast WHERE supplier_name LIKE 't-%'`)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM promotion_fee_share WHERE supplier_name LIKE 't-%'`)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM cash_balance WHERE note LIKE 't-%'`)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM promotion_fee WHERE supplier_name LIKE 't-%'`)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM parse_row WHERE matched_supp LIKE 't-%'`)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM parse_session WHERE supplier_name LIKE 't-%'`)
		pool.Close()
	}
	return pool, cleanup
}

// clearTestData 清空所有 't-%' 测试数据 (跨表)
func clearTestData(ctx context.Context, pool *pgxpool.Pool) {
	_, _ = pool.Exec(ctx, `DELETE FROM supplier_payment_suggestion WHERE supplier_name LIKE 't-%'`)
	_, _ = pool.Exec(ctx, `DELETE FROM supplier_forecast WHERE supplier_name LIKE 't-%'`)
	_, _ = pool.Exec(ctx, `DELETE FROM promotion_fee_share WHERE supplier_name LIKE 't-%'`)
	_, _ = pool.Exec(ctx, `DELETE FROM cash_balance WHERE note LIKE 't-%'`)
	_, _ = pool.Exec(ctx, `DELETE FROM promotion_fee WHERE supplier_name LIKE 't-%' OR note = 't-test'`)
	_, _ = pool.Exec(ctx, `DELETE FROM parse_row WHERE matched_supp LIKE 't-%'`)
	_, _ = pool.Exec(ctx, `DELETE FROM parse_session WHERE supplier_name LIKE 't-%'`)
}

func migrateW4Tables(ctx context.Context, pool *pgxpool.Pool) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS supplier_forecast (
			id              BIGSERIAL PRIMARY KEY,
			supplier_name   TEXT NOT NULL,
			forecast_date   DATE NOT NULL,
			horizon_days    INT NOT NULL,
			amount          NUMERIC(12,2) NOT NULL,
			basis           TEXT NOT NULL DEFAULT '',
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS supplier_payment_suggestion (
			id                    BIGSERIAL PRIMARY KEY,
			supplier_name         TEXT NOT NULL,
			period_days           INT NOT NULL,
			base_forecast         NUMERIC(12,2) NOT NULL,
			investment_weight     NUMERIC(4,2) NOT NULL,
			promo_weight          NUMERIC(4,2) NOT NULL,
			sellthrough_weight    NUMERIC(4,2) NOT NULL,
			payment_cycle_days    INT NOT NULL,
			amount                NUMERIC(12,2) NOT NULL,
			basis                 JSONB NOT NULL DEFAULT '{}'::jsonb,
			status                TEXT NOT NULL DEFAULT 'pending',
			acked_by              TEXT NOT NULL DEFAULT '',
			acked_at              TIMESTAMPTZ,
			created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS promotion_fee_share (
			id              BIGSERIAL PRIMARY KEY,
			supplier_name   TEXT NOT NULL,
			share_month     DATE NOT NULL,
			kind            TEXT NOT NULL,
			amount          NUMERIC(12,2) NOT NULL,
			period_start    DATE NOT NULL,
			period_end      DATE NOT NULL,
			days_in_month   INT NOT NULL,
			note            TEXT NOT NULL DEFAULT '',
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS promotion_fee (
			id              BIGSERIAL PRIMARY KEY,
			supplier_name   TEXT NOT NULL,
			kind            TEXT NOT NULL,
			amount          NUMERIC(12,2) NOT NULL,
			period_start    DATE NOT NULL,
			period_end      DATE NOT NULL,
			note            TEXT NOT NULL DEFAULT '',
			source          TEXT NOT NULL,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS cash_balance (
			id              BIGSERIAL PRIMARY KEY,
			balance_date    DATE NOT NULL UNIQUE,
			amount          NUMERIC(14,2) NOT NULL,
			source          TEXT NOT NULL,
			note            TEXT NOT NULL DEFAULT '',
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS parse_session (
			id              UUID PRIMARY KEY,
			supplier_name   TEXT NOT NULL,
			template_id     TEXT NOT NULL,
			template_name   TEXT NOT NULL,
			mode            TEXT NOT NULL,
			image_path      TEXT NOT NULL,
			image_url       TEXT NOT NULL DEFAULT '',
			raw_ocr_json    JSONB,
			raw_llm_json    JSONB,
			note            TEXT,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS parse_row (
			id              BIGSERIAL PRIMARY KEY,
			session_id      UUID NOT NULL REFERENCES parse_session(id) ON DELETE CASCADE,
			seq             INT NOT NULL,
			raw_barcode     TEXT,
			raw_name        TEXT,
			raw_qty         TEXT,
			matched_barcode TEXT,
			matched_name    TEXT,
			matched_supp    TEXT,
			matched_src     TEXT,
			qty             INT,
			unit_price      NUMERIC(12,2),
			status          TEXT,
			is_new          BOOLEAN,
			stock_qty       NUMERIC(12,2),
			stock_diff      NUMERIC(12,2),
			stock_mismatch  BOOLEAN,
			is_deleted      BOOLEAN NOT NULL DEFAULT FALSE,
			UNIQUE (session_id, seq)
		)`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

func testDSN(t *testing.T) string {
	t.Helper()
	if v := testGetEnv("PG_TEST_DSN"); v != "" {
		return v
	}
	if dsn := testReadEnvFile(".env"); dsn != "" {
		return dsn
	}
	// 试 repo 根
	dsn := testReadEnvFileFromRoot()
	if dsn == "" {
		t.Skip("PG_TEST_DSN 未设置且 .env 读不到")
	}
	return dsn
}

// 简化 env 读取
func testGetEnv(k string) string {
	if v := getEnvDirect(k); v != "" {
		return v
	}
	return ""
}

// ============================================================
// 1. DailyForecast
// ============================================================

func TestRunDailyForecast_NoData_NoCrash(t *testing.T) {
	// devMode PG 有真实数据, 不可能 count=0
	// 测目标: 即使有数据, cron 跑不崩, 返回 err=nil
	pool, cleanup := testPool(t)
	defer cleanup()
	svc := NewService(pool, "")
	svc.SetNow(func() time.Time { return time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC) })

	_, err := svc.RunDailyForecast(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// 不验证 count, 验证 cron 跑通
}

func TestRunDailyForecast_WithData(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	svc := NewService(pool, "")
	svc.SetNow(func() time.Time { return now })

	sup := "t-sup-fc-" + uniq()
	// 准备 1 个 purchase session + 1 row
	insertPurchaseSession(t, pool, sup, now.AddDate(0, 0, -10), "b1", "t-name", 100, 50)

	// 跑前 count
	var beforeCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM supplier_forecast`).Scan(&beforeCount)

	_, err := svc.RunDailyForecast(ctx)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// 验证: 我 insert 的 supplier 出现 3 个 horizon
	var found int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM supplier_forecast WHERE supplier_name=$1`, sup).Scan(&found)
	if found != 3 {
		t.Errorf("supplier %s 应有 3 个 horizon 记录, got %d", sup, found)
	}

	// 总数应 ≥ beforeCount + 3
	var afterCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM supplier_forecast`).Scan(&afterCount)
	if afterCount < beforeCount+3 {
		t.Errorf("after=%d, before=%d, 应至少 +3", afterCount, beforeCount)
	}
}

// ============================================================
// 2. WeeklySuggestions
// ============================================================

func TestRunWeeklySuggestions(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	svc := NewService(pool, "")
	svc.SetNow(func() time.Time { return now })

	sup := "t-sup-pay2-" + uniq()
	insertPurchaseSession(t, pool, sup, now.AddDate(0, 0, -10), "b1", "t-name", 100, 50)

	// 跑前 count
	var beforeCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM supplier_payment_suggestion WHERE status='pending'`).Scan(&beforeCount)

	_, err := svc.RunWeeklySuggestions(ctx)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// 验证: 我 insert 的 supplier 出现在 pending 里
	var found int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM supplier_payment_suggestion WHERE supplier_name=$1 AND status='pending'`, sup).Scan(&found)
	if found < 1 {
		t.Errorf("supplier %s 应在 pending, got %d", sup, found)
	}

	// 总数应 ≥ beforeCount + 1
	var afterCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM supplier_payment_suggestion WHERE status='pending'`).Scan(&afterCount)
	if afterCount < beforeCount+1 {
		t.Errorf("after=%d, before=%d, 应至少 +1", afterCount, beforeCount)
	}
}

// ============================================================
// 3. MonthlyShare
// ============================================================

func TestRunMonthlyShare(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	svc := NewService(pool, "")
	svc.SetNow(func() time.Time { return now })

	// 上月 (8 月) 一笔
	sup := "t-sup-share-" + uniq()
	_, err := pool.Exec(ctx, `
		INSERT INTO promotion_fee (supplier_name, kind, amount, period_start, period_end, source, note)
		VALUES ($1, '堆头', 3000, '2026-08-01', '2026-08-31', 'test', 't-test')
	`, sup)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// 跑前 count
	var beforeCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM promotion_fee_share`).Scan(&beforeCount)

	_, err = svc.RunMonthlyShare(ctx)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// 验证: 我 insert 的 sup 出现
	var found int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM promotion_fee_share WHERE supplier_name=$1`, sup).Scan(&found)
	if found != 1 {
		t.Errorf("supplier %s 应有 1 条分摊, got %d", sup, found)
	}

	// 总数应 ≥ beforeCount + 1
	var afterCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM promotion_fee_share`).Scan(&afterCount)
	if afterCount < beforeCount+1 {
		t.Errorf("after=%d, before=%d, 应至少 +1", afterCount, beforeCount)
	}
}

// ============================================================
// 4. DailyCashCheck
// ============================================================

func TestRunDailyCashCheck_NoData_NoPush(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	svc := NewService(pool, "owner-chat")
	svc.SetNow(func() time.Time { return now })
	sender := &mockSender{}

	res, err := svc.RunDailyCashCheck(ctx, sender)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// 无 cash / 无 pending → shortBy=0, 不推
	if res.ShortBy != 0 {
		t.Errorf("shortBy = %v, want 0", res.ShortBy)
	}
	if res.AlertPushed {
		t.Error("无缺口不应推送")
	}
}

func TestRunDailyCashCheck_Shortage_Push(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	svc := NewService(pool, "owner-chat")
	svc.SetNow(func() time.Time { return now })

	// 1) 录入 cash = 1000
	today := now.UTC().Truncate(24 * time.Hour)
	_, err := pool.Exec(ctx, `
		INSERT INTO cash_balance (balance_date, amount, source, note) VALUES ($1, 1000, 'test', 't-test')
	`, today)
	if err != nil {
		t.Fatalf("insert cash: %v", err)
	}

	// 2) 插 pending 5000
	_, err = pool.Exec(ctx, `
		INSERT INTO supplier_payment_suggestion
		(supplier_name, period_days, base_forecast, investment_weight, promo_weight, sellthrough_weight, payment_cycle_days, amount, basis, status)
		VALUES ('t-sup-shortage-' || gen_random_uuid()::text, 30, 5000, 1.0, 1.0, 1.0, 30, 5000, '{}'::jsonb, 'pending')
	`)
	if err != nil {
		t.Fatalf("insert pending: %v", err)
	}

	sender := &mockSender{}
	res, err := svc.RunDailyCashCheck(ctx, sender)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.ShortBy >= 0 {
		t.Errorf("shortBy = %v, want < 0 (缺口)", res.ShortBy)
	}
	if !res.AlertPushed {
		t.Error("缺口应推送 owner")
	}
	if got := len(sender.Sent()); got != 1 {
		t.Errorf("应发 1 条, got %d", got)
	}
	if got := sender.Sent()[0].Text; !contains(got, "现金日报") {
		t.Errorf("msg 应含'现金日报', got: %q", got)
	}
}

// ============================================================
// helpers
// ============================================================

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func uniq() string {
	return time.Now().Format("150405.000000") + "-" + fmt.Sprintf("%d", time.Now().UnixNano()%100000)
}

func insertPurchaseSession(t *testing.T, pool *pgxpool.Pool, sup string, createdAt time.Time, barcode, name string, qty, price int) {
	t.Helper()
	ctx := context.Background()
	var sessID string
	err := pool.QueryRow(ctx, `
		INSERT INTO parse_session (id, supplier_name, template_id, template_name, mode, image_path, source, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, 't-tpl', 't-tpl', 'purchase', '/tmp/x.jpg', 'test', $2, $2)
		RETURNING id
	`, sup, createdAt).Scan(&sessID)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO parse_row (session_id, seq, raw_barcode, matched_barcode, matched_name, matched_supp, qty, unit_price, status, is_deleted)
		VALUES ($1, 1, $2, $2, $3, $4, $5, $6, 'OK', FALSE)
	`, sessID, barcode, name, sup, qty, price)
	if err != nil {
		t.Fatalf("insert row: %v", err)
	}
}
