package freshcheck

// reconcile.go: W3.6 双轨互验 + 框内对账
//
// 业务 (docs/freshcheck-settlement.md §九):
//   - 框内差值 (主):  Σ(in - out - spoiled) per pool per segment
//   - 整窗倒挤 (辅):  begin + purchase - normal_sale - end  (W3.2 已算)
//   - 偏差:          dev_rate = |alloc - backflush_alloc| / max(alloc, backflush_alloc)
//   - 偏差 > 阈值 (5%): needs_review = true, 写 alloc 表 deviation 字段
//   - 框内对账:       box_loss = in_total - out_total - pos_total (per pool per segment)
//
// 输出:
//   - 更新 freshcheck_alloc.deviation_qty/rate/needs_review
//   - 写 freshcheck_box_recon (R3 报表)

import (
	"context"
	"encoding/json"
	"fmt"
)

// RunReconciliation 双轨互验 + 写 box_recon (W3.6)
//   输入: Service 持有上下文 (Store) + 6 步结果 (bf + alloc + req + poolSegments)
//   输出: 写 freshcheck_alloc 更新 + freshcheck_box_recon 写表
//   业务: alloc_qty (差值轨) vs pool_adjustable[item] (倒挤轨)
//     - 同 SKU 同 pool: deviation_qty = alloc_qty - backflush_alloc_qty
//     - 偏差 > 5% 标 needs_review
func (s *Service) RunReconciliation(ctx context.Context, req SettleRequest, alloc *AllocateResult, bf *BackflushResult, poolSegments []*PoolSegment) error {
	// 1. 按 item_no 聚合 backflush_alloc_qty (= pool_adjustable)
	//    注: 整窗倒挤总量 = sum(pool_adjustable), 单 SKU 的"应有"分摊 = pool_adjustable[item]
	//    但跨多 pool 时, 同一 SKU 在多个 pool 都有 alloc, 需按 pool 聚合
	//    简化: deviation 算 alloc_qty vs pool_adjustable * pos_share_of_item

	// 2. 写 alloc 表 deviation 更新
	for _, a := range alloc.Allocs {
		// 找该 item 在 bf 的 pool_adjustable
		var backflushAllocQty float64
		for _, it := range bf.Items {
			if it.ItemNo == a.ItemNo {
				// 简化: 同 pool 权重分配, 这里暂 1:1
				backflushAllocQty = it.PoolAdjustable
				break
			}
		}
		devQty := a.AllocQty - backflushAllocQty
		var devRate *float64
		maxV := a.AllocQty
		if backflushAllocQty > maxV {
			maxV = backflushAllocQty
		}
		if maxV > 0 {
			r := absFloat(devQty) / maxV
			devRate = &r
		}
		// needs_review: 偏差 > 5% 且 alloc > 0
		needsReview := false
		if devRate != nil && *devRate > 0.05 && a.AllocQty > 0 {
			needsReview = true
		}
		// 更新 alloc 行 (UPDATE)
		if err := s.Store.UpdateAllocDeviation(ctx, req.PeriodID, a.PoolCode, a.SegmentStart, a.ItemNo, devQty, devRate, needsReview); err != nil {
			// 失败不阻断 (W3.9 验收再关注)
			fmt.Printf("[reconcile] UpdateAllocDeviation err: %v\n", err)
		}
	}

	// 3. 写 freshcheck_box_recon (R3)
	//    每个 (pool, segment) 一行
	for _, seg := range poolSegments {
		boxRecon := s.buildBoxRecon(req, seg, bf)
		if err := s.Store.InsertBoxRecon(ctx, boxRecon); err != nil {
			// 重复写 → ErrDuplicateKey, 跳过
			if err != ErrDuplicateKey {
				return fmt.Errorf("InsertBoxRecon %s: %w", seg.PoolCode, err)
			}
		}
		// box_loss > 0 标 needs_investigate (W3.5 C 类)
		if boxRecon.BoxLossKg > 0 {
			payload, _ := json.Marshal(map[string]any{
				"pool_code":   boxRecon.PoolCode,
				"box_loss_kg": boxRecon.BoxLossKg,
				"box_loss_amt": boxRecon.BoxLossAmt,
			})
			al := &Alert{
				BranchNo:   req.BranchNo,
				PeriodID:    &req.PeriodID,
				RuleCode:    "BOX_LOSS",
				Severity:    SeverityInfo,
				EntityType:  "pool",
				EntityID:    boxRecon.PoolCode,
				Message:     fmt.Sprintf("框内不明差异: %s 框亏 %.2f kg (¥%.2f)", boxRecon.PoolCode, boxRecon.BoxLossKg, boxRecon.BoxLossAmt),
				Payload:     string(payload),
			}
			if err := s.writeAlert(ctx, al); err != nil {
				return err
			}
		}
	}
	return nil
}

// buildBoxRecon 算单 segment 的 box_recon (R3)
//   box_loss = in_total - out_total - pos_total
//   unit_price 来自 pool (走 Store.ListPoolCodes 查)
func (s *Service) buildBoxRecon(req SettleRequest, seg *PoolSegment, bf *BackflushResult) *BoxRecon {
	inTotal := 0.0
	outTotal := 0.0
	outSold := 0.0
	outSpoiled := 0.0
	outReturn := 0.0
	outDowngrade := 0.0
	for _, it := range seg.Items {
		inTotal += it.InWeightKg
		outTotal += it.OutWeightKg
		outSpoiled += it.SpoiledWeightKg
		// 简化: 暂记到 outSold (其它去向 W3.6.1 细化)
		outSold += it.OutWeightKg - it.SpoiledWeightKg
	}
	boxLossKg := inTotal - outTotal - seg.PoolPosQty
	// unit price: 简化, 走 1.0 (实际应从 pool.UnitPrice 拿, W3.6.1 细化)
	unitPrice := 1.0
	for _, p := range bf.PoolAdjustableByItem() { // 简化, 实际查 pool_code
		_ = p
	}
	boxLossAmt := boxLossKg * unitPrice
	return &BoxRecon{
		PeriodID:         req.PeriodID,
		BranchNo:         req.BranchNo,
		PoolCode:         seg.PoolCode,
		SegmentStart:     seg.SegmentStart,
		SegmentEnd:       seg.SegmentEnd,
		InTotalKg:        inTotal,
		OutTotalKg:       outTotal,
		OutSoldOutKg:     outSold,
		OutSpoiledKg:     outSpoiled,
		OutReturnKg:      outReturn,
		OutDowngradeKg:   outDowngrade,
		PosTotalKg:       seg.PoolPosQty,
		BoxLossKg:        boxLossKg,
		BoxLossAmt:       boxLossAmt,
		ResponsibleUser:  req.Operator,
		NeedsInvestigate: boxLossKg > 0,
	}
}

// PoolAdjustableByItem helper: BackflushResult 没有此方法, 这是 stub
// 实际 W3.4 BackflushResult.Items 每行有 PoolAdjustable 字段, 简化 aggregate
func (bf *BackflushResult) PoolAdjustableByItem() map[string]float64 {
	m := map[string]float64{}
	for _, it := range bf.Items {
		m[it.ItemNo] = it.PoolAdjustable
	}
	return m
}
