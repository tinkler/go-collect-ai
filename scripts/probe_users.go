//go:build ignore
// +build ignore

// probe users table
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, "postgres://postgres:postgres@127.0.0.1:5432/collectai?sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, "SELECT id, name, role, source, status, created_at FROM users ORDER BY id")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	fmt.Println("==== users ====")
	for rows.Next() {
		var id, name, role, source, status string
		var createdAt interface{}
		_ = rows.Scan(&id, &name, &role, &source, &status, &createdAt)
		fmt.Printf("  %-12s %-20s role=%-8s src=%-5s status=%-6s\n", id, name, role, source, status)
	}
	// restock tables
	for _, tbl := range []string{"restock_task", "restock_feedback", "restock_need_purchase"} {
		var n int
		_ = conn.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s", tbl)).Scan(&n)
		fmt.Printf("  %-22s %d\n", tbl, n)
	}
	// sample restock_need_purchase
	rows2, _ := conn.Query(ctx, "SELECT id, branch_no, item_no, item_name, supplier_name, suggest_qty, status FROM restock_need_purchase ORDER BY id LIMIT 5")
	defer rows2.Close()
	fmt.Println("==== restock_need_purchase sample ====")
	for rows2.Next() {
		var id int64
		var branch, item, name, sup, status string
		var qty int
		_ = rows2.Scan(&id, &branch, &item, &name, &sup, &qty, &status)
		fmt.Printf("  #%d  %s/%s  %-30s  sup=%s  qty=%d  %s\n", id, branch, item, name, sup, qty, status)
	}
}
