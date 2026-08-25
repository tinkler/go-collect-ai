package restock

import "encoding/json"

// RenderOfficeCard 办公室员工群卡片
//
// 模板: text_notice (无按钮)
// 内容: 完整信息(库存/销量/安全库存/建议/促销/触发原因)
// 触发场景:
//   - 短补/低于安全库存 → 推 P0/P1
//   - 员工反馈 SHORT → 推 P0
//   - task 静默升级 → 推 P0/P1
//   - task 关闭(入库)→ 推通知
func RenderOfficeCard(event string, sku *SkuSnapshot, qty int, taskID string) []byte {
	var title, desc string
	switch event {
	case "short_feedback":
		title = "⚠️ 员工反馈缺货"
		desc = "门店 " + sku.BranchNo + " · 需立即采购"
	case "below_safety":
		title = "📉 库存严重不足"
		desc = "门店 " + sku.BranchNo + " · 低于安全库存"
	case "restocked":
		title = "✅ 已入库"
		desc = "门店 " + sku.BranchNo + " · 自动关闭补货任务"
	case "acked":
		title = "👌 员工已补货"
		desc = "门店 " + sku.BranchNo + " · 等待库存增加确认"
	default:
		title = "📦 补货通知"
		desc = "门店 " + sku.BranchNo
	}

	card := map[string]any{
		"msgtype": "template_card",
		"template_card": map[string]any{
			"card_type": "text_notice",
			"source": map[string]any{
				"desc": "商超 AI 机器人",
			},
			"main_title": map[string]any{
				"title": title,
				"desc":  desc,
			},
			"horizontal_content_list": []map[string]any{
				{"keyname": "商品", "value": sku.ItemName},
				{"keyname": "当前库存", "value": itoa(sku.Stock) + " 件"},
				{"keyname": "昨日销量", "value": itoa(sku.YesterdaySales) + " 件"},
				{"keyname": "建议补货", "value": itoa(qty) + " 件"},
				{"keyname": "近期促销", "value": promoText(sku)},
				{"keyname": "供应商", "value": orDash(sku.SupplierName)},
				{"keyname": "触发原因", "value": event},
			},
		},
	}
	bs, _ := json.Marshal(card)
	return bs
}

func promoText(sku *SkuSnapshot) string {
	if sku.HasPromo7d {
		return "未来 7 天有促销"
	}
	return "无"
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
