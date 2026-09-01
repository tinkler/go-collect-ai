package promotionalert

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

// findRepoRoot 跟 agent/tools/testhelper 一致: 用 runtime.Caller 找 .env
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

func testDSN(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("PG_TEST_DSN"); v != "" {
		return v
	}
	if dsn, err := readEnvFile(".env"); err == nil {
		return dsn
	}
	root := findRepoRoot()
	if root != "" {
		if dsn, err := readEnvFile(filepath.Join(root, ".env")); err == nil {
			return dsn
		}
	}
	t.Skipf("PG_TEST_DSN 未设置且 .env 读不到")
	return ""
}

func testPool(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	dsn := testDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("PG 不可达: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("PG ping 失败: %v", err)
	}
	// 确保 promotion_fee 表存在 (W1 已 migrate, 幂等)
	if err := migratePromotionFee(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	cleanup := func() {
		cleanCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM promotion_fee WHERE supplier_name LIKE 't-%'`)
		pool.Close()
	}
	return pool, cleanup
}

func migratePromotionFee(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS promotion_fee (
			id              BIGSERIAL PRIMARY KEY,
			supplier_name   TEXT NOT NULL,
			kind            TEXT NOT NULL,
			amount          NUMERIC(12,2) NOT NULL,
			period_start    DATE NOT NULL,
			period_end      DATE NOT NULL,
			note            TEXT NOT NULL DEFAULT '',
			source          TEXT NOT NULL,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`)
	return err
}

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
		if !isASCII(line) {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		m[strings.TrimSpace(line[:eq])] = strings.TrimSpace(line[eq+1:])
	}
	host, user, db := m["PG_HOST"], m["PG_USER"], m["PG_DATABASE"]
	port := m["PG_PORT"]
	if port == "" {
		port = "5432"
	}
	if host == "" || user == "" || db == "" {
		return "", fmt.Errorf(".env 缺关键 PG_*")
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", user, m["PG_PASSWORD"], host, port, db), nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}
