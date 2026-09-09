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
	"errors"
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
	_, _ = s.pool.Exec(ctx, `DELETE FROM freshcheck_period_stock WHERE branch_no = 'TEST'`)
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

// ============== 7. freshcheck_pool_event (W2.1) ==============

// helper: 准备一个特价码 + SKU 映射 (RecordPoolEventIn 需要校验)
func setupPoolAndSku(t *testing.T, s *Store) {
	t.Helper()
	pc := &PoolCode{
		BranchNo: "TEST", PoolCode: "TESTPOOL", PoolName: "1元/斤",
		PricingMode: "weight", UnitPrice: 1.0, PriorityRank: 0, IsActive: true,
	}
	if err := s.UpsertPoolCode(ctx(), pc); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	sm := &SkuMap{
		BranchNo: "TEST", ItemNo: "TESTSKU1", ItemName: "test spinach",
		FreshCategory: "leaf", TurnoverClass: "fast", ShelfLifeDays: 5, IsActive: true,
	}
	if err := s.UpsertSkuMap(ctx(), sm); err != nil {
		t.Fatalf("seed sku: %v", err)
	}
}

func teardownPoolEvents(t *testing.T, s *Store) {
	t.Helper()
	_, _ = s.pool.Exec(ctx(), `DELETE FROM freshcheck_pool_event WHERE branch_no = 'TEST'`)
}

func TestPoolEvent_RecordIn_OK(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownPoolEvents(t, s)
	setupPoolAndSku(t, s)

	eventTime := time.Now().Add(-1 * time.Hour)
	ev, err := s.RecordPoolEventIn(ctx(), "TEST", "TESTPOOL", "TESTSKU1",
		0.5, 0, "u_floor", eventTime, "high", "")
	if err != nil {
		t.Fatalf("RecordPoolEventIn: %v", err)
	}
	if ev.ID == 0 {
		t.Error("ID should be set")
	}
	if ev.EventKind != PoolEventIn {
		t.Errorf("EventKind = %s, want in", ev.EventKind)
	}
	if ev.WeightKg != 0.5 {
		t.Errorf("WeightKg = %f, want 0.5", ev.WeightKg)
	}
	if ev.Confidence != "high" {
		t.Errorf("Confidence = %s, want high", ev.Confidence)
	}
}

func TestPoolEvent_RecordIn_InvalidPool(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownPoolEvents(t, s)
	setupPoolAndSku(t, s)

	// POOL 不存在
	_, err := s.RecordPoolEventIn(ctx(), "TEST", "NONEXIST", "TESTSKU1",
		0.5, 0, "u_floor", time.Now(), "high", "")
	if err == nil {
		t.Error("不存在的 pool_code 应返 ErrInvalidInput")
	}
}

func TestPoolEvent_RecordIn_InvalidSku(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownPoolEvents(t, s)
	setupPoolAndSku(t, s)

	// SKU 不存在
	_, err := s.RecordPoolEventIn(ctx(), "TEST", "TESTPOOL", "NONEXIST_SKU",
		0.5, 0, "u_floor", time.Now(), "high", "")
	if err == nil {
		t.Error("不存在的 sku 应返 ErrInvalidInput")
	}
}

func TestPoolEvent_RecordIn_ConfidenceLow(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownPoolEvents(t, s)
	setupPoolAndSku(t, s)

	ev, err := s.RecordPoolEventIn(ctx(), "TEST", "TESTPOOL", "TESTSKU1",
		0.5, 0, "u_floor", time.Now(), "low", "补录")
	if err != nil {
		t.Fatalf("RecordPoolEventIn: %v", err)
	}
	if ev.Confidence != "low" {
		t.Errorf("Confidence = %s, want low", ev.Confidence)
	}
	if ev.Note != "补录" {
		t.Errorf("Note = %s, want 补录", ev.Note)
	}
}

func TestPoolEvent_RecordIn_DuplicateUnique(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownPoolEvents(t, s)
	setupPoolAndSku(t, s)

	eventTime := time.Now().Add(-2 * time.Hour)
	_, err := s.RecordPoolEventIn(ctx(), "TEST", "TESTPOOL", "TESTSKU1",
		0.5, 0, "u_floor", eventTime, "high", "")
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// 同 (branch, pool, item, kind, time) 重复 → ErrDuplicateKey
	_, err = s.RecordPoolEventIn(ctx(), "TEST", "TESTPOOL", "TESTSKU1",
		0.3, 0, "u_floor", eventTime, "high", "")
	if err != ErrDuplicateKey {
		t.Errorf("重复应返 ErrDuplicateKey, got %v", err)
	}
}

func TestPoolEvent_RecordOut_AllDestinations(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownPoolEvents(t, s)
	setupPoolAndSku(t, s)

	// 准备 4 个 SKU (对应 4 种去向)
	for i, it := range []string{"TESTSKU1", "TESTSKU2", "TESTSKU3", "TESTSKU4"} {
		sm := &SkuMap{
			BranchNo: "TEST", ItemNo: it, ItemName: "test",
			FreshCategory: "leaf", TurnoverClass: "fast", ShelfLifeDays: 5, IsActive: true,
		}
		_ = s.UpsertSkuMap(ctx(), sm)
		_ = i // dummy
	}
	// 准备降级目标特价码
	pc2 := &PoolCode{
		BranchNo: "TEST", PoolCode: "TESTPOOL_LOW", PoolName: "0.5元/斤",
		PricingMode: "weight", UnitPrice: 0.5, PriorityRank: 1, IsActive: true,
	}
	_ = s.UpsertPoolCode(ctx(), pc2)

	// 录 4 条 in (避免漏录)
	for _, it := range []string{"TESTSKU1", "TESTSKU2", "TESTSKU3", "TESTSKU4"} {
		_, _ = s.RecordPoolEventIn(ctx(), "TEST", "TESTPOOL", it,
			0.5, 0, "u_floor", time.Now().Add(-12*time.Hour), "high", "")
	}

	// 出框
	items := []PoolOutItem{
		{ItemNo: "TESTSKU1", WeightKg: 0, OutDestination: DestSoldOut},
		{ItemNo: "TESTSKU2", WeightKg: 0, OutDestination: DestSpoiled, SpoiledWeightKg: 0.3},
		{ItemNo: "TESTSKU3", WeightKg: 0.2, OutDestination: DestReturnShelf},
		{ItemNo: "TESTSKU4", WeightKg: 0.4, OutDestination: DestDowngrade, DowngradeTo: "TESTPOOL_LOW"},
	}
	created, missing, err := s.RecordPoolEventOut(ctx(), "TEST", "TESTPOOL",
		items, "u_floor", time.Now())
	if err != nil {
		t.Fatalf("RecordPoolEventOut: %v", err)
	}
	if created != 4 {
		t.Errorf("created = %d, want 4", created)
	}
	if len(missing) != 0 {
		t.Errorf("不应该有 missing, got %d", len(missing))
	}
}

func TestPoolEvent_RecordOut_DetectMissing(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownPoolEvents(t, s)
	setupPoolAndSku(t, s)

	// 直接录 out, 不录 in → 应触发 missingIn
	items := []PoolOutItem{
		{ItemNo: "TESTSKU1", WeightKg: 0.5, OutDestination: DestSoldOut},
	}
	created, missing, err := s.RecordPoolEventOut(ctx(), "TEST", "TESTPOOL",
		items, "u_floor", time.Now())
	if err != nil {
		t.Fatalf("RecordPoolEventOut: %v", err)
	}
	if created != 1 {
		t.Errorf("created = %d, want 1", created)
	}
	if len(missing) != 1 {
		t.Fatalf("missing = %d, want 1", len(missing))
	}
	if missing[0].ItemNo != "TESTSKU1" {
		t.Errorf("missing item = %s, want TESTSKU1", missing[0].ItemNo)
	}
	if missing[0].SuggestedInWeight != 0.5 {
		t.Errorf("suggested weight = %f, want 0.5", missing[0].SuggestedInWeight)
	}
}

func TestPoolEvent_RecordOut_InvalidDestination(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownPoolEvents(t, s)
	setupPoolAndSku(t, s)
	_, _ = s.RecordPoolEventIn(ctx(), "TEST", "TESTPOOL", "TESTSKU1",
		0.5, 0, "u_floor", time.Now().Add(-1*time.Hour), "high", "")

	items := []PoolOutItem{
		{ItemNo: "TESTSKU1", WeightKg: 0, OutDestination: "invalid_dest"},
	}
	_, _, err := s.RecordPoolEventOut(ctx(), "TEST", "TESTPOOL",
		items, "u_floor", time.Now())
	if err == nil {
		t.Error("无效 out_destination 应返 ErrInvalidInput")
	}
}

func TestPoolEvent_RecordOut_DowngradeRequiresTarget(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownPoolEvents(t, s)
	setupPoolAndSku(t, s)
	_, _ = s.RecordPoolEventIn(ctx(), "TEST", "TESTPOOL", "TESTSKU1",
		0.5, 0, "u_floor", time.Now().Add(-1*time.Hour), "high", "")

	items := []PoolOutItem{
		{ItemNo: "TESTSKU1", WeightKg: 0.5, OutDestination: DestDowngrade}, // 缺 downgrade_to
	}
	_, _, err := s.RecordPoolEventOut(ctx(), "TEST", "TESTPOOL",
		items, "u_floor", time.Now())
	if err == nil {
		t.Error("降级转框缺 downgrade_to 应返 ErrInvalidInput")
	}
}

func TestPoolEvent_ListByPool(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownPoolEvents(t, s)
	setupPoolAndSku(t, s)

	// 录 2 in 1 out
	now := time.Now()
	_, _ = s.RecordPoolEventIn(ctx(), "TEST", "TESTPOOL", "TESTSKU1",
		0.5, 0, "u_floor", now.Add(-2*time.Hour), "high", "")
	_, _ = s.RecordPoolEventIn(ctx(), "TEST", "TESTPOOL", "TESTSKU1",
		0.3, 0, "u_floor", now.Add(-1*time.Hour), "high", "")

	events, err := s.ListPoolEventsByPool(ctx(), "TEST", "TESTPOOL",
		now.Add(-3*time.Hour), now.Add(1*time.Hour))
	if err != nil {
		t.Fatalf("ListPoolEventsByPool: %v", err)
	}
	if len(events) != 2 {
		t.Errorf("events = %d, want 2", len(events))
	}
	// 按时间序
	if events[0].WeightKg != 0.5 {
		t.Errorf("events[0].WeightKg = %f, want 0.5", events[0].WeightKg)
	}
	if events[1].WeightKg != 0.3 {
		t.Errorf("events[1].WeightKg = %f, want 0.3", events[1].WeightKg)
	}
}

func TestPoolEvent_GetLastEventForItem(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownPoolEvents(t, s)
	setupPoolAndSku(t, s)

	now := time.Now()
	_, _ = s.RecordPoolEventIn(ctx(), "TEST", "TESTPOOL", "TESTSKU1",
		0.5, 0, "u_floor", now.Add(-2*time.Hour), "high", "")
	_, _ = s.RecordPoolEventIn(ctx(), "TEST", "TESTPOOL", "TESTSKU1",
		0.3, 0, "u_floor", now.Add(-1*time.Hour), "high", "")

	last, err := s.GetLastEventForItem(ctx(), "TEST", "TESTPOOL", "TESTSKU1")
	if err != nil {
		t.Fatalf("GetLastEventForItem: %v", err)
	}
	if last.WeightKg != 0.3 {
		t.Errorf("last.WeightKg = %f, want 0.3 (最近一次)", last.WeightKg)
	}
}

// ============== 7. freshcheck_period_stock (W3.1 周期盘点) 测试 ==============

func TestPeriodStock_Create_Success(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownTestStore(t, s)
	setupSkuMapForStock(t, s) // 注册 2 个原 SKU + 1 个特价码

	ps := &PeriodStock{
		BranchNo:  "TEST",
		PeriodID:  202609,
		ItemNo:    "TESTSTK1", // 原 SKU, 不是特价码
		ItemName:  "test cabbage",
		Qty:       12.5,
		Unit:      "kg",
		StockTime: time.Now(),
		Operator:  "u_floor",
		Note:      "smoke",
	}
	if err := s.CreatePeriodStock(ctx(), ps); err != nil {
		t.Fatalf("CreatePeriodStock: %v", err)
	}
	if ps.ID == 0 {
		t.Error("ID should be set")
	}
	if ps.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set")
	}
	if ps.Source != "h5" {
		t.Errorf("Source = %q, want h5 (default)", ps.Source)
	}
	if ps.Confidence != "high" {
		t.Errorf("Confidence = %q, want high (default)", ps.Confidence)
	}
}

func TestPeriodStock_Create_RejectPoolCode(t *testing.T) {
	// C8 强校验: 特价码不能当作原 SKU 录入
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownTestStore(t, s)
	setupSkuMapForStock(t, s)

	ps := &PeriodStock{
		BranchNo:  "TEST",
		PeriodID:  202609,
		ItemNo:    "TESTPOOL1", // ← 这是特价码, 不是原 SKU
		Qty:       5.0,
		Unit:      "kg",
		Operator:  "u_floor",
	}
	err := s.CreatePeriodStock(ctx(), ps)
	if err == nil {
		t.Fatal("CreatePeriodStock 应拒绝特价码 item_no, 但成功了")
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("err = %v, want ErrInvalidInput", err)
	}
}

func TestPeriodStock_Create_Duplicate(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownTestStore(t, s)
	setupSkuMapForStock(t, s)

	ps := &PeriodStock{
		BranchNo:  "TEST",
		PeriodID:  202609,
		ItemNo:    "TESTSTK1",
		Qty:       1.0,
		Operator:  "u_floor",
		StockTime: time.Now(),
	}
	if err := s.CreatePeriodStock(ctx(), ps); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	// 第二次同 (period_id, item_no) → ErrDuplicateKey
	ps2 := &PeriodStock{
		BranchNo:  "TEST",
		PeriodID:  202609,
		ItemNo:    "TESTSTK1",
		Qty:       2.0,
		Operator:  "u_floor",
		StockTime: time.Now(),
	}
	err := s.CreatePeriodStock(ctx(), ps2)
	if !errors.Is(err, ErrDuplicateKey) {
		t.Errorf("err = %v, want ErrDuplicateKey", err)
	}
}

func TestPeriodStock_Get_NotFound(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	_, err := s.GetPeriodStock(ctx(), "TEST", 99999999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPeriodStock_ListByPeriod(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownTestStore(t, s)
	setupSkuMapForStock(t, s)

	// 录 2 行同一期
	for _, it := range []string{"TESTSTK1", "TESTSTK2"} {
		err := s.CreatePeriodStock(ctx(), &PeriodStock{
			BranchNo:  "TEST",
			PeriodID:  202609,
			ItemNo:    it,
			Qty:       3.0,
			Operator:  "u_floor",
			StockTime: time.Now(),
		})
		if err != nil {
			t.Fatalf("Create %s: %v", it, err)
		}
	}
	// 列查
	items, err := s.ListPeriodStocksByPeriod(ctx(), "TEST", 202609)
	if err != nil {
		t.Fatalf("ListPeriodStocksByPeriod: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("items = %d, want 2", len(items))
	}
	// 按 item_no 升序
	if items[0].ItemNo != "TESTSTK1" || items[1].ItemNo != "TESTSTK2" {
		t.Errorf("order = [%s, %s], want [TESTSTK1, TESTSTK2]", items[0].ItemNo, items[1].ItemNo)
	}
}

func TestPeriodStock_Update(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownTestStore(t, s)
	setupSkuMapForStock(t, s)

	ps := &PeriodStock{
		BranchNo: "TEST", PeriodID: 202609, ItemNo: "TESTSTK1",
		Qty: 5.0, Unit: "kg", Operator: "u_floor", StockTime: time.Now(),
	}
	if err := s.CreatePeriodStock(ctx(), ps); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// update: 改 qty + note
	if err := s.UpdatePeriodStock(ctx(), ps.ID, "renamed", 9.5, "kg", "corrected"); err != nil {
		t.Fatalf("UpdatePeriodStock: %v", err)
	}
	got, err := s.GetPeriodStock(ctx(), "TEST", ps.ID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.Qty != 9.5 {
		t.Errorf("Qty = %f, want 9.5", got.Qty)
	}
	if got.ItemName != "renamed" {
		t.Errorf("ItemName = %q, want renamed", got.ItemName)
	}
	if got.Note != "corrected" {
		t.Errorf("Note = %q, want corrected", got.Note)
	}
	// 校验 qty < 0 拒绝
	if err := s.UpdatePeriodStock(ctx(), ps.ID, "", -1, "", ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("Update qty=-1 err = %v, want ErrInvalidInput", err)
	}
}

func TestPeriodStock_Delete(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownTestStore(t, s)
	setupSkuMapForStock(t, s)

	ps := &PeriodStock{
		BranchNo: "TEST", PeriodID: 202609, ItemNo: "TESTSTK1",
		Qty: 1.0, Operator: "u_floor", StockTime: time.Now(),
	}
	if err := s.CreatePeriodStock(ctx(), ps); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.DeletePeriodStock(ctx(), ps.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// 二次删 → ErrNotFound
	if err := s.DeletePeriodStock(ctx(), ps.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("二次删 err = %v, want ErrNotFound", err)
	}
}

func TestPeriodStock_Coverage(t *testing.T) {
	pool := setupTestPool(t)
	s := NewStore(pool)
	defer teardownTestStore(t, s)
	setupSkuMapForStock(t, s) // 2 个 active SKU (TESTSTK1, TESTSTK2)

	// 一开始 0% (没盘)
	cov, err := s.CheckPeriodStockCoverage(ctx(), "TEST", 202609)
	if err != nil {
		t.Fatalf("CheckPeriodStockCoverage (0/2): %v", err)
	}
	if cov.Total != 2 || cov.Counted != 0 {
		t.Errorf("Total=%d Counted=%d, want 2/0", cov.Total, cov.Counted)
	}
	if cov.CoveragePct != 0 {
		t.Errorf("CoveragePct = %f, want 0", cov.CoveragePct)
	}
	if cov.MeetsC8 {
		t.Error("MeetsC8 should be false when 0% coverage")
	}
	if len(cov.Missing) != 2 {
		t.Errorf("Missing = %d items, want 2", len(cov.Missing))
	}

	// 盘 1 个 → 50%
	if err := s.CreatePeriodStock(ctx(), &PeriodStock{
		BranchNo: "TEST", PeriodID: 202609, ItemNo: "TESTSTK1",
		Qty: 5, Operator: "u_floor", StockTime: time.Now(),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cov, _ = s.CheckPeriodStockCoverage(ctx(), "TEST", 202609)
	if cov.Counted != 1 || cov.Total != 2 {
		t.Errorf("Total=%d Counted=%d, want 2/1", cov.Total, cov.Counted)
	}
	if cov.CoveragePct != 50.0 {
		t.Errorf("CoveragePct = %f, want 50.0", cov.CoveragePct)
	}
	if cov.MeetsC8 {
		t.Error("MeetsC8 should be false at 50% (default threshold 100)")
	}

	// 盘 2 个 → 100% → meets
	if err := s.CreatePeriodStock(ctx(), &PeriodStock{
		BranchNo: "TEST", PeriodID: 202609, ItemNo: "TESTSTK2",
		Qty: 5, Operator: "u_floor", StockTime: time.Now(),
	}); err != nil {
		t.Fatalf("Create STK2: %v", err)
	}
	cov, _ = s.CheckPeriodStockCoverage(ctx(), "TEST", 202609)
	if cov.Counted != 2 {
		t.Errorf("Counted = %d, want 2", cov.Counted)
	}
	if cov.CoveragePct != 100.0 {
		t.Errorf("CoveragePct = %f, want 100", cov.CoveragePct)
	}
	if !cov.MeetsC8 {
		t.Error("MeetsC8 should be true at 100%")
	}
}

// setupSkuMapForStock: 准备 2 个 active SKU + 1 个特价码
//   - TESTSTK1, TESTSTK2 (leaf, fast) → 应盘
//   - TESTPOOL1 (特价码) → 强校验拒绝录入
func setupSkuMapForStock(t *testing.T, s *Store) {
	t.Helper()
	for _, sku := range []SkuMap{
		{BranchNo: "TEST", ItemNo: "TESTSTK1", ItemName: "spinach", FreshCategory: "leaf", TurnoverClass: "fast", ShelfLifeDays: 5, IsActive: true},
		{BranchNo: "TEST", ItemNo: "TESTSTK2", ItemName: "lettuce", FreshCategory: "leaf", TurnoverClass: "fast", ShelfLifeDays: 5, IsActive: true},
	} {
		if err := s.UpsertSkuMap(ctx(), &sku); err != nil {
			t.Fatalf("UpsertSkuMap %s: %v", sku.ItemNo, err)
		}
	}
	if err := s.UpsertPoolCode(ctx(), &PoolCode{
		BranchNo: "TEST", PoolCode: "TESTPOOL1", PoolName: "1元/斤",
		PricingMode: "weight", UnitPrice: 1.0, PriorityRank: 0, IsActive: true,
	}); err != nil {
		t.Fatalf("UpsertPoolCode: %v", err)
	}
}

// ============== helpers ==============

func ctx() context.Context {
	return context.Background()
}
