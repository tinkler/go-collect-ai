package freshcheck

// ============================================================
// freshcheck CubeQuerier 单元测试 (W1.5)
//
// 依赖: business.CubeClient (interface, 可 mock)
// 跑法: go test ./internal/freshcheck/...
// 不需要 PG / 真实 cube
// ============================================================

import (
	"context"
	"testing"
	"time"

	"github.com/tinkler/collect-ai/internal/business"
)

// ============== mock CubeClient ==============

type mockCube struct {
	ds             string
	execCalls      []mockCall
	execTimeCalls  []mockCall
	pingErr        error
	defaultRows    []map[string]any
}

type mockCall struct {
	Cube       string
	Measures   []string
	Dimensions []string
	Filters    []map[string]any
	Segments   []string
	Limit      int
	TimeDims   []map[string]any
}

func newMockCube() *mockCube {
	return &mockCube{ds: "hbpos"}
}

func (m *mockCube) GetDataSource() string { return m.ds }
func (m *mockCube) Ping() error           { return m.pingErr }

func (m *mockCube) Execute(cube string, measures, dimensions []string, filters []map[string]any, segments []string, limit int) ([]map[string]any, error) {
	m.execCalls = append(m.execCalls, mockCall{
		Cube: cube, Measures: measures, Dimensions: dimensions,
		Filters: filters, Segments: segments, Limit: limit,
	})
	return m.defaultRows, nil
}

func (m *mockCube) ExecuteWithTime(cube string, measures, dimensions []string, filters []map[string]any, segments []string, limit int, timeDims []map[string]any) ([]map[string]any, error) {
	m.execTimeCalls = append(m.execTimeCalls, mockCall{
		Cube: cube, Measures: measures, Dimensions: dimensions,
		Filters: filters, Segments: segments, Limit: limit, TimeDims: timeDims,
	})
	return m.defaultRows, nil
}

// helper 构造 CubeQuerier + 注入 mock
//   用 NewRegistryFromYAMLBytes 注册 freshcheck 专用的 entity (items_with_clsno + sales_with_refund)
//   默认 NewDefaultRegistry 只有 products/returns/suppliers, 不够
func newTestCubeQuerier(mc *mockCube) *CubeQuerier {
	yamlData := []byte(`
entities:
  items_with_clsno:
    name: items_with_clsno
    fields:
      item_no:         {type: dimension, description: 货号}
      item_name:       {type: dimension, description: 品名}
      item_clsno:      {type: dimension, description: 分类}
      item_clsname:    {type: dimension, description: 分类名}
      item_brand:      {type: dimension}
      item_brandname:  {type: dimension}
      main_supcust:    {type: dimension}
      unit:            {type: dimension}
      fresh_category:  {type: dimension}
      turnover_class:  {type: dimension}
      stock_qty:       {type: measure}
    sources:
      hbpos:
        cube: items_with_clsno
        fields:
          item_no:         items_with_clsno.item_no
          item_name:       items_with_clsno.item_name
          item_clsno:      items_with_clsno.item_clsno
          item_clsname:    items_with_clsno.item_clsname
          item_brand:      items_with_clsno.item_brand
          item_brandname:  items_with_clsno.item_brandname
          main_supcust:    items_with_clsno.main_supcust
          unit:            items_with_clsno.unit
          fresh_category:  items_with_clsno.fresh_category
          turnover_class:  items_with_clsno.turnover_class
          stock_qty:       items_with_clsno.stock_qty
`)
	reg, err := business.NewRegistryFromYAMLBytes(yamlData)
	if err != nil {
		panic("test registry load failed: " + err.Error())
	}
	gw := business.NewGateway(mc, reg)
	return &CubeQuerier{Gateway: gw, Store: nil}
}

// ============== Test: SalesWithRefundInWindow ==============

func TestCube_SalesWithRefundInWindow_CallsCorrectCube(t *testing.T) {
	mc := newMockCube()
	// 2026-09-10: 走 measure 聚合后, mock 用 total_qnty/total_revenue (cube 端 GROUP BY item_no 结果, 净额)
	mc.defaultRows = []map[string]any{
		{
			"sales_with_refund.item_no":       "6901028001234",
			"sales_with_refund.total_qnty":    0.5,  // 净额
			"sales_with_refund.total_revenue": 0.5,  // 净额
			"sales_with_refund.total_cost":    0.2,
			"sales_with_refund.total_gross_profit": 0.3,
		},
	}
	q := newTestCubeQuerier(mc)

	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	rows, err := q.SalesWithRefundInWindow(context.Background(), "0001", from, to)
	if err != nil {
		t.Fatalf("SalesWithRefundInWindow: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("rows = %d, want 1", len(rows))
	}

	// 1) 调了 Execute (无 time filter, 走 inDateRange filter 代替)
	if len(mc.execCalls) != 1 {
		t.Fatalf("execCalls = %d, want 1", len(mc.execCalls))
	}
	// 2) cube 名是 sales_with_refund
	if mc.execCalls[0].Cube != "sales_with_refund" {
		t.Errorf("Cube = %s, want sales_with_refund", mc.execCalls[0].Cube)
	}
	// 3) 验证 measure 是 cube measure (total_qnty/total_revenue/total_cost/total_gross_profit)
	wantMeasures := map[string]bool{
		"sales_with_refund.total_qnty":          true,
		"sales_with_refund.total_revenue":       true,
		"sales_with_refund.total_cost":          true,
		"sales_with_refund.total_gross_profit":  true,
	}
	for _, m := range mc.execCalls[0].Measures {
		if !wantMeasures[m] {
			t.Errorf("unexpected measure: %s", m)
		}
	}
	// 4) filter 含 branch_no + oper_date(inDateRange)
	foundBranch := false
	foundDate := false
	for _, f := range mc.execCalls[0].Filters {
		if f["member"] == "sales_with_refund.branch_no" {
			foundBranch = true
			if vals, ok := f["values"].([]string); !ok || len(vals) == 0 || vals[0] != "0001" {
				t.Errorf("branch filter values = %v, want [0001]", f["values"])
			}
		}
		if f["member"] == "sales_with_refund.oper_date" {
			foundDate = true
			if f["operator"] != "inDateRange" {
				t.Errorf("oper_date operator = %v, want inDateRange", f["operator"])
			}
		}
	}
	if !foundBranch {
		t.Error("filters 缺 branch_no")
	}
	if !foundDate {
		t.Error("filters 缺 oper_date inDateRange")
	}
}

// ============== Test: PurchasesInWindow ==============

func TestCube_PurchasesInWindow_ParsesRows(t *testing.T) {
	mc := newMockCube()
	// 2026-09-10: 走 measure 聚合后, mock 用 total_qty/total_cost (cube 端 GROUP BY item_no 结果)
	mc.defaultRows = []map[string]any{
		{
			"purchases.item_no":    "6901028001234",
			"purchases.item_name":  "菠菜",
			"purchases.count":      3.0,   // 3 笔入库单
			"purchases.total_qty":  10.0,  // SUM(real_qty)
			"purchases.total_cost": 25.0,  // SUM(real_qty*cost_price)
		},
	}
	q := newTestCubeQuerier(mc)

	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	rows, err := q.PurchasesInWindow(context.Background(), "0001", from, to)
	if err != nil {
		t.Fatalf("PurchasesInWindow: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.ItemNo != "6901028001234" {
		t.Errorf("ItemNo = %s, want 6901028001234", r.ItemNo)
	}
	if r.RealQty != 10.0 {
		t.Errorf("RealQty = %f, want 10.0", r.RealQty)
	}
	if r.TotalCost != 25.0 {
		t.Errorf("TotalCost = %f, want 25.0", r.TotalCost)
	}
	// 派生 cost_price = total_cost / total_qty = 25/10 = 2.5
	if r.CostPrice != 2.5 {
		t.Errorf("CostPrice = %f, want 2.5 (派生自 total_cost/total_qty)", r.CostPrice)
	}
	// 聚合后 voucher_no/oper_date 留空
	if r.VoucherNo != "" {
		t.Errorf("VoucherNo = %q, want empty (聚合后无单据号)", r.VoucherNo)
	}
	if !r.OperDate.IsZero() {
		t.Error("OperDate 未解析")
	}

	// 验证 cube 名 (2026-09-10: 改用 RawQuery + inDateRange filter, 不再走 timeDim)
	if len(mc.execCalls) != 1 || mc.execCalls[0].Cube != "purchases" {
		t.Errorf("期望调 purchases cube, 实际 %v", mc.execCalls)
	}
}

// ============== Test: AvgCost ==============

func TestCube_AvgCost_ReturnsCost(t *testing.T) {
	mc := newMockCube()
	mc.defaultRows = []map[string]any{
		{"inventory_current.branch_no": "0001", "inventory_current.item_no": "6901028001234", "inventory_current.avg_cost": 2.5},
	}
	q := newTestCubeQuerier(mc)

	cost, err := q.AvgCost(context.Background(), "0001", "6901028001234")
	if err != nil {
		t.Fatalf("AvgCost: %v", err)
	}
	if cost != 2.5 {
		t.Errorf("cost = %f, want 2.5", cost)
	}
	if len(mc.execCalls) != 1 {
		t.Fatalf("execCalls = %d, want 1", len(mc.execCalls))
	}
	if mc.execCalls[0].Cube != "inventory_current" {
		t.Errorf("Cube = %s, want inventory_current", mc.execCalls[0].Cube)
	}
}

func TestCube_AvgCost_NotFound(t *testing.T) {
	mc := newMockCube()
	mc.defaultRows = []map[string]any{} // 空
	q := newTestCubeQuerier(mc)

	cost, err := q.AvgCost(context.Background(), "0001", "NOT_EXIST")
	if err != nil {
		t.Fatalf("AvgCost: %v", err)
	}
	if cost != 0 {
		t.Errorf("无库存应返 0, got %f", cost)
	}
}

// ============== Test: StockSnapshot ==============

func TestCube_StockSnapshot_Aggregates(t *testing.T) {
	mc := newMockCube()
	mc.defaultRows = []map[string]any{
		{"inventory_current.branch_no": "0001", "inventory_current.item_no": "ITEM1", "inventory_current.stock_qty": 10.0, "inventory_current.avg_cost": 1.5},
		{"inventory_current.branch_no": "0001", "inventory_current.item_no": "ITEM2", "inventory_current.stock_qty": 5.0, "inventory_current.avg_cost": 3.0},
	}
	q := newTestCubeQuerier(mc)

	snap, err := q.StockSnapshot(context.Background(), "0001")
	if err != nil {
		t.Fatalf("StockSnapshot: %v", err)
	}
	if len(snap) != 2 {
		t.Errorf("snap len = %d, want 2", len(snap))
	}
	if snap["ITEM1"].StockQty != 10.0 {
		t.Errorf("ITEM1.StockQty = %f, want 10.0", snap["ITEM1"].StockQty)
	}
	if snap["ITEM2"].AvgCost != 3.0 {
		t.Errorf("ITEM2.AvgCost = %f, want 3.0", snap["ITEM2"].AvgCost)
	}
}

// ============== Test: FreshItemsByCategory (走 Executor) ==============

func TestCube_FreshItemsByCategory_TranslatesToPhysicalName(t *testing.T) {
	mc := newMockCube()
	// mock 走 Registry 翻译: bizField "item_no" → 物理 "items_with_clsno.item_no"
	mc.defaultRows = []map[string]any{
		{
			"items_with_clsno.item_no":        "6901028001234",
			"items_with_clsno.item_name":      "菠菜",
			"items_with_clsno.fresh_category":  "leaf",
			"items_with_clsno.turnover_class":  "fast",
			"items_with_clsno.unit":           "kg",
			"items_with_clsno.stock_qty":      47.0,
		},
	}
	q := newTestCubeQuerier(mc)

	rows, err := q.FreshItemsByCategory(context.Background(), "leaf")
	if err != nil {
		t.Fatalf("FreshItemsByCategory: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].FreshCategory != "leaf" {
		t.Errorf("FreshCategory = %s, want leaf", rows[0].FreshCategory)
	}
	if rows[0].StockQty != 47.0 {
		t.Errorf("StockQty = %f, want 47.0", rows[0].StockQty)
	}

	// 验证走了 Execute 而非 ExecuteWithTime (无 time dim)
	if len(mc.execCalls) != 1 {
		t.Errorf("execCalls = %d, want 1", len(mc.execCalls))
	}
	if len(mc.execTimeCalls) != 0 {
		t.Errorf("execTimeCalls 应为 0 (无 time filter), got %d", len(mc.execTimeCalls))
	}
	// 验证 cube 名翻译成物理名
	if mc.execCalls[0].Cube != "items_with_clsno" {
		t.Errorf("Cube = %s, want items_with_clsno", mc.execCalls[0].Cube)
	}
	// 验证 filter 含 fresh_category
	foundCat := false
	for _, f := range mc.execCalls[0].Filters {
		if f["member"] == "items_with_clsno.fresh_category" {
			foundCat = true
		}
	}
	if !foundCat {
		t.Error("filters 缺 fresh_category")
	}
}

func TestCube_FreshItemsByCategory_AllFreshWhenEmpty(t *testing.T) {
	mc := newMockCube()
	mc.defaultRows = []map[string]any{}
	q := newTestCubeQuerier(mc)

	_, err := q.FreshItemsByCategory(context.Background(), "") // 空=全量生鲜
	if err != nil {
		t.Fatalf("FreshItemsByCategory(''): %v", err)
	}
	// 验证: 空 freshCategory 时不传 fresh_category filter (拉全量生鲜)
	if len(mc.execCalls) != 1 {
		t.Errorf("execCalls = %d, want 1", len(mc.execCalls))
	}
	for _, f := range mc.execCalls[0].Filters {
		if f["member"] == "items_with_clsno.fresh_category" {
			t.Error("空 freshCategory 时不应有 fresh_category filter")
		}
	}
}

// ============== Test: helpers ==============

func TestAsString_Float_Int_String(t *testing.T) {
	m := map[string]any{
		"s":   "hello",
		"f":   3.14,
		"i":   42,
		"i64": int64(100),
		"missing_key_should_empty": nil,
	}
	if asString(m, "s") != "hello" {
		t.Errorf("asString(s) = %s", asString(m, "s"))
	}
	if asString(m, "f") != "3.14" {
		t.Errorf("asString(f) = %s", asString(m, "f"))
	}
	if asString(m, "missing") != "" {
		t.Errorf("asString(missing) 应为空")
	}
}

func TestAsFloat_StringToFloat(t *testing.T) {
	m := map[string]any{
		"f":   3.14,
		"s":   "2.5",
		"i":   10,
		"bad": "not a number",
	}
	if asFloat(m, "f") != 3.14 {
		t.Errorf("asFloat(f) = %f, want 3.14", asFloat(m, "f"))
	}
	if asFloat(m, "s") != 2.5 {
		t.Errorf("asFloat(s) = %f, want 2.5", asFloat(m, "s"))
	}
	if asFloat(m, "bad") != 0 {
		t.Errorf("asFloat(bad string) 应返 0")
	}
}
