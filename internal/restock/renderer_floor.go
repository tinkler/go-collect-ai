package restock

import (
	"encoding/json"
	"fmt"
	"time"
)

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

// RenderFloorCardAfterConfirm 员工点完按钮后,卡片 in-place 更新为"无按钮的已确认状态"
//   kind: "DONE" | "SHORT"
//   用于 aibot_respond_update_msg 帧(5 秒内通过长连接回发)
func RenderFloorCardAfterConfirm(sku *SkuSnapshot, qty int, kind string) []byte {
	var title string
	var desc string
	switch kind {
	case FeedbackDone:
		title = "✅ 已补货"
		desc = fmt.Sprintf("%s · 已确认", sku.ItemName)
	case FeedbackShort:
		title = "⚠️ 已上报缺货"
		desc = fmt.Sprintf("%s · 等采购", sku.ItemName)
	default:
		title = "已处理"
		desc = sku.ItemName
	}

	card := map[string]any{
		"msgtype": "template_card",
		"template_card": map[string]any{
			"card_type": "text_notice", // 改成无按钮的纯通知卡
			"source": map[string]any{
				"desc": "商超 AI 机器人",
			},
			"main_title": map[string]any{
				"title": title,
				"desc":  desc,
			},
			"horizontal_content_list": []map[string]any{
				{"keyname": "建议补货", "value": itoa(qty) + " 件"},
				{"keyname": "处理时间", "value": time.Now().Format("15:04")},
			},
		},
	}
	bs, _ := json.Marshal(card)
	return bs
}
