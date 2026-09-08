// sqltool 临时 SQL 查询工具 (本地调试用)
//   go run ./cmd/sqltool -sql "SELECT TOP 5 * FROM t_rm_saleflow"
//   go run ./cmd/sqltool -file query.sql
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/microsoft/go-mssqldb"
)

func main() {
	sqlStr := flag.String("sql", "", "SQL to run (or use -file)")
	sqlFile := flag.String("file", "", "SQL file to run")
	dsn := flag.String("dsn", "sqlserver://ai:ai6725.@127.0.0.1:1433?database=hbposv10&encrypt=disable&trustservercertificate=true", "MSSQL DSN")
	flag.Parse()

	var q string
	if *sqlFile != "" {
		b, err := os.ReadFile(*sqlFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read file: %v\n", err)
			os.Exit(1)
		}
		q = string(b)
	} else if *sqlStr != "" {
		q = *sqlStr
	} else {
		fmt.Fprintln(os.Stderr, "specify -sql or -file")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := sql.Open("mssql", *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	fmt.Println(strings.Join(cols, " | "))
	fmt.Println(strings.Repeat("-", 80))

	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			fmt.Fprintf(os.Stderr, "scan: %v\n", err)
			continue
		}
		parts := make([]string, len(cols))
		for i, v := range vals {
			if v == nil {
				parts[i] = "NULL"
			} else if b, ok := v.([]byte); ok {
				parts[i] = string(b)
			} else {
				parts[i] = fmt.Sprintf("%v", v)
			}
		}
		fmt.Println(strings.Join(parts, " | "))
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "rows: %v\n", err)
		os.Exit(1)
	}
}
