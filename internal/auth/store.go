package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// User 用户
type User struct {
	ID         string
	Name       string
	Role       string
	TenantID   string
	ExternalID string
	Source     string
	Status     string
	Group      string // 2026-08-30: 'floor' / 'office' / '' (H5 视图分组)
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// AuthSession 鉴权会话
type AuthSession struct {
	ID         string
	UserID     string
	RefreshHash string
	ExpiresAt  time.Time
	CreatedAt  time.Time
	RevokedAt  *time.Time
}

// Store 持久化层 (pgx)
type Store struct {
	pool *pgxpool.Pool
}

// NewStore
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// GetUserByID 按 id 查 user
func (s *Store) GetUserByID(ctx context.Context, id string) (*User, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, name, role, tenant_id, COALESCE(external_id,''), source, status, COALESCE("group",''), created_at, updated_at
		FROM users WHERE id = $1`, id)
	u := &User{}
	err := row.Scan(&u.ID, &u.Name, &u.Role, &u.TenantID, &u.ExternalID, &u.Source, &u.Status, &u.Group, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return u, nil
}

// UpsertUserByExternalID 企微登录时按 external_id (userid) 找/建用户
//   - 已存在: 返回
//   - 不存在: 插入, 默认 role=cashier (最小权限, 后续 admin 改)
func (s *Store) UpsertUserByExternalID(ctx context.Context, externalID, name string) (*User, error) {
	if externalID == "" {
		return nil, errors.New("external_id required")
	}
	// user_id: 企微 userid 通常是字母数字, 加 u_ 前缀防歧义
	userID := "u_" + externalID
	u, err := s.GetUserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u != nil {
		// 更新 name (企微改名时同步)
		_, _ = s.pool.Exec(ctx, `UPDATE users SET name=$1, updated_at=now() WHERE id=$2`, name, userID)
		u.Name = name
		return u, nil
	}
	// 新建, 默认 role=cashier
	_, err = s.pool.Exec(ctx, `
		INSERT INTO users (id, name, role, tenant_id, external_id, source)
		VALUES ($1, $2, 'cashier', 't_dev', $3, 'wecom')
		ON CONFLICT (id) DO NOTHING`,
		userID, name, externalID)
	if err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}
	return s.GetUserByID(ctx, userID)
}

// CreateSession 创建新 session
//   refreshHash 是 bcrypt(refreshToken)
//   返回 session_id (供外部记录 / 调试)
func (s *Store) CreateSession(ctx context.Context, userID, refreshHash string, expiresAt time.Time) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO auth_sessions (user_id, refresh_hash, expires_at)
		VALUES ($1, $2, $3)
		RETURNING id`,
		userID, refreshHash, expiresAt).Scan(&id)
	if err != nil {
		return "", err
	}
	return id, nil
}

// RevokeSessionsForUser 软撤销某 user 的所有 session (logout)
func (s *Store) RevokeSessionsForUser(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE auth_sessions
		SET revoked_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return err
}

// RevokeSessionByID 软撤销单个 session (refresh 失败时)
func (s *Store) RevokeSessionByID(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE auth_sessions SET revoked_at = now()
		WHERE id = $1 AND revoked_at IS NULL`, id)
	return err
}

// VerifyRefresh 验签: 查 user 未撤销未过期的 session, 找 refreshHash 匹配的那条
//   返回: session, user
func (s *Store) VerifyRefresh(ctx context.Context, userID, candidateHash string) (*AuthSession, *User, error) {
	// 拉这个 user 全部 active + 未过期 session, 然后 bcrypt 比对
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, refresh_hash, expires_at, created_at, revoked_at
		FROM auth_sessions
		WHERE user_id = $1
		  AND revoked_at IS NULL
		  AND expires_at > now()`, userID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		ss := &AuthSession{}
		if err := rows.Scan(&ss.ID, &ss.UserID, &ss.RefreshHash, &ss.ExpiresAt, &ss.CreatedAt, &ss.RevokedAt); err != nil {
			return nil, nil, err
		}
		// bcrypt 比对 (这步 CPU, 故意不在 SQL 里)
		if bcryptCompare(ss.RefreshHash, candidateHash) {
			u, err := s.GetUserByID(ctx, userID)
			if err != nil || u == nil {
				return nil, nil, err
			}
			return ss, u, nil
		}
	}
	return nil, nil, nil
}

// ============== RBAC 内存缓存 ==============
//
// 设计: 启动时 LoadAllRolePerms, 缓存在内存.
//   简单 sync.RWMutex + map[role]map[perm]bool
//   4 个 role × ~25 perm, 内存忽略不计
//   修改权限: 直接 SQL, 然后 Reload (暴露给运维 HTTP, 本期不实现)

var (
	rolePermsMu sync.RWMutex
	rolePerms   = map[string]map[string]bool{} // role -> set(perm) ; "*" 表示通配
)

// LoadAllRolePerms 加载 role_permissions 全表到内存
// 2026-08-30: 改读新表 (role_id, perm_id)
func (s *Store) LoadAllRolePerms(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT role_id, perm_id FROM role_permissions`)
	if err != nil {
		return err
	}
	defer rows.Close()
	tmp := map[string]map[string]bool{}
	for rows.Next() {
		var role, perm string
		if err := rows.Scan(&role, &perm); err != nil {
			return err
		}
		if tmp[role] == nil {
			tmp[role] = map[string]bool{}
		}
		tmp[role][perm] = true
	}
	rolePermsMu.Lock()
	rolePerms = tmp
	rolePermsMu.Unlock()
	return nil
}

// ReloadRolePerms 对外暴露 (运维 / 测试用)
func (s *Store) ReloadRolePerms(ctx context.Context) error {
	return s.LoadAllRolePerms(ctx)
}

// ============== Last Page (2026-08-31) ==============
//
// 用户最后访问的页面, 用于登录后自动跳回.
// 设计:
//   - 一行 per user, 覆盖式写入, 无历史
//   - 路径白名单在 handler 层校验, 这里只做持久化
//   - 不存在的 user_id 由 FK 保证, 不会孤儿

// GetLastPage 读某 user 最后访问的页 (无记录返回 "")
func (s *Store) GetLastPage(ctx context.Context, userID string) (string, error) {
	var page string
	err := s.pool.QueryRow(ctx, `
		SELECT last_page FROM user_last_page WHERE user_id = $1`, userID).Scan(&page)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return page, nil
}

// SetLastPage 写最后访问页 (UPSERT 幂等, 防前端重复点)
func (s *Store) SetLastPage(ctx context.Context, userID, page string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO user_last_page (user_id, last_page, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (user_id) DO UPDATE
		SET last_page = EXCLUDED.last_page, updated_at = now()`,
		userID, page)
	return err
}

// HasPerm 检查 role 是否有 perm
//   - role 有 "*" → 任何 perm 都过
//   - 否则精确匹配
func HasPerm(role, perm string) bool {
	rolePermsMu.RLock()
	defer rolePermsMu.RUnlock()
	perms, ok := rolePerms[role]
	if !ok {
		return false
	}
	if perms["*"] {
		return true
	}
	return perms[perm]
}

// AllRolesForTest 暴露给测试 (不要在生产代码用)
func AllRolesForTest() map[string]map[string]bool {
	rolePermsMu.RLock()
	defer rolePermsMu.RUnlock()
	out := make(map[string]map[string]bool, len(rolePerms))
	for k, v := range rolePerms {
		cp := make(map[string]bool, len(v))
		for kk, vv := range v {
			cp[kk] = vv
		}
		out[k] = cp
	}
	return out
}
