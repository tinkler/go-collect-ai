package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ============================================================
// remember_supplier_policy
// ============================================================

func TestRememberSupplierPolicy_CreateAndUpdate(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()

	tool := RememberSupplierPolicy(pool)
	supplier := "t-supplier-" + uniq()

	t.Run("create", func(t *testing.T) {
		rawArgs := mustJSON(t, RememberSupplierPolicyReq{
			Supplier: supplier, Key: "is_self_procure", Value: true, Source: "test",
		})
		out, err := tool.Call(ctx, rawArgs)
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		got := mustParse[RememberSupplierPolicyResp](t, out)
		if got.Action != "created" {
			t.Errorf("action = %q, want created", got.Action)
		}
		if got.PreviousVal != nil {
			t.Errorf("previous_value = %v, want nil", got.PreviousVal)
		}
	})

	t.Run("update", func(t *testing.T) {
		rawArgs := mustJSON(t, RememberSupplierPolicyReq{
			Supplier: supplier, Key: "is_self_procure", Value: false, Source: "test",
		})
		out, err := tool.Call(ctx, rawArgs)
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		got := mustParse[RememberSupplierPolicyResp](t, out)
		if got.Action != "updated" {
			t.Errorf("action = %q, want updated", got.Action)
		}
		if got.PreviousVal != true {
			t.Errorf("previous_value = %v, want true", got.PreviousVal)
		}
	})

	t.Run("unchanged_same_value", func(t *testing.T) {
		rawArgs := mustJSON(t, RememberSupplierPolicyReq{
			Supplier: supplier, Key: "is_self_procure", Value: false, Source: "test",
		})
		out, err := tool.Call(ctx, rawArgs)
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		got := mustParse[RememberSupplierPolicyResp](t, out)
		if got.Action != "unchanged" {
			t.Errorf("action = %q, want unchanged", got.Action)
		}
	})

	t.Run("dry_run", func(t *testing.T) {
		rawArgs := mustJSON(t, RememberSupplierPolicyReq{
			Supplier: supplier, Key: "has_duitou", Value: true, DryRun: true,
		})
		out, err := tool.Call(ctx, rawArgs)
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		got := mustParse[RememberSupplierPolicyResp](t, out)
		if got.Action != "dry_run" {
			t.Errorf("action = %q, want dry_run", got.Action)
		}
		// 验证: dry_run 之后真没写入
		rawArgs2 := mustJSON(t, RememberSupplierPolicyReq{Supplier: supplier})
		out2, _ := QuerySupplierPolicy(pool).Call(ctx, rawArgs2)
		qr := mustParse[QuerySupplierPolicyResp](t, out2)
		for _, p := range qr.Policies {
			if p.Key == "has_duitou" {
				t.Errorf("dry_run 也写入了,key=has_duitou 出现")
			}
		}
	})

	t.Run("invalid_key", func(t *testing.T) {
		rawArgs := mustJSON(t, RememberSupplierPolicyReq{
			Supplier: supplier, Key: "hacker_field", Value: "x",
		})
		_, err := tool.Call(ctx, rawArgs)
		if err == nil {
			t.Fatal("expected error for invalid key, got nil")
		}
		if !strings.Contains(err.Error(), "白名单") {
			t.Errorf("err = %v, want 含'白名单'", err)
		}
	})

	t.Run("missing_supplier", func(t *testing.T) {
		rawArgs := mustJSON(t, RememberSupplierPolicyReq{
			Supplier: "", Key: "is_self_procure", Value: true,
		})
		_, err := tool.Call(ctx, rawArgs)
		if err == nil {
			t.Fatal("expected error for empty supplier, got nil")
		}
	})

	t.Run("pool_nil", func(t *testing.T) {
		rawArgs := mustJSON(t, RememberSupplierPolicyReq{
			Supplier: "x", Key: "is_self_procure", Value: true,
		})
		_, err := RememberSupplierPolicy(nil).Call(ctx, rawArgs)
		if err == nil {
			t.Fatal("expected error when pool is nil")
		}
	})
}

// ============================================================
// query_supplier_policy
// ============================================================

func TestQuerySupplierPolicy(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	supplier := "t-supplier-" + uniq()

	// 准备: 写 2 条
	RememberSupplierPolicy(pool).Call(ctx, mustJSON(t, RememberSupplierPolicyReq{
		Supplier: supplier, Key: "is_self_procure", Value: true,
	}))
	RememberSupplierPolicy(pool).Call(ctx, mustJSON(t, RememberSupplierPolicyReq{
		Supplier: supplier, Key: "has_duitou", Value: false,
	}))

	t.Run("all_keys", func(t *testing.T) {
		out, err := QuerySupplierPolicy(pool).Call(ctx, mustJSON(t, QuerySupplierPolicyReq{Supplier: supplier}))
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		got := mustParse[QuerySupplierPolicyResp](t, out)
		if got.Count != 2 {
			t.Errorf("count = %d, want 2", got.Count)
		}
	})

	t.Run("filter_key", func(t *testing.T) {
		out, _ := QuerySupplierPolicy(pool).Call(ctx, mustJSON(t, QuerySupplierPolicyReq{
			Supplier: supplier, Key: "has_duitou",
		}))
		got := mustParse[QuerySupplierPolicyResp](t, out)
		if got.Count != 1 || got.Policies[0].Key != "has_duitou" {
			t.Errorf("got = %+v, want 1 条 has_duitou", got)
		}
		if v, ok := got.Policies[0].Value.(bool); !ok || v {
			t.Errorf("value = %v, want false", got.Policies[0].Value)
		}
	})

	t.Run("not_found_empty", func(t *testing.T) {
		out, _ := QuerySupplierPolicy(pool).Call(ctx, mustJSON(t, QuerySupplierPolicyReq{
			Supplier: "t-not-exist-" + uniq(),
		}))
		got := mustParse[QuerySupplierPolicyResp](t, out)
		if got.Count != 0 {
			t.Errorf("count = %d, want 0", got.Count)
		}
	})
}

// ============================================================
// record_special_date
// ============================================================

func TestRecordSpecialDate(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	date := time.Now().AddDate(0, 1, 0).Format("2006-01-02") // 下个月同一天
	name := "t-midautumn-" + uniq()

	t.Run("create", func(t *testing.T) {
		out, err := RecordSpecialDate(pool).Call(ctx, mustJSON(t, RecordSpecialDateReq{
			Date: date, Type: "holiday", Name: name, LeadDays: 3, Note: "t-test",
		}))
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		got := mustParse[RecordSpecialDateResp](t, out)
		if got.Action != "created" {
			t.Errorf("action = %q, want created", got.Action)
		}
		if got.LeadDays != 3 {
			t.Errorf("lead_days = %d, want 3", got.LeadDays)
		}
	})

	t.Run("update_lead_days", func(t *testing.T) {
		out, _ := RecordSpecialDate(pool).Call(ctx, mustJSON(t, RecordSpecialDateReq{
			Date: date, Type: "holiday", Name: name, LeadDays: 5,
		}))
		got := mustParse[RecordSpecialDateResp](t, out)
		if got.Action != "updated" {
			t.Errorf("action = %q, want updated", got.Action)
		}
		if got.LeadDays != 5 {
			t.Errorf("lead_days = %d, want 5", got.LeadDays)
		}
	})

	t.Run("unchanged", func(t *testing.T) {
		out, _ := RecordSpecialDate(pool).Call(ctx, mustJSON(t, RecordSpecialDateReq{
			Date: date, Type: "holiday", Name: name, LeadDays: 5,
		}))
		got := mustParse[RecordSpecialDateResp](t, out)
		if got.Action != "unchanged" {
			t.Errorf("action = %q, want unchanged", got.Action)
		}
	})

	t.Run("bad_date_format", func(t *testing.T) {
		_, err := RecordSpecialDate(pool).Call(ctx, mustJSON(t, RecordSpecialDateReq{
			Date: "2026/10/01", Type: "holiday", Name: "t-x",
		}))
		if err == nil {
			t.Fatal("expected error for bad date format")
		}
	})

	t.Run("bad_type", func(t *testing.T) {
		_, err := RecordSpecialDate(pool).Call(ctx, mustJSON(t, RecordSpecialDateReq{
			Date: "2026-10-01", Type: "made_up", Name: "t-x",
		}))
		if err == nil || !strings.Contains(err.Error(), "白名单") {
			t.Errorf("err = %v, want 白名单错误", err)
		}
	})

	t.Run("dry_run", func(t *testing.T) {
		out, _ := RecordSpecialDate(pool).Call(ctx, mustJSON(t, RecordSpecialDateReq{
			Date: date, Type: "promo", Name: "t-promo-" + uniq(), DryRun: true,
		}))
		got := mustParse[RecordSpecialDateResp](t, out)
		if got.Action != "dry_run" {
			t.Errorf("action = %q, want dry_run", got.Action)
		}
	})
}

// ============================================================
// query_upcoming_dates
// ============================================================

func TestQueryUpcomingDates(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()

	// 准备: 写 1 个未来 7 天内
	date := time.Now().AddDate(0, 0, 3).Format("2006-01-02")
	name := "t-test-" + uniq()
	RecordSpecialDate(pool).Call(ctx, mustJSON(t, RecordSpecialDateReq{
		Date: date, Type: "holiday", Name: name, LeadDays: 2,
	}))

	t.Run("default_window", func(t *testing.T) {
		out, _ := QueryUpcomingDates(pool).Call(ctx, mustJSON(t, QueryUpcomingDatesReq{
			DaysAhead: 7,
		}))
		got := mustParse[QueryUpcomingDatesResp](t, out)
		// 至少有 1 条(我们刚写的)
		found := false
		for _, it := range got.Items {
			if it.Name == name {
				found = true
				if it.LeadDays != 2 {
					t.Errorf("lead_days = %d, want 2", it.LeadDays)
				}
			}
		}
		if !found {
			t.Errorf("没找到刚写入的 %s in %d items", name, got.Count)
		}
	})

	t.Run("filter_type", func(t *testing.T) {
		out, _ := QueryUpcomingDates(pool).Call(ctx, mustJSON(t, QueryUpcomingDatesReq{
			DaysAhead: 7, Type: "made_up_type",
		}))
		got := mustParse[QueryUpcomingDatesResp](t, out)
		if got.Count != 0 {
			t.Errorf("count = %d, want 0 for 非存在 type", got.Count)
		}
	})

	t.Run("days_ahead_zero", func(t *testing.T) {
		_, err := QueryUpcomingDates(pool).Call(ctx, mustJSON(t, QueryUpcomingDatesReq{
			DaysAhead: 0,
		}))
		if err == nil {
			t.Fatal("expected error for days_ahead=0")
		}
	})
}

// ============================================================
// record_promotion_fee
// ============================================================

func TestRecordPromotionFee(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	supplier := "t-supplier-" + uniq()

	t.Run("insert", func(t *testing.T) {
		out, err := RecordPromotionFee(pool).Call(ctx, mustJSON(t, RecordPromotionFeeReq{
			Supplier: supplier, Kind: "堆头", Amount: 5000,
			PeriodStart: "2026-09-01", PeriodEnd: "2026-12-31", Note: "t-test",
		}))
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		got := mustParse[RecordPromotionFeeResp](t, out)
		if got.Action != "inserted" {
			t.Errorf("action = %q, want inserted", got.Action)
		}
		if got.FeeID == 0 {
			t.Error("fee_id = 0, want auto-incremented")
		}
		if got.Amount != 5000 {
			t.Errorf("amount = %v, want 5000", got.Amount)
		}
	})

	t.Run("bad_kind", func(t *testing.T) {
		_, err := RecordPromotionFee(pool).Call(ctx, mustJSON(t, RecordPromotionFeeReq{
			Supplier: supplier, Kind: "未知种类", Amount: 100,
			PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30",
		}))
		if err == nil {
			t.Fatal("expected error for bad kind")
		}
	})

	t.Run("amount_zero", func(t *testing.T) {
		_, err := RecordPromotionFee(pool).Call(ctx, mustJSON(t, RecordPromotionFeeReq{
			Supplier: supplier, Kind: "堆头", Amount: 0,
			PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30",
		}))
		if err == nil {
			t.Fatal("expected error for amount=0")
		}
	})

	t.Run("period_end_before_start", func(t *testing.T) {
		_, err := RecordPromotionFee(pool).Call(ctx, mustJSON(t, RecordPromotionFeeReq{
			Supplier: supplier, Kind: "堆头", Amount: 100,
			PeriodStart: "2026-09-30", PeriodEnd: "2026-09-01",
		}))
		if err == nil {
			t.Fatal("expected error for period_end < period_start")
		}
	})

	t.Run("dry_run", func(t *testing.T) {
		out, _ := RecordPromotionFee(pool).Call(ctx, mustJSON(t, RecordPromotionFeeReq{
			Supplier: supplier, Kind: "端架", Amount: 200, DryRun: true,
			PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30",
		}))
		got := mustParse[RecordPromotionFeeResp](t, out)
		if got.Action != "dry_run" {
			t.Errorf("action = %q, want dry_run", got.Action)
		}
	})
}

// ============================================================
// list_promotion_fee
// ============================================================

func TestListPromotionFee(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	supplier := "t-supplier-" + uniq()

	// 准备 2 条
	RecordPromotionFee(pool).Call(ctx, mustJSON(t, RecordPromotionFeeReq{
		Supplier: supplier, Kind: "堆头", Amount: 1000,
		PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30",
	}))
	RecordPromotionFee(pool).Call(ctx, mustJSON(t, RecordPromotionFeeReq{
		Supplier: supplier, Kind: "端架", Amount: 2000,
		PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30",
	}))

	t.Run("filter_supplier", func(t *testing.T) {
		out, _ := ListPromotionFee(pool).Call(ctx, mustJSON(t, ListPromotionFeeReq{
			Supplier: supplier,
		}))
		got := mustParse[ListPromotionFeeResp](t, out)
		if got.Count != 2 {
			t.Errorf("count = %d, want 2", got.Count)
		}
		if got.Total != 3000 {
			t.Errorf("total = %v, want 3000", got.Total)
		}
	})

	t.Run("filter_kind", func(t *testing.T) {
		out, _ := ListPromotionFee(pool).Call(ctx, mustJSON(t, ListPromotionFeeReq{
			Supplier: supplier, Kind: "堆头",
		}))
		got := mustParse[ListPromotionFeeResp](t, out)
		if got.Count != 1 {
			t.Errorf("count = %d, want 1", got.Count)
		}
		if got.Items[0].Kind != "堆头" {
			t.Errorf("kind = %q, want 堆头", got.Items[0].Kind)
		}
	})

	t.Run("limit_clamp", func(t *testing.T) {
		out, _ := ListPromotionFee(pool).Call(ctx, mustJSON(t, ListPromotionFeeReq{
			Supplier: supplier, Limit: 10000, // > 500 应被夹到 500
		}))
		_ = mustParse[ListPromotionFeeResp](t, out)
		// 这里只验证不崩,limit 实际不影响小数据集
	})

	t.Run("period_filter", func(t *testing.T) {
		// 周期: 10-01 ~ 10-31,刚写的 09-01~09-30 不在
		out, _ := ListPromotionFee(pool).Call(ctx, mustJSON(t, ListPromotionFeeReq{
			Supplier: supplier, PeriodStart: "2026-10-01", PeriodEnd: "2026-10-31",
		}))
		got := mustParse[ListPromotionFeeResp](t, out)
		if got.Count != 0 {
			t.Errorf("count = %d, want 0 (out of period)", got.Count)
		}
	})
}

// ============================================================
// helpers
// ============================================================

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func mustParse[T any](t *testing.T, raw any) T {
	t.Helper()
	// trpc-agent-go 的 tool.Call 返回的是已反序列化的 Go 值(value 或 pointer)
	// 通过 JSON 双向 round-trip 适配到 T,屏蔽类型差异
	bs, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal round-trip: %v (raw: %+v)", err, raw)
	}
	var out T
	if err := json.Unmarshal(bs, &out); err != nil {
		t.Fatalf("unmarshal to %T: %v (raw: %s)", *new(T), err, string(bs))
	}
	return out
}

var uniqCounter int64

func uniq() string {
	uniqCounter++
	return time.Now().Format("150405.000000") + "-" + itoa(uniqCounter)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
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
