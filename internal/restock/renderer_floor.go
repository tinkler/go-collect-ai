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

// ============== 新版陈列补货卡片 (2026-08-30) ==============
//   与 RenderFloorCard 的差别:
//     - 标题改为 "陈列补货建议"
//     - 内容: 商品名 | 销售量 | 建议补 | 当前库存 (vertical_content_list)
//     - 按钮 eventKey 全部小写 + 冒号分隔 (新协议):
//         "display-<branch>-<item>:short" / ":done"
//     - 配套 H5 页面用同一 eventKey, 企微按钮和 H5 走同一处理路径
//   保留 RenderFloorCard 不动, 用于 fallback / 旧群组

// RenderFloorCardDisplay 新版陈列补货建议卡 (button_interaction)
//   branch/item 必填 — 拼到 eventKey 前缀
//   salesQty = 今日累计销售 (来自 cube, 0 也显)
//   suggestQty / stock 为必填显示字段
func RenderFloorCardDisplay(branch, itemNo, itemName string, salesQty, suggestQty, stock int) []byte {
	eventBase := "display-" + branch + "-" + itemNo
	card := map[string]any{
		"msgtype": "template_card",
		"template_card": map[string]any{
			"card_type": "button_interaction",
			"source": map[string]any{
				"desc": "商超 AI 机器人",
			},
			"main_title": map[string]any{
				"title": "🛒 陈列补货建议 · " + itemName,
				"desc":  "门店 " + branch + " · 建议补 " + itoa(suggestQty) + " 件",
			},
			"vertical_content_list": []map[string]any{
				{"title": "商品", "desc": itemName},
				{"title": "今日销售", "desc": itoa(salesQty) + " 件"},
				{"title": "建议补货", "desc": itoa(suggestQty) + " 件"},
				{"title": "当前库存", "desc": itoa(stock) + " 件"},
			},
			"button_list": []map[string]any{
				{
					"text":  "缺货",
					"style": 2,
					"key":   eventBase + ":short",
				},
				{
					"text":  "已完成",
					"style": 1,
					"key":   eventBase + ":done",
				},
			},
		},
	}
	bs, _ := json.Marshal(card)
	return bs
}

// RenderFloorCardAfterConfirmDisplay 员工点完按钮后, 卡片 in-place 更新
//   kind: FeedbackDone | FeedbackShort (新协议用小写值)
//   salesQty / suggestQty / stock 都传入, 显示完整状态
func RenderFloorCardAfterConfirmDisplay(branch, itemNo, itemName string, salesQty, suggestQty, stock int, kind string) []byte {
	var title, desc, content string
	switch kind {
	case FeedbackDone:
		title = "✅ 已完成补货"
		desc = itemName + " · 已确认"
		content = "本轮陈列补货已完成, 等下次 tick"
	case FeedbackShort:
		title = "⚠️ 已上报缺货"
		desc = itemName + " · 等采购"
		content = "已加入采购计划, 等供应商送达"
	default:
		title = "已处理"
		desc = itemName
		content = ""
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
			"vertical_content_list": []map[string]any{
				{"title": "今日销售", "desc": itoa(salesQty) + " 件"},
				{"title": "建议补货", "desc": itoa(suggestQty) + " 件"},
				{"title": "当前库存", "desc": itoa(stock) + " 件"},
				{"title": "状态", "desc": content},
			},
		},
	}
	bs, _ := json.Marshal(card)
	return bs
}
