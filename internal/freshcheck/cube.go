package freshcheck

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/tinkler/collect-ai/internal/business"
)

// CubeQuerier freshcheck 模块 cube 查询封装 (W1.5, 2026-09-09)
//
// 职责: 把 6 步结算需要的数据从 cube 拉过来
//   - 走 business.Gateway.RawQueryWithTime (物理 cube + time filter)
//   - 走 business.Executor (业务名 + 自动翻译, 适合简单查询)
//   - 走 Store (PG 本地表, 走 freshcheck_pool_code 等)
//
// 复用 collect-ai 现有 business.Gateway (跟 restock 一致)
//
// 不直连思迅 HB POS (AGENTS.md §12.1 强制)
type CubeQuerier struct {
	Gateway *business.Gateway
	Store   *Store // 用于查 PG 本地表 (pool_code, sku_map 等)
}

// NewCubeQuerier 构造
func NewCubeQuerier(g *business.Gateway, s *Store) *CubeQuerier {
	return &CubeQuerier{Gateway: g, Store: s}
}

// ============== 1. SalesWithRefundInWindow ==============
//
// 拉窗口内销售流水(含退货标记 + 原单号)
//   cube: sales_with_refund (W1.2 已在 cube-agent-server 配好)
//   给 freshcheck Step 2 退货回冲用
//   ⚠️ 跟 restock 一样: 不要 .UTC(), 用 mssql 通用 datetime
//
// 返回: []map (业务字段名, key 含 cube 前缀, 调用方按 is_refund 过滤)

const (
	// SalesWithRefundCube cube 物理名 (W1.2 plugin.yaml metadata.name)
	SalesWithRefundCube = "sales_with_refund"
	// PurchasesCube cube 物理名 (已有 plugin, 沿用)
	PurchasesCube = "purchases"
	// InventoryCurrentCube cube 物理名 (已有 plugin, 沿用)
	InventoryCurrentCube = "inventory_current"
	// FreshItemsCube cube 物理名 (W1.2 plugin.yaml metadata.name)
	FreshItemsCube = "items_with_clsno"
)

// mssqlTimeFmt SQL Server 2008 R2 通用 datetime 格式 (任何 locale 都认)
const mssqlTimeFmt = "2006-01-02 15:04:05"

// SalesWithRefundInWindow 拉窗口内 sales_with_refund 全量
//   - 2026-09-10 集成 A 优化: 改走 cube measure 聚合 (按 item_no GROUP BY, 净额 = 销售 - 退货)
//     旧: 拉 13 列明细 70 万行 → cube 端 LEFT JOIN 4 表 SELECT 12s+
//     新: measures=[total_qnty, total_revenue, total_cost, total_gross_profit]
//         dimensions=[item_no] + filter inDateRange (不进 GROUP BY, 只做 WHERE)
//         依赖 plugin SQL 把退货行 sale_qnty/sale_money 净额化 (sum 自动抵消)
//   - 返回: 每 item 一行 {item_no, total_qnty (净额), total_revenue, total_cost, gross_profit}
func (q *CubeQuerier) SalesWithRefundInWindow(ctx context.Context, branchNo string, from, to time.Time) ([]map[string]any, error) {
	rows, err := q.Gateway.RawQuery(SalesWithRefundCube,
		[]string{
			SalesWithRefundCube + ".total_qnty",        // SUM(sale_qnty) — 净额 (退货转负)
			SalesWithRefundCube + ".total_revenue",     // SUM(sale_money) — 净额
			SalesWithRefundCube + ".total_cost",        // SUM(in_price * sale_qnty) — 净额
			SalesWithRefundCube + ".total_gross_profit", // SUM(sale_money - cost) — 净额
		},
		[]string{SalesWithRefundCube + ".item_no"},
		[]map[string]any{
			{"member": SalesWithRefundCube + ".branch_no", "operator": "equals", "values": []string{branchNo}},
			// 用 inDateRange filter 而不是 timeDimensions, 避免 cube 把 oper_date 加进 GROUP BY
			{"member": SalesWithRefundCube + ".oper_date", "operator": "inDateRange", "values": []string{from.Format("2006-01-02"), to.Format("2006-01-02")}},
		},
		nil,
		5000)
	if err != nil {
		return nil, fmt.Errorf("cube %s SalesWithRefundInWindow: %w", SalesWithRefundCube, err)
	}
	return rows, nil
}

// ============== 2. PurchasesInWindow ==============
//
// 拉窗口内采购入库流水
//   cube: purchases (已有 plugin)

type PurchaseRow struct {
	ItemNo    string
	ItemName  string
	VoucherNo string
	RealQty   float64
	CostPrice float64
	TotalCost float64
	OperDate  time.Time
}

// ============== 2b. PoolSalesInWindow (W3.4 Step 5 分摊) ==============
//
// 拉窗口内"按特价码货号卖的 POS 量" (sales_with_refund.item_no = pool_code)
//   业务: pool_pos_qty/amt 进 alloc (R2 报表)
//   含退货: is_refund=1 减扣
//   返回: map[pool_code] -> {qty, amt}
//
// 性能: 单 pool 1 年约 1 万行, 秒级

// PoolSalesRow 单特价码窗口汇总
type PoolSalesRow struct {
	PoolCode string
	Qty      float64
	Amt      float64
}

// PoolSalesInWindow 拉窗口内"按特价码货号"的 POS 销量
//   - item_no = pool_code 视为"特价码" (W1 spec: 特价码本身是货号, 进 sales 流)
//   - is_refund=0 计销售, is_refund=1 减扣
//   - 按 pool_code 聚合
// PoolSalesInWindow 拉窗口内"按特价码货号"的 POS 销量
//   - item_no = pool_code 视为"特价码" (W1 spec: 特价码本身是货号, 进 sales 流)
//   - 净额 = 销售 - 退货 (cube SQL 把退货行 sale_qnty/sale_money 转负, sum 自动抵消)
//
// 2026-09-10 集成 A 优化: 单次查 (28s→1-2s), 不用 timeDimensions (避免 cube 加 oper_date 进 GROUP BY)
//   旧: 分两次 is_refund=0/1 各 14s, GO 端相减
//   新: 单次查 total_qnty + total_revenue measure + inDateRange filter, cube 端 sum 净额
func (q *CubeQuerier) PoolSalesInWindow(ctx context.Context, branchNo string, from, to time.Time) (map[string]*PoolSalesRow, error) {
	rows, err := q.Gateway.RawQuery(SalesWithRefundCube,
		[]string{
			SalesWithRefundCube + ".total_qnty",   // SUM(sale_qnty) — 净额 (退货行转负)
			SalesWithRefundCube + ".total_revenue", // SUM(sale_money) — 净额
		},
		[]string{SalesWithRefundCube + ".item_no"},
		[]map[string]any{
			{"member": SalesWithRefundCube + ".branch_no", "operator": "equals", "values": []string{branchNo}},
			{"member": SalesWithRefundCube + ".oper_date", "operator": "inDateRange", "values": []string{from.Format("2006-01-02"), to.Format("2006-01-02")}},
		},
		nil,
		5000)
	if err != nil {
		return nil, fmt.Errorf("cube %s PoolSalesInWindow: %w", SalesWithRefundCube, err)
	}
	out := map[string]*PoolSalesRow{}
	for _, row := range rows {
		poolCode := toString(row[SalesWithRefundCube+".item_no"])
		if poolCode == "" {
			continue
		}
		out[poolCode] = &PoolSalesRow{
			PoolCode: poolCode,
			Qty:      toFloat(row[SalesWithRefundCube+".total_qnty"]),
			Amt:      toFloat(row[SalesWithRefundCube+".total_revenue"]),
		}
	}
	return out, nil
}

// PurchasesInWindow 拉窗口内采购入库 (用于 Step 3 倒挤公式的 purchase_qty)
//
// 2026-09-10: 改走 cube measure 聚合 (按 item_no GROUP BY, 用 inDateRange filter 不用 timeDimensions)
//   旧: 拉 5 万行明细 → cube 端 LEFT JOIN 商品主表慢, 25-27s timeout
//   新: measures=count+total_qty+total_cost, dimensions=item_no+item_name
//       cube 端 GROUP BY item_no (不加 oper_date 进 GROUP BY), 行数 = SKU 数 (几百), 1-3s 返回
//   voucher_no/oper_date 字段: 聚合后无意义, 留空 (下游 backflush 只用 RealQty)
func (q *CubeQuerier) PurchasesInWindow(ctx context.Context, branchNo string, from, to time.Time) ([]*PurchaseRow, error) {
	rows, err := q.Gateway.RawQuery(PurchasesCube,
		[]string{
			PurchasesCube + ".count",      // 笔数 (参考)
			PurchasesCube + ".total_qty",  // SUM(real_qty) — 倒挤公式核心
			PurchasesCube + ".total_cost", // SUM(real_qty*cost_price) — 派生 cost_price
		},
		[]string{
			PurchasesCube + ".item_no",
			PurchasesCube + ".item_name",
		},
		[]map[string]any{
			{"member": PurchasesCube + ".branch_no", "operator": "equals", "values": []string{branchNo}},
			// 用 inDateRange filter 而不是 timeDimensions, 避免 cube 把 oper_date 加进 GROUP BY
			{"member": PurchasesCube + ".oper_date", "operator": "inDateRange", "values": []string{from.Format("2006-01-02"), to.Format("2006-01-02")}},
		},
		nil,
		5000)
	if err != nil {
		return nil, fmt.Errorf("cube %s PurchasesInWindow: %w", PurchasesCube, err)
	}
	out := make([]*PurchaseRow, 0, len(rows))
	for _, r := range rows {
		totalQty := asFloat(r, PurchasesCube+".total_qty")
		totalCost := asFloat(r, PurchasesCube+".total_cost")
		costPrice := 0.0
		if totalQty > 0 {
			costPrice = totalCost / totalQty
		}
		out = append(out, &PurchaseRow{
			ItemNo:    asString(r, PurchasesCube+".item_no"),
			ItemName:  asString(r, PurchasesCube+".item_name"),
			VoucherNo: "", // 聚合后无单据号
			RealQty:   totalQty,
			CostPrice: costPrice,
			TotalCost: totalCost,
			OperDate:  time.Time{}, // 聚合后无具体日期
		})
	}
	return out, nil
}

// ============== 3. AvgCost (库存 + 成本) ==============
//
// 查某 SKU 当前加权平均成本
//   cube: inventory_current (已有 plugin)
//
// ⚠️ TODO W1.7 / W3: inventory_current cube 的 avg_cost 是简单 AVG (行级均值的再次平均),
//   不等于加权平均. 准确做法: cube 端加 weighted_avg_cost = SUM(stock_qty * avg_cost) / SUM(stock_qty)
//   当前实现: 用 cube 的 avg measure 近似 (单门店只有 1 行 = 准确; 多门店时不准)
//   W3 性能优化时一并改 cube

// AvgCost 查某 SKU 在某门店的当前平均成本
//   返 0 表示无库存 / 无记录 (调用方决定 fallback)
func (q *CubeQuerier) AvgCost(ctx context.Context, branchNo, itemNo string) (float64, error) {
	rows, err := q.Gateway.RawQuery(InventoryCurrentCube,
		[]string{InventoryCurrentCube + ".avg_cost"},
		[]string{InventoryCurrentCube + ".branch_no", InventoryCurrentCube + ".item_no"},
		[]map[string]any{
			{"member": InventoryCurrentCube + ".branch_no", "operator": "equals", "values": []string{branchNo}},
			{"member": InventoryCurrentCube + ".item_no", "operator": "equals", "values": []string{itemNo}},
		},
		nil, 10)
	if err != nil {
		return 0, fmt.Errorf("cube %s AvgCost: %w", InventoryCurrentCube, err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return asFloat(rows[0], InventoryCurrentCube+".avg_cost"), nil
}

// StockSnapshot 查某门店所有 SKU 的当前库存快照
//   返回: map[itemNo] -> stockQty (kg 或 件, 看 unit)
//   性能: 1 门店 ~100-500 行, <1s
type StockRow struct {
	ItemNo   string
	StockQty float64
	AvgCost  float64
}

func (q *CubeQuerier) StockSnapshot(ctx context.Context, branchNo string) (map[string]*StockRow, error) {
	rows, err := q.Gateway.RawQuery(InventoryCurrentCube,
		[]string{InventoryCurrentCube + ".stock_qty", InventoryCurrentCube + ".avg_cost"},
		[]string{InventoryCurrentCube + ".branch_no", InventoryCurrentCube + ".item_no"},
		[]map[string]any{
			{"member": InventoryCurrentCube + ".branch_no", "operator": "equals", "values": []string{branchNo}},
		},
		nil, 10000)
	if err != nil {
		return nil, fmt.Errorf("cube %s StockSnapshot: %w", InventoryCurrentCube, err)
	}
	out := make(map[string]*StockRow, len(rows))
	for _, r := range rows {
		itemNo := asString(r, InventoryCurrentCube+".item_no")
		if itemNo == "" {
			continue
		}
		out[itemNo] = &StockRow{
			ItemNo:   itemNo,
			StockQty: asFloat(r, InventoryCurrentCube+".stock_qty"),
			AvgCost:  asFloat(r, InventoryCurrentCube+".avg_cost"),
		}
	}
	return out, nil
}

// ============== 4. FreshItemsByCategory (走 Executor 业务名) ==============
//
// 走 business.Executor.FreshItemsByCategory (W1.3 加的)
//   不用走 Gateway, 因为 items_with_clsno cube 已有完整业务字段映射
//   freshcheck 建账用: 全量生鲜 SKU 拉一次, admin 审核, 写入 freshcheck_sku_map

// FreshItemRow 生鲜 SKU 简版 (建账筛选用)
type FreshItemRow struct {
	ItemNo        string
	ItemName      string
	FreshCategory string
	TurnoverClass string
	Unit          string
	StockQty      float64
}

// FreshItemsByCategory 拉某生鲜子类下的所有 SKU (走业务名)
//   freshCategory 为空时拉全量 (fresh_category NOT NULL)
func (q *CubeQuerier) FreshItemsByCategory(ctx context.Context, freshCategory string) ([]*FreshItemRow, error) {
	executor := business.NewExecutorFromGateway(q.Gateway)
	rows, err := executor.FreshItemsByCategory(freshCategory, 0)
	if err != nil {
		return nil, fmt.Errorf("Executor.FreshItemsByCategory(%s): %w", freshCategory, err)
	}
	out := make([]*FreshItemRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, &FreshItemRow{
			ItemNo:        asString(r, "item_no"),
			ItemName:      asString(r, "item_name"),
			FreshCategory: asString(r, "fresh_category"),
			TurnoverClass: asString(r, "turnover_class"),
			Unit:          asString(r, "unit"),
			StockQty:      asFloat(r, "stock_qty"),
		})
	}
	return out, nil
}

// ============== 5. PoolCodes (本地 PG, 不走 cube) ==============
//
// 特价码定义在 collect-ai PG 表 freshcheck_pool_code (W1.4)
//   不走 cube, 因为定义是业务主数据, 跟思迅 HB POS 解耦

// ActivePoolCodes 拉某门店所有 active 特价码 (按 unit_price DESC, 即归因优先级)
func (q *CubeQuerier) ActivePoolCodes(ctx context.Context, branchNo string) ([]*PoolCode, error) {
	return q.Store.ListPoolCodes(ctx, branchNo)
}

// ActiveFreshSKUs 拉某门店所有 active 生鲜 SKU (走 PG 本地表, 不是 cube)
func (q *CubeQuerier) ActiveFreshSKUs(ctx context.Context, branchNo string) ([]*SkuMap, error) {
	return q.Store.ListAllSkuMap(ctx, branchNo)
}

// GetLossRate 查某子类某周转类的当前损耗率 (走 PG 本地表)
func (q *CubeQuerier) GetLossRate(ctx context.Context, freshCategory, turnoverClass, lossType string) (*LossRate, error) {
	return q.Store.GetActiveLossRate(ctx, freshCategory, turnoverClass, lossType)
}

// GetThreshold 查单个阈值
func (q *CubeQuerier) GetThreshold(ctx context.Context, key string) (float64, error) {
	return q.Store.GetThreshold(ctx, key)
}

// ============== helpers ==============

func asString(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprintf("%v", v)
	}
	return ""
}

func asFloat(m map[string]any, key string) float64 {
	if v, ok := m[key]; ok {
		switch x := v.(type) {
		case float64:
			return x
		case float32:
			return float64(x)
		case int:
			return float64(x)
		case int64:
			return float64(x)
		case string:
			f, _ := strconv.ParseFloat(x, 64)
			return f
		}
	}
	return 0
}

func asInt(m map[string]any, key string) int {
	if v, ok := m[key]; ok {
		switch x := v.(type) {
		case int:
			return x
		case int64:
			return int(x)
		case float64:
			return int(x)
		case string:
			n, _ := strconv.Atoi(x)
			return n
		}
	}
	return 0
}
