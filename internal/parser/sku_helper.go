package parser

import (
	"fmt"

	"github.com/tinkler/collect-ai/internal/model"
)

// toSkuRecords 把 agent.LoadSupplierSkus 返回的物理字段 map 列表
// 转成 model.SkuRecord(给 matcher 用)
//
//   map key 形如 "products.main_supp_name" / "products.barcode" / "products.qty" / ...
//   这层只做"物理字段名 → SkuRecord 字段"的固定转换
//   (业务层 collect-ai handler 不在这里做)
func toSkuRecords(rows []map[string]any) []model.SkuRecord {
	out := make([]model.SkuRecord, 0, len(rows))
	for _, r := range rows {
		rec := model.SkuRecord{
			Barcode:      asString(r, "products.barcode"),
			Name:         asString(r, "products.name"),
			MainSuppId:   asString(r, "products.main_supp_id"),
			MainSuppName: asString(r, "products.main_supp_name"),
			SrcSheet:     asString(r, "products.src_sheet"),
		}
		if v, ok := r["products.qty"]; ok && v != nil {
			switch x := v.(type) {
			case float64:
				f := x
				rec.StockQty = &f
			case int:
				f := float64(x)
				rec.StockQty = &f
			}
		}
		out = append(out, rec)
	}
	return out
}

func asString(m map[string]any, key string) string {
	if v, ok := m[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprintf("%v", v)
	}
	return ""
}
