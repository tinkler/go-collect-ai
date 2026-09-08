package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ============================================================
// W4.2 promo-harvester: cancel_promotion_fee 单测
// ============================================================

// insertPromo helper
func insertPromo(t *testing.T, ctx context.Context, pool *pgxpool.Pool, supplier, kind, start, end, amount, note string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO promotion_fee (supplier_name, kind, amount, period_start, period_end, source, note)
		VALUES ($1, $2, $3::numeric, $4::date, $5::date, 'test', $6)
	`, supplier, kind, amount, start, end, note)
	if err != nil {
		t.Fatalf("insert promo: %v", err)
	}
}

func TestCancelPromotionFee_HappyPath(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	uniq := uniqueSuffix()
	sup := "t-cancel-1-" + uniq
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM promotion_fee WHERE supplier_name = $1`, sup) })

	// 准备: 2 条 active 堆头
	insertPromo(t, ctx, pool, sup, "堆头", "2026-09-01", "2026-12-31", "5000", "")
	insertPromo(t, ctx, pool, sup, "堆头", "2026-10-01", "2026-11-30", "3000", "短期")

	tool := CancelPromotionFee(pool)
	respAny, err := tool.Call(ctx, marshalForTest(CancelPromotionFeeReq{
		Supplier: sup,
		Kind:     "堆头",
		DryRun:   false,
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp, _ := respAny.(CancelPromotionFeeResp)
	if resp.Action != "cancelled" {
		t.Errorf("action = %q, want cancelled", resp.Action)
	}
	if resp.CancelledCount != 2 {
		t.Errorf("cancelled_count = %d, want 2", resp.CancelledCount)
	}

	// 验: 2 条记录的 period_end 都被改了 (今天 - 1天)
	todayMinus1 := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM promotion_fee
		WHERE supplier_name = $1 AND period_end = $2::date
	`, sup, todayMinus1).Scan(&n); err != nil {
		t.Fatalf("re-query: %v", err)
	}
	if n != 2 {
		t.Errorf("after cancel, %d records should have period_end=%s, want 2", n, todayMinus1)
	}
}

func TestCancelPromotionFee_DryRun(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	uniq := uniqueSuffix()
	sup := "t-cancel-2-" + uniq
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM promotion_fee WHERE supplier_name = $1`, sup) })

	insertPromo(t, ctx, pool, sup, "端架", "2026-09-01", "2026-09-30", "2000", "")

	tool := CancelPromotionFee(pool)
	respAny, err := tool.Call(ctx, marshalForTest(CancelPromotionFeeReq{
		Supplier: sup,
		Kind:     "端架",
		DryRun:   true,
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp, _ := respAny.(CancelPromotionFeeResp)
	if resp.Action != "dry_run" {
		t.Errorf("action = %q, want dry_run", resp.Action)
	}
	if resp.CancelledCount != 1 {
		t.Errorf("cancelled_count = %d, want 1", resp.CancelledCount)
	}

	// 验: 1 条记录, period_end 没动 (dry_run 不写)
	var periodEnd string
	if err := pool.QueryRow(ctx, `SELECT period_end FROM promotion_fee WHERE supplier_name = $1`, sup).Scan(&periodEnd); err != nil {
		t.Fatalf("re-query: %v", err)
	}
	if periodEnd != "2026-09-30" {
		t.Errorf("dry_run should not modify, period_end = %s, want 2026-09-30", periodEnd)
	}
}

func TestCancelPromotionFee_NotFound(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	uniq := uniqueSuffix()
	sup := "t-cancel-3-" + uniq

	tool := CancelPromotionFee(pool)
	respAny, err := tool.Call(ctx, marshalForTest(CancelPromotionFeeReq{
		Supplier: sup,
		Kind:     "DM",
		DryRun:   false,
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp, _ := respAny.(CancelPromotionFeeResp)
	if resp.Action != "not_found" {
		t.Errorf("action = %q, want not_found", resp.Action)
	}
	if resp.CancelledCount != 0 {
		t.Errorf("cancelled_count = %d, want 0", resp.CancelledCount)
	}
}

func TestCancelPromotionFee_InvalidKind(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	tool := CancelPromotionFee(pool)
	_, err := tool.Call(context.Background(), marshalForTest(CancelPromotionFeeReq{
		Supplier: "x",
		Kind:     "乱写",
		DryRun:   false,
	}))
	if err == nil {
		t.Error("invalid kind should fail")
	}
}

func TestCancelPromotionFee_EmptySupplier(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	tool := CancelPromotionFee(pool)
	_, err := tool.Call(context.Background(), marshalForTest(CancelPromotionFeeReq{
		Supplier: "",
		Kind:     "堆头",
		DryRun:   false,
	}))
	if err == nil {
		t.Error("empty supplier should fail")
	}
}

func TestCancelPromotionFee_BadDateFormat(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	tool := CancelPromotionFee(pool)
	_, err := tool.Call(context.Background(), marshalForTest(CancelPromotionFeeReq{
		Supplier:  "x",
		Kind:      "堆头",
		PeriodEnd: "bad-date",
		DryRun:    false,
	}))
	if err == nil {
		t.Error("bad date should fail")
	}
}

func TestCancelPromotionFee_OnlyActiveRecords(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	uniq := uniqueSuffix()
	sup := "t-cancel-4-" + uniq
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM promotion_fee WHERE supplier_name = $1`, sup) })

	// 1 条已过期 (2024) + 1 条 active (2026-12-31)
	insertPromo(t, ctx, pool, sup, "快讯", "2024-09-01", "2024-09-30", "100", "已过期")
	insertPromo(t, ctx, pool, sup, "快讯", "2026-09-01", "2026-12-31", "500", "active")

	tool := CancelPromotionFee(pool)
	respAny, err := tool.Call(ctx, marshalForTest(CancelPromotionFeeReq{
		Supplier: sup,
		Kind:     "快讯",
		DryRun:   false,
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp, _ := respAny.(CancelPromotionFeeResp)
	// 期望: 只 cancel 1 条 (active 那个), 过期的不动
	if resp.CancelledCount != 1 {
		t.Errorf("cancelled_count = %d, want 1 (only active)", resp.CancelledCount)
	}

	// 验: 已过期的 period_end 仍是 2024-09-30
	var expiredPeriodEnd string
	if err := pool.QueryRow(ctx, `SELECT period_end FROM promotion_fee WHERE supplier_name = $1 AND amount = 100`, sup).Scan(&expiredPeriodEnd); err != nil {
		t.Fatalf("re-query: %v", err)
	}
	if expiredPeriodEnd != "2024-09-30" {
		t.Errorf("expired record should not be modified, got period_end = %s", expiredPeriodEnd)
	}
}

func TestCancelPromotionFee_WithNote(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	uniq := uniqueSuffix()
	sup := "t-cancel-5-" + uniq
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM promotion_fee WHERE supplier_name = $1`, sup) })

	insertPromo(t, ctx, pool, sup, "堆头", "2026-09-01", "2026-12-31", "5000", "原 note")

	tool := CancelPromotionFee(pool)
	_, err := tool.Call(ctx, marshalForTest(CancelPromotionFeeReq{
		Supplier: sup,
		Kind:     "堆头",
		Note:     "老板说取消",
		DryRun:   false,
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// 验: note 被 append
	var note string
	if err := pool.QueryRow(ctx, `SELECT note FROM promotion_fee WHERE supplier_name = $1`, sup).Scan(&note); err != nil {
		t.Fatalf("re-query: %v", err)
	}
	if note == "原 note" {
		t.Error("note should be appended")
	}
	if !strings.Contains(note, "老板说取消") {
		t.Errorf("note should contain '老板说取消', got: %q", note)
	}
}
