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
	for _, t := range []string{"roles", "permissions", "role_permissions", "user_roles"} {
		var n int
		_ = conn.QueryRow(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", t)).Scan(&n)
		fmt.Printf("%-20s %d\n", t, n)
	}
}
