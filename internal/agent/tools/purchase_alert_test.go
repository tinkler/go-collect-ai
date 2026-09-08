package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tinkler/collect-ai/internal/business"
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

// marshalForTest 把任意 struct 编成 json.RawMessage 供 tool.Call 调
//   之前 (2026-09-04 之前) 散落在多个 _test.go 引用但未定义, 致 tools 包 build 失败
//   2026-09-04 统一补在 purchase_alert_test.go (payment_w42_test.go / policy_w42_test.go 都用这个)
func marshalForTest(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// itoaTest small helper for test assertions
func itoaTest(n int64) string {
	return strconv.FormatInt(n, 10)
}

// contains / indexOf 简单字符串包含检查 (避免引入 strings.Contains 编译时差)
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
// QueryReturnOrder: W4.4 新增, 等 cube 数据源 (Fn=nil = 降级)
// 覆盖: Fn=nil 自动降级 / Fn 返 hint / Fn 返 list / days 校验 / supplier 必填
// ============================================================

func TestQueryReturnOrder_FnNil_Downgrade(t *testing.T) {
	// 没注入 Fn → cube 数据源未接入, 应返 not_available=true + hint
	resp, err := queryReturnOrderImpl(context.Background(), nil, QueryReturnOrderReq{
		Supplier: "汇一",
		Status:   "pending",
		Days:     30,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !resp.NotAvailable {
		t.Error("NotAvailable should be true when Fn is nil")
	}
	if resp.Count != 0 {
		t.Errorf("Count = %d, want 0", resp.Count)
	}
	if resp.Hint == "" {
		t.Error("Hint should be set for downgrade path")
	}
	if !contains(resp.Hint, "数据源未注入") {
		t.Errorf("Hint 应含 '数据源未注入', got %q", resp.Hint)
	}
}

func TestQueryReturnOrder_SupplierRequired(t *testing.T) {
	fn := func(ctx context.Context, supplier, status string, days int) ([]business.ReturnOrder, string, error) {
		t.Fatal("Fn should not be called when supplier empty")
		return nil, "", nil
	}
	_, err := queryReturnOrderImpl(context.Background(), fn, QueryReturnOrderReq{
		Supplier: "",
		Days:     30,
	})
	if err == nil {
		t.Error("应报错: supplier 必填")
	}
}

func TestQueryReturnOrder_InvalidDays(t *testing.T) {
	fn := func(ctx context.Context, supplier, status string, days int) ([]business.ReturnOrder, string, error) {
		t.Fatal("Fn should not be called when days invalid")
		return nil, "", nil
	}
	_, err := queryReturnOrderImpl(context.Background(), fn, QueryReturnOrderReq{
		Supplier: "汇一",
		Days:     45, // 不在 7/30/60/90
	})
	if err == nil {
		t.Error("应报错: days 非法")
	}
	if !contains(err.Error(), "days 必须") {
		t.Errorf("err 应含 'days 必须', got %q", err.Error())
	}
}

func TestQueryReturnOrder_DefaultDays(t *testing.T) {
	// days 留空 = 0, 应默认 30
	var capturedDays int
	fn := func(ctx context.Context, supplier, status string, days int) ([]business.ReturnOrder, string, error) {
		capturedDays = days
		return []business.ReturnOrder{}, "", nil
	}
	_, err := queryReturnOrderImpl(context.Background(), fn, QueryReturnOrderReq{
		Supplier: "汇一",
		// Days 留空
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if capturedDays != 30 {
		t.Errorf("default days = %d, want 30", capturedDays)
	}
}

func TestQueryReturnOrder_FnReturnsHint_Downgrade(t *testing.T) {
	// Fn 自己返 hint (e.g. "mapping 缺失") → 走降级路径
	fn := func(ctx context.Context, supplier, status string, days int) ([]business.ReturnOrder, string, error) {
		return nil, "entities.returns 未在 mapping.yaml 配", nil
	}
	resp, err := queryReturnOrderImpl(context.Background(), fn, QueryReturnOrderReq{
		Supplier: "汇一",
		Status:   "pending",
		Days:     30,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !resp.NotAvailable {
		t.Error("NotAvailable should be true when Fn returns hint")
	}
	if !contains(resp.Hint, "mapping.yaml") {
		t.Errorf("Hint 应透传 Fn 的提示, got %q", resp.Hint)
	}
}

func TestQueryReturnOrder_FnReturnsError_Downgrade(t *testing.T) {
	// Fn 返 error → 也走降级, 不阻断
	fn := func(ctx context.Context, supplier, status string, days int) ([]business.ReturnOrder, string, error) {
		return nil, "", errors.New("cube 连接超时")
	}
	resp, err := queryReturnOrderImpl(context.Background(), fn, QueryReturnOrderReq{
		Supplier: "汇一",
		Status:   "pending",
		Days:     30,
	})
	if err != nil {
		t.Fatalf("应降级不报错, got err: %v", err)
	}
	if !resp.NotAvailable {
		t.Error("NotAvailable should be true when Fn errors")
	}
	if !contains(resp.Hint, "cube query 失败") {
		t.Errorf("Hint 应说明失败原因, got %q", resp.Hint)
	}
}

func TestQueryReturnOrder_FnReturnsList(t *testing.T) {
	// 正常路径: Fn 返 list → 计数 + 金额汇总 + 透传
	fn := func(ctx context.Context, supplier, status string, days int) ([]business.ReturnOrder, string, error) {
		return []business.ReturnOrder{
			{BillNo: "RO202609030001", SupplierID: "0001", Supplier: "汇一",
				ReturnMoney: 36.00, Status: "pending", CreateDate: "2026-09-01", BranchNo: "0001"},
			{BillNo: "RO202609030002", SupplierID: "0001", Supplier: "汇一",
				ReturnMoney: 84.50, Status: "pending", CreateDate: "2026-09-02", BranchNo: "0001"},
		}, "", nil
	}
	resp, err := queryReturnOrderImpl(context.Background(), fn, QueryReturnOrderReq{
		Supplier: "汇一",
		Status:   "pending",
		Days:     30,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.NotAvailable {
		t.Error("NotAvailable should be false on success path")
	}
	if resp.Count != 2 {
		t.Errorf("Count = %d, want 2", resp.Count)
	}
	if resp.TotalMoney != 120.50 {
		t.Errorf("TotalMoney = %f, want 120.50", resp.TotalMoney)
	}
	if len(resp.Returns) != 2 {
		t.Errorf("Returns len = %d, want 2", len(resp.Returns))
	}
	if resp.Returns[0].BillNo != "RO202609030001" {
		t.Errorf("Returns[0].BillNo = %q", resp.Returns[0].BillNo)
	}
	if resp.Hint != "" {
		t.Errorf("Hint should be empty on success, got %q", resp.Hint)
	}
}

func TestQueryReturnOrder_FnReturnsEmpty(t *testing.T) {
	// Fn 返空 list = 该 supplier 无未审批退货单 = 不报 (但 tool 调用成功)
	fn := func(ctx context.Context, supplier, status string, days int) ([]business.ReturnOrder, string, error) {
		return []business.ReturnOrder{}, "", nil
	}
	resp, err := queryReturnOrderImpl(context.Background(), fn, QueryReturnOrderReq{
		Supplier: "汇一",
		Status:   "pending",
		Days:     30,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.NotAvailable {
		t.Error("NotAvailable should be false (有数据, 只是空)")
	}
	if resp.Count != 0 {
		t.Errorf("Count = %d, want 0", resp.Count)
	}
	if resp.TotalMoney != 0 {
		t.Errorf("TotalMoney = %f, want 0", resp.TotalMoney)
	}
}

func TestQueryReturnOrder_AllDaysValid(t *testing.T) {
	// 验证 4 个合法 days 都能通过
	for _, d := range []int{7, 30, 60, 90} {
		fn := func(ctx context.Context, supplier, status string, days int) ([]business.ReturnOrder, string, error) {
			return nil, "", nil
		}
		_, err := queryReturnOrderImpl(context.Background(), fn, QueryReturnOrderReq{
			Supplier: "汇一",
			Days:     d,
		})
		if err != nil {
			t.Errorf("days=%d 应合法, got err: %v", d, err)
		}
	}
}
