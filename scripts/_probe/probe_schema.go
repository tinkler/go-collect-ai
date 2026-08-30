//go:build ignore
// +build ignore
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
	// 列出所有 rbac 相关表
	rows, _ := conn.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('roles','permissions','role_permissions','user_roles','wecom_departments','permission_audit') ORDER BY table_name`)
	defer rows.Close()
	fmt.Println("==== tables ====")
	for rows.Next() {
		var t string
		_ = rows.Scan(&t)
		fmt.Printf("  %s\n", t)
	}
	// 每个表的列
	for _, t := range []string{"roles", "permissions", "role_permissions", "user_roles", "wecom_departments", "permission_audit"} {
		fmt.Printf("\n==== %s columns ====\n", t)
		r2, _ := conn.Query(ctx, `SELECT column_name, data_type FROM information_schema.columns WHERE table_name=$1 ORDER BY ordinal_position`, t)
		if r2 == nil {
			fmt.Println("  (no rows)")
			continue
		}
		for r2.Next() {
			var c, ty string
			_ = r2.Scan(&c, &ty)
			fmt.Printf("  %-25s %s\n", c, ty)
		}
		r2.Close()
	}
}
