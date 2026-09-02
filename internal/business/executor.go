// Package business - 业务查询执行器
//
// 供 handler / parser 调用的高层 API
//   不暴露物理字段名,只接受业务字段名
//   内部用 CubeClient + business.Registry 翻译
package business

import (
	"fmt"
	"strings"
)

// supplierSegments 默认传给 cube-agent-server 的 segments
//   "sup_only" 在 t_bd_item_info cube YAML 里声明(过滤客户,只保留供应商)
//   在其他 cube(products/suppliers/sales) 是 no-op(未声明的 segment 被 pass3 静默忽略)
var supplierSegments = []string{"sup_only"}

// Executor 业务查询执行器
//   封装 "业务字段名 → 物理 query → agent → 物理响应 → 业务响应" 完整链路
//
// 2026-09-02 重构:
//   改持 CubeClient interface(原 *agent.Client)
//   作用: 单测可注入 mock + 跟 Gateway 用同一个 client 避免双重连接
//   新 Executor 推荐通过 NewExecutorFromGateway 构造
type Executor struct {
	client CubeClient
	mapper *Registry
}

// NewExecutor 构造执行器
//   接受 CubeClient interface (实参 *agent.Client,或单测 mock)
func NewExecutor(c CubeClient, reg *Registry) *Executor {
	return &Executor{client: c, mapper: reg}
}

// NewExecutorFromGateway 从 Gateway 构造 Executor(推荐)
//   复用 Gateway 的 client,避免双重连接
func NewExecutorFromGateway(g *Gateway) *Executor {
	return &Executor{client: g.client, mapper: g.mapper}
}

// SearchProducts 按业务字段名搜索商品
//   supplierKeyword: 供应商关键词(多关键词用 ; 分隔),为空返回全量
//   limit: 上限
//   返回业务字段名 (barcode / product_name / ...) 的 map 列表
func (e *Executor) SearchProducts(supplierKeyword string, limit int) ([]map[string]any, error) {
	return e.searchProducts(supplierKeyword, limit, e.client.GetDataSource())
}

// SearchProductsByDS 同上,显式指定数据源(用于单请求不同 ds)
func (e *Executor) SearchProductsByDS(supplierKeyword, ds string, limit int) ([]map[string]any, error) {
	return e.searchProducts(supplierKeyword, limit, ds)
}

func (e *Executor) searchProducts(supplierKeyword string, limit int, ds string) ([]map[string]any, error) {
	ent, ok := e.mapper.Get("products")
	if !ok {
		return nil, fmt.Errorf("business: products entity not found")
	}
	src, ok := ent.Sources[ds]
	if !ok {
		return nil, fmt.Errorf("business: products %s not configured", ds)
	}
	bizFields := []string{"barcode", "product_name", "supplier_id", "supplier_name", "category", "brand", "stock_qty"}

	// 2026-09-02 重构: filter 翻译走 Registry,不再手拼 ref
	keywords := splitAndTrim(supplierKeyword, ";,\n\r\t ")
	if len(keywords) == 0 {
		// 不带 filter,直接拉
		return e.query("products", ds, bizFields, nil, limit)
	}

	seen := make(map[string]struct{})
	var merged []map[string]any
	for _, kw := range keywords {
		filters := []BusinessFilter{
			{Field: "supplier_name", Op: "contains", Values: []any{kw}},
		}
		bizRows, err := e.query("products", ds, bizFields, filters, limit)
		if err != nil {
			return nil, err
		}
		for _, br := range bizRows {
			key := ""
			if bc, ok := br["barcode"].(string); ok && bc != "" {
				key = "bc:" + bc
			} else if pn, ok := br["product_name"].(string); ok {
				if sn, ok2 := br["supplier_name"].(string); ok2 {
					key = "sn:" + sn + "|" + pn
				}
			}
			if key == "" {
				continue
			}
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				merged = append(merged, br)
			}
		}
	}
	_ = src // 保留 src 引用以备后续加 ds 校验
	return merged, nil
}

// DistinctSuppliers 拉所有 distinct 供应商
//   ds-specific 行为(原 hardcode 保留):
//     hbpos: suppliers cube 用 "suppliers.count" measure
//     erp:   products cube,measures 留空(原代码就这样,无 measure 也跑)
func (e *Executor) DistinctSuppliers(scanLimit int) ([]string, error) {
	ds := e.client.GetDataSource()
	ent, ok := e.mapper.Get("suppliers")
	if !ok {
		return nil, fmt.Errorf("business: suppliers entity not found")
	}
	src, ok := ent.Sources[ds]
	if !ok {
		return nil, fmt.Errorf("business: suppliers %s not configured", ds)
	}
	supplierNameRef := src.FieldRefs["supplier_name"]
	if supplierNameRef == "" {
		return nil, fmt.Errorf("business: suppliers %s has no supplier_name mapping", ds)
	}

	// ds-specific measures (cube.js 至少要 1 个 measure)
	measures := []string{}
	if ds == "hbpos" {
		measures = []string{"suppliers.count"}
	}
	// erp: measures 留空,原 hardcode 行为

	rows, err := e.client.Execute(src.Cube, measures, []string{supplierNameRef}, nil, supplierSegments, scanLimit)
	if err != nil {
		return nil, err
	}
	bizRows, err := e.mapper.ToBusinessResponse("suppliers", ds, rows, []string{"supplier_name"})
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{})
	for _, br := range bizRows {
		if s, ok := br["supplier_name"].(string); ok {
			s = strings.TrimSpace(s)
			if s != "" {
				set[s] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out, nil
}

// SearchProductsByBrand 按品牌(产品名 contains)反查商品,返回 product × supplier 行
//   2026-09-02 加,替代 handler.ListSuppliersByBrand 重复的翻译/调用/翻回部分
//   handler 端再做"按 supplier 聚合 distinct product count"业务聚合
//   返回: 业务字段名 map 列表,含 product_name + supplier_name
//   注: brand 字段常空,实际语义是"商品名包含关键词的产品归属于哪些供应商"
func (e *Executor) SearchProductsByBrand(brand string, limit int) ([]map[string]any, error) {
	ds := e.client.GetDataSource()
	if _, ok := e.mapper.Get("products"); !ok {
		return nil, fmt.Errorf("business: products entity not found")
	}
	bizFields := []string{"product_name", "supplier_name"}
	filters := []BusinessFilter{
		{Field: "product_name", Op: "contains", Values: []any{brand}},
	}
	return e.query("products", ds, bizFields, filters, limit)
}

// Query 通用业务字段名查询 (2026-09-02 公开)
//   handler 通用 cube 调用入口,所有业务名 + filter 翻译由 Registry 处理
//   调用方:handler / parser / 未来 trpc-agent-go tools
//   返回: 业务字段名 map 列表
func (e *Executor) Query(
	entity string,
	bizFields []string,
	filters []BusinessFilter,
	limit int,
) ([]map[string]any, error) {
	ds := e.client.GetDataSource()
	return e.query(entity, ds, bizFields, filters, limit)
}

// CubeOf 返回 entity 当前 ds 用的物理 cube 名
//   2026-09-02 加,handler 返回给前端 meta.cube 用
func (e *Executor) CubeOf(entity string) string {
	ds := e.client.GetDataSource()
	ent, ok := e.mapper.Get(entity)
	if !ok {
		return ""
	}
	src, ok := ent.Sources[ds]
	if !ok {
		return ""
	}
	return src.Cube
}

// query 内部统一:Registry 翻译 + 调 agent + 翻回
func (e *Executor) query(
	entity, ds string,
	bizFields []string,
	filters []BusinessFilter,
	limit int,
) ([]map[string]any, error) {
	pq, err := e.mapper.ToPhysicalQuery(entity, ds, bizFields, filters, limit)
	if err != nil {
		return nil, err
	}
	if pq.Cube == "" {
		return nil, fmt.Errorf("business: entity %q datasource %q has no cube", entity, ds)
	}
	rows, err := e.client.Execute(pq.Cube, pq.Measures, pq.Dimensions, toAgentFilters(pq.Filters), supplierSegments, limit)
	if err != nil {
		return nil, err
	}
	return e.mapper.ToBusinessResponse(entity, ds, rows, bizFields)
}

// splitAndTrim 按多个分隔符切字符串并去重 trim
func splitAndTrim(s string, seps string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		for _, c := range seps {
			if r == c {
				return true
			}
		}
		return false
	})
	seen := make(map[string]struct{})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}
