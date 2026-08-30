//go:build ignore
// +build ignore
package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()
	conn, _ := pgx.Connect(ctx, "postgres://postgres:postgres@127.0.0.1:5432/collectai?sslmode=disable")
	defer conn.Close(ctx)

	// 删所有 rbac 表 (顺序无关, CASCADE 全清)
	for _, t := range []string{
		"role_permissions", "user_roles", "permission_audit",
		"roles", "permissions", "wecom_departments",
	} {
		_, _ = conn.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s CASCADE`, t))
	}
	fmt.Println("dropped all rbac tables")
}
