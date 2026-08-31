package restock

import (
	"context"
	"fmt"
	"log"
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
type WindowSaleRow struct {
	ItemNo       string
	ItemName     string
	Barcode      string
	SupplierName string
	SaleQty      int
	InvSnapshot  int
}

// SalesInWindow 拉指定时间窗口的销量 + 当前库存快照
//   适配 cube-agent-server 上 display_restock_window cube
//   from/to 用 mssql 通用 datetime 格式 (yyyy-MM-dd HH:mm:ss)
//   ⚠️ 不要用 RFC3339 / ISO 8601:SQL Server 2008 R2 无法解析 '2026-08-31T12:34:56Z'
//   返回: map[item_no] -> WindowSaleRow(SaleQty 是窗口内合计,InvSnapshot 是当前值)
//   性能:< 10s / 次(子 session 实测 100-208ms,3.18M 行)
func (q *CubeQuerier) SalesInWindow(ctx context.Context, branchNo string, from, to time.Time) (map[string]*WindowSaleRow, error) {
	cube := q.cfg.DisplayRestockCubeName
	if cube == "" {
		cube = "display_restock_window"
	}
	// mssql 2008 R2 通用 datetime 格式 (任何 locale 都认)
	const mssqlTimeFmt = "2006-01-02 15:04:05"
	timeDimensions := []map[string]any{
		{
			"dimension": cube + ".oper_date",
			"dateRange": []string{
				from.UTC().Format(mssqlTimeFmt),
				to.UTC().Format(mssqlTimeFmt),
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
		out[itemNo] = &WindowSaleRow{
			ItemNo:       itemNo,
			ItemName:     asString(r, cube+".item_name"),
			Barcode:      asString(r, cube+".barcode"),
			SupplierName: asString(r, cube+".supplier_name"),
			SaleQty:      asInt(r, cube+".sale_qty"),
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
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}
