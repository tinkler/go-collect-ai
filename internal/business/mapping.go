// Package business 业务字段映射层
//
// 职责:
//  1. 维护"业务字段名" → "物理 cube 字段名" 的映射表(每个 entity × datasource 一份)
//  2. 接收前端发来的业务查询(BusinessQuery),翻译成物理 query 调用 cube-agent-server
//  3. 收到 cube-agent-server 物理响应,翻回业务字段名返回前端
//
// 不做:
//   - 不连数据库(走 cube-agent-server)
//   - 不知道 SQL 怎么写(数据治理层职责)
//   - 不维护 cube 定义(cube-agent-server 负责)
//
// 设计原则:
//   - 一个 entity (products / suppliers / ...) 在每个 datasource 都有独立 mapping
//   - mapping 字段可能是 dimension 引用 / measure 引用(都映射到物理 cube 字段名)
//   - 业务字段和物理字段 1:1 映射(无 join / 无 expression),SQL 拼接交给 cube-agent-server
package business

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// FieldType 业务字段类型
type FieldType string

const (
	FieldTypeDimension FieldType = "dimension" // 普通 dimension
	FieldTypeMeasure   FieldType = "measure"   // 聚合 measure
	FieldTypeTime      FieldType = "time"      // 时间维度
)

// FieldDef 业务字段定义
type FieldDef struct {
	Name        string    // 业务字段名(对外,前端用)
	Type        FieldType // dimension / measure / time
	Required    bool      // 是否必填
	Description string
}

// SourceMapping 单个数据源下,业务字段 → 物理 cube 字段的映射
type SourceMapping struct {
	// 物理 cube 名(cube-agent-server 里的 cube)
	Cube string

	// 业务字段 → 物理 cube 字段引用
	//   "barcode"        → "products.barcode"      (dimension 引用)
	//   "stock_qty"      → "products.qty"          (measure 引用)
	//   "category"       → "(空字符串)"            (该 ds 不支持,返回空)
	FieldRefs map[string]string

	// 可用业务字段清单(用于 metadata 暴露)
	AvailableFields []string
}

// EntityMapping 一个业务实体的完整映射
type EntityMapping struct {
	Name        string                   // "products" / "suppliers"
	Description string                   // 业务说明
	Fields      map[string]FieldDef      // 业务字段名 → 定义
	Sources     map[string]SourceMapping // key = datasource (erp/hbpos)
}

// Registry 业务映射注册表
//
//	并发安全,启动时初始化一次,运行时只读
type Registry struct {
	mu       sync.RWMutex
	entities map[string]*EntityMapping
}

// NewDefaultRegistry 默认注册 products + suppliers 两个业务实体
//
//	hardcode 业务字段映射规则,后续可改用 config/field_mappings.yaml
func NewDefaultRegistry() *Registry {
	r := &Registry{entities: map[string]*EntityMapping{}}
	r.registerProducts()
	r.registerSuppliers()
	return r
}

// Get 取一个 entity 的 mapping
func (r *Registry) Get(entity string) (*EntityMapping, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entities[entity]
	return e, ok
}

// List 所有 entity 名
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.entities))
	for k := range r.entities {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ToPhysicalQuery 把业务查询翻译成物理 query
//
// 输入:
//
//	entity:     "products"
//	datasource: "erp" | "hbpos"
//	businessFields: ["barcode", "product_name", "supplier_name", "stock_qty"]
//	filter:     [{Field: "supplier_name", Op: "contains", Value: "X"}]  (业务字段)
//
// 输出:
//
//	cube:           物理 cube 名
//	measures:       ["products.qty"]
//	dimensions:     ["products.barcode", "products.name", "products.main_supp_name"]
//	filterRefs:     [{Member: "products.main_supp_name", Op: "contains", Values: ["X"]}]
func (r *Registry) ToPhysicalQuery(
	entity, datasource string,
	businessFields []string,
	businessFilters []BusinessFilter,
	limit int,
) (*PhysicalQuery, error) {
	ent, ok := r.Get(entity)
	if !ok {
		return nil, fmt.Errorf("business: unknown entity %q (available: %v)", entity, r.List())
	}
	src, ok := ent.Sources[datasource]
	if !ok {
		return nil, fmt.Errorf("business: entity %q has no mapping for datasource %q (available: %v)",
			entity, datasource, availableDS(ent))
	}
	if src.Cube == "" {
		return nil, fmt.Errorf("business: entity %q datasource %q has no physical cube mapping", entity, datasource)
	}

	q := &PhysicalQuery{
		Cube:       src.Cube,
		Limit:      limit,
		Dimensions: []string{},
		Measures:   []string{},
		Filters:    []PhysicalFilter{},
	}

	// 业务字段 → 物理字段
	for _, bf := range businessFields {
		ref, ok := src.FieldRefs[bf]
		if !ok || ref == "" {
			// 该 ds 没这个字段,跳过
			continue
		}
		// 判定 type
		fd, fdOK := ent.Fields[bf]
		if !fdOK {
			// 业务字段没定义 type,默认 dimension
			q.Dimensions = append(q.Dimensions, ref)
			continue
		}
		switch fd.Type {
		case FieldTypeMeasure:
			q.Measures = append(q.Measures, ref)
		default:
			q.Dimensions = append(q.Dimensions, ref)
		}
	}

	// 业务 filter → 物理 filter
	for _, bf := range businessFilters {
		ref, ok := src.FieldRefs[bf.Field]
		if !ok || ref == "" {
			// 该 ds 没这个 filter 字段,跳过
			continue
		}
		q.Filters = append(q.Filters, PhysicalFilter{
			Member: ref,
			Op:     bf.Op,
			Values: bf.Values,
		})
	}

	return q, nil
}

// ToBusinessResponse 把物理响应翻回业务字段名
//
//	data 形如 [{"products.main_supp_name": "X", "products.name": "Y"}, ...]
//	businessFields 是请求里指定的业务字段清单
//	返回: [{"supplier_name": "X", "product_name": "Y"}, ...] (业务字段名,不带 cube 前缀)
func (r *Registry) ToBusinessResponse(
	entity, datasource string,
	physicalRows []map[string]any,
	businessFields []string,
) ([]map[string]any, error) {
	ent, ok := r.Get(entity)
	if !ok {
		return nil, fmt.Errorf("business: unknown entity %q", entity)
	}
	src, ok := ent.Sources[datasource]
	if !ok {
		return nil, fmt.Errorf("business: entity %q has no mapping for datasource %q", entity, datasource)
	}

	// 物理 ref (e.g. "products.main_supp_name") → 业务字段名 (e.g. "supplier_name")
	//   同时也允许只 match 后缀,比如 row key = "sup_name" 时,前缀可能因 cube 不同而不同
	refToBiz := map[string]string{}
	for bizName, physicalRef := range src.FieldRefs {
		if physicalRef == "" {
			continue
		}
		refToBiz[physicalRef] = bizName
		// 兼容:也存后缀形式 (e.g. "main_supp_name" → "supplier_name")
		// 这处理 cube SQL 里没显式带 cube 别名的情况
		if idx := strings.LastIndex(physicalRef, "."); idx >= 0 {
			suffix := physicalRef[idx+1:]
			if _, exists := refToBiz[suffix]; !exists {
				refToBiz[suffix] = bizName
			}
		}
	}

	out := make([]map[string]any, 0, len(physicalRows))
	for _, row := range physicalRows {
		biz := map[string]any{}
		// 1) 遍历请求的业务字段
		for _, bf := range businessFields {
			ref, ok := src.FieldRefs[bf]
			if !ok || ref == "" {
				// 不支持,跳过(返回空值或不输出)
				continue
			}
			// row keys 形如 "products.main_supp_name" 或 "main_supp_name"
			if v, hit := row[ref]; hit {
				biz[bf] = v
			} else if idx := strings.LastIndex(ref, "."); idx >= 0 {
				if v, hit := row[ref[idx+1:]]; hit {
					biz[bf] = v
				}
			}
		}
		// 2) measure 字段(原 key 是 "cube.field",提取 field 做对照)
		for k, v := range row {
			// 已处理过,跳过
			if _, used := biz[k]; used {
				continue
			}
			// measure 字段通常 key 就是物理 ref
			if bizName, ok := refToBiz[k]; ok {
				biz[bizName] = v
			}
		}
		out = append(out, biz)
	}
	return out, nil
}

// AvailableDataSources 取 entity 的所有可用数据源
func (r *Registry) AvailableDataSources(entity string) []string {
	ent, ok := r.Get(entity)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(ent.Sources))
	for k := range ent.Sources {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// AvailableFields 取 entity 的所有业务字段
func (r *Registry) AvailableFields(entity string) []string {
	ent, ok := r.Get(entity)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(ent.Fields))
	for k := range ent.Fields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ============================================================
// Types
// ============================================================

// BusinessFilter 业务 filter(前端用)
type BusinessFilter struct {
	Field  string `json:"field"` // 业务字段名
	Op     string `json:"op"`    // operator (equals/contains/in/...)
	Values []any  `json:"values"`
}

// PhysicalQuery 物理 query(给 cube-agent-server 用)
type PhysicalQuery struct {
	Cube       string           `json:"cube"`
	Dimensions []string         `json:"dimensions,omitempty"`
	Measures   []string         `json:"measures,omitempty"`
	Filters    []PhysicalFilter `json:"filters,omitempty"`
	Limit      int              `json:"limit,omitempty"`
}

// PhysicalFilter 物理 filter
type PhysicalFilter struct {
	Member string `json:"member"`
	Op     string `json:"op"`
	Values []any  `json:"values"`
}

// ============================================================
// Default mappings (hardcode,启动时加载)
// ============================================================

func (r *Registry) registerProducts() {
	ent := &EntityMapping{
		Name:        "products",
		Description: "商品主表(跨数据源业务统一)",
		Fields: map[string]FieldDef{
			"barcode":       {Name: "barcode", Type: FieldTypeDimension, Description: "商品条码"},
			"product_name":  {Name: "product_name", Type: FieldTypeDimension, Description: "商品名称"},
			"supplier_id":   {Name: "supplier_id", Type: FieldTypeDimension, Description: "主供应商 ID"},
			"supplier_name": {Name: "supplier_name", Type: FieldTypeDimension, Description: "主供应商名"},
			"category":      {Name: "category", Type: FieldTypeDimension, Description: "商品分类"},
			"brand":         {Name: "brand", Type: FieldTypeDimension, Description: "品牌"},
			"stock_qty":     {Name: "stock_qty", Type: FieldTypeMeasure, Description: "库存数(已聚合)"},
			"price":         {Name: "price", Type: FieldTypeMeasure, Description: "当前售价(已聚合)"},
			// 2026-09-02: restock 收编,LoadItemDict 从 products 拿 (HBPoS cube 已有这些字段)
			"clsno":         {Name: "clsno", Type: FieldTypeDimension, Description: "商品分类编码 (HBPoS item_clsno)"},
			"clsname":       {Name: "clsname", Type: FieldTypeDimension, Description: "商品分类名 (HBPoS item_clsname)"},
			"unit":          {Name: "unit", Type: FieldTypeDimension, Description: "计量单位 (HBPoS unit_no)"},
		},
		Sources: map[string]SourceMapping{
			"erp": {
				Cube: "products",
				FieldRefs: map[string]string{
					"barcode":       "products.barcode",
					"product_name":  "products.name",
					"supplier_id":   "products.main_supp_id",
					"supplier_name": "products.main_supp_name",
					"category":      "", // ERP 没分类字段
					"brand":         "", // ERP 没品牌字段
					"stock_qty":     "products.qty",
					"price":         "", // ERP 当前没暴露售价字段, 留空 (返回空)
					"clsno":         "",
					"clsname":       "",
					"unit":          "",
				},
			},
			"hbpos": {
				// cube 自己处理 LEFT JOIN t_bd_supcust_info(数据治理层职责)
				// 这里直接用物理字段 supplier_name
				Cube: "t_bd_item_info",
				FieldRefs: map[string]string{
					"barcode":       "t_bd_item_info.item_no", // HBPoS 无独立条码字段,用 item_no 兜底
					"product_name":  "t_bd_item_info.item_name",
					"supplier_id":   "t_bd_item_info.main_supcust",
					"supplier_name": "t_bd_item_info.supplier_name", // cube SQL 里已 LEFT JOIN
					"category":      "t_bd_item_info.item_clsno",    // 简化:取 clsno,完整取 clsname 需要子查询
					"brand":         "t_bd_item_info.item_brandname",
					// 2026-09-01: cube 加了 stock_qty measure (Scalar Subquery SUM t_im_branch_stock)
					//   扫商品场景不再显示"- (无库存字段)"
					"stock_qty":     "t_bd_item_info.stock_qty",
					"price":         "t_bd_item_info.sale_price", // 2026-08-31: 售价用 sale_price (原 price 是进价, sale_price 才是零售)
					"clsno":         "t_bd_item_info.item_clsno",
					"clsname":       "t_bd_item_info.item_clsname",
					"unit":          "t_bd_item_info.unit_no",
				},
			},
		},
	}
	r.entities["products"] = ent
}

func (r *Registry) registerSuppliers() {
	ent := &EntityMapping{
		Name:        "suppliers",
		Description: "供应商主表(跨数据源业务统一)",
		Fields: map[string]FieldDef{
			"supplier_id":   {Name: "supplier_id", Type: FieldTypeDimension, Description: "供应商 ID"},
			"supplier_name": {Name: "supplier_name", Type: FieldTypeDimension, Description: "供应商名"},
			"contact":       {Name: "contact", Type: FieldTypeDimension, Description: "联系人"},
			"phone":         {Name: "phone", Type: FieldTypeDimension, Description: "联系电话"},
		},
		Sources: map[string]SourceMapping{
			"erp": {
				// ERP 没独立的 supplier 表,数据从 products cube DISTINCT
				// 用 products cube,只查 supplier_id / supplier_name
				Cube: "products",
				FieldRefs: map[string]string{
					"supplier_id":   "products.main_supp_id",
					"supplier_name": "products.main_supp_name",
					"contact":       "", // ERP 无
					"phone":         "", // ERP 无
				},
			},
			"hbpos": {
				// HBPoS 有独立 suppliers cube
				Cube: "suppliers",
				FieldRefs: map[string]string{
					"supplier_id":   "suppliers.supcust_no",
					"supplier_name": "suppliers.sup_name",
					"contact":       "suppliers.sup_man",
					"phone":         "suppliers.sup_tel",
				},
			},
		},
	}
	r.entities["suppliers"] = ent
}

// availableDS helper
func availableDS(ent *EntityMapping) []string {
	out := make([]string, 0, len(ent.Sources))
	for k := range ent.Sources {
		out = append(out, k)
	}
	return out
}
