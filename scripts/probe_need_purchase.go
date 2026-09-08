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
	rows, _ := conn.Query(ctx, "SELECT id, branch_no, item_no, item_name, supplier_name, suggest_qty, trigger_kind, status FROM restock_need_purchase ORDER BY id")
	defer rows.Close()
	fmt.Println("==== restock_need_purchase ====")
	for rows.Next() {
		var id int64
		var branch, ino, iname, sup, tk, st string
		var qty int
		_ = rows.Scan(&id, &branch, &ino, &iname, &sup, &qty, &tk, &st)
		fmt.Printf("  #%d  %s  %s  %-30s  sup=%s  qty=%d  %s/%s\n", id, branch, ino, iname, sup, qty, tk, st)
	}
	rows2, _ := conn.Query(ctx, "SELECT task_id, item_no, item_name, status, push_count FROM restock_task WHERE status IN ('acked','short') ORDER BY last_update_at DESC LIMIT 10")
	defer rows2.Close()
	fmt.Println("==== restock_task (acked/short) ====")
	for rows2.Next() {
		var tid, ino, iname, st string
		var pc int
		_ = rows2.Scan(&tid, &ino, &iname, &st, &pc)
		fmt.Printf("  %s  %s  %-30s  %s  push=%d\n", tid, ino, iname, st, pc)
	}
	rows3, _ := conn.Query(ctx, "SELECT task_id, feedback_type, feedback_user, feedback_time FROM restock_feedback ORDER BY id DESC LIMIT 10")
	defer rows3.Close()
	fmt.Println("==== restock_feedback ====")
	for rows3.Next() {
		var tid, ft, fu string
		var ftm interface{}
		_ = rows3.Scan(&tid, &ft, &fu, &ftm)
		fmt.Printf("  %s  %s  user=%s\n", tid, ft, fu)
	}
}
