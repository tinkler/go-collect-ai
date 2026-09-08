// 临时诊断工具: 拉最近 1 个 hengyi 公司的 session 全部 rows, 看 status 分布 / barcode 长度 / 顺序
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()
	dsn := "postgres://postgres:postgres@127.0.0.1:5432/collectai?sslmode=disable"
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close(ctx)

	// 找最近一个恒怡公司的 session
	var sessionID string
	err = conn.QueryRow(ctx, `
		SELECT id FROM parse_session
		WHERE supplier_name = '恒怡 公司'
		ORDER BY created_at DESC LIMIT 1
	`).Scan(&sessionID)
	if err != nil {
		log.Fatal("查 session: ", err)
	}
	fmt.Printf("=== session_id: %s ===\n\n", sessionID)

	// 拉 rows
	rows, err := conn.Query(ctx, `
		SELECT seq, image_index, raw_barcode, raw_name, raw_qty,
		       COALESCE(matched_barcode,''), COALESCE(matched_name,''),
		       COALESCE(status,''), is_new, qty
		FROM parse_row
		WHERE session_id = $1
		ORDER BY image_index, seq
	`, sessionID)
	if err != nil {
		log.Fatal("查 rows: ", err)
	}
	defer rows.Close()

	fmt.Printf("%-4s %-3s %-14s %-25s %-6s %-14s %-25s %-30s %-3s %s\n",
		"seq", "img", "raw_bc", "raw_name", "raw_q", "match_bc", "match_name", "status", "new", "qty")
	fmt.Println("--------------------------------------------------------------------------------------------------------------------------------------------")

	statusCount := make(map[string]int)
	bcLenCount := make(map[int]int)
	for rows.Next() {
		var seq, imgIdx int
		var rawBc, rawName, rawQty sql.NullString
		var matchBc, matchName, status sql.NullString
		var isNew bool
		var qty sql.NullInt64
		if err := rows.Scan(&seq, &imgIdx, &rawBc, &rawName, &rawQty, &matchBc, &matchName, &status, &isNew, &qty); err != nil {
			log.Fatal(err)
		}
		rawBcStr := rawBc.String
		if len(rawBcStr) > 13 {
			rawBcStr = rawBcStr[:13] + "..."
		}
		matchBcStr := matchBc.String
		if len(matchBcStr) > 13 {
			matchBcStr = matchBcStr[:13] + "..."
		}
		rawNameStr := rawName.String
		if len(rawNameStr) > 24 {
			rawNameStr = rawNameStr[:24]
		}
		matchNameStr := matchName.String
		if len(matchNameStr) > 24 {
			matchNameStr = matchNameStr[:24]
		}
		fmt.Printf("%-4d %-3d %-14s %-25s %-6s %-14s %-25s %-30s %-3v %v\n",
			seq, imgIdx, rawBcStr, rawNameStr, rawQty.String,
			matchBcStr, matchNameStr, status.String, isNew, qty.Int64)

		statusCount[status.String]++
		bcLenCount[len(rawBc.String)]++
	}

	fmt.Println()
	fmt.Println("=== status 分布 ===")
	for k, v := range statusCount {
		fmt.Printf("  %-30s %d\n", k, v)
	}
	fmt.Println()
	fmt.Println("=== raw_barcode 长度分布 ===")
	for k, v := range bcLenCount {
		fmt.Printf("  len=%d  count=%d\n", k, v)
	}

	os.Exit(0)
}
