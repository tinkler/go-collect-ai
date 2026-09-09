package freshcheck

// backflush.go: W3.2 倒挤公式 + 报损剥离 (Step 3 + Step 4)
//
// 业务规则 (docs/freshcheck-settlement.md §五):
//   Step 3: backflush = begin + purchase - normal_sale - end
//   Step 4: 损耗剥离 (快/慢周转公式不同)
//     - fast: loss = purchase × daily_rate × min(shelf_life, days)
//     - slow: loss = avg_in_stock × daily_rate × days  (avg_in_stock = (begin+end)/2)
//   Step 4: pool_adjustable = max(backflush - loss, 0)
//
// 责任:
//   - 只算算式, 不查 cube 也不查 PG (由调用方准备 inputs)
//   - 缺 end_qty → missing_end=true, 不阻断, 由 W3.5 C8 校验告警
//   - 缺损耗率   → missing_loss_rate=true, loss=0 (按"零损耗"保守估算, 由 W3.5 告警)
//
// 不依赖 LLM, 不依赖全局状态, 纯函数, 易单测.

import (
	"context"
	"fmt"
	"time"
)

// BackflushInput 倒挤算法输入 (调用方准备, 算法不直接拉 cube/PG)
type BackflushInput struct {
	BranchNo    string                       // 门店
	PeriodID    int64                        // 周期
	WindowFrom  time.Time                    // 窗口起点
	WindowTo    time.Time                    // 窗口终点
	BeginByItem map[string]float64           // 上期 end_qty 映射 (item_no → qty)
	EndByItem   map[string]float64           // 本期 end_qty 映射 (from freshcheck_period_stock)
	PurchaseByItem map[string]float64        // 本期采购入库 (from cube purchases)
	NormalSaleByItem map[string]float64      // 本期正常销售 (cube sales - refund, 按 item_no 聚合)
	SKUs        []*SkuMap                    // 本期应盘 SKU (from sku_map.is_active=TRUE), 含 shelf_life + turnover
	// 损耗率查表 (调用方批量查, 避免算法里 N+1)
	// key: "category:turnover:loss_type" (loss_type 固定 "natural")
	LossRateByKey map[string]float64
}

// ComputeBackflush 算整期倒挤 + 报损剥离
//   入参: BackfillInput
//   出参: BackflushResult (含每 SKU 详情 + 整期汇总)
//   错误: 仅参数错误 (时间窗口反序)
func ComputeBackflush(in BackflushInput) (*BackflushResult, error) {
	if !in.WindowTo.After(in.WindowFrom) {
		return nil, fmt.Errorf("window_to 必须 > window_from")
	}
	windowDays := daysBetween(in.WindowFrom, in.WindowTo)
	res := &BackflushResult{
		BranchNo:   in.BranchNo,
		PeriodID:   in.PeriodID,
		WindowFrom: in.WindowFrom,
		WindowTo:   in.WindowTo,
		WindowDays: windowDays,
		Items:      []*BackflushItem{},
	}
	// 一个 SKU 一行结果
	for _, sku := range in.SKUs {
		item := computeOne(sku, in, windowDays)
		res.Items = append(res.Items, item)
		// 汇总
		res.Summary.SKUsCount++
		res.Summary.TotalBackflush += item.Backflush
		res.Summary.TotalExpectedLoss += item.ExpectedLoss
		res.Summary.TotalPoolAdjustable += item.PoolAdjustable
		if item.MissingEnd {
			res.Summary.MissingEndCount++
		}
		if item.MissingLossRate {
			res.Summary.MissingLossCount++
		}
	}
	return res, nil
}

// computeOne 算单个 SKU
func computeOne(sku *SkuMap, in BackflushInput, windowDays int) *BackflushItem {
	item := &BackflushItem{
		ItemNo:        sku.ItemNo,
		ItemName:      sku.ItemName,
		FreshCategory: sku.FreshCategory,
		TurnoverClass: sku.TurnoverClass,
		ShelfLifeDays: sku.ShelfLifeDays,
		BeginQty:      in.BeginByItem[sku.ItemNo],
		PurchaseQty:   in.PurchaseByItem[sku.ItemNo],
		NormalSaleQty: in.NormalSaleByItem[sku.ItemNo],
		WindowDays:    windowDays,
	}
	// end_qty 缺则标 missing, 不阻断
	endQty, ok := in.EndByItem[sku.ItemNo]
	if !ok {
		item.MissingEnd = true
		endQty = 0 // 缺失时 backflush 算式仍然能跑, 但 W3.5 C8 校验会告警
	}
	item.EndQty = endQty

	// Step 3: 倒挤
	item.Backflush = item.BeginQty + item.PurchaseQty - item.NormalSaleQty - endQty

	// Step 4: 损耗剥离
	rate, hasRate := lookupLossRate(in.LossRateByKey, sku.FreshCategory, sku.TurnoverClass, LossNatural)
	if !hasRate {
		item.MissingLossRate = true
	}
	item.ExpectedLoss = computeLoss(sku, item.BeginQty, item.PurchaseQty, endQty, windowDays, rate, hasRate)

	// Step 4: 修正特价消耗 (不能为负)
	if item.Backflush-item.ExpectedLoss > 0 {
		item.PoolAdjustable = item.Backflush - item.ExpectedLoss
	} else {
		item.PoolAdjustable = 0
	}
	return item
}

// computeLoss 算期望损耗 (Step 4 核心)
//   fast: loss = purchase × daily_rate × min(shelf_life, days)
//   slow: loss = avg_in_stock × daily_rate × days
//   缺损耗率 → 0 (保守估算, 由 W3.5 告警)
func computeLoss(sku *SkuMap, begin, purchase, end float64, days int, rate float64, hasRate bool) float64 {
	if !hasRate {
		return 0
	}
	switch sku.TurnoverClass {
	case TurnoverFast:
		// 快: 封顶生命期
		effectiveDays := days
		if sku.ShelfLifeDays > 0 && sku.ShelfLifeDays < effectiveDays {
			effectiveDays = sku.ShelfLifeDays
		}
		return purchase * rate * float64(effectiveDays)
	case TurnoverSlow:
		// 慢: 平均在库量
		avgInStock := (begin + end) / 2.0
		return avgInStock * rate * float64(days)
	default:
		// 未知周转类 → 当 fast 处理 (保守: 用采购基数)
		effectiveDays := days
		if sku.ShelfLifeDays > 0 && sku.ShelfLifeDays < effectiveDays {
			effectiveDays = sku.ShelfLifeDays
		}
		return purchase * rate * float64(effectiveDays)
	}
}

// lookupLossRate 查损耗率表 (key: "category:turnover:loss_type")
func lookupLossRate(table map[string]float64, category, turnover, lossType string) (float64, bool) {
	if table == nil {
		return 0, false
	}
	key := category + ":" + turnover + ":" + lossType
	v, ok := table[key]
	return v, ok
}

// daysBetween 算两时间点相隔天数 (向上取整, 至少 1 天)
//   e.g. 2026-09-01 00:00:00 ~ 2026-09-08 00:00:00 = 7 天
func daysBetween(from, to time.Time) int {
	if !to.After(from) {
		return 0
	}
	d := int(to.Sub(from).Hours() / 24)
	if d < 1 {
		d = 1
	}
	return d
}

// BuildBackflushInput 辅助函数: 从 CubeQuerier + Store 准备 BackflushInput
//   给 W3.4 编排用, 集中所有查询逻辑
//   - 拉 sales_with_refund (cube), 按 item_no 聚合并扣退货
//   - 拉 purchases (cube), 按 item_no 聚合
//   - 拉本期 period_stock (PG), 得 endByItem
//   - 拉上期 period_stock (PG), 得 beginByItem
//   - 拉 sku_map.active (PG), 得 SKUs
//   - 拉 loss_rate (PG, effective_to IS NULL), 得 LossRateByKey
//
// 参数:
//   - cubeQuerier: W1.5 已建
//   - store:      W1.4 已建
//   - branchNo:   门店
//   - periodID:   本期
//   - windowFrom/windowTo: 窗口
//   - prevPeriodID: 上期 (用于 begin), 0 = 没有上期, begin 全 0
func BuildBackflushInput(
	ctx context.Context,
	cubeQuerier *CubeQuerier,
	store *Store,
	branchNo string,
	periodID int64,
	prevPeriodID int64,
	windowFrom, windowTo time.Time,
) (*BackflushInput, error) {
	in := &BackflushInput{
		BranchNo:        branchNo,
		PeriodID:        periodID,
		WindowFrom:      windowFrom,
		WindowTo:        windowTo,
		BeginByItem:     map[string]float64{},
		EndByItem:       map[string]float64{},
		PurchaseByItem:  map[string]float64{},
		NormalSaleByItem: map[string]float64{},
		LossRateByKey:   map[string]float64{},
	}

	// 1. 本期 end_qty
	cur, err := store.ListPeriodStocksByPeriod(ctx, branchNo, periodID)
	if err != nil {
		return nil, fmt.Errorf("list period_stock %d: %w", periodID, err)
	}
	for _, ps := range cur {
		in.EndByItem[ps.ItemNo] = ps.Qty
	}

	// 2. 上期 end_qty 作 begin (prevPeriodID > 0 才查)
	if prevPeriodID > 0 {
		prev, err := store.ListPeriodStocksByPeriod(ctx, branchNo, prevPeriodID)
		if err != nil {
			return nil, fmt.Errorf("list period_stock %d: %w", prevPeriodID, err)
		}
		for _, ps := range prev {
			in.BeginByItem[ps.ItemNo] = ps.Qty
		}
	}

	// 3. 拉 normal_sale (cube sales_with_refund, 过滤 is_refund=0, 按 item_no 聚合)
	salesRows, err := cubeQuerier.SalesWithRefundInWindow(ctx, branchNo, windowFrom, windowTo)
	if err != nil {
		return nil, fmt.Errorf("cube sales: %w", err)
	}
	for _, row := range salesRows {
		isRefund := toFloat(row[SalesWithRefundCube+".is_refund"]) != 0
		if isRefund {
			continue
		}
		itemNo := toString(row[SalesWithRefundCube+".item_no"])
		if itemNo == "" {
			continue
		}
		qnty := toFloat(row[SalesWithRefundCube+".sale_qnty"])
		in.NormalSaleByItem[itemNo] += qnty
	}

	// 4. 拉 purchase (cube purchases, 按 item_no 聚合)
	purchases, err := cubeQuerier.PurchasesInWindow(ctx, branchNo, windowFrom, windowTo)
	if err != nil {
		return nil, fmt.Errorf("cube purchases: %w", err)
	}
	for _, p := range purchases {
		in.PurchaseByItem[p.ItemNo] += p.RealQty
	}

	// 5. 拉应盘 SKU
	skus, err := store.ListAllSkuMap(ctx, branchNo)
	if err != nil {
		return nil, fmt.Errorf("list sku_map: %w", err)
	}
	in.SKUs = skus

	// 6. 批量查损耗率 (按 category+turnover 组合, 避免 N+1)
	seen := map[string]bool{}
	for _, sku := range skus {
		key := sku.FreshCategory + ":" + sku.TurnoverClass
		if seen[key] {
			continue
		}
		seen[key] = true
		lr, err := store.GetActiveLossRate(ctx, sku.FreshCategory, sku.TurnoverClass, LossNatural)
		if err == nil && lr != nil {
			in.LossRateByKey[key+":"+LossNatural] = lr.DailyRate
		}
	}

	return in, nil
}
