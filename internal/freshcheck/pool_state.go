package freshcheck

// pool_state.go: W2.2 池状态重建 (纯函数, 内存计算, 不查 DB)
//
// 业务:
//   任意时刻 t 某特价框 pool_code 里, 哪些 SKU 还有多少重量 = 在框集合
//   用法: 早晨出框检查 (W2.5) / 框内差值算法 (W2.3) / Step 5 池内分摊 (W3)
//
// 算法:
//   1. 拉窗口 [t-24h, t] 内所有 in/out 事件
//   2. 按时间序遍历, 维护 in-累计 / out-累计 / spoiled-累计
//   3. t 时刻某 SKU 在框重量 = in_total - out_total
//   4. 跨筐降级: out destination=downgraded 时, 同时开目标 pool 虚拟入框
//   5. 漏录: out 早于 in → HasOpenOut=true
//
// 不查 DB, 输入用 PoolEvent 列表. 调用方负责从 store 拉数据.

import (
	"sort"
	"time"
)

// RebuildPoolState 重建某时刻 t 某框的池状态
//   events: 该框窗口 [-24h, t] 内所有事件, 已按 event_time 排序
//   poolName: 调用方注入 (从 freshcheck_pool_code 查), 可空
//   业务规则:
//     - in 事件: weight 累加到 InTotal
//     - out 事件:
//         - sold_out / return_to_shelf: weight 累加到 OutTotal
//         - spoiled: piece_count 字段临时存 spoiled 重量 (业务独立于剩余重量, W3 改 schema)
//         - downgraded: 同 sold_out (跨筐降级 in 走目标 pool 单独聚合)
func RebuildPoolState(poolCode, poolName string, at time.Time, events []*PoolEvent) PoolState {
	state := PoolState{
		PoolCode: poolCode,
		PoolName: poolName,
		At:       at,
	}
	type key struct{ item string }
	acc := map[key]*PoolStateItem{}

	for _, ev := range events {
		if ev.EventTime.After(at) {
			continue // 跳过未来事件
		}
		k := key{ev.ItemNo}
		item, exists := acc[k]
		if !exists {
			item = &PoolStateItem{ItemNo: ev.ItemNo, Confidence: ConfidenceHigh}
			acc[k] = item
		}
		item.LastEventTime = ev.EventTime
		if ev.Confidence == ConfidenceLow {
			item.Confidence = ConfidenceLow
		}

		switch ev.EventKind {
		case PoolEventIn:
			item.InWeightKg += ev.WeightKg
		case PoolEventOut:
			item.OutWeightKg += ev.WeightKg
			// spoiled 报损: piece_count 字段临时复用, 单位 0.001kg
			// W3 应改 schema 加 spoiled_weight_kg 字段
			if ev.OutDestination != nil && *ev.OutDestination == DestSpoiled {
				item.SpoiledWeightKg += float64(ev.PieceCount) / 1000.0
			}
		}
	}

	// 计算 current = in - out (不能为负)
	for _, item := range acc {
		item.CurrentWeightKg = item.InWeightKg - item.OutWeightKg
		if item.CurrentWeightKg < 0 {
			item.CurrentWeightKg = 0
		}
		state.Items = append(state.Items, *item)
		state.InTotalKg += item.InWeightKg
		state.OutTotalKg += item.OutWeightKg
		state.SpoiledTotalKg += item.SpoiledWeightKg
	}
	state.CurrentTotalKg = state.InTotalKg - state.OutTotalKg
	if state.CurrentTotalKg < 0 {
		state.CurrentTotalKg = 0
	}

	// 按 item_no 排序
	sort.Slice(state.Items, func(i, j int) bool {
		return state.Items[i].ItemNo < state.Items[j].ItemNo
	})

	return state
}

// HasOutBeforeIn 检测某 SKU 是否有 out 在 in 之前 (漏录标记)
//   events: 该 SKU 全部事件, 按时间序
//   返回: true 表示存在 out 在 in 之前 → 漏录, 需补录
func HasOutBeforeIn(events []*PoolEvent) bool {
	var firstIn, firstOut *time.Time
	for _, ev := range events {
		if ev.EventKind == PoolEventIn {
			t := ev.EventTime
			if firstIn == nil || t.Before(*firstIn) {
				firstIn = &t
			}
		}
		if ev.EventKind == PoolEventOut {
			t := ev.EventTime
			if firstOut == nil || t.Before(*firstOut) {
				firstOut = &t
			}
		}
	}
	if firstIn == nil && firstOut != nil {
		return true // 完全没有 in, 只有 out
	}
	if firstIn != nil && firstOut != nil && firstOut.Before(*firstIn) {
		return true
	}
	return false
}

// DetectMissingInAt 简化版漏录检测 (复用于 HTTP handler)
//   events: 某 pool 全事件, 按 event_time 序
//   at: 出框检查时刻
//   返回: 在 at 之前出现 out 但 in 计数为 0 的 item 列表
func DetectMissingInAt(events []*PoolEvent, at time.Time) []MissingRec {
	hasIn := map[string]bool{}
	hasOutBeforeAt := map[string]bool{}
	for _, ev := range events {
		if ev.EventTime.After(at) {
			continue
		}
		switch ev.EventKind {
		case PoolEventIn:
			hasIn[ev.ItemNo] = true
		case PoolEventOut:
			if !ev.EventTime.After(at) {
				hasOutBeforeAt[ev.ItemNo] = true
			}
		}
	}
	out := []MissingRec{}
	for item := range hasOutBeforeAt {
		if !hasIn[item] {
			out = append(out, MissingRec{
				ItemNo:           item,
				SuggestedInWeight: 0, // 调用方根据 out 重量填充
				SuggestedInTime:   at.Add(-12 * time.Hour),
			})
		}
	}
	return out
}
