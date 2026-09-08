package rbac

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

// RBAC 表(users / user_roles / roles / role_permissions)由部署环境手工建好,
// store.Migrate 不包含这些表;测试前若表不存在则 t.Skip 而非 Fail.
//
// 测试用前缀 't_role_' / 't_u_' 避免污染生产数据,结束时清理.

const (
	testRoleA = "t_role_a"
	testRoleB = "t_role_b"
	testUser  = "t_u_grant_test"
)

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
	if root := findRepoRoot(); root != "" {
		if dsn, err := readEnvFile(filepath.Join(root, ".env")); err == nil {
			return dsn
		}
	}
	t.Skipf("PG_TEST_DSN 未设置且 .env 读不到")
	return ""
}

func readEnvFile(path string) (string, error) {
	bs, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(bs), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !isASCII(line) {
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

// testPool 起一个真 PG pool;PG 不可达或 RBAC 表不存在则 Skip.
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
	// 检查 RBAC 表是否齐 (Migrate 不包含这些表)
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM information_schema.tables
		WHERE table_name IN ('users','user_roles','roles')
	`).Scan(&n); err != nil {
		pool.Close()
		t.Skipf("无法查 information_schema: %v", err)
	}
	if n < 3 {
		pool.Close()
		t.Skipf("RBAC 表未建 (users/user_roles/roles),跳过")
	}
	cleanup := func() {
		cctx, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		_, _ = pool.Exec(cctx, `DELETE FROM user_roles WHERE user_id=$1`, testUser)
		_, _ = pool.Exec(cctx, `DELETE FROM users WHERE id=$1`, testUser)
		_, _ = pool.Exec(cctx, `DELETE FROM role_permissions WHERE role_id IN ($1,$2)`, testRoleA, testRoleB)
		_, _ = pool.Exec(cctx, `DELETE FROM roles WHERE id IN ($1,$2)`, testRoleA, testRoleB)
		pool.Close()
	}
	return pool, cleanup
}

// seedUser 建测试 user + 两个测试 role,确保起始状态干净.
func seedUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	// 两个测试 role (is_builtin=false, 避免冲突真实内置角色)
	for _, r := range []string{testRoleA, testRoleB} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO roles (id, name, scope, is_builtin) VALUES ($1, $1, 'platform', false)
			ON CONFLICT (id) DO NOTHING
		`, r); err != nil {
			t.Fatalf("seed role %s: %v", r, err)
		}
	}
	// 测试 user, 起始 role='cashier' (登录默认)
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, name, role, tenant_id, source, status)
		VALUES ($1, 'grant_test', 'cashier', 't_dev', 'test', 'active')
		ON CONFLICT (id) DO UPDATE SET role='cashier', status='active', left_at=NULL
	`, testUser); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM user_roles WHERE user_id=$1`, testUser); err != nil {
		t.Fatalf("clear user_roles: %v", err)
	}
}

func userRoleNow(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var role string
	if err := pool.QueryRow(ctx, `SELECT role FROM users WHERE id=$1`, testUser).Scan(&role); err != nil {
		t.Fatalf("query users.role: %v", err)
	}
	return role
}

// TestGrantRole_SyncsToUsersRole_NoExistingPrimary: admin 没勾 IsPrimary,
// user 当前无任何 primary role → 兜底逻辑应把本次 grant 升为 primary,
// 且 users.role 被同步成新 role_id.
func TestGrantRole_SyncsToUsersRole_NoExistingPrimary(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	seedUser(t, ctx, pool)

	s := &Store{Pool: pool}
	if err := s.GrantRole(ctx, &UserRole{
		UserID: testUser, RoleID: testRoleA,
		ScopeType: "all", IsPrimary: false, GrantedBy: "tester",
	}); err != nil {
		t.Fatalf("GrantRole: %v", err)
	}
	if got := userRoleNow(t, ctx, pool); got != testRoleA {
		t.Errorf("users.role = %q, want %q (兜底应升本次为 primary 并同步)", got, testRoleA)
	}
	// 校验 user_roles 行的 is_primary=true (兜底升上去了)
	var isPrimary bool
	if err := pool.QueryRow(ctx,
		`SELECT is_primary FROM user_roles WHERE user_id=$1 AND role_id=$2 AND scope_type='all'`,
		testUser, testRoleA).Scan(&isPrimary); err != nil {
		t.Fatalf("query user_roles: %v", err)
	}
	if !isPrimary {
		t.Errorf("user_roles.is_primary = false, want true (无既有 primary 时应升本次)")
	}
}

// TestGrantRole_PrimaryClearsOtherPrimary: 新 primary=true grant 应清掉旧 primary.
func TestGrantRole_PrimaryClearsOtherPrimary(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	seedUser(t, ctx, pool)

	s := &Store{Pool: pool}
	// 第一笔 primary=role_a
	if err := s.GrantRole(ctx, &UserRole{UserID: testUser, RoleID: testRoleA, ScopeType: "all", IsPrimary: true, GrantedBy: "tester"}); err != nil {
		t.Fatalf("GrantRole A: %v", err)
	}
	// 第二笔 primary=role_b, 应清掉 role_a 的 primary
	if err := s.GrantRole(ctx, &UserRole{UserID: testUser, RoleID: testRoleB, ScopeType: "all", IsPrimary: true, GrantedBy: "tester"}); err != nil {
		t.Fatalf("GrantRole B: %v", err)
	}
	if got := userRoleNow(t, ctx, pool); got != testRoleB {
		t.Errorf("users.role = %q, want %q", got, testRoleB)
	}
	// role_a 应该 is_primary=false (被清掉)
	var aPrimary bool
	if err := pool.QueryRow(ctx, `SELECT is_primary FROM user_roles WHERE user_id=$1 AND role_id=$2 AND scope_type='all'`, testUser, testRoleA).Scan(&aPrimary); err != nil {
		t.Fatalf("query role_a: %v", err)
	}
	if aPrimary {
		t.Errorf("role_a.is_primary = true, want false (新 primary 应清旧)")
	}
}

// TestRevokeRole_Primary_FallsBackToCashier: 撤销当前主角色且无其它有效角色 → users.role 退回 'cashier'.
func TestRevokeRole_Primary_FallsBackToCashier(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	seedUser(t, ctx, pool)

	s := &Store{Pool: pool}
	if err := s.GrantRole(ctx, &UserRole{UserID: testUser, RoleID: testRoleA, ScopeType: "all", IsPrimary: true, GrantedBy: "tester"}); err != nil {
		t.Fatalf("GrantRole: %v", err)
	}
	if got := userRoleNow(t, ctx, pool); got != testRoleA {
		t.Fatalf("pre-revoke users.role = %q, want %q", got, testRoleA)
	}
	if err := s.RevokeRole(ctx, testUser, testRoleA, "all", ""); err != nil {
		t.Fatalf("RevokeRole: %v", err)
	}
	if got := userRoleNow(t, ctx, pool); got != "cashier" {
		t.Errorf("users.role = %q, want 'cashier' (撤销唯一主角色应回退默认)", got)
	}
}

// TestRevokeRole_Primary_FallsBackToNext: 撤销主角色但有其它剩余有效角色 → 回填到那个.
func TestRevokeRole_Primary_FallsBackToNext(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	seedUser(t, ctx, pool)

	s := &Store{Pool: pool}
	// 先 grant role_a 为 primary
	if err := s.GrantRole(ctx, &UserRole{UserID: testUser, RoleID: testRoleA, ScopeType: "all", IsPrimary: true, GrantedBy: "tester"}); err != nil {
		t.Fatalf("GrantRole A: %v", err)
	}
	// 再 grant role_b (IsPrimary=false, role_a 已是 primary → role_b 不会升)
	if err := s.GrantRole(ctx, &UserRole{UserID: testUser, RoleID: testRoleB, ScopeType: "all", IsPrimary: false, GrantedBy: "tester"}); err != nil {
		t.Fatalf("GrantRole B: %v", err)
	}
	if got := userRoleNow(t, ctx, pool); got != testRoleA {
		t.Fatalf("pre-revoke users.role = %q, want %q (role_b 不应抢 primary)", got, testRoleA)
	}
	// 撤销 role_a → 回填 role_b
	if err := s.RevokeRole(ctx, testUser, testRoleA, "all", ""); err != nil {
		t.Fatalf("RevokeRole A: %v", err)
	}
	if got := userRoleNow(t, ctx, pool); got != testRoleB {
		t.Errorf("users.role = %q, want %q (撤销主角色应回填剩余角色)", got, testRoleB)
	}
}

// TestRevokeRole_NonPrimary_DoesNotChangeUsersRole: 撤销非主角色时 users.role 不变.
func TestRevokeRole_NonPrimary_DoesNotChangeUsersRole(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	seedUser(t, ctx, pool)

	s := &Store{Pool: pool}
	if err := s.GrantRole(ctx, &UserRole{UserID: testUser, RoleID: testRoleA, ScopeType: "all", IsPrimary: true, GrantedBy: "tester"}); err != nil {
		t.Fatalf("GrantRole A: %v", err)
	}
	if err := s.GrantRole(ctx, &UserRole{UserID: testUser, RoleID: testRoleB, ScopeType: "all", IsPrimary: false, GrantedBy: "tester"}); err != nil {
		t.Fatalf("GrantRole B: %v", err)
	}
	// 撤销 role_b (非主), users.role 应保持 role_a
	if err := s.RevokeRole(ctx, testUser, testRoleB, "all", ""); err != nil {
		t.Fatalf("RevokeRole B: %v", err)
	}
	if got := userRoleNow(t, ctx, pool); got != testRoleA {
		t.Errorf("users.role = %q, want %q (撤销非主角色不应影响 users.role)", got, testRoleA)
	}
}
