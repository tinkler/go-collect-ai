// Package parser - sku_helper.go 已废弃 (Phase A, 2026-09-02)
//
// 历史: 把 agent.LoadSupplierSkus 返的物理字段 map 转成 model.SkuRecord
// 现状: handler.loadSupplierSkusBiz 直接用业务字段返 []model.SkuRecord
//      Orchestrator 直接用 skus 传给 matcher, 不再需要这层翻译
//
// Phase C 整体清理时直接删除整个文件
package parser
