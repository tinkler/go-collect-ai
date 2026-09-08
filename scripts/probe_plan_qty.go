//go:build ignore
// +build ignore

// probe_plan_qty.go - 直接测 AttachPlanQtyToRows 的 plan_qty 拼接
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tinkler/collect-ai/internal/model"
	"github.com/tinkler/collect-ai/internal/restock"
)

func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://postgres:postgres@127.0.0.1:5432/collectai?sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	store := restock.NewStore(pool)
	svc := &restock.Service{
		Store: store,
	}

	// 1) 查 need_purchase 拿到 supplier
	plans, _ := store.ListPendingNeedsBySupplier(ctx, "岑溪市博才晨光（文具文体系列）", "")
	fmt.Println("==== restock_need_purchase for 岑溪市博才晨光（文具文体系列） ====")
	for _, p := range plans {
		fmt.Printf("  #%d item_no=%s name=%s barcode=%s suggest_qty=%d\n",
			p.ID, p.ItemNo, p.ItemName, p.Barcode, p.SuggestQty)
	}

	// 2) 模拟 3 个 row, 其中 1 个匹配 (item_no)
	//   row1: 完全匹配 item_no=6971308040149 (6005宇恒铅笔)
	//   row2: 部分匹配 barcode
	//   row3: 完全不匹配
	qty1 := 99
	qty2 := 88
	qty3 := 77
	price := 1.5
	rows := []model.SkuRow{
		{RowID: 1, Seq: 1, RawBarcode: "6971308040149", RawName: "6005宇恒铅笔",
			MatchedBarcode: "6971308040149", MatchedName: "6005宇恒铅笔",
			MatchedSupp: "岑溪市博才晨光（文具文体系列）", Qty: &qty1, UnitPrice: &price},
		{RowID: 2, Seq: 2, RawBarcode: "1111111", RawName: "随便",
			MatchedBarcode: "", MatchedName: "随便",
			MatchedSupp: "岑溪市博才晨光（文具文体系列）", Qty: &qty2, UnitPrice: &price},
		{RowID: 3, Seq: 3, RawBarcode: "2222222", RawName: "另一个",
			MatchedBarcode: "2222222", MatchedName: "另一个",
			MatchedSupp: "其他供应商", Qty: &qty3, UnitPrice: &price},
	}

	fmt.Println("==== 调 AttachPlanQtyToRows ====")
	if err := svc.AttachPlanQtyToRows(ctx, "岑溪市博才晨光（文具文体系列）", rows); err != nil {
		fmt.Println("  err:", err)
	}

	fmt.Println("==== 拼接结果 ====")
	for _, r := range rows {
		planTxt := "(无)"
		if r.PlanQty != nil {
			planTxt = fmt.Sprintf("%d (item_no=%s name=%s)",
				*r.PlanQty, r.PlanItemNo, r.PlanItemName)
		}
		fmt.Printf("  row#%d item_no=%s raw_qty=%d → plan_qty=%s\n",
			r.RowID, r.MatchedBarcode, *r.Qty, planTxt)
	}
}
