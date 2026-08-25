package restock

import "encoding/json"

// RenderFloorCard 卖场员工群卡片
//
// 模板: button_interaction
// 内容: 极简(只显示当前库存)
// 按钮: "✅ 已补货" / "❌ 缺货/异常"
// 按钮 key 格式: "DONE|<task_id>" / "SHORT|<task_id>"
func RenderFloorCard(sku *SkuSnapshot, qty int, taskID string) []byte {
	card := map[string]any{
		"msgtype": "template_card",
		"template_card": map[string]any{
			"card_type": "button_interaction",
			"source": map[string]any{
				"desc": "商超 AI 机器人",
			},
			"main_title": map[string]any{
				"title": "🛒 补货 · " + sku.ItemName,
				"desc":  "门店 " + sku.BranchNo + " · 建议补 " + itoa(qty),
			},
			"horizontal_content_list": []map[string]any{
				{"keyname": "当前库存", "value": itoa(sku.Stock) + " 件"},
			},
			"task_id": taskID,
			"button_list": []map[string]any{
				{
					"text":  "✅ 已补货",
					"style": 1,
					"key":   "DONE|" + taskID,
				},
				{
					"text":  "❌ 缺货/异常",
					"style": 2,
					"key":   "SHORT|" + taskID,
				},
			},
		},
	}
	bs, _ := json.Marshal(card)
	return bs
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := [20]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
