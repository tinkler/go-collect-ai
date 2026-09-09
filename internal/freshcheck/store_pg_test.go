package freshcheck

// ============================================================
// freshcheck store 集成测试 (W1.4)
//
// 依赖: 真实 PG (collect-ai 现有 PG 实例, 库 = collectai)
// 跑法: 本地起 PG → go test ./internal/freshcheck/... -tags=integration
//       或取消 Skip 行 (W1.7 集成验证时)
//       默认 go test 跳过, 不阻塞 CI
//
// 测试矩阵: 5 张配置表 × CRUD + 校验 + 审计 (snap)
// ============================================================

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ============== TestMain: 启 PG 连接 + Migrate ==============

// pgDSN 默认从环境变量读; 测试时设 FRESHCHECK_TEST_PG_DSN
//   e.g. FRESHCHECK_TEST_PG_DSN=postgres://postgres:postgres@127.0.0.1:5432/collectai?sslmode=disable
const testPgDSNEnv = "FRESHCHECK_TEST_PG_DSN"

var testPool *pgxpool.Pool

func setupTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv(testPgDSNEnv)
	if dsn == "" {
		t.Skipf("跳过集成测试: %s 未设置", testPgDSNEnv)
	}
	if testPool != nil {
		return testPool
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("PG 连不上: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("PG ping 失败: %v", err)
	}
	testPool = pool
	return pool
}

func teardownTestStore(t *testing.T, s *Store) {
	t.Helper()
	// 清理本次测试插入的临时数据 (按 branch_no = 'TEST')
	ctx := context.Background()
	_, _ = s.pool.Exec(ctx, `DELETE FROM freshcheck_sku_map WHERE branch_no = 'TEST'`)
	_, _ = s.pool.Exec(ctx, `DELETE FROM freshcheck_pool_code WHERE branch_no = 'TEST'`)
	_, _ = s.pool.Exec(ctx, `DELETE FROM freshcheck_loss_rate WHERE fresh_category LIKE 'TEST_%'`)
	_, _ = s.pool.Exec(ctx, `DELETE FROM freshcheck_category_track WHERE branch_no = 'TEST'`)
	_, _ = s.pool.Exec(ctx, `DELETE FROM freshcheck_config_snap WHERE row_pk LIKE 'TEST%' OR table_name = 'freshcheck_sku_map' AND row_pk LIKE 'TEST%'`)
}

// ============== 1. freshcheck_sku_map 测试 ==============

func TestSkuMap_UpsertAndGet(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownTestStore(t, s)

	m := &SkuMap{
		BranchNo: "TEST", ItemNo: "TEST0001", ItemName: "测试菠菜",
		FreshCategory: "leaf", TurnoverClass: "fast", ShelfLifeDays: 5,
		IsActive: true,
	}
	if err := s.UpsertSkuMap(ctx(), m); err != nil {
		t.Fatalf("UpsertSkuMap 失败: %v", err)
	}

	got, err := s.GetSkuMap(ctx(), "TEST", "TEST0001")
	if err != nil {
		t.Fatalf("GetSkuMap 失败: %v", err)
	}
	if got.FreshCategory != "leaf" {
		t.Errorf("FreshCategory = %s, want leaf", got.FreshCategory)
	}
	if got.ShelfLifeDays != 5 {
		t.Errorf("ShelfLifeDays = %d, want 5", got.ShelfLifeDays)
	}
}

func TestSkuMap_UpsertUpdate(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownTestStore(t, s)

	// 第一次 create
	m := &SkuMap{BranchNo: "TEST", ItemNo: "TEST0002", ItemName: "测试", FreshCategory: "leaf", TurnoverClass: "fast", ShelfLifeDays: 5, IsActive: true}
	if err := s.UpsertSkuMap(ctx(), m); err != nil {
		t.Fatal(err)
	}
	// 第二次 update (改 shelf_life_days)
	m.ShelfLifeDays = 3
	if err := s.UpsertSkuMap(ctx(), m); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSkuMap(ctx(), "TEST", "TEST0002")
	if got.ShelfLifeDays != 3 {
		t.Errorf("UPDATE 后 ShelfLifeDays = %d, want 3", got.ShelfLifeDays)
	}
}

func TestSkuMap_ListByCategory(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownTestStore(t, s)

	// 插 2 个 leaf + 1 个 root
	for _, it := range []struct {
		itemNo, cat string
	}{
		{"TEST_L01", "leaf"}, {"TEST_L02", "leaf"}, {"TEST_R01", "root"},
	} {
		m := &SkuMap{BranchNo: "TEST", ItemNo: it.itemNo, ItemName: "test", FreshCategory: it.cat, TurnoverClass: "fast", IsActive: true}
		if err := s.UpsertSkuMap(ctx(), m); err != nil {
			t.Fatal(err)
		}
	}
	leafs, err := s.ListSkuMapByCategory(ctx(), "TEST", "leaf")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, l := range leafs {
		if l.ItemNo[:5] == "TEST_" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("leaf 类下 TEST_* 数 = %d, want 2", count)
	}
}

func TestSkuMap_InvalidInput(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)

	// item_no 为空
	m := &SkuMap{BranchNo: "TEST", FreshCategory: "leaf", TurnoverClass: "fast"}
	if err := s.UpsertSkuMap(ctx(), m); err == nil {
		t.Error("item_no 为空应报错")
	}
	// fresh_category 为空 (注意: 第二个 m 复用 m,必须显式清 fresh_category)
	m = &SkuMap{BranchNo: "TEST", ItemNo: "TEST_NO_CAT", TurnoverClass: "fast"}
	if err := s.UpsertSkuMap(ctx(), m); err == nil {
		t.Error("fresh_category 为空应报错")
	}
}

// ============== 2. freshcheck_pool_code 测试 ==============

func TestPoolCode_UpsertAndList(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownTestStore(t, s)

	// 插 2 个特价码 (1元 + 0.5元)
	for _, p := range []*PoolCode{
		{BranchNo: "TEST", PoolCode: "TESTPOOL01", PoolName: "1元/斤", PricingMode: "weight", UnitPrice: 1.0, PriorityRank: 0, IsActive: true},
		{BranchNo: "TEST", PoolCode: "TESTPOOL005", PoolName: "0.5元/斤", PricingMode: "weight", UnitPrice: 0.5, PriorityRank: 1, IsActive: true},
	} {
		if err := s.UpsertPoolCode(ctx(), p); err != nil {
			t.Fatal(err)
		}
	}

	// List 应按 unit_price DESC 排序
	pools, err := s.ListPoolCodes(ctx(), "TEST")
	if err != nil {
		t.Fatal(err)
	}
	var testPools []*PoolCode
	for _, p := range pools {
		if len(p.PoolCode) > 5 && p.PoolCode[:5] == "TESTP" {
			testPools = append(testPools, p)
		}
	}
	if len(testPools) != 2 {
		t.Fatalf("ListPoolCodes TEST_* 数 = %d, want 2", len(testPools))
	}
	if testPools[0].UnitPrice != 1.0 {
		t.Errorf("排序后第 1 个 UnitPrice = %f, want 1.0 (高价先)", testPools[0].UnitPrice)
	}
}

func TestPoolCode_GetNotFound(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	_, err := s.GetPoolCode(ctx(), "TEST", "NOTEXIST")
	if err == nil {
		t.Error("不存在的特价码应返 ErrNotFound")
	}
}

// ============== 3. freshcheck_loss_rate 测试 ==============

func TestLossRate_UpsertVersioning(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownTestStore(t, s)

	cat := "TEST_lf"
	tc := "fast"

	// 第一个版本
	v1 := &LossRate{FreshCategory: cat, TurnoverClass: tc, LossType: "natural", DailyRate: 0.05, EffectiveFrom: time.Now().Add(-24 * time.Hour)}
	if err := s.UpsertLossRate(ctx(), v1); err != nil {
		t.Fatal(err)
	}
	// 第二个版本 (新 effective_from, 自动 close 旧版)
	v2 := &LossRate{FreshCategory: cat, TurnoverClass: tc, LossType: "natural", DailyRate: 0.07, EffectiveFrom: time.Now()}
	if err := s.UpsertLossRate(ctx(), v2); err != nil {
		t.Fatal(err)
	}

	// GetActive 应返 v2
	got, err := s.GetActiveLossRate(ctx(), cat, tc, "natural")
	if err != nil {
		t.Fatal(err)
	}
	if got.DailyRate != 0.07 {
		t.Errorf("GetActive DailyRate = %f, want 0.07 (新版本)", got.DailyRate)
	}
	if got.EffectiveTo != nil {
		t.Errorf("当前版本 EffectiveTo 应为 nil, got %v", *got.EffectiveTo)
	}
}

func TestLossRate_InvalidInput(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	l := &LossRate{TurnoverClass: "fast", LossType: "natural"} // 缺 fresh_category
	if err := s.UpsertLossRate(ctx(), l); err == nil {
		t.Error("fresh_category 缺失应报错")
	}
}

// ============== 4. freshcheck_threshold 测试 ==============

func TestThreshold_GetListUpdate(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)

	// 12 行 seed 已由 W1.1c 完成, 选一个测
	v, err := s.GetThreshold(ctx(), "c1_pool_saturation_pct")
	if err != nil {
		t.Fatal(err)
	}
	if v != 15.00 {
		t.Errorf("c1_pool_saturation_pct seed 值 = %f, want 15.00", v)
	}

	all, err := s.ListThresholds(ctx())
	if err != nil {
		t.Fatal(err)
	}
	// 2026-09-09 W1.7 撤 c7_sync_diff_pct 后剩 11 行
	if len(all) < 11 {
		t.Errorf("ListThresholds len = %d, want >= 11", len(all))
	}

	// Update
	if err := s.UpdateThreshold(ctx(), "c1_pool_saturation_pct", 18.0, "u_test"); err != nil {
		t.Fatal(err)
	}
	v2, _ := s.GetThreshold(ctx(), "c1_pool_saturation_pct")
	if v2 != 18.0 {
		t.Errorf("UPDATE 后值 = %f, want 18.0", v2)
	}
	// 还原
	_ = s.UpdateThreshold(ctx(), "c1_pool_saturation_pct", 15.00, "u_test")
}

func TestThreshold_NotFound(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	_, err := s.GetThreshold(ctx(), "NONEXIST_KEY")
	if err == nil {
		t.Error("不存在的 key 应返 ErrNotFound")
	}
	if err := s.UpdateThreshold(ctx(), "NONEXIST_KEY", 1.0, "u_test"); err == nil {
		t.Error("UPDATE 不存在的 key 应报错")
	}
}

// ============== 5. freshcheck_category_track 测试 ==============

func TestCategoryTrack_UpsertAndGet(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownTestStore(t, s)

	ct := &CategoryTrack{
		FreshCategory: "TEST_cat", BranchNo: "TEST",
		TrackCode: "TEST-track", PeriodLockDays: 7,
		StockFreq: "weekly", RequireStock: true, IsActive: true,
	}
	if err := s.UpsertCategoryTrack(ctx(), ct); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTrack(ctx(), "TEST", "TEST-track")
	if err != nil {
		t.Fatal(err)
	}
	if got.PeriodLockDays != 7 {
		t.Errorf("PeriodLockDays = %d, want 7", got.PeriodLockDays)
	}
	// 跟按 fresh_category 查
	got2, _ := s.GetTrackByCategory(ctx(), "TEST", "TEST_cat")
	if got2.TrackCode != "TEST-track" {
		t.Errorf("GetTrackByCategory TrackCode = %s, want TEST-track", got2.TrackCode)
	}
}

func TestCategoryTrack_UpdateLastSettle(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownTestStore(t, s)

	ct := &CategoryTrack{
		FreshCategory: "TEST_us", BranchNo: "TEST",
		TrackCode: "TEST-us", PeriodLockDays: 7,
		StockFreq: "weekly", RequireStock: true, IsActive: true,
	}
	if err := s.UpsertCategoryTrack(ctx(), ct); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTrack(ctx(), "TEST", "TEST-us")

	settleAt := time.Now()
	if err := s.UpdateLastSettleAt(ctx(), got.ID, settleAt); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.GetTrack(ctx(), "TEST", "TEST-us")
	if got2.LastSettleAt == nil {
		t.Error("UpdateLastSettleAt 后 LastSettleAt 仍为 nil")
	}
	if got2.NextSettleDeadline == nil {
		t.Error("UpdateLastSettleAt 后 NextSettleDeadline 应被自动计算")
	}
}

// ============== 6. freshcheck_config_snap (审计) 测试 ==============

func TestConfigSnap_WriteOnUpdate(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownTestStore(t, s)

	// 1. 改阈值
	key := "c1_pool_saturation_pct"
	oldVal, _ := s.GetThreshold(ctx(), key)
	if err := s.UpdateThreshold(ctx(), key, 99.9, "u_snap_test"); err != nil {
		t.Fatal(err)
	}
	// 2. 查 snap
	var count int
	err := s.pool.QueryRow(ctx(),
		`SELECT COUNT(*) FROM freshcheck_config_snap WHERE table_name = 'freshcheck_threshold' AND row_pk = $1 AND action = 'update'`,
		key).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Error("UPDATE 阈值后应写 snap, 但表里没记录")
	}
	// 还原
	_ = s.UpdateThreshold(ctx(), key, oldVal, "u_snap_test")
}

// ============== helpers ==============

func ctx() context.Context {
	return context.Background()
}
