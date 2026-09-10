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
//   - 含 is_refund 标记 (0/1) + voucher_no 原单号
//   - 含 oper_date / branch_no / item_no / item_name / sale_qnty / sale_money 等
//   - 调用方按 is_refund 过滤退货, 按 voucher_no 关联原单
//
// 性能: 1 年数据约 70 万行, 5-10s 一次
func (q *CubeQuerier) SalesWithRefundInWindow(ctx context.Context, branchNo string, from, to time.Time) ([]map[string]any, error) {
	timeDims := []map[string]any{
		{
			"dimension": SalesWithRefundCube + ".oper_date",
			"dateRange": []string{
				from.Format(mssqlTimeFmt),
				to.Format(mssqlTimeFmt),
			},
		},
	}
	rows, err := q.Gateway.RawQueryWithTime(SalesWithRefundCube,
		[]string{SalesWithRefundCube + ".count"},
		[]string{
			SalesWithRefundCube + ".row_id",
			SalesWithRefundCube + ".oper_date",
			SalesWithRefundCube + ".branch_no",
			SalesWithRefundCube + ".item_no",
			SalesWithRefundCube + ".item_name",
			SalesWithRefundCube + ".item_clsno",
			SalesWithRefundCube + ".item_clsname",
			SalesWithRefundCube + ".main_supcust",
			SalesWithRefundCube + ".is_refund",
			SalesWithRefundCube + ".sale_qnty",
			SalesWithRefundCube + ".sale_money",
			SalesWithRefundCube + ".total_cost",
			SalesWithRefundCube + ".gross_profit",
		},
		[]map[string]any{
			{"member": SalesWithRefundCube + ".branch_no", "operator": "equals", "values": []string{branchNo}},
		},
		nil, 50000, timeDims)
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
//   - is_refund=0 计销售, is_refund=1 减扣
//   - 按 pool_code 聚合
//
// 2026-09-10: 分两次查 (normal + refund) 走 measure 聚合
//   - cube SQL Server 2008 R2 不支持 measure SQL 是 CASE WHEN 表达式
//   - 走 total_qnty (sum sale_qnty) + total_revenue (sum sale_money) measure
//   - 分两次拉, filter 区分 is_refund=0/1, GO 端相减
func (q *CubeQuerier) PoolSalesInWindow(ctx context.Context, branchNo string, from, to time.Time) (map[string]*PoolSalesRow, error) {
	timeDims := []map[string]any{
		{
			"dimension": SalesWithRefundCube + ".oper_date",
			"dateRange": []string{
				from.Format(mssqlTimeFmt),
				to.Format(mssqlTimeFmt),
			},
		},
	}
	// 查正常销售: is_refund=0
	normalRows, err := q.Gateway.RawQueryWithTime(SalesWithRefundCube,
		[]string{
			SalesWithRefundCube + ".total_qnty",   // SUM(sale_qnty)
			SalesWithRefundCube + ".total_revenue", // SUM(sale_money)
		},
		[]string{SalesWithRefundCube + ".item_no"},
		[]map[string]any{
			{"member": SalesWithRefundCube + ".branch_no", "operator": "equals", "values": []string{branchNo}},
			{"member": SalesWithRefundCube + ".is_refund", "operator": "equals", "values": []string{"0"}},
		},
		nil, 5000, timeDims)
	if err != nil {
		return nil, fmt.Errorf("cube %s PoolSalesInWindow normal: %w", SalesWithRefundCube, err)
	}
	// 查退货: is_refund=1
	refundRows, err := q.Gateway.RawQueryWithTime(SalesWithRefundCube,
		[]string{
			SalesWithRefundCube + ".total_qnty",   // SUM(sale_qnty) — 退的 sale_qnty 是负
			SalesWithRefundCube + ".total_revenue", // SUM(sale_money) — 退的 sale_money 是负
		},
		[]string{SalesWithRefundCube + ".item_no"},
		[]map[string]any{
			{"member": SalesWithRefundCube + ".branch_no", "operator": "equals", "values": []string{branchNo}},
			{"member": SalesWithRefundCube + ".is_refund", "operator": "equals", "values": []string{"1"}},
		},
		nil, 5000, timeDims)
	if err != nil {
		return nil, fmt.Errorf("cube %s PoolSalesInWindow refund: %w", SalesWithRefundCube, err)
	}
	out := map[string]*PoolSalesRow{}
	for _, row := range normalRows {
		poolCode := toString(row[SalesWithRefundCube+".item_no"])
		if poolCode == "" {
			continue
		}
		qty := toFloat(row[SalesWithRefundCube+".total_qnty"])
		amt := toFloat(row[SalesWithRefundCube+".total_revenue"])
		out[poolCode] = &PoolSalesRow{PoolCode: poolCode, Qty: qty, Amt: amt}
	}
	// 减扣退货
	for _, row := range refundRows {
		poolCode := toString(row[SalesWithRefundCube+".item_no"])
		if poolCode == "" {
			continue
		}
		// 退货 sale_qnty/sale_money 在 cube 端已经存为负数 (siss_saleflow view 的 is_refund 行)
		// total_qnty 已经是 sum(sale_qnty) 自动抵消 (退的为负)
		// 正常 + 退货 = 净额
		qty := toFloat(row[SalesWithRefundCube+".total_qnty"])
		amt := toFloat(row[SalesWithRefundCube+".total_revenue"])
		if r, ok := out[poolCode]; ok {
			r.Qty += qty
			r.Amt += amt
		} else {
			out[poolCode] = &PoolSalesRow{PoolCode: poolCode, Qty: qty, Amt: amt}
		}
	}
	return out, nil
}

// PurchasesInWindow 拉窗口内采购入库 (用于 Step 3 倒挤公式的 purchase_qty)
//
// 2026-09-10: 改走 cube measure 聚合 (按 item_no GROUP BY)
//   旧: 拉 5 万行明细 → cube 端 LEFT JOIN 商品主表慢, 25-27s timeout
//   新: measures=count+total_qty+total_cost, dimensions=item_no+item_name
//       cube 端 GROUP BY item_no, 行数 = SKU 数 (几百), < 1s 返回
//   voucher_no/oper_date/cost_price 字段: 聚合后无意义, 留空 (下游 backflush 只用 RealQty)
func (q *CubeQuerier) PurchasesInWindow(ctx context.Context, branchNo string, from, to time.Time) ([]*PurchaseRow, error) {
	timeDims := []map[string]any{
		{
			"dimension": PurchasesCube + ".oper_date",
			"dateRange": []string{
				from.Format(mssqlTimeFmt),
				to.Format(mssqlTimeFmt),
			},
		},
	}
	rows, err := q.Gateway.RawQueryWithTime(PurchasesCube,
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
		},
		nil, 5000, timeDims)
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
