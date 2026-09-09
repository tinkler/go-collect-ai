package freshcheck

// box_diff.go: W2.3 框内差值算法 (双轨互验的"框内轨")
//
// 业务:
//   R3 框内对账表 (freshcheck_box_recon) 每天每框每时段 1 行
//   计算: 框内物理流 (in/out) 跟 POS 销售数 的差异
//     box_loss_kg = in_total - out_total - pos_total
//   差异来源: 偷损 / 称差 / 漏登
//
// 算法:
//   box_loss = (Σin_weight) - (Σout_weight) - (Σpos_sale_weight_per_pool)
//
// 输入:
//   events: 框事件流 (in/out, 已按 event_time 排序)
//   posSalesKg: POS 系统返回的"该特价码销售汇总" (kg)
//               (暂由调用方从 cube 拉)
//
// 输出:
//   *types.BoxRecon (算法输出, 不带 ID/PeriodID/CreatedAt)

import (
	"sort"
	"time"
)

// ComputeBoxDiff 算框内差值 (W2 简化: 整窗口 1 段)
//   W3 可改成按天切 (每天 1 段)
//
//   box_loss = in - out - pos
//   box_loss_amt = box_loss * unit_price
//
// 业务:
//   - 偷损: box_loss 显著 > 0 (in 多, out 少, pos 更少) → 触发调查
//   - 称差: box_loss 接近 0.5 kg (合理范围, 不调查)
//   - 漏登: box_loss < 0 (out > in, 异常, 但代码有保护不出现负)
func ComputeBoxDiff(
	branchNo, poolCode string,
	unitPrice float64,
	from, to time.Time,
	events []*PoolEvent,
	posSalesKg float64,
) *BoxRecon {
	r := &BoxRecon{
		BranchNo:     branchNo,
		PoolCode:     poolCode,
		SegmentStart: from,
		SegmentEnd:   to,
	}
	for _, ev := range events {
		if ev.EventTime.Before(from) || ev.EventTime.After(to) {
			continue
		}
		switch ev.EventKind {
		case PoolEventIn:
			r.InTotalKg += ev.WeightKg
		case PoolEventOut:
			r.OutTotalKg += ev.WeightKg
			if ev.OutDestination != nil {
				switch *ev.OutDestination {
				case DestSoldOut:
					r.OutSoldOutKg += ev.WeightKg
				case DestSpoiled:
					r.OutSpoiledKg += float64(ev.PieceCount) / 1000.0
				case DestReturnShelf:
					r.OutReturnKg += ev.WeightKg
				case DestDowngrade:
					r.OutDowngradeKg += ev.WeightKg
				}
			}
		}
	}
	r.PosTotalKg = posSalesKg
	r.BoxLossKg = r.InTotalKg - r.OutTotalKg - r.PosTotalKg
	r.BoxLossAmt = r.BoxLossKg * unitPrice

	// 阈值判断 (可配, 默认 0.5 kg)
	diff := r.BoxLossKg
	if diff < 0 {
		diff = -diff
	}
	if diff > 0.5 {
		r.NeedsInvestigate = true
	}
	return r
}

// ComputeBoxDiffSegments 算多段 (W3 用, 每天 1 段)
//   segmentDefs: [{start, end}] 多段
//   段内独立算 in/out, 最后合计
func ComputeBoxDiffSegments(
	branchNo, poolCode string,
	unitPrice float64,
	segmentDefs []struct{ Start, End time.Time },
	events []*PoolEvent,
	posSalesKgBySeg []float64,
) []*BoxRecon {
	if len(segmentDefs) != len(posSalesKgBySeg) {
		return nil
	}
	out := make([]*BoxRecon, 0, len(segmentDefs))
	for i, seg := range segmentDefs {
		r := ComputeBoxDiff(branchNo, poolCode, unitPrice, seg.Start, seg.End, events, posSalesKgBySeg[i])
		// sort.Slice 是 no-op (1 段) 但保持一致
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].SegmentStart.Before(out[j].SegmentStart)
	})
	return out
}
