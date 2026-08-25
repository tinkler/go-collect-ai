package restock

import (
	"fmt"
	"math"
	"time"
)

// ShouldRestock 决策:这个 SKU 现在该不该补?补多少?什么优先级?
//
// 输入: 实时快照 + 已存在的 open task(可能 nil) + 配置 + 当前时间
// 输出: need / qty / prio / reason
//
// 规则编号(对应最初设计文档):
//   R1: 6h 内已推送过 → 只更新数据,不触发新推送 (need=false 但调方仍写库)
//   R2: 24h 内员工反馈 DONE 且 24h 内有销售 → 不补(说明补货后真在卖)
//   R2b: 24h 内员工反馈 DONE 但 24h 内无销售 → 陈列满,不补
//   R3: 24h 内员工反馈 SHORT → 写入 need_purchase(由 caller 调 UpsertNeedPurchase)
//   R4: 库存跌到 ROP(Reorder Point)= max(daily_avg×1.5, 5)→ 触发
//   R5: 建议补货量 = max(OUT_LEVEL - stock, daily_avg),按 supplier fill_rate 放大
func ShouldRestock(
	sku *SkuSnapshot,
	existingTask *Task,
	hasRecentDoneWithSales, hasRecentDoneNoSales, hasRecentShort bool,
	cfg *RestockConfig,
	now time.Time,
) (need bool, qty int, priority, reason string) {

	// 加权日均
	dailyAvg := float64(sku.YesterdaySales)*cfg.WYesterday +
		float64(sku.SevenDayAvg)*cfg.WSevenDay +
		float64(sku.ThirtyDayAvg)*cfg.WThirtyDay
	if dailyAvg < 0.1 {
		dailyAvg = 0.1
	}

	rop := math.Max(dailyAvg*cfg.ROPFactor, float64(cfg.SafetyMin))

	// R1: 6h 内已推送过
	if existingTask != nil && existingTask.LastPushAt != nil &&
		now.Sub(*existingTask.LastPushAt) < 6*time.Hour {
		return false, 0, existingTask.Priority,
			fmt.Sprintf("R1_suppress_within_6h (last_push=%s)", existingTask.LastPushAt.Format("15:04"))
	}

	// R2 / R2b
	if hasRecentDoneWithSales {
		return false, 0, "", "R2_done_with_sales_in_24h"
	}
	if hasRecentDoneNoSales {
		return false, 0, "", "R2b_silent_full_shelf_24h_no_sales"
	}

	// R4: 库存未触底
	if float64(sku.Stock) > rop {
		return false, 0, "", fmt.Sprintf("above_rop(stock=%d rop=%.1f)", sku.Stock, rop)
	}

	// 优先级
	daysOfStock := float64(sku.Stock) / dailyAvg
	priority = computePriority(daysOfStock)

	// 目标量 OUT
	outLevel := dailyAvg * float64(cfg.OUTDays)
	if sku.HasPromo7d {
		outLevel *= cfg.OUTPromoBoost
	}

	// 基础建议量:补到 OUT 所需量
	target := outLevel - float64(sku.Stock)
	if target < dailyAvg {
		target = dailyAvg // 至少补一天量
	}

	// R5: 按 supplier fill_rate 放大(由 caller 在传回来前已查)
	// 这里仅返回 base 值,LLM planner 会再覆盖

	qty = int(math.Ceil(target))
	if qty < 1 {
		qty = 1
	}

	reason = fmt.Sprintf("R4_below_rop(stock=%d rop=%.1f daily=%.1f days=%.2f)",
		sku.Stock, rop, dailyAvg, daysOfStock)
	return true, qty, priority, reason
}

func computePriority(daysOfStock float64) string {
	switch {
	case daysOfStock < 0.5:
		return PriorityP0
	case daysOfStock < 1.5:
		return PriorityP1
	case daysOfStock < 3.0:
		return PriorityP2
	default:
		return PriorityP3
	}
}

// ShouldEscalate 静默升级:根据 task 持续 open 时长提级
//   P2 → P1  (默认 24h)
//   P1 → P0  (默认 12h)
func ShouldEscalate(task *Task, cfg *RestockConfig, now time.Time) (newPrio string, escalated bool) {
	if task.Status != TaskStatusOpen {
		return task.Priority, false
	}
	openSince := task.FirstPushAt
	if openSince == nil {
		return task.Priority, false
	}
	elapsed := now.Sub(*openSince)
	switch task.Priority {
	case PriorityP2:
		if elapsed > time.Duration(cfg.EscalateP2ToP1Hours)*time.Hour {
			return PriorityP1, true
		}
	case PriorityP1:
		if elapsed > time.Duration(cfg.EscalateP1ToP0Hours)*time.Hour {
			return PriorityP0, true
		}
	}
	return task.Priority, false
}
