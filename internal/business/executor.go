// Package business - 业务查询执行器
//
// 供 handler / parser 调用的高层 API
//   不暴露物理字段名,只接受业务字段名
//   内部用 agent.Client + business.Registry 翻译
package business

import (
	"fmt"
	"strings"

	"github.com/tinkler/collect-ai/internal/parser/agent"
)

// supplierSegments 默认传给 cube-agent-server 的 segments
//   "sup_only" 在 t_bd_item_info cube YAML 里声明(过滤客户,只保留供应商)
//   在其他 cube(products/suppliers/sales) 是 no-op(未声明的 segment 被 pass3 静默忽略)
var supplierSegments = []string{"sup_only"}

// Executor 业务查询执行器
//   封装 "业务字段名 → 物理 query → agent → 物理响应 → 业务响应" 完整链路
type Executor struct {
	agent  *agent.Client
	mapper *Registry
}

// NewExecutor 构造执行器
func NewExecutor(ac *agent.Client, reg *Registry) *Executor {
	return &Executor{agent: ac, mapper: reg}
}

// SearchProducts 按业务字段名搜索商品
//   supplierKeyword: 供应商关键词(多关键词用 ; 分隔),为空返回全量
//   limit: 上限
//   返回业务字段名 (barcode / product_name / ...) 的 map 列表
func (e *Executor) SearchProducts(supplierKeyword string, limit int) ([]map[string]any, error) {
	return e.searchProducts(supplierKeyword, limit, e.agent.GetDataSource())
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
	pq, err := e.mapper.ToPhysicalQuery("products", ds, bizFields, nil, 0)
	if err != nil {
		return nil, err
	}
	measures := pq.Measures
	dimensions := pq.Dimensions

	// filter
	supplierNameRef := src.FieldRefs["supplier_name"]
	keywords := splitAndTrim(supplierKeyword, ";,\n\r\t ")
	if len(keywords) == 0 {
		// 不带 filter,直接拉
		rows, err := e.agent.Execute(src.Cube, measures, dimensions, nil, supplierSegments, limit)
		if err != nil {
			return nil, err
		}
		return e.mapper.ToBusinessResponse("products", ds, rows, bizFields)
	}

	seen := make(map[string]struct{})
	var merged []map[string]any
	for _, kw := range keywords {
		filters := []map[string]any{
			{"member": supplierNameRef, "operator": "contains", "values": []string{kw}},
		}
		rows, err := e.agent.Execute(src.Cube, measures, dimensions, filters, supplierSegments, limit)
		if err != nil {
			return nil, err
		}
		bizRows, err := e.mapper.ToBusinessResponse("products", ds, rows, bizFields)
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
	return merged, nil
}

// DistinctSuppliers 拉所有 distinct 供应商
func (e *Executor) DistinctSuppliers(scanLimit int) ([]string, error) {
	ds := e.agent.GetDataSource()
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
	measures := []string{}
	if ds == "erp" {
		if r, ok := src.FieldRefs["stock_qty"]; ok && r != "" {
			measures = []string{r}
		}
	} else {
		measures = []string{"suppliers.count"}
	}
	rows, err := e.agent.Execute(src.Cube, measures, []string{supplierNameRef}, nil, supplierSegments, scanLimit)
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
