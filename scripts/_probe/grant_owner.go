//go:build ignore
// +build ignore
package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func main() {
	conn, _ := pgx.Connect(context.Background(), "postgres://postgres:postgres@127.0.0.1:5432/collectai?sslmode=disable")
	defer conn.Close(context.Background())
	_, _ = conn.Exec(context.Background(), `INSERT INTO role_permissions (role_id, perm_id) VALUES ('owner', 'user:manage') ON CONFLICT DO NOTHING`)
	fmt.Println("owner +user:manage OK")
}
