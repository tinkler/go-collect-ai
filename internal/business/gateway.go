// Package business - cube 调用统一网关
//
// 职责:
//   整个 collect-ai 调 cube-agent-server 的唯一入口
//   - Query:        业务字段名,自动经 Registry 翻译 → 物理 → 调 agent → 翻回业务名
//   - RawQuery:     物理字段名直传(给 restock / supplierpayment 这种业务专用 cube)
//   - RawQueryWithTime: 带时间窗口的物理查询
//
// 设计动机(2026-09-02 重构):
//
//	原架构: handler / restock / supplierpayment / executor 各处直接 import parser/agent.Client
//	         → 5 个调用入口,3 个绕过 mapping 旁路,2 个重复 Executor
//	         → 未来加 trace/permission/mock 缓存要在 5 处加
//
//	新架构: 所有调用收编到 Gateway,业务代码只依赖 business 包,不 import parser/agent
//	         → 1 个调用入口,统一加 trace/permission/mock/缓存
//	         → handler 重复的 4 处改调 Executor(在 Gateway 之上),restsok/supplierpayment 改 RawQuery
//
// 不做:
//   - 不改 agent.Client 行为(Gateway 是薄封装,签名 1:1)
//   - 不加业务逻辑(放 Executor / handler)
//   - 不加 ctx(agent.Client.Execute 当前不收 ctx,后续要加时再扩 Gateway 签名)
package business

import "fmt"

// CubeClient cube-agent-server 客户端接口
//
//	2026-09-02 重构引入,把 *agent.Client 包成 interface
//	作用:
//	  - Gateway/Executor/supplierpayment 都持这个 interface,不直接 import *agent.Client
//	  - 单测可注入 mock,无需启真实 cube-agent-server
//	  - 未来加 trace/permission/cache 装饰器,在 Gateway 包一层
//	满足条件: agent.Client 的 Execute / ExecuteWithTime / GetDataSource / Ping 4 个方法
type CubeClient interface {
	Execute(cube string, measures, dimensions []string, filters []map[string]any, segments []string, limit int) ([]map[string]any, error)
	ExecuteWithTime(cube string, measures, dimensions []string, filters []map[string]any, segments []string, limit int, timeDimensions []map[string]any) ([]map[string]any, error)
	GetDataSource() string
	Ping() error
}

// Compile-time check: *agent.Client 满足 CubeClient (实际 import 在 caller 包完成)
// 这里只定义 interface,compile check 放在 caller 包(避免 import 循环)
var _ CubeClient = (CubeClient)(nil) // 占位,真检查在 agent 包(下面会写)

// Gateway cube 调用统一网关
//
//	持有 CubeClient 和 Registry,所有 cube 调用走这里
//	未来加 trace/metric/permission/mock 在本文件改一处
type Gateway struct {
	client CubeClient
	mapper *Registry
}

// NewGateway 构造网关
//
//	c: cube-agent-server 客户端(实参 *agent.Client,或单测 mock)
//	reg: 业务字段映射表(可来自 yaml 或 NewDefaultRegistry)
func NewGateway(c CubeClient, reg *Registry) *Gateway {
	if c == nil {
		panic("business: Gateway requires non-nil CubeClient")
	}
	if reg == nil {
		panic("business: Gateway requires non-nil Registry")
	}
	return &Gateway{client: c, mapper: reg}
}

// Client 返回底层 CubeClient (供仅需 ds 信息的场景使用,如 NewExecutor)
func (g *Gateway) Client() CubeClient {
	return g.client
}

// Mapper 返回 Registry
func (g *Gateway) Mapper() *Registry {
	return g.mapper
}

// =====================================================================
// 业务字段 API (走 Registry 翻译,handler / Executor 调用)
// =====================================================================

// Query 业务字段名查询
//
//	entity:      "products" | "suppliers" | ...
//	ds:          "erp" | "hbpos" | ...
//	bizFields:   业务字段名清单 ["barcode", "product_name", ...]
//	filters:     业务字段 filter (自动翻译)
//	limit:       上限,0 = 不限
//	返回:        业务字段名 map 列表 (跟 executor.searchProducts 输出一致)
func (g *Gateway) Query(
	entity, ds string,
	bizFields []string,
	filters []BusinessFilter,
	limit int,
) ([]map[string]any, error) {
	pq, err := g.mapper.ToPhysicalQuery(entity, ds, bizFields, filters, limit)
	if err != nil {
		return nil, fmt.Errorf("gateway.Query translate: %w", err)
	}
	if pq == nil {
		return nil, nil
	}
	rows, err := g.client.Execute(pq.Cube, pq.Measures, pq.Dimensions, toAgentFilters(pq.Filters), supplierSegments, limit)
	if err != nil {
		return nil, fmt.Errorf("gateway.Query execute: %w", err)
	}
	return g.mapper.ToBusinessResponse(entity, ds, rows, bizFields)
}

// =====================================================================
// 物理字段 API (restock / supplierpayment 业务专用 cube 用)
// =====================================================================

// RawQuery 物理字段名直传
//
//	签名 1:1 对应 agent.Client.Execute,只多一层 trace/log
//	restock / supplierpayment 这种业务专用 cube(没有"业务实体"概念)用这个
func (g *Gateway) RawQuery(
	cube string,
	measures, dimensions []string,
	filters []map[string]any,
	segments []string,
	limit int,
) ([]map[string]any, error) {
	return g.client.Execute(cube, measures, dimensions, filters, segments, limit)
}

// RawQueryWithTime 带时间窗口的物理查询
//
//	签名 1:1 对应 agent.Client.ExecuteWithTime
//	restock SalesInWindow 用这个(dateRange filter)
func (g *Gateway) RawQueryWithTime(
	cube string,
	measures, dimensions []string,
	filters []map[string]any,
	segments []string,
	limit int,
	timeDimensions []map[string]any,
) ([]map[string]any, error) {
	return g.client.ExecuteWithTime(cube, measures, dimensions, filters, segments, limit, timeDimensions)
}

// =====================================================================
// Helpers
// =====================================================================

// toAgentFilters 把 business.PhysicalFilter 转成 agent 期望的 map 形式
//
//	agent.Execute 收 []map[string]any {"member":..,"operator":..,"values":..}
//	Registry.ToPhysicalQuery 返回的是强类型 []PhysicalFilter
//	Gateway 统一在这里做转换,业务代码不用关心
func toAgentFilters(filters []PhysicalFilter) []map[string]any {
	if len(filters) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(filters))
	for _, f := range filters {
		out = append(out, map[string]any{
			"member":   f.Member,
			"operator": f.Op,
			"values":   f.Values,
		})
	}
	return out
}

// SupplierByBrand 按品牌反查供应商的结果(给 Executor.SearchSuppliersByBrand 返回)
type SupplierByBrand struct {
	SupplierName string `json:"supplier_name"`
	ProductCount int    `json:"product_count"`
}
