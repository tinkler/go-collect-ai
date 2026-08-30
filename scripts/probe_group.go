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
	rows, _ := conn.Query(ctx, `SELECT column_name, data_type FROM information_schema.columns WHERE table_name='users' ORDER BY ordinal_position`)
	defer rows.Close()
	fmt.Println("==== users columns ====")
	for rows.Next() {
		var n, t string
		_ = rows.Scan(&n, &t)
		fmt.Printf("  %-15s %s\n", n, t)
	}
	rows2, _ := conn.Query(ctx, `SELECT id, name, role, "group" FROM users ORDER BY id`)
	defer rows2.Close()
	fmt.Println("==== users with group ====")
	for rows2.Next() {
		var id, name, role, grp string
		_ = rows2.Scan(&id, &name, &role, &grp)
		fmt.Printf("  %-12s %-20s role=%-8s group=%s\n", id, name, role, grp)
	}
}
