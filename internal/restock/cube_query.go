package restock

import (
	"context"
	"fmt"
	"log"
	"math"
	"strconv"
	"time"

	"github.com/tinkler/collect-ai/internal/parser/agent"
)

// CubeQuerier 包装 collect-ai 现有 agent.Client
//
// 复用:
//   agent.Client.Execute(cube, measures, dimensions, filters, segments, limit)
//   agent.Client.Ping()
//
// 3 个 cube 名从 cfg 读,允许 env 覆盖:
//   RESTOCK_CUBE_SALES     默认 "sales_yesterday"
//   RESTOCK_CUBE_INVENTORY 默认 "inventory_current"
//   RESTOCK_CUBE_PROMOTION 默认 "promotion_plan_7d"
//
// 第一版如果 cube 还没建,函数会返回错误(不阻塞业务,降级 mock 数据)
type CubeQuerier struct {
	agent *agent.Client
	cfg   *RestockConfig
}

func NewCubeQuerier(a *agent.Client, cfg *RestockConfig) *CubeQuerier {
	return &CubeQuerier{agent: a, cfg: cfg}
}

// SalesYesterday 拉昨日 + 7日均 + 30日均销量
//   适配 cube-agent-server 上 sales_yesterday cube
//   返回: map[item_no] -> { YesterdaySales, SevenDayAvg, ThirtyDayAvg }
func (q *CubeQuerier) SalesYesterday(ctx context.Context, branchNo string) (map[string]*SkuSnapshot, error) {
	cube := q.cfg.CubeSales
	if cube == "" {
		cube = "sales_yesterday"
	}
	rows, err := q.agent.Execute(cube,
		[]string{"sales_yesterday.sale_qty_yesterday", "sales_yesterday.sale_qty_7d_avg", "sales_yesterday.sale_qty_30d_avg"},
		[]string{"sales_yesterday.item_no", "sales_yesterday.item_name", "sales_yesterday.barcode", "sales_yesterday.supplier_name"},
		[]map[string]any{
			{"member": "sales_yesterday.branch_no", "operator": "equals", "values": []string{branchNo}},
		},
		nil, 5000)
	if err != nil {
		return nil, fmt.Errorf("cube %s query: %w", cube, err)
	}
	out := make(map[string]*SkuSnapshot, len(rows))
	for _, r := range rows {
		sku := &SkuSnapshot{
			BranchNo:       branchNo,
			ItemNo:         asString(r, "sales_yesterday.item_no"),
			ItemName:       asString(r, "sales_yesterday.item_name"),
			Barcode:        asString(r, "sales_yesterday.barcode"),
			SupplierName:   asString(r, "sales_yesterday.supplier_name"),
			YesterdaySales: asInt(r, "sales_yesterday.sale_qty_yesterday"),
			SevenDayAvg:    asInt(r, "sales_yesterday.sale_qty_7d_avg"),
			ThirtyDayAvg:   asInt(r, "sales_yesterday.sale_qty_30d_avg"),
		}
		out[sku.ItemNo] = sku
	}
	log.Printf("[restock] sales(%s): %d items, branch=%s", cube, len(out), branchNo)
	return out, nil
}

// InventoryCurrent 拉当前库存
//   适配 inventory_current cube
//   返回: map[item_no] -> { Stock }
func (q *CubeQuerier) InventoryCurrent(ctx context.Context, branchNo string) (map[string]int, error) {
	cube := q.cfg.CubeInventory
	if cube == "" {
		cube = "inventory_current"
	}
	rows, err := q.agent.Execute(cube,
		[]string{"inventory_current.stock_qty"},
		[]string{"inventory_current.item_no"},
		[]map[string]any{
			{"member": "inventory_current.branch_no", "operator": "equals", "values": []string{branchNo}},
		},
		nil, 10000)
	if err != nil {
		return nil, fmt.Errorf("cube %s query: %w", cube, err)
	}
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		itemNo := asString(r, "inventory_current.item_no")
		if itemNo == "" {
			continue
		}
		out[itemNo] = asInt(r, "inventory_current.stock_qty")
	}
	log.Printf("[restock] inventory(%s): %d items", cube, len(out))
	return out, nil
}

// Promotion7d 拉未来 7 天促销计划(只关心哪些 item 有促销)
//   适配 promotion_plan_7d cube
func (q *CubeQuerier) Promotion7d(ctx context.Context, branchNo string) (map[string]bool, error) {
	cube := q.cfg.CubePromotion
	if cube == "" {
		cube = "promotion_plan_7d"
	}
	rows, err := q.agent.Execute(cube,
		[]string{"promotion_plan_7d.count"},
		[]string{"promotion_plan_7d.item_no"},
		[]map[string]any{
			{"member": "promotion_plan_7d.branch_no", "operator": "equals", "values": []string{branchNo}},
		},
		nil, 10000)
	if err != nil {
		return nil, fmt.Errorf("cube %s query: %w", cube, err)
	}
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		itemNo := asString(r, "promotion_plan_7d.item_no")
		if itemNo == "" {
			continue
		}
		out[itemNo] = true
	}
	return out, nil
}

// ============== Display Restock Window (2026-08-30 新增) ==============

// WindowSaleRow 时间窗口销量 + 库存快照的一行
//   2026-08-31: SaleQty 改 float64,适配 SQL Server decimal(18,4) 称重件
//   例如 0.6361 kg 鲜肉 / 1.3693 件散装 — int 截断会丢精度导致建议补 0
type WindowSaleRow struct {
	ItemNo       string
	ItemName     string
	Barcode      string
	SupplierName string
	SaleQty      float64
	InvSnapshot  int
}

// SalesInWindow 拉指定时间窗口的销量 + 当前库存快照
//   适配 cube-agent-server 上 display_restock_window cube
//   from/to 用 mssql 通用 datetime 格式 (yyyy-MM-dd HH:mm:ss)
//   ⚠️ 不要用 RFC3339 / ISO 8601:SQL Server 2008 R2 无法解析 '2026-08-31T12:34:56Z'
//   返回: map[item_no] -> WindowSaleRow(SaleQty 是窗口内合计,InvSnapshot 是当前值)
//   性能:< 10s / 次(子 session 实测 100-208ms,3.18M 行)
//
// ⚠️ 2026-08-31 累加修复:
//   cube 引擎按 5 个 dimension + oper_date(time) GROUP BY,
//   同一个 item 在窗口内不同 oper_date 时间点会返回多行(每行 cube 内部已 SUM 过 sale_qnty)
//   collect-ai 端必须把同 item 多行的 sale_qty 累加(不是覆盖),
//   否则后写覆盖前写,sale_qty 偶发只留最后一行(1.0 变 0.6361 被过滤 / 0.6361 变 1.0 假象)
func (q *CubeQuerier) SalesInWindow(ctx context.Context, branchNo string, from, to time.Time) (map[string]*WindowSaleRow, error) {
	cube := q.cfg.DisplayRestockCubeName
	if cube == "" {
		cube = "display_restock_window"
	}
	// mssql 2008 R2 通用 datetime 格式 (任何 locale 都认)
	const mssqlTimeFmt = "2006-01-02 15:04:05"
	// ⚠️ 2026-08-31: 不要 .UTC()!
	//   t_rm_saleflow.oper_date 是 mssql datetime (无时区 wall clock,业务系统按本地时间写入)
	//   .UTC() 会把"本地 22:11"变成"UTC 14:11",差 8 小时,
	//   导致查到的是 UTC 14-15 北京的销售,不是想要的 22-23 北京窗口
	//   Format 用 time 自身的 location 输出,本地时区直接出"22:11:45"
	timeDimensions := []map[string]any{
		{
			"dimension": cube + ".oper_date",
			"dateRange": []string{
				from.Format(mssqlTimeFmt),
				to.Format(mssqlTimeFmt),
			},
		},
	}
	rows, err := q.agent.ExecuteWithTime(cube,
		[]string{
			cube + ".sale_qty",
			cube + ".inv_snapshot",
		},
		[]string{
			cube + ".branch_no",
			cube + ".item_no",
			cube + ".item_name",
			cube + ".barcode",
			cube + ".supplier_name",
		},
		[]map[string]any{
			{"member": cube + ".branch_no", "operator": "equals", "values": []string{branchNo}},
		},
		nil, 10000, timeDimensions)
	if err != nil {
		return nil, fmt.Errorf("cube %s SalesInWindow: %w", cube, err)
	}
	out := make(map[string]*WindowSaleRow, len(rows))
	for _, r := range rows {
		itemNo := asString(r, cube+".item_no")
		if itemNo == "" {
			continue
		}
		// 累加:cube 按 5 dim + oper_date(time) GROUP BY,同 item 多行
		//   sale_qty = sum of (oper_date 切片),必须累加,不能覆盖
		//   inv_snapshot = max,取大值(1h 内 inv 通常不变;若变取最新)
		if existing, ok := out[itemNo]; ok {
			existing.SaleQty += asFloat(r, cube+".sale_qty")
			if inv := asInt(r, cube+".inv_snapshot"); inv > existing.InvSnapshot {
				existing.InvSnapshot = inv
			}
			continue
		}
		out[itemNo] = &WindowSaleRow{
			ItemNo:       itemNo,
			ItemName:     asString(r, cube+".item_name"),
			Barcode:      asString(r, cube+".barcode"),
			SupplierName: asString(r, cube+".supplier_name"),
			SaleQty:      asFloat(r, cube+".sale_qty"),
			InvSnapshot:  asInt(r, cube+".inv_snapshot"),
		}
	}
	log.Printf("[restock] SalesInWindow(%s) [%s ~ %s] branch=%s items=%d",
		cube, from.Format("15:04"), to.Format("15:04"), branchNo, len(out))
	return out, nil
}

func rowToSnapshot(r map[string]any, branchNo string) *SkuSnapshot {
	return &SkuSnapshot{
		BranchNo:     branchNo,
		ItemNo:       asString(r, "sales_yesterday.item_no"),
		ItemName:     asString(r, "sales_yesterday.item_name"),
		Barcode:      asString(r, "sales_yesterday.barcode"),
		SupplierName: asString(r, "sales_yesterday.supplier_name"),
	}
}

func asString(m map[string]any, key string) string {
	if v, ok := m[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprintf("%v", v)
	}
	return ""
}

func asInt(m map[string]any, key string) int {
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int(math.Round(x))
	case int:
		return x
	case int64:
		return int(x)
	case string:
		// 2026-09-01 修复: cube 端 DECIMAL 列以字符串返回 (e.g. "47.0000"),
		// strconv.Atoi 不能解析小数串会返回 0, 导致 inv_snapshot 全部被存为 0
		// 改用 ParseFloat + 四舍五入, 既兼容整数串("12")也兼容小数串("12.47")
		if f, err := strconv.ParseFloat(x, 64); err == nil {
			return int(math.Round(f))
		}
		return 0
	}
	return 0
}

// asFloat 2026-08-31: 给 cube decimal 列用 (如 t_rm_saleflow.sale_qnty)
//   保留小数,业务层 service.go 再 math.Round
func asFloat(m map[string]any, key string) float64 {
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
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
	return 0
}
