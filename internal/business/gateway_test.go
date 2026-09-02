package business

import (
	"errors"
	"testing"
)

// mockCubeClient 模拟 CubeClient,记录调用参数 + 返回固定数据
type mockCubeClient struct {
	ds             string
	execCalls      []mockExecCall
	execWithTimeCalls []mockExecCall
	pingErr        error
}

type mockExecCall struct {
	Cube       string
	Measures   []string
	Dimensions []string
	Filters    []map[string]any
	Segments   []string
	Limit      int
	TimeDims   []map[string]any // for ExecuteWithTime
}

func newMock(ds string) *mockCubeClient {
	return &mockCubeClient{ds: ds}
}

func (m *mockCubeClient) GetDataSource() string { return m.ds }
func (m *mockCubeClient) Ping() error           { return m.pingErr }

func (m *mockCubeClient) Execute(cube string, measures, dimensions []string, filters []map[string]any, segments []string, limit int) ([]map[string]any, error) {
	m.execCalls = append(m.execCalls, mockExecCall{
		Cube: cube, Measures: measures, Dimensions: dimensions,
		Filters: filters, Segments: segments, Limit: limit,
	})
	// 返回物理 ref 格式,key 跟 hbpos mapping 对齐
	//   barcode     → t_bd_item_info.item_no
	//   product_name → t_bd_item_info.item_name
	//   supplier_name → t_bd_item_info.supplier_name
	return []map[string]any{
		{
			"t_bd_item_info.item_no":         "6901028001234",
			"t_bd_item_info.item_name":       "蒙牛纯牛奶",
			"t_bd_item_info.supplier_name":   "蒙牛",
			"t_bd_item_info.stock_qty":       47.0,
		},
	}, nil
}

func (m *mockCubeClient) ExecuteWithTime(cube string, measures, dimensions []string, filters []map[string]any, segments []string, limit int, timeDims []map[string]any) ([]map[string]any, error) {
	m.execWithTimeCalls = append(m.execWithTimeCalls, mockExecCall{
		Cube: cube, Measures: measures, Dimensions: dimensions,
		Filters: filters, Segments: segments, Limit: limit, TimeDims: timeDims,
	})
	return []map[string]any{
		{"sales.item_no": "001", "sales.sale_qty": 1.5, "sales.inv_snapshot": 10},
	}, nil
}

// =====================================================================
// Gateway 单元测试
// =====================================================================

func TestGateway_Query_BusinessFieldsTranslate(t *testing.T) {
	// 验证: 传业务字段名 (barcode) → Registry 翻译成物理 ref (products.barcode)
	//   → Execute 用物理字段名
	//   → 响应翻回业务字段名 (barcode)
	mock := newMock("hbpos")
	reg := NewDefaultRegistry()
	gw := NewGateway(mock, reg)

	bizFields := []string{"barcode", "product_name", "supplier_name"}
	filters := []BusinessFilter{
		{Field: "supplier_name", Op: "contains", Values: []any{"蒙牛"}},
	}
	rows, err := gw.Query("products", "hbpos", bizFields, filters, 100)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: want 1, got %d", len(rows))
	}
	// 验证: 响应是业务字段名 (barcode, product_name, supplier_name)
	row := rows[0]
	if _, ok := row["barcode"]; !ok {
		t.Error("response missing business field 'barcode'")
	}
	if _, ok := row["product_name"]; !ok {
		t.Error("response missing business field 'product_name'")
	}
	// 验证: 物理 ref 不应该出现在响应 key 里
	if _, ok := row["products.barcode"]; ok {
		t.Error("response contains physical ref 'products.barcode', should be translated to 'barcode'")
	}

	// 验证: Execute 收到的是物理 ref
	if len(mock.execCalls) != 1 {
		t.Fatalf("Execute calls: want 1, got %d", len(mock.execCalls))
	}
	call := mock.execCalls[0]
	if call.Cube != "t_bd_item_info" {
		t.Errorf("cube: want t_bd_item_info, got %s", call.Cube)
	}
	// dimensions 应包含物理 ref "t_bd_item_info.item_no" (barcode 映射)
	hasBarcodeRef := false
	for _, d := range call.Dimensions {
		if d == "t_bd_item_info.item_no" {
			hasBarcodeRef = true
		}
	}
	if !hasBarcodeRef {
		t.Errorf("dimensions missing t_bd_item_info.item_no (barcode 翻译): %v", call.Dimensions)
	}
	// filter 应被翻译成物理 ref
	if len(call.Filters) != 1 {
		t.Fatalf("filters: want 1, got %d", len(call.Filters))
	}
	if call.Filters[0]["member"] != "t_bd_item_info.supplier_name" {
		t.Errorf("filter member: want t_bd_item_info.supplier_name, got %v", call.Filters[0]["member"])
	}
}

func TestGateway_Query_EmptyFieldsReturnEmpty(t *testing.T) {
	mock := newMock("hbpos")
	reg := NewDefaultRegistry()
	gw := NewGateway(mock, reg)

	// 业务字段在 hbpos 全没映射(空 bizFields)
	rows, err := gw.Query("products", "hbpos", nil, nil, 100)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// 不应崩;Execute 可能不调或调空
	_ = rows
}

func TestGateway_RawQuery_PassThrough(t *testing.T) {
	// 验证: RawQuery 是物理名直传,不翻译
	mock := newMock("hbpos")
	reg := NewDefaultRegistry()
	gw := NewGateway(mock, reg)

	rows, err := gw.RawQuery("siss_saleflow",
		[]string{"sales.sale_money"},
		[]string{"sales.supplier_name"},
		[]map[string]any{{"member": "sales.oper_date", "operator": "afterOrOn", "values": []string{"2026-01-01"}}},
		nil, 1000)
	if err != nil {
		t.Fatalf("RawQuery: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: want 1, got %d", len(rows))
	}
	if len(mock.execCalls) != 1 {
		t.Fatalf("Execute calls: want 1, got %d", len(mock.execCalls))
	}
	call := mock.execCalls[0]
	if call.Cube != "siss_saleflow" {
		t.Errorf("RawQuery cube should pass through, got %s", call.Cube)
	}
}

func TestGateway_RawQueryWithTime_PassTimeDims(t *testing.T) {
	mock := newMock("hbpos")
	reg := NewDefaultRegistry()
	gw := NewGateway(mock, reg)

	timeDims := []map[string]any{
		{"dimension": "display_restock_window.oper_date", "dateRange": []string{"2026-08-31 00:00:00", "2026-08-31 23:59:59"}},
	}
	_, err := gw.RawQueryWithTime("display_restock_window",
		[]string{"sales.sale_qty"}, []string{"sales.item_no"},
		[]map[string]any{{"member": "sales.branch_no", "operator": "equals", "values": []string{"001"}}},
		nil, 10000, timeDims)
	if err != nil {
		t.Fatalf("RawQueryWithTime: %v", err)
	}
	if len(mock.execWithTimeCalls) != 1 {
		t.Fatalf("ExecuteWithTime calls: want 1, got %d", len(mock.execWithTimeCalls))
	}
	if len(mock.execWithTimeCalls[0].TimeDims) != 1 {
		t.Errorf("TimeDims not passed through")
	}
}

func TestGateway_Ping_Propagate(t *testing.T) {
	mock := newMock("hbpos")
	mock.pingErr = errors.New("network down")
	reg := NewDefaultRegistry()
	gw := NewGateway(mock, reg)
	if err := gw.Client().Ping(); err == nil {
		t.Error("expected ping error")
	}
}

// =====================================================================
// Executor 新方法单测
// =====================================================================

func TestExecutor_SearchProductsByBrand_TranslateAndCall(t *testing.T) {
	mock := newMock("hbpos")
	reg := NewDefaultRegistry()
	exec := NewExecutor(mock, reg)

	rows, err := exec.SearchProductsByBrand("蒙牛", 100)
	if err != nil {
		t.Fatalf("SearchProductsByBrand: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: want 1, got %d", len(rows))
	}
	// 验证: filter 用物理 ref 拼 "contains"
	if len(mock.execCalls) != 1 {
		t.Fatalf("Execute calls: want 1, got %d", len(mock.execCalls))
	}
	call := mock.execCalls[0]
	if len(call.Filters) != 1 {
		t.Fatalf("filters: want 1, got %d", len(call.Filters))
	}
	if call.Filters[0]["member"] != "t_bd_item_info.item_name" {
		t.Errorf("filter member: want t_bd_item_info.item_name (product_name 翻译), got %v", call.Filters[0]["member"])
	}
	if call.Filters[0]["operator"] != "contains" {
		t.Errorf("operator: want contains, got %v", call.Filters[0]["operator"])
	}
}

func TestExecutor_CubeOf_ReturnCurrentDSCube(t *testing.T) {
	mock := newMock("hbpos")
	reg := NewDefaultRegistry()
	exec := NewExecutor(mock, reg)
	if got := exec.CubeOf("products"); got != "t_bd_item_info" {
		t.Errorf("CubeOf(products) on hbpos: want t_bd_item_info, got %s", got)
	}

	mock2 := newMock("erp")
	exec2 := NewExecutor(mock2, reg)
	if got := exec2.CubeOf("products"); got != "products" {
		t.Errorf("CubeOf(products) on erp: want products, got %s", got)
	}
}

func TestExecutor_Query_GenericAPI(t *testing.T) {
	// 验证: Executor.Query 是通用入口,handler 可以用任意 bizFields + filter
	mock := newMock("hbpos")
	reg := NewDefaultRegistry()
	exec := NewExecutor(mock, reg)

	bizFields := []string{"barcode", "product_name", "supplier_name", "stock_qty"}
	filters := []BusinessFilter{
		{Field: "barcode", Op: "equals", Values: []any{"6901028001234"}},
	}
	rows, err := exec.Query("products", bizFields, filters, 1)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: want 1, got %d", len(rows))
	}
	// 验证: stock_qty 是 measure,进了 measures 而非 dimensions
	call := mock.execCalls[0]
	hasMeasure := false
	for _, m := range call.Measures {
		if m == "t_bd_item_info.stock_qty" {
			hasMeasure = true
		}
	}
	if !hasMeasure {
		t.Errorf("stock_qty 应进 measures (FieldTypeMeasure), got measures=%v", call.Measures)
	}
}
