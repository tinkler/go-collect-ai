package tools

import (
	"context"
	"testing"
	"time"
)

// ============================================================
// W4 D 模块工具单测
// 4 个工具: compute_promotion_fee_share / upcoming_promotion_expiry /
//           forecast_purchase_amount / suggest_supplier_payment
// ============================================================

func TestComputePromotionFeeShare_HappyPath(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	sup := "t-sup-share-" + uniq()

	// 准备: 9 月一笔堆头 3000 元 (9/1~9/30 全月覆盖)
	_, err := pool.Exec(ctx, `
		INSERT INTO promotion_fee (supplier_name, kind, amount, period_start, period_end, source, note)
		VALUES ($1, '堆头', 3000, '2026-09-01', '2026-09-30', 'test', 't-test')
	`, sup)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	tool := ComputePromotionFeeShare(pool)
	rawArgs := mustJSON(t, ComputePromotionFeeShareReq{Supplier: sup, Month: "2026-09"})
	out, err := tool.Call(ctx, rawArgs)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	got := mustParse[ComputePromotionFeeShareResp](t, out)
	if got.Count != 1 {
		t.Errorf("count = %d, want 1", got.Count)
	}
	if got.Items[0].Kind != "堆头" {
		t.Errorf("kind = %q, want 堆头", got.Items[0].Kind)
	}
	if got.Items[0].MonthShare != 3000 {
		// 9/1~9/30 全月 = 30 天, amount=3000, overlap=30, total=30 → month_share = 3000 * 30/30 = 3000
		t.Errorf("month_share = %v, want 3000", got.Items[0].MonthShare)
	}
	if got.TotalShare != 3000 {
		t.Errorf("total_share = %v, want 3000", got.TotalShare)
	}
}

func TestComputePromotionFeeShare_OverlapHalfMonth(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	sup := "t-sup-half-" + uniq()
	// 9/1~9/15 一半月
	_, err := pool.Exec(ctx, `
		INSERT INTO promotion_fee (supplier_name, kind, amount, period_start, period_end, source, note)
		VALUES ($1, '端架', 2000, '2026-09-01', '2026-09-15', 'test', 't-test')
	`, sup)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	tool := ComputePromotionFeeShare(pool)
	rawArgs := mustJSON(t, ComputePromotionFeeShareReq{Supplier: sup, Month: "2026-09"})
	out, _ := tool.Call(ctx, rawArgs)
	got := mustParse[ComputePromotionFeeShareResp](t, out)
	// 9/1~9/15 = 15 天, 当月 9 月 30 天, amount=2000
	// month_share = 2000 * 15/15 = 2000 (period 全在当月)
	if got.Items[0].MonthShare != 2000 {
		t.Errorf("half-month share = %v, want 2000", got.Items[0].MonthShare)
	}
}

func TestUpcomingPromotionExpiry(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	sup := "t-sup-up-" + uniq()

	// 准备: 5 天后到期一笔
	now := time.Now().UTC().Truncate(24 * time.Hour)
	expiry := now.AddDate(0, 0, 5)
	_, err := pool.Exec(ctx, `
		INSERT INTO promotion_fee (supplier_name, kind, amount, period_start, period_end, source, note)
		VALUES ($1, '堆头', 5000, $2, $3, 'test', 't-test')
	`, sup, now, expiry)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// 清理可能遗留
	defer pool.Exec(ctx, `DELETE FROM promotion_fee WHERE supplier_name = $1`, sup)

	tool := UpcomingPromotionExpiry(pool)
	rawArgs := mustJSON(t, UpcomingPromotionExpiryReq{Supplier: sup, DaysAhead: 7})
	out, err := tool.Call(ctx, rawArgs)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	got := mustParse[UpcomingPromotionExpiryResp](t, out)
	if got.Count != 1 {
		t.Errorf("count = %d, want 1", got.Count)
	}
	if got.Items[0].DaysLeft < 4 || got.Items[0].DaysLeft > 5 {
		t.Errorf("days_left = %d, want 4-5", got.Items[0].DaysLeft)
	}
}

func TestForecastPurchaseAmount_NoData(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	sup := "t-sup-fc-empty-" + uniq()

	tool := ForecastPurchaseAmount(pool)
	rawArgs := mustJSON(t, ForecastPurchaseAmountReq{Supplier: sup, Days: 30})
	out, err := tool.Call(ctx, rawArgs)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	got := mustParse[ForecastPurchaseAmountResp](t, out)
	if got.ForecastAmount != 0 {
		t.Errorf("无数据应 forecast=0, got %v", got.ForecastAmount)
	}
	if got.SampleRows != 0 {
		t.Errorf("sample_rows = %d, want 0", got.SampleRows)
	}
}

func TestSuggestSupplierPayment_Basic(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	sup := "t-sup-pay-" + uniq()

	tool := SuggestSupplierPayment(pool)
	rawArgs := mustJSON(t, SuggestSupplierPaymentReq{
		Supplier:   sup,
		PeriodDays: 30,
	})
	out, err := tool.Call(ctx, rawArgs)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	got := mustParse[SuggestSupplierPaymentResp](t, out)
	if got.Supplier != sup {
		t.Errorf("supplier = %q, want %q", got.Supplier, sup)
	}
	if got.PeriodDays != 30 {
		t.Errorf("period_days = %d, want 30", got.PeriodDays)
	}
	if got.InvestmentWeight < 0.8 || got.InvestmentWeight > 1.5 {
		t.Errorf("inv_weight = %v, want 0.8-1.5", got.InvestmentWeight)
	}
	if got.Amount < 0 {
		t.Errorf("amount = %v, want ≥ 0", got.Amount)
	}
	if got.Action != "dry_run" {
		t.Errorf("action = %q, want dry_run", got.Action)
	}
	if got.Basis == nil {
		t.Error("basis 不应为 nil")
	}
}

func TestSuggestSupplierPayment_WithHighInvestment(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	sup := "t-sup-pay-hi-" + uniq()
	now := time.Now().UTC().Truncate(24 * time.Hour)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	monthEnd := monthStart.AddDate(0, 1, 0)

	// 准备: 大额 promotion_fee 当月 (高投资)
	_, err := pool.Exec(ctx, `
		INSERT INTO promotion_fee (supplier_name, kind, amount, period_start, period_end, source, note)
		VALUES ($1, '堆头', 100000, $2, $3, 'test', 't-test')
	`, sup, monthStart, monthEnd.AddDate(0, 0, -1))
	if err != nil {
		t.Fatalf("insert promotion_fee: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM promotion_fee WHERE supplier_name = $1`, sup)

	// 准备: 近 30 天 parse_session (有 base forecast)
	var sessID string
	err = pool.QueryRow(ctx, `
		INSERT INTO parse_session (id, supplier_name, template_id, template_name, mode, image_path, source, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, 't-tpl', 't-tpl', 'purchase', '/tmp/x.jpg', 'test', $2, $2)
		RETURNING id
	`, sup, now.AddDate(0, 0, -10)).Scan(&sessID)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM parse_session WHERE id = $1`, sessID)
	_, err = pool.Exec(ctx, `
		INSERT INTO parse_row (session_id, seq, raw_barcode, matched_barcode, matched_name, matched_supp, qty, unit_price, status, is_deleted)
		VALUES ($1, 1, 'b1', 'b1', 't-name', $2, 100, 50, 'OK', FALSE)
	`, sessID, sup)
	if err != nil {
		t.Fatalf("insert row: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM parse_row WHERE session_id = $1`, sessID)

	tool := SuggestSupplierPayment(pool)
	rawArgs := mustJSON(t, SuggestSupplierPaymentReq{Supplier: sup, PeriodDays: 30})
	out, _ := tool.Call(ctx, rawArgs)
	got := mustParse[SuggestSupplierPaymentResp](t, out)
	// 高投资 → inv_weight 应接近上限 1.5
	if got.InvestmentWeight < 1.2 {
		t.Errorf("高投资场景 inv_weight = %v, want ≥ 1.2", got.InvestmentWeight)
	}
}
