//go:build ignore
// +build ignore

// probe_session.go - 模拟完整 receive-session 流程, 验证 plan_qty 拼接
package main

import (
	"context"
	"encoding/json"
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
	svc := &restock.Service{Store: store}

	// 1) 准备 rows 模拟 OCR 识别结果
	qty1, qty2, qty3 := 10, 5, 8
	rows := []model.SkuRow{
		{RowID: 100, Seq: 1, RawBarcode: "6971308040149", RawName: "6005宇恒铅笔",
			MatchedBarcode: "6971308040149", MatchedName: "6005宇恒铅笔",
			MatchedSupp: "岑溪市博才晨光（文具文体系列）", Qty: &qty1},
		{RowID: 101, Seq: 2, RawBarcode: "1111111", RawName: "杂物A",
			MatchedBarcode: "1111111", MatchedName: "杂物A",
			MatchedSupp: "岑溪市博才晨光（文具文体系列）", Qty: &qty2},
		{RowID: 102, Seq: 3, RawBarcode: "2222222", RawName: "杂物B",
			MatchedBarcode: "2222222", MatchedName: "杂物B",
			MatchedSupp: "其他供应商", Qty: &qty3},
	}

	// 2) 调 AttachPlanQtyToRows (GetSession 的核心逻辑)
	supplier := "岑溪市博才晨光（文具文体系列）"
	if err := svc.AttachPlanQtyToRows(ctx, supplier, rows); err != nil {
		log.Fatal(err)
	}

	// 3) 模拟 JSON 序列化 (实际 GetSession 响应)
	out, _ := json.MarshalIndent(rows, "", "  ")
	fmt.Println("==== rows (GetSession 响应) ====")
	fmt.Println(string(out))

	// 4) 关键断言
	pass := true
	if rows[0].PlanQty == nil || *rows[0].PlanQty != 1 {
		fmt.Println("  ❌ row[0] (6005宇恒铅笔) plan_qty 应该是 1, 实际", rows[0].PlanQty)
		pass = false
	} else {
		fmt.Println("  ✅ row[0] (6005宇恒铅笔) plan_qty = 1, 拼接成功 (来自 need_purchase #1)")
	}
	if rows[1].PlanQty != nil {
		fmt.Println("  ❌ row[1] 不应有 plan_qty")
		pass = false
	} else {
		fmt.Println("  ✅ row[1] (杂物A) 无 plan_qty (无匹配计划)")
	}
	if rows[2].PlanQty != nil {
		fmt.Println("  ❌ row[2] 不应有 plan_qty")
		pass = false
	} else {
		fmt.Println("  ✅ row[2] (杂物B) 无 plan_qty (其他供应商)")
	}

	if pass {
		fmt.Println("\n🎉 plan_qty 拼接测试全部通过")
	} else {
		fmt.Println("\n❌ 部分断言失败")
	}
}
