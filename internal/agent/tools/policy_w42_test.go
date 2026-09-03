package tools

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ============================================================
// W4.2 decision-memory: delete_supplier_policy 单测
// ============================================================

// insertPolicy helper: 直接插一条 (test-only, 不走 tool)
func insertPolicy(t *testing.T, ctx context.Context, pool *pgxpool.Pool, supplier, key string, value any) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO supplier_policy (supplier_name, key, value, source) VALUES ($1, $2, $3::jsonb, 'test')`,
		supplier, key, value)
	if err != nil {
		t.Fatalf("insert policy: %v", err)
	}
}

func TestDeleteSupplierPolicy_SingleKey(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	uniq := uniqueSuffix()
	sup := "t-del-1-" + uniq
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM supplier_policy WHERE supplier_name = $1`, sup) })

	// 准备: 3 条 policy
	insertPolicy(t, ctx, pool, sup, "is_self_procure", true)
	insertPolicy(t, ctx, pool, sup, "has_duitou", true)
	insertPolicy(t, ctx, pool, sup, "allow_return", false)

	tool := DeleteSupplierPolicy(pool)
	respAny, err := tool.Call(ctx, marshalForTest(DeleteSupplierPolicyReq{
		Supplier: sup,
		Key:      "has_duitou",
		DryRun:   false,
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp, _ := respAny.(DeleteSupplierPolicyResp)
	if resp.Action != "deleted" {
		t.Errorf("action = %q, want deleted", resp.Action)
	}
	if resp.DeletedCount != 1 {
		t.Errorf("deleted_count = %d, want 1", resp.DeletedCount)
	}
	if len(resp.DeletedKeys) != 1 || resp.DeletedKeys[0] != "has_duitou" {
		t.Errorf("deleted_keys = %v, want [has_duitou]", resp.DeletedKeys)
	}

	// 验: 剩 2 条
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM supplier_policy WHERE supplier_name = $1`, sup).Scan(&n); err != nil {
		t.Fatalf("re-query: %v", err)
	}
	if n != 2 {
		t.Errorf("after delete, count = %d, want 2", n)
	}
}

func TestDeleteSupplierPolicy_FullRevoke(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	uniq := uniqueSuffix()
	sup := "t-del-2-" + uniq
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM supplier_policy WHERE supplier_name = $1`, sup) })

	insertPolicy(t, ctx, pool, sup, "is_self_procure", true)
	insertPolicy(t, ctx, pool, sup, "has_duitou", true)

	tool := DeleteSupplierPolicy(pool)
	respAny, err := tool.Call(ctx, marshalForTest(DeleteSupplierPolicyReq{
		Supplier: sup,
		DryRun:   false,
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp, _ := respAny.(DeleteSupplierPolicyResp)
	if resp.DeletedCount != 2 {
		t.Errorf("deleted_count = %d, want 2", resp.DeletedCount)
	}

	// 验: 0 条
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM supplier_policy WHERE supplier_name = $1`, sup).Scan(&n); err != nil {
		t.Fatalf("re-query: %v", err)
	}
	if n != 0 {
		t.Errorf("after full revoke, count = %d, want 0", n)
	}
}

func TestDeleteSupplierPolicy_NotFound(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	uniq := uniqueSuffix()
	sup := "t-del-3-" + uniq

	tool := DeleteSupplierPolicy(pool)
	respAny, err := tool.Call(ctx, marshalForTest(DeleteSupplierPolicyReq{
		Supplier: sup,
		Key:      "block_entry",
		DryRun:   false,
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp, _ := respAny.(DeleteSupplierPolicyResp)
	if resp.Action != "not_found" {
		t.Errorf("action = %q, want not_found", resp.Action)
	}
	if resp.DeletedCount != 0 {
		t.Errorf("deleted_count = %d, want 0", resp.DeletedCount)
	}
}

func TestDeleteSupplierPolicy_DryRun(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	uniq := uniqueSuffix()
	sup := "t-del-4-" + uniq
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM supplier_policy WHERE supplier_name = $1`, sup) })

	insertPolicy(t, ctx, pool, sup, "is_self_procure", true)

	tool := DeleteSupplierPolicy(pool)
	respAny, err := tool.Call(ctx, marshalForTest(DeleteSupplierPolicyReq{
		Supplier: sup,
		Key:      "is_self_procure",
		DryRun:   true,
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp, _ := respAny.(DeleteSupplierPolicyResp)
	if resp.Action != "dry_run" {
		t.Errorf("action = %q, want dry_run", resp.Action)
	}
	if resp.DeletedCount != 1 {
		t.Errorf("deleted_count = %d, want 1", resp.DeletedCount)
	}

	// 验: 仍存在 (dry_run 不动 DB)
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM supplier_policy WHERE supplier_name = $1`, sup).Scan(&n); err != nil {
		t.Fatalf("re-query: %v", err)
	}
	if n != 1 {
		t.Errorf("dry_run should not delete, count = %d, want 1", n)
	}
}

func TestDeleteSupplierPolicy_EmptySupplier(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	tool := DeleteSupplierPolicy(pool)
	_, err := tool.Call(context.Background(), marshalForTest(DeleteSupplierPolicyReq{
		Supplier: "",
	}))
	if err == nil {
		t.Error("empty supplier should fail")
	}
}

// ============================================================
// W4.2: list_supplier_keys 单测
// ============================================================

func TestListSupplierKeys(t *testing.T) {
	tool := ListSupplierKeys(nil) // list_supplier_keys 不需 DB
	respAny, err := tool.Call(context.Background(), marshalForTest(ListSupplierKeysReq{}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp, _ := respAny.(ListSupplierKeysResp)
	if len(resp.Keys) != 7 {
		t.Errorf("keys count = %d, want 7", len(resp.Keys))
	}

	// 验: 必须有这 7 个 key
	want := map[string]bool{
		"is_self_procure": false,
		"allow_return":    false,
		"has_duitou":      false,
		"has_duanjia":     false,
		"block_entry":     false,
		"block_reason":    false,
		"note":            false,
	}
	for _, k := range resp.Keys {
		if _, ok := want[k.Key]; ok {
			want[k.Key] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("missing key: %s", k)
		}
	}

	// 验: block_entry description 含 "硬阻断" / "永不降级"
	for _, k := range resp.Keys {
		if k.Key == "block_entry" {
			if k.ValueType != "bool" {
				t.Errorf("block_entry value_type = %q, want bool", k.ValueType)
			}
		}
	}
}

// ============================================================
// integration: 撤销后下游 effect 验证 (用 AlertSvc 跑 Go rules)
//   撤销 block_entry → 不再触发 BlockEntryRule
// ============================================================

func TestRevokeBlockEntry_DownstreamEffect(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	uniq := uniqueSuffix()
	sup := "t-revoke-1-" + uniq
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM supplier_policy WHERE supplier_name = $1`, sup) })

	// 1) 准备: block_entry=true
	insertPolicy(t, ctx, pool, sup, "block_entry", true)

	// 2) 跑 Apply → 应该报 block
	insertSession(t, pool, "sess-block-1-"+uniq, sup)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM parse_session WHERE supplier_name = $1`, sup)
		_, _ = pool.Exec(ctx, `DELETE FROM purchase_session_alert WHERE session_id = 'sess-block-1-`+uniq+`'`)
	})

	// 3) 撤销 block_entry
	tool := DeleteSupplierPolicy(pool)
	_, err := tool.Call(ctx, marshalForTest(DeleteSupplierPolicyReq{
		Supplier: sup,
		Key:      "block_entry",
		DryRun:   false,
	}))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	// 4) 验: DB 中 block_entry 已删
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM supplier_policy WHERE supplier_name=$1 AND key='block_entry')`, sup).Scan(&exists); err != nil {
		t.Fatalf("check: %v", err)
	}
	if exists {
		t.Error("block_entry should be deleted")
	}
}

// ============================================================
// helpers
// ============================================================

func uniqueSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func intToStr(n int64) string {
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

// insertSession helper: 插一条 parse_session 供 Apply 测试用
func insertSession(t *testing.T, pool *pgxpool.Pool, sessID, supplier string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO parse_session (id, supplier_name, mode, image_path, image_url, source, analysis_status)
		VALUES ($1, $2, 'purchase', '/test.jpg', '/test.jpg', 'test', 'pending')
		ON CONFLICT (id) DO NOTHING
	`, sessID, supplier)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
}
