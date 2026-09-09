package freshcheck

// allocate.go: W3.3 多筐串联分摊 (Step 5)
//
// 业务规则 (docs/freshcheck-settlement.md §六):
//   1. 多筐串联: 按 unit_price DESC (priority_rank=0 最高, 价高先)
//   2. 主算法 (框内差值法): weight = (in - out - spoiled) × confidence
//      - 退化条件: sumWeights <= 0 → 切到备算法
//   3. 备算法 (倒挤回退): weight = remaining[item] × confidence
//   4. 物理约束: 单 SKU 总分摊 ≤ remaining[itemNo]
//      - 多次重分配 (5 iter 上限), excess 按权重比例再分给未满 SKU
//   5. 串联扣减: 每框分摊后 remaining[item] -= alloc[item]
//   6. C1 饱和度: |total_alloc - total_pos| / total_pos ≤ 阈值 (默认 15%)
//
// 简化 (W3.3 范围):
//   - 跨筐降级 (out_destination=downgrade): W3.6 才做 (需虚拟入框事件)
//   - 写 freshcheck_alloc 表: W3.4 编排才做
//   - C1 告警: W3.5 校验才做
//
// 不依赖 LLM, 不查 PG/cube, 纯函数, 易单测.

import (
	"fmt"
)

// DefaultMaxIterations 物理约束重分配上限
const DefaultMaxIterations = 5

// ComputeAllocate 算整期多筐串联分摊
//   输入: AllocateInput (调用方准备)
//   输出: AllocateResult
//   错误: 仅参数错误 (Pools 跟 PoolSegments 数量不匹配)
func ComputeAllocate(in AllocateInput) (*AllocateResult, error) {
	if len(in.Pools) != len(in.PoolSegments) {
		return nil, fmt.Errorf("Pools(%d) 跟 PoolSegments(%d) 数量不匹配", len(in.Pools), len(in.PoolSegments))
	}
	if in.MaxIterations <= 0 {
		in.MaxIterations = DefaultMaxIterations
	}

	// 1. 初始化 remaining[item] = pool_adjustable
	remaining := makeRemainByItem(in.BackflushItems)

	res := &AllocateResult{
		BranchNo: in.BranchNo,
		PeriodID: in.PeriodID,
		Allocs:   []*AllocItem{},
	}

	// 2. 串联分摊
	for i, pool := range in.Pools {
		seg := in.PoolSegments[i]
		segAllocs := allocateSegment(pool, seg, remaining, in.MaxIterations)
		res.Allocs = append(res.Allocs, segAllocs...)

		// 串联扣减: 把本段分摊从 remaining 里扣
		for _, a := range segAllocs {
			remaining[a.ItemNo] -= a.AllocQty
			if remaining[a.ItemNo] < 0 {
				remaining[a.ItemNo] = 0
			}
		}
	}

	// 3. 汇总
	res.Summary = summarize(in.PoolSegments, res.Allocs)
	return res, nil
}

// allocateSegment 单 segment 分摊 (主算法 + 备算法 + 物理约束)
//   业务:
//     1. 算 weights (主: 框内差值; 备: 倒挤剩余)
//     2. initial_alloc[pool][seg][item] = pos_qty × w / sumWeights
//     3. 物理约束: 5 次重分配
func allocateSegment(pool *PoolCode, seg *PoolSegment, remaining map[string]float64, maxIter int) []*AllocItem {
	// 1. 主算法: 框内差值
	weights := make(map[string]float64)
	for _, it := range seg.Items {
		if remaining[it.ItemNo] <= 0 {
			continue // 已分完, 跳过
		}
		diff := it.InWeightKg - it.OutWeightKg - it.SpoiledWeightKg
		if diff <= 0 {
			continue // 净出, 跳过
		}
		weights[it.ItemNo] = diff * it.ConfidenceFactor
	}
	method := "box_diff"
	sumW := sumMap(weights)
	if sumW <= 0 {
		// 退化到备算法
		method = "backflush_fallback"
		weights = make(map[string]float64)
		for _, it := range seg.Items {
			if remaining[it.ItemNo] <= 0 {
				continue
			}
			weights[it.ItemNo] = remaining[it.ItemNo] * it.ConfidenceFactor
		}
		sumW = sumMap(weights)
	}

	out := []*AllocItem{}
	if sumW <= 0 {
		// 还是 0, 没东西可分
		return out
	}

	// 2. 初步分摊
	alloc := make(map[string]float64)
	for itemNo, w := range weights {
		alloc[itemNo] = seg.PoolPosQty * w / sumW
	}

	// 3. 物理约束: 多次重分配
	alloc = redistribute(alloc, weights, remaining, maxIter)

	// 4. 装 AllocItem
	for itemNo, qty := range alloc {
		// 找 ItemName
		var itemName string
		for _, it := range seg.Items {
			if it.ItemNo == itemNo {
				itemName = it.ItemName
				break
			}
		}
		// 金额按特价单价 (简化: 都用 pool.UnitPrice, 后续 W3.4 细化)
		amt := qty * pool.UnitPrice
		out = append(out, &AllocItem{
			PoolCode:     seg.PoolCode,
			SegmentStart: seg.SegmentStart,
			SegmentEnd:   seg.SegmentEnd,
			ItemNo:       itemNo,
			ItemName:     itemName,
			AllocQty:     qty,
			AllocAmt:     amt,
			Method:       method,
			Confidence:   1.0, // 简化: 暂全 high (W3.6 区分)
			NeedsReview:  false,
		})
	}
	return out
}

// redistribute 物理约束: 单 SKU 分摊 ≤ remaining[item], 多余重分配
//   多次重分配 (maxIter 上限, 避免极端场景死循环)
//   业务: 高价框分摊可能把某 SKU 一次分完, 多余 excess 按权重比例分给未满的
func redistribute(alloc, weights, remaining map[string]float64, maxIter int) map[string]float64 {
	for iter := 0; iter < maxIter; iter++ {
		excess := 0.0
		for itemNo, q := range alloc {
			if q > remaining[itemNo] {
				excess += q - remaining[itemNo]
				alloc[itemNo] = remaining[itemNo]
			}
		}
		if excess == 0 || excess < 1e-9 {
			break
		}
		// 重分配给未满的 SKU (alloc[itemNo] < remaining[itemNo])
		others := 0.0
		for itemNo, q := range alloc {
			if q < remaining[itemNo] {
				others += weights[itemNo]
			}
		}
		if others <= 0 {
			// 没有未满的, 强制 break (excess 浪费)
			break
		}
		for itemNo, q := range alloc {
			if q < remaining[itemNo] {
				alloc[itemNo] += excess * weights[itemNo] / others
			}
		}
	}
	return alloc
}

// makeRemainByItem 从 BackflushItem 列表建 remaining map
func makeRemainByItem(items []*BackflushItem) map[string]float64 {
	m := make(map[string]float64)
	for _, it := range items {
		m[it.ItemNo] = it.PoolAdjustable
	}
	return m
}

// sumMap 求和
func sumMap(m map[string]float64) float64 {
	s := 0.0
	for _, v := range m {
		s += v
	}
	return s
}

// summarize 算整期汇总 (含 C1 饱和度)
//   saturation = |total_alloc - total_pos| / total_pos
//   meets_c1   = saturation <= c1_pool_saturation_pct / 100  (默认 15%)
func summarize(segs []*PoolSegment, allocs []*AllocItem) AllocateSummary {
	s := AllocateSummary{
		PoolsCount:    len(segs),
		SegmentsCount: len(segs),
		AllocsCount:   len(allocs),
	}
	for _, a := range allocs {
		s.TotalAllocQty += a.AllocQty
	}
	for _, seg := range segs {
		s.TotalPosQty += seg.PoolPosQty
	}
	if s.TotalPosQty > 0 {
		s.SaturationPct = absFloat(s.TotalAllocQty-s.TotalPosQty) / s.TotalPosQty * 100.0
	}
	// 阈值默认 15% (W3.5 才读 c1_pool_saturation_pct 动态, 这里写死 15.0)
	s.MeetsC1 = s.SaturationPct <= 15.0
	return s
}

func absFloat(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
