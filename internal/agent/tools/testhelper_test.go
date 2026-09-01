package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// findRepoRoot 用 runtime.Caller 拿到当前测试文件路径,向上找 .env(标志文件)
func findRepoRoot() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	dir := filepath.Dir(thisFile)
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, ".env")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// 测试用 DSN: 优先 env PG_TEST_DSN,否则从 .env 读
func testDSN(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("PG_TEST_DSN"); v != "" {
		return v
	}
	// 1) 试当前 cwd
	if dsn, err := readEnvFile(".env"); err == nil {
		return dsn
	}
	// 2) 试 repo 根
	root := findRepoRoot()
	if root != "" {
		if dsn, err := readEnvFile(filepath.Join(root, ".env")); err == nil {
			return dsn
		}
	}
	t.Skipf("PG_TEST_DSN 未设置且 .env 在 cwd 和 repo 根都读不到;设 PG_TEST_DSN 或在 repo 根放 .env")
	return ""
}

// testPool 起一个真 PG pool,跑 store.Migrate(幂等)
//   返回: pool + cleanup func
//   PG 不可达 / 无表权限 → Skip 而不是 Fail,这样 unit-test 默认还能跑
func testPool(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	dsn := testDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("PG 不可达 (%s): %v", maskDSN(dsn), err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("PG ping 失败: %v", err)
	}
	// 跑 migration(只关心 3 张新表,可能 store.Migrate 不可达 — 直接建 3 表的精简版)
	if err := migrateAgentTables(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate 失败: %v", err)
	}
	cleanup := func() {
		// 清空所有以 t- 开头的测试数据,不影响生产数据
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanCancel()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM supplier_policy WHERE supplier_name LIKE 't-%'`)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM special_calendar WHERE name LIKE 't-%' OR note LIKE 't-%'`)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM promotion_fee WHERE supplier_name LIKE 't-%'`)
		pool.Close()
	}
	return pool, cleanup
}

// migrateAgentTables 复刻 store.Migrate 里的 3 张表 DDL(避免 import 整个 store 包)
//   CREATE TABLE IF NOT EXISTS = 幂等,可重复跑
func migrateAgentTables(ctx context.Context, pool *pgxpool.Pool) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS supplier_policy (
			id              BIGSERIAL PRIMARY KEY,
			supplier_name   TEXT NOT NULL,
			key             TEXT NOT NULL,
			value           JSONB NOT NULL,
			source          TEXT NOT NULL,
			chat_id         TEXT NOT NULL DEFAULT '',
			message_id      TEXT NOT NULL DEFAULT '',
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (supplier_name, key)
		)`,
		`CREATE TABLE IF NOT EXISTS special_calendar (
			id              BIGSERIAL PRIMARY KEY,
			date            DATE NOT NULL,
			type            TEXT NOT NULL,
			name            TEXT NOT NULL,
			lead_days       INT NOT NULL DEFAULT 0,
			note            TEXT NOT NULL DEFAULT '',
			source          TEXT NOT NULL,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (date, type, name)
		)`,
		`CREATE TABLE IF NOT EXISTS promotion_fee (
			id              BIGSERIAL PRIMARY KEY,
			supplier_name   TEXT NOT NULL,
			kind            TEXT NOT NULL,
			amount          NUMERIC(12,2) NOT NULL,
			period_start    DATE NOT NULL,
			period_end      DATE NOT NULL,
			note            TEXT NOT NULL DEFAULT '',
			source          TEXT NOT NULL,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return fmt.Errorf("migrate: %w (sql: %.80s)", err, s)
		}
	}
	return nil
}

// readEnvFile 读 .env 拼成 DSN (PG_HOST/PG_PORT/PG_USER/PG_PASSWORD/PG_DATABASE)
//   简单 key=value 解析,只支持 ASCII (用户 .env 含中文 header,跳过)
func readEnvFile(path string) (string, error) {
	bs, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(bs), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 跳过含非 ASCII 的行(header)
		if !isASCII(line) {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := strings.TrimSpace(line[eq+1:])
		m[k] = v
	}
	host := m["PG_HOST"]
	port := m["PG_PORT"]
	user := m["PG_USER"]
	pass := m["PG_PASSWORD"]
	db := m["PG_DATABASE"]
	if host == "" || user == "" || db == "" {
		return "", fmt.Errorf(".env 缺关键 PG_* (host=%q user=%q db=%q)", host, user, db)
	}
	if port == "" {
		port = "5432"
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", user, pass, host, port, db), nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

// maskDSN 隐藏 DSN 里的密码(给日志用)
func maskDSN(dsn string) string {
	at := strings.Index(dsn, "@")
	if at < 0 {
		return dsn
	}
	// 找到 "://" 后第一个 ":"
	proto := strings.Index(dsn, "://")
	if proto < 0 || proto+3 > at {
		return dsn
	}
	colon := strings.Index(dsn[proto+3:at], ":")
	if colon < 0 {
		return dsn
	}
	realColon := proto + 3 + colon
	return dsn[:realColon+1] + "***" + dsn[at:]
}
