package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ============================================================
// QuerySkuStock: no DB required, mock QuerySkuStockFn
// ============================================================

func TestQuerySkuStock_Hit(t *testing.T) {
	fixedTime := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	fn := func(ctx context.Context, itemNo, barcode string) (string, string, float64, string, time.Time, bool, error) {
		if barcode != "6901234567890" {
			return "", "", 0, "", time.Time{}, true, nil
		}
		return "000123", "Coca Cola 330ml", 47.0, "0001", fixedTime, false, nil
	}

	resp, err := querySkuStockImpl(context.Background(), fn, QuerySkuStockReq{Barcode: "6901234567890"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.ItemNo != "000123" {
		t.Errorf("item_no = %q, want 000123", resp.ItemNo)
	}
	if resp.StockQty != 47.0 {
		t.Errorf("stock_qty = %f, want 47", resp.StockQty)
	}
	if resp.ItemName != "Coca Cola 330ml" {
		t.Errorf("item_name = %q", resp.ItemName)
	}
	if resp.NotFound {
		t.Error("NotFound should be false")
	}
}

func TestQuerySkuStock_NotFound(t *testing.T) {
	fn := func(ctx context.Context, itemNo, barcode string) (string, string, float64, string, time.Time, bool, error) {
		return "", "", 0, "", time.Now(), true, nil
	}
	resp, err := querySkuStockImpl(context.Background(), fn, QuerySkuStockReq{ItemNo: "999999"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !resp.NotFound {
		t.Error("NotFound should be true")
	}
}

func TestQuerySkuStock_EmptyArgs(t *testing.T) {
	fn := func(ctx context.Context, itemNo, barcode string) (string, string, float64, string, time.Time, bool, error) {
		return "", "", 0, "", time.Time{}, false, nil
	}
	_, err := querySkuStockImpl(context.Background(), fn, QuerySkuStockReq{})
	if err == nil {
		t.Error("empty item_no and barcode should fail")
	}
}

func TestQuerySkuStock_CubeError(t *testing.T) {
	fn := func(ctx context.Context, itemNo, barcode string) (string, string, float64, string, time.Time, bool, error) {
		return "", "", 0, "", time.Time{}, false, errors.New("cube timeout")
	}
	_, err := querySkuStockImpl(context.Background(), fn, QuerySkuStockReq{Barcode: "123"})
	if err == nil {
		t.Error("cube error should be returned")
	}
}

func TestQuerySkuStock_NilFn(t *testing.T) {
	_, err := querySkuStockImpl(context.Background(), nil, QuerySkuStockReq{Barcode: "123"})
	if err == nil {
		t.Error("nil fn should fail")
	}
}

// ============================================================
// QuerySkuSales: no DB required, mock
// ============================================================

func TestQuerySkuSales_Hit(t *testing.T) {
	fn := func(ctx context.Context, itemNo string, days int) (string, float64, float64, bool, error) {
		if itemNo == "000123" && days == 30 {
			return "Coca Cola 330ml", 12.0, 30.0, false, nil
		}
		return "", 0, 0, true, nil
	}

	resp, err := querySkuSalesImpl(context.Background(), fn, QuerySkuSalesReq{ItemNo: "000123", Days: 30})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.ItemName != "Coca Cola 330ml" {
		t.Errorf("item_name = %q", resp.ItemName)
	}
	if resp.TotalQty != 12.0 {
		t.Errorf("total_qty = %f, want 12", resp.TotalQty)
	}
	if resp.DailyAvg != 0.4 {
		t.Errorf("daily_avg = %f, want 0.4 (12/30)", resp.DailyAvg)
	}
}

func TestQuerySkuSales_NotFound(t *testing.T) {
	fn := func(ctx context.Context, itemNo string, days int) (string, float64, float64, bool, error) {
		return "", 0, 0, true, nil
	}
	resp, err := querySkuSalesImpl(context.Background(), fn, QuerySkuSalesReq{ItemNo: "999999", Days: 30})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !resp.NotFound {
		t.Error("NotFound should be true")
	}
}

func TestQuerySkuSales_InvalidDays(t *testing.T) {
	fn := func(ctx context.Context, itemNo string, days int) (string, float64, float64, bool, error) {
		return "", 0, 0, false, nil
	}
	_, err := querySkuSalesImpl(context.Background(), fn, QuerySkuSalesReq{ItemNo: "1", Days: 7})
	if err == nil {
		t.Error("days=7 should fail (only accept 30/60/90)")
	}
}

func TestQuerySkuSales_EmptyItemNo(t *testing.T) {
	fn := func(ctx context.Context, itemNo string, days int) (string, float64, float64, bool, error) {
		return "", 0, 0, false, nil
	}
	_, err := querySkuSalesImpl(context.Background(), fn, QuerySkuSalesReq{Days: 30})
	if err == nil {
		t.Error("empty item_no should fail")
	}
}

// ============================================================
// QueryAppSettings / InsertPurchaseAlert / UpdateAnalysisStatus:
//   DB required, use testPool from testhelper_test.go
// ============================================================

func setupPurchaseAlertTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS app_settings (
			key             TEXT PRIMARY KEY,
			value           JSONB NOT NULL,
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS purchase_session_alert (
			id              BIGSERIAL PRIMARY KEY,
			session_id      UUID NOT NULL,
			row_id          BIGINT,
			rule            TEXT NOT NULL,
			severity        TEXT NOT NULL,
			category        TEXT NOT NULL DEFAULT 'info',
			message         TEXT NOT NULL,
			acked_at        TIMESTAMPTZ,
			acked_by        TEXT NOT NULL DEFAULT '',
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS parse_session (
			id              UUID PRIMARY KEY,
			supplier_name   TEXT NOT NULL,
			mode            TEXT NOT NULL,
			image_path      TEXT NOT NULL DEFAULT '',
			image_url       TEXT NOT NULL DEFAULT '',
			image_paths     JSONB NOT NULL DEFAULT '[]'::jsonb,
			image_urls      JSONB NOT NULL DEFAULT '[]'::jsonb,
			image_hashes    JSONB NOT NULL DEFAULT '[]'::jsonb,
			source          TEXT NOT NULL DEFAULT 'test',
			note            TEXT NOT NULL DEFAULT '',
			strategy_version INT NOT NULL DEFAULT 0,
			analysis_status TEXT NOT NULL DEFAULT 'pending',
			analysis_at     TIMESTAMPTZ,
			analysis_error  TEXT NOT NULL DEFAULT '',
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("create table failed: %v (sql: %s)", err, s)
		}
	}
}

// queryAppSettingsImpl helper: wraps tool.Call and asserts type
func queryAppSettingsImpl(ctx context.Context, pool *pgxpool.Pool, req QueryAppSettingsReq) (QueryAppSettingsResp, error) {
	tool := QueryAppSettings(pool)
	respAny, err := tool.Call(ctx, marshalForTest(req))
	if err != nil {
		return QueryAppSettingsResp{}, err
	}
	resp, _ := respAny.(QueryAppSettingsResp)
	return resp, nil
}

func TestQueryAppSettings_Hit(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	setupPurchaseAlertTables(t, pool)
	ctx := context.Background()
	uniq := time.Now().UnixNano()
	key := "high_stock_threshold_" + itoaTest(uniq)

	_, err := pool.Exec(ctx, `INSERT INTO app_settings (key, value) VALUES ($1, $2::jsonb)`,
		key, "80")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM app_settings WHERE key = $1`, key) })

	resp, err := queryAppSettingsImpl(ctx, pool, QueryAppSettingsReq{Key: key})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Key != key {
		t.Errorf("key = %q", resp.Key)
	}
}

func TestQueryAppSettings_NotFound(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	setupPurchaseAlertTables(t, pool)
	_, err := queryAppSettingsImpl(context.Background(), pool,
		QueryAppSettingsReq{Key: "non_existent_" + itoaTest(time.Now().UnixNano())})
	if err != nil {
		t.Fatalf("err: %v (not-found should not error, returns empty)", err)
	}
}

func TestQueryAppSettings_EmptyKey(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	setupPurchaseAlertTables(t, pool)
	_, err := queryAppSettingsImpl(context.Background(), pool, QueryAppSettingsReq{Key: ""})
	if err == nil {
		t.Error("empty key should fail")
	}
}

func TestInsertPurchaseAlert_RoundTrip(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	setupPurchaseAlertTables(t, pool)
	ctx := context.Background()
	sessID := "11111111-1111-1111-1111-111111111111"
	_, err := pool.Exec(ctx, `INSERT INTO parse_session (id, supplier_name, mode) VALUES ($1, 'test', 'purchase')`, sessID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM purchase_session_alert WHERE session_id = $1`, sessID)
		_, _ = pool.Exec(ctx, `DELETE FROM parse_session WHERE id = $1`, sessID)
	})

	tool := InsertPurchaseAlert(pool)
	respAny, err := tool.Call(ctx, marshalForTest(InsertPurchaseAlertReq{
		SessionID: sessID,
		RowID:     0,
		Rule:      "high_stock",
		Severity:  "warn",
		Category:  "warn",
		Message:   "Test alert message",
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp, ok := respAny.(InsertPurchaseAlertResp)
	if !ok {
		t.Fatalf("type = %T", respAny)
	}
	if resp.AlertID <= 0 {
		t.Errorf("alert_id = %d, want > 0", resp.AlertID)
	}

	var cat string
	if err := pool.QueryRow(ctx, `SELECT category FROM purchase_session_alert WHERE id = $1`, resp.AlertID).Scan(&cat); err != nil {
		t.Fatalf("re-query: %v", err)
	}
	if cat != "warn" {
		t.Errorf("category = %q, want warn", cat)
	}
}

func TestUpdateAnalysisStatus_Done(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	setupPurchaseAlertTables(t, pool)
	ctx := context.Background()
	sessID := "22222222-2222-2222-2222-222222222222"
	_, err := pool.Exec(ctx, `INSERT INTO parse_session (id, supplier_name, mode) VALUES ($1, 'test', 'purchase')`, sessID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM parse_session WHERE id = $1`, sessID)
	})

	tool := UpdateAnalysisStatus(pool)
	respAny, err := tool.Call(ctx, marshalForTest(UpdateAnalysisStatusReq{
		SessionID: sessID,
		Status:    "done",
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp, _ := respAny.(UpdateAnalysisStatusResp)
	if resp.AnalysisStatus != "done" {
		t.Errorf("status = %q", resp.AnalysisStatus)
	}
	if resp.AnalysisAt == nil {
		t.Error("done should set analysis_at")
	}

	var at *time.Time
	if err := pool.QueryRow(ctx, `SELECT analysis_at FROM parse_session WHERE id = $1`, sessID).Scan(&at); err != nil {
		t.Fatalf("re-query: %v", err)
	}
	if at == nil {
		t.Error("DB analysis_at should be non-null")
	}
}

func TestUpdateAnalysisStatus_Failed(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	setupPurchaseAlertTables(t, pool)
	ctx := context.Background()
	sessID := "33333333-3333-3333-3333-333333333333"
	_, err := pool.Exec(ctx, `INSERT INTO parse_session (id, supplier_name, mode) VALUES ($1, 'test', 'purchase')`, sessID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM parse_session WHERE id = $1`, sessID)
	})

	tool := UpdateAnalysisStatus(pool)
	respAny, err := tool.Call(ctx, marshalForTest(UpdateAnalysisStatusReq{
		SessionID: sessID,
		Status:    "failed",
		Error:     "LLM timeout",
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp, _ := respAny.(UpdateAnalysisStatusResp)
	if resp.AnalysisError != "LLM timeout" {
		t.Errorf("error = %q", resp.AnalysisError)
	}
	if resp.AnalysisAt != nil {
		t.Error("failed should not set analysis_at")
	}
}

func TestUpdateAnalysisStatus_InvalidStatus(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	setupPurchaseAlertTables(t, pool)
	tool := UpdateAnalysisStatus(pool)
	_, err := tool.Call(context.Background(), marshalForTest(UpdateAnalysisStatusReq{
		SessionID: "00000000-0000-0000-0000-000000000000",
		Status:    "bogus",
	}))
	if err == nil {
		t.Error("bogus status should fail")
	}
}

// ============================================================
// helpers
// ============================================================

// itoaTest small helper, named differently from tools_test.go's itoa
func itoaTest(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// marshalForTest wraps json.Marshal for tool.Call args
func marshalForTest(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
