package freshcheck

// settle.go: W3.4 6 步流程编排
//
// 业务规则 (docs/freshcheck-settlement.md §一):
//   Step 1: 窗口确定 (由调用方传入 from/to, 这里只校验)
//   Step 2: 流水切窗 (拉 cube sales/purchase 到 window + 本地 period_stock + pool_event)
//   Step 3: 倒挤 (W3.2)
//   Step 4: 报损剥离 (W3.2 包含)
//   Step 5: 池内分摊 (W3.3 多筐串联, 主算法 + 物理约束)
//   Step 6: 毛利计算 + 守恒 (本文件) + 写 freshcheck_settlement/alloc
//
// 简化 (W3.4 范围):
//   - box_recon 写入 + C1 校验: W3.5 校验才做
//   - alert 写入 (C1-C8 校验告警): W3.5 才做
//   - 双轨互验 + 校准: W3.6 才做
//   - 跨筐降级: 暂不处理 (W3.6)
//   - 事务: 暂逐条 Insert (失败不阻断, 由幂等键防重)
//
// 幂等键: hash(branch + track + period_id + operator) — 重复调用返已有结果

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"time"
)

// Service freshcheck 业务编排层 (W3.4)
//   依赖: Store (PG 派生表写) + CubeQuerier (cube 数据拉)
type Service struct {
	Store *Store
	Cube  *CubeQuerier
}

// NewService 构造
func NewService(store *Store, cube *CubeQuerier) *Service {
	return &Service{Store: store, Cube: cube}
}

// RunSettlement 6 步编排 (W3.4 核心入口)
//   输入: SettleRequest (W1.4 已建)
//   输出: SettleResult (W1.4 已建) + 写 freshcheck_settlement/alloc 表
//   错误: cube 拉取失败 / 写表失败
func (s *Service) RunSettlement(ctx context.Context, req SettleRequest) (*SettleResult, error) {
	// Step 1: 窗口确定
	windowStart, windowEnd, windowDays := determineWindow(req)
	if !windowEnd.After(windowStart) {
		return nil, fmt.Errorf("window_end 必须 > window_start")
	}

	// Step 2: 流水切窗 — 拉数据准备
	bfInput, err := BuildBackflushInput(ctx, s.Cube, s.Store, req.BranchNo, req.PeriodID, 0, windowStart, windowEnd)
	if err != nil {
		return nil, fmt.Errorf("step 2 BuildBackflushInput: %w", err)
	}
	// pool_pos (从 cube 按 pool_code 聚合)
	poolSales, err := s.Cube.PoolSalesInWindow(ctx, req.BranchNo, windowStart, windowEnd)
	if err != nil {
		return nil, fmt.Errorf("step 2 PoolSalesInWindow: %w", err)
	}

	// Step 3 + 4: 倒挤 + 报损剥离
	bfResult, err := ComputeBackflush(*bfInput)
	if err != nil {
		return nil, fmt.Errorf("step 3+4 ComputeBackflush: %w", err)
	}

	// Step 5: 池内分摊
	pools, err := s.Store.ListPoolCodes(ctx, req.BranchNo)
	if err != nil {
		return nil, fmt.Errorf("step 5 ListPoolCodes: %w", err)
	}
	SortPoolsByPrice(pools) // 价高先
	allocIn := buildAllocateInput(req.BranchNo, req.PeriodID, bfResult, pools, poolSales, windowStart, windowEnd, s)
	allocRes, err := ComputeAllocate(allocIn)
	if err != nil {
		return nil, fmt.Errorf("step 5 ComputeAllocate: %w", err)
	}

	// 写 freshcheck_alloc 表
	for _, a := range allocRes.Allocs {
		dbAlloc := allocItemToDB(a, req.PeriodID)
		if err := s.Store.InsertAlloc(ctx, dbAlloc); err != nil {
			// 幂等: 重复写同一 (period, pool, seg, item) 返 ErrDuplicateKey, 忽略
			if err != ErrDuplicateKey {
				return nil, fmt.Errorf("step 5 InsertAlloc: %w", err)
			}
		}
	}

	// Step 6: 毛利 + 写 settlement 表
	settlements, err := s.writeSettlements(ctx, req, bfResult, allocRes, windowStart, windowEnd, windowDays)
	if err != nil {
		return nil, fmt.Errorf("step 6 writeSettlements: %w", err)
	}

	// W3.5 7 校验 + 写 alert
	vr, _ := s.RunValidation(ctx, req, allocRes, bfResult)
	_ = vr // 即使 block 也不中断返回 (W3.4 编排)

	// W3.6 双轨互验 + R3 框内对账
	if err := s.RunReconciliation(ctx, req, allocRes, bfResult, allocIn.PoolSegments); err != nil {
		// reconcile 失败不中断 (W3.9 验收再关注)
		fmt.Printf("[RunSettlement] RunReconciliation err: %v\n", err)
	}

	// 汇总
	return &SettleResult{
		PeriodID:    req.PeriodID,
		TrackCode:   req.TrackCode,
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
		Summary: summarizeSettlement(settlements, bfResult),
	}, nil
}

// determineWindow 窗口确定 (Step 1)
//   业务: track_code 决定窗口长度 (leaf 7 天, root 30 天 等)
//   简化: 直接用 req.PeriodEnd - track 对应天数
func determineWindow(req SettleRequest) (time.Time, time.Time, int) {
	days := 7 // 简化: 默认 7 天 (W3.5 加 track_code -> days 查表)
	switch req.TrackCode {
	case "root-monthly", "frozen-monthly":
		days = 30
	case "meat-biweekly", "aquatic-biweekly":
		days = 14
	}
	end := req.PeriodEnd
	if end.IsZero() {
		end = time.Now()
	}
	start := end.Add(-time.Duration(days) * 24 * time.Hour)
	return start, end, days
}

// buildAllocateInput 准备 W3.3 输入
//   - pools 已按 unit_price DESC 排好
//   - 每 pool 一段 (W3.4 简化: 全窗口 1 段)
//   - 从 poolSales 取 pos_qty/amt
//   - 从 pool_event 拉框内 in/out/spoiled (W2 ListPoolEventsByPool + RebuildPoolState)
//   - W4.1: 跨筐降级 — 拉全门店 downgrade 事件, 给目标 pool 加虚拟入 (confidence=high)
func buildAllocateInput(
	branchNo string,
	periodID int64,
	bf *BackflushResult,
	pools []*PoolCode,
	poolSales map[string]*PoolSalesRow,
	windowStart, windowEnd time.Time,
	s *Service,
) AllocateInput {
	segs := []*PoolSegment{}

	// W4.1: 拉全门店 downgrade 事件 (按 target_pool_code 分组)
	downgrades, _ := s.Store.ListDowngradeEventsInWindow(context.Background(), branchNo, windowStart, windowEnd)

	for _, pool := range pools {
		ps, ok := poolSales[pool.PoolCode]
		posQty := 0.0
		posAmt := 0.0
		if ok {
			posQty = ps.Qty
			posAmt = ps.Amt
		}
		// 拉该 pool 窗口内 events, 重建 state
		events, _ := s.Store.ListPoolEventsByPool(context.Background(), branchNo, pool.PoolCode, windowStart, windowEnd)
		state := RebuildPoolState(pool.PoolCode, pool.PoolName, windowEnd, events)
		items := []*PoolSegmentItem{}
		// 用 map 跟踪 item_no (跨筐降级虚拟入会按 item_no 累加)
		itemByNo := map[string]*PoolSegmentItem{}
		for _, it := range state.Items {
			if it.CurrentWeightKg <= 0 {
				continue
			}
			psi := &PoolSegmentItem{
				ItemNo:           it.ItemNo,
				ItemName:         it.ItemName,
				InWeightKg:       it.InWeightKg,
				OutWeightKg:      it.OutWeightKg,
				SpoiledWeightKg:  it.SpoiledWeightKg,
				ConfidenceFactor: 1.0, // 简化
			}
			items = append(items, psi)
			itemByNo[it.ItemNo] = psi
		}

		// W4.1: 跨筐降级虚拟入 — 给本 pool 加其他 pool downgrade 过来的"虚拟入"
		for _, dg := range downgrades[pool.PoolCode] {
			existing, ok := itemByNo[dg.ItemNo]
			if !ok {
				// 目标 pool 之前没这个 SKU, 新建一个 (只有 in_weight, confidence=high)
				psi := &PoolSegmentItem{
					ItemNo:           dg.ItemNo,
					ItemName:         "", // 跨筐降级时 ItemName 拿不到 (PoolEvent 没存), 留空
					InWeightKg:       dg.WeightKg,
					OutWeightKg:      0,
					SpoiledWeightKg:  0,
					ConfidenceFactor: 1.0, // 虚拟入, confidence=high
				}
				items = append(items, psi)
				itemByNo[dg.ItemNo] = psi
			} else {
				// 已有, 累加 in_weight, confidence 提升到 1.0
				existing.InWeightKg += dg.WeightKg
				if existing.ConfidenceFactor < 1.0 {
					existing.ConfidenceFactor = 1.0
				}
			}
		}

		segs = append(segs, &PoolSegment{
			PoolCode:     pool.PoolCode,
			SegmentStart: windowStart,
			SegmentEnd:   windowEnd,
			PoolPosQty:   posQty,
			PoolPosAmt:   posAmt,
			Items:        items,
		})
	}
	return AllocateInput{
		BranchNo:       branchNo,
		PeriodID:       periodID,
		Pools:          pools,
		PoolSegments:   segs,
		BackflushItems: bf.Items,
	}
}

// allocItemToDB AllocItem (W3.3 算法) → Alloc (PG 写)
//   数据桥接: 填 freshcheck_alloc 字段
func allocItemToDB(a *AllocItem, periodID int64) *Alloc {
	return &Alloc{
		PeriodID:         periodID,
		PoolCode:         a.PoolCode,
		SegmentStart:     a.SegmentStart,
		SegmentEnd:       a.SegmentEnd,
		ItemNo:           a.ItemNo,
		AllocQty:         a.AllocQty,
		AllocAmt:         a.AllocAmt,
		ConfidenceFactor: a.Confidence,
		NeedsReview:      a.NeedsReview,
	}
}

// writeSettlements Step 6: 算每 SKU 毛利 + 写 settlement
//   业务: settlement 1 行 = 1 SKU 在 1 期内的完整毛利
//   守恒: (normal + alloc + loss + box_loss) == (begin + purchase - end)
func (s *Service) writeSettlements(
	ctx context.Context,
	req SettleRequest,
	bf *BackflushResult,
	alloc *AllocateResult,
	windowStart, windowEnd time.Time,
	windowDays int,
) ([]*Settlement, error) {
	// 建 alloc 按 item_no 聚合
	allocByItem := map[string]float64{}
	allocAmtByItem := map[string]float64{}
	for _, a := range alloc.Allocs {
		allocByItem[a.ItemNo] += a.AllocQty
		allocAmtByItem[a.ItemNo] += a.AllocAmt
	}

	// 写每 SKU 1 行
	out := []*Settlement{}
	for _, bi := range bf.Items {
		avgCost, _ := s.Cube.AvgCost(ctx, req.BranchNo, bi.ItemNo)
		endQty := bi.EndQty
		if bi.MissingEnd {
			endQty = 0 // C8 阻断时仍可结算, 但 end 视 0 (保守)
		}
		st := &Settlement{
			BranchNo:      req.BranchNo,
			PeriodID:      req.PeriodID,
			TrackCode:     req.TrackCode,
			FreshCategory: bi.FreshCategory,
			ItemNo:        bi.ItemNo,
			ItemName:      bi.ItemName,
			BeginQty:      bi.BeginQty,
			PurchaseQty:   bi.PurchaseQty,
			NormalSaleQty: bi.NormalSaleQty,
			EndQty:        endQty,
			BackflushQty:  bi.Backflush,
			LossQty:       bi.ExpectedLoss,
			PoolAllocQty:  allocByItem[bi.ItemNo],
			PoolAllocAmt:  allocAmtByItem[bi.ItemNo],
			AvgCost:       avgCost,
			WindowStart:   windowStart,
			WindowEnd:     windowEnd,
			WindowDays:    windowDays,
			Status:        SettleFinalized,
			SettledBy:     req.Operator,
			Confidence:    "high",
		}
		st.SaleCost = (st.NormalSaleQty + st.PoolAllocQty + st.LossQty) * avgCost
		st.TotalRevenue = st.NormalSaleAmt + st.PoolAllocAmt
		st.GrossProfit = st.TotalRevenue - st.SaleCost
		if st.TotalRevenue > 0 {
			rate := st.GrossProfit / st.TotalRevenue
			st.GrossProfitRate = &rate
		}
		// C4 守恒: (normal + alloc + loss + box_loss) == (begin + purchase - end)
		// box_loss 暂 0 (W3.5 才算)
		total := st.NormalSaleQty + st.PoolAllocQty + st.LossQty + st.BoxLossQty
		expected := st.BeginQty + st.PurchaseQty - st.EndQty
		diff := total - expected
		if absFloat(diff) > 0.01 {
			st.ConservationOK = false
			st.ConservationMsg = fmt.Sprintf("C4 守恒失败: total=%.4f != expected=%.4f (diff=%.4f)", total, expected, diff)
		} else {
			st.ConservationOK = true
			st.ConservationMsg = ""
		}
		// 幂等键: hash(branch + track + period + item + operator)
		st.IdempotencyKey = idempotencyKey(req.BranchNo, req.TrackCode, req.PeriodID, st.ItemNo, req.Operator)

		if err := s.Store.InsertSettlement(ctx, st); err != nil {
			if err != ErrDuplicateKey {
				return nil, fmt.Errorf("InsertSettlement %s: %w", st.ItemNo, err)
			}
		}
		out = append(out, st)
	}
	return out, nil
}

// summarizeSettlement 整期汇总
func summarizeSettlement(sts []*Settlement, bf *BackflushResult) SettleSummary {
	s := SettleSummary{
		ItemsCount: len(sts),
	}
	for _, st := range sts {
		s.TotalNormalSaleAmt += st.NormalSaleAmt
		s.TotalPoolAllocAmt += st.PoolAllocAmt
		s.TotalLossAmt += st.LossQty * st.AvgCost
		s.TotalBoxLossAmt += st.BoxLossQty * st.AvgCost
		s.TotalGrossProfit += st.GrossProfit
	}
	if s.TotalNormalSaleAmt+s.TotalPoolAllocAmt > 0 {
		s.GrossProfitRate = s.TotalGrossProfit / (s.TotalNormalSaleAmt + s.TotalPoolAllocAmt)
	}
	return s
}

// idempotencyKey 幂等键 hash
func idempotencyKey(branch, track string, period int64, item, operator string) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s|%s|%d|%s|%s", branch, track, period, item, operator)
	return hex.EncodeToString(h.Sum(nil))
}

// SortPoolsByPrice 按 unit_price DESC 排序 (W3.3 多筐串联要求)
//   公开 helper, 给调用方 (W3.5 / W3.7 报表) 复用
func SortPoolsByPrice(pools []*PoolCode) {
	sort.Slice(pools, func(i, j int) bool {
		return pools[i].UnitPrice > pools[j].UnitPrice
	})
}
