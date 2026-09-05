// Package rbac store - PG repository
package rbac

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	Pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{Pool: pool}
}

// ============== Role ==============

// ListRoles returns all roles with permission count + user count.
func (s *Store) ListRoles(ctx context.Context) ([]*Role, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT r.id, r.name, r.scope, COALESCE(r.description,''), r.is_builtin, r.created_at,
			(SELECT COUNT(*) FROM role_permissions WHERE role_id=r.id),
			(SELECT COUNT(*) FROM user_roles WHERE role_id=r.id)
		FROM roles r
		ORDER BY r.is_builtin DESC, r.id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Role
	for rows.Next() {
		r := &Role{}
		var permCount, userCount int
		if err := rows.Scan(&r.ID, &r.Name, &r.Scope, &r.Description, &r.IsBuiltin, &r.CreatedAt, &permCount, &userCount); err != nil {
			return nil, err
		}
		// PermCount not stored in Role struct, drop it (UI counts permissions via GetRolePermissions)
		_ = permCount
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetRole(ctx context.Context, id string) (*Role, error) {
	row := s.Pool.QueryRow(ctx, `SELECT id, name, scope, COALESCE(description,''), is_builtin, created_at FROM roles WHERE id=$1`, id)
	r := &Role{}
	if err := row.Scan(&r.ID, &r.Name, &r.Scope, &r.Description, &r.IsBuiltin, &r.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return r, nil
}

func (s *Store) CreateRole(ctx context.Context, r *Role) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO roles (id, name, scope, description, is_builtin) VALUES ($1,$2,$3,$4,false)
		ON CONFLICT (id) DO NOTHING
	`, r.ID, r.Name, r.Scope, r.Description)
	return err
}

func (s *Store) UpdateRole(ctx context.Context, id string, name, scope, description string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE roles SET name=$2, scope=$3, description=$4 WHERE id=$1`, id, name, scope, description)
	return err
}

func (s *Store) DeleteRole(ctx context.Context, id string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM roles WHERE id=$1 AND is_builtin=false`, id)
	return err
}

// ============== Permission ==============

func (s *Store) ListPermissions(ctx context.Context) ([]*Permission, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, domain, action, COALESCE(description,'') FROM permissions ORDER BY domain, action`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Permission
	for rows.Next() {
		p := &Permission{}
		if err := rows.Scan(&p.ID, &p.Domain, &p.Action, &p.Description); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetRolePermissions(ctx context.Context, roleID string) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT perm_id FROM role_permissions WHERE role_id=$1 ORDER BY perm_id`, roleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) SetRolePermissions(ctx context.Context, roleID string, perms []string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM role_permissions WHERE role_id=$1`, roleID); err != nil {
		return err
	}
	for _, p := range perms {
		if _, err := tx.Exec(ctx, `INSERT INTO role_permissions (role_id, perm_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, roleID, p); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ============== User Role ==============

// GrantRole grants role to user. Idempotent.
//
// 单角色同步 (2026-09-05): 鉴权热路径只读 users.role 单字段 + rolePerms 内存缓存,
// 不读 user_roles 表;因此 GrantRole 必须把生效的 primary role 同步写到 users.role,
// 否则授权后重新登录 / 重启后端都不生效 (见 router.go#L156-176 的 RequirePerm 链路).
//
// 规则:
//   - IsPrimary=true: 先清空该 user 其他 is_primary=true 行 (保证 primary 唯一)
//   - UPSERT user_roles
//   - 若 user 当前无任何 is_primary=true 的有效角色 → 把本次 grant 升为 primary
//     (覆盖 admin 前端不勾 IsPrimary 的常见场景, 至少让一个角色生效)
//   - 同步 users.role = 该 user 当前有效 primary role_id
func (s *Store) GrantRole(ctx context.Context, ur *UserRole) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if ur.IsPrimary {
		if _, err := tx.Exec(ctx, `UPDATE user_roles SET is_primary=false WHERE user_id=$1 AND is_primary=true`, ur.UserID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO user_roles (user_id, role_id, scope_type, scope_id, is_primary, granted_by, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (user_id, role_id, scope_type, scope_id) DO UPDATE SET
			is_primary = EXCLUDED.is_primary,
			granted_by = EXCLUDED.granted_by,
			expires_at = EXCLUDED.expires_at
	`, ur.UserID, ur.RoleID, ur.ScopeType, ur.ScopeID, ur.IsPrimary, ur.GrantedBy, ur.ExpiresAt); err != nil {
		return err
	}
	// 兜底: 若 user 当前没有 is_primary=true 的有效角色, 把本次 grant 升为 primary
	//   (admin 前端没勾 IsPrimary 时, 至少让一个角色生效, 避免授权后仍无权限)
	if _, err := tx.Exec(ctx, `
		UPDATE user_roles ur SET is_primary=true
		WHERE user_id=$1 AND role_id=$2 AND scope_type=$3 AND scope_id=$4
		  AND NOT EXISTS (
		    SELECT 1 FROM user_roles
		    WHERE user_id=$1 AND is_primary=true
		      AND (expires_at IS NULL OR expires_at > now())
		  )
	`, ur.UserID, ur.RoleID, ur.ScopeType, ur.ScopeID); err != nil {
		return err
	}
	// 同步 users.role = 当前有效 primary role_id (无则保留原值)
	if _, err := tx.Exec(ctx, `
		UPDATE users SET
			role = COALESCE((
				SELECT role_id FROM user_roles
				WHERE user_id=$1 AND is_primary=true
				  AND (expires_at IS NULL OR expires_at > now())
				ORDER BY granted_at DESC LIMIT 1
			), users.role),
			updated_at = now()
		WHERE id=$1
	`, ur.UserID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RevokeRole 删除 user_roles 绑定, 并在必要时回填 users.role.
//
// 单角色同步 (2026-09-05): 若被删的 role_id == users.role (即当前生效的主角色),
// 需从剩余有效角色里挑一个回填: is_primary=true 优先, 否则 granted_at 最早的;
// 都没有则退回 'cashier' (登录时的默认最小角色).
func (s *Store) RevokeRole(ctx context.Context, userID, roleID, scopeType, scopeID string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var currentRole string
	_ = tx.QueryRow(ctx, `SELECT COALESCE(role,'') FROM users WHERE id=$1`, userID).Scan(&currentRole)

	if _, err := tx.Exec(ctx, `
		DELETE FROM user_roles
		WHERE user_id=$1 AND role_id=$2 AND scope_type=$3 AND scope_id=$4
	`, userID, roleID, scopeType, scopeID); err != nil {
		return err
	}

	// 被删的就是 users.role 当前值 → 需要回填
	if currentRole == roleID {
		var newRole string
		err := tx.QueryRow(ctx, `
			SELECT role_id FROM user_roles
			WHERE user_id=$1 AND (expires_at IS NULL OR expires_at > now())
			ORDER BY is_primary DESC, granted_at ASC
			LIMIT 1
		`, userID).Scan(&newRole)
		if err != nil || newRole == "" {
			newRole = "cashier" // 退回登录默认角色 (UpsertUserByExternalID 的兜底)
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET role=$2, updated_at=now() WHERE id=$1`, userID, newRole); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ListUserRoles returns active (non-expired) roles for user.
func (s *Store) ListUserRoles(ctx context.Context, userID string) ([]UserRole, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT ur.user_id, ur.role_id, ur.scope_type, ur.scope_id, ur.is_primary, ur.granted_by, ur.granted_at, ur.expires_at,
			COALESCE(r.name, '')
		FROM user_roles ur
		LEFT JOIN roles r ON r.id = ur.role_id
		WHERE ur.user_id = $1
		  AND (ur.expires_at IS NULL OR ur.expires_at > now())
		ORDER BY ur.is_primary DESC, ur.granted_at
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserRole
	for rows.Next() {
		var ur UserRole
		if err := rows.Scan(&ur.UserID, &ur.RoleID, &ur.ScopeType, &ur.ScopeID, &ur.IsPrimary, &ur.GrantedBy, &ur.GrantedAt, &ur.ExpiresAt, &ur.RoleName); err != nil {
			return nil, err
		}
		out = append(out, ur)
	}
	return out, rows.Err()
}

// ListUsersWithRoles returns user list + their roles.
func (s *Store) ListUsersWithRoles(ctx context.Context, search, status string, limit int) ([]*UserWithRoles, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `
		SELECT u.id, u.name, u.role, COALESCE(u."group",''), COALESCE(u.mobile,''),
			u.department_id, COALESCE(u.department_path,''), COALESCE(u.position,''),
			COALESCE(u.external_id,''), u.source, u.status,
			u.hired_at, u.left_at, u.sync_at
		FROM users u
		WHERE 1=1
	`
	args := []any{}
	if search != "" {
		args = append(args, "%"+search+"%")
		q += " AND (u.name ILIKE $1 OR u.id ILIKE $1 OR u.mobile ILIKE $1)"
	}
	if status == "active" {
		q += " AND u.left_at IS NULL"
	} else if status == "left" {
		q += " AND u.left_at IS NOT NULL"
	}
	q += " ORDER BY u.left_at NULLS FIRST, u.id LIMIT $" + strconv.Itoa(len(args)+1)
	args = append(args, limit)
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*UserWithRoles
	for rows.Next() {
		u := &UserWithRoles{}
		if err := rows.Scan(&u.ID, &u.Name, &u.Role, &u.Group, &u.Mobile,
			&u.DepartmentID, &u.DepartmentPath, &u.Position,
			&u.ExternalID, &u.Source, &u.Status,
			&u.HiredAt, &u.LeftAt, &u.SyncAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	// Hydrate roles
	for _, u := range out {
		roles, _ := s.ListUserRoles(ctx, u.ID)
		u.Roles = roles
		if u.PrimaryRole == "" && len(roles) > 0 {
			u.PrimaryRole = roles[0].RoleID
		}
	}
	return out, rows.Err()
}

// GetUserWithRoles returns single user with roles.
func (s *Store) GetUserWithRoles(ctx context.Context, userID string) (*UserWithRoles, error) {
	row := s.Pool.QueryRow(ctx, `
		SELECT u.id, u.name, u.role, COALESCE(u."group",''), COALESCE(u.mobile,''),
			u.department_id, COALESCE(u.department_path,''), COALESCE(u.position,''),
			COALESCE(u.external_id,''), u.source, u.status,
			u.hired_at, u.left_at, u.sync_at
		FROM users u WHERE u.id=$1
	`, userID)
	u := &UserWithRoles{}
	if err := row.Scan(&u.ID, &u.Name, &u.Role, &u.Group, &u.Mobile,
		&u.DepartmentID, &u.DepartmentPath, &u.Position,
		&u.ExternalID, &u.Source, &u.Status,
		&u.HiredAt, &u.LeftAt, &u.SyncAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	u.Roles, _ = s.ListUserRoles(ctx, userID)
	if u.PrimaryRole == "" && len(u.Roles) > 0 {
		u.PrimaryRole = u.Roles[0].RoleID
	}
	return u, nil
}

func (s *Store) MarkLeft(ctx context.Context, userID string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE users SET left_at=NOW(), status='inactive', "group"='' WHERE id=$1`, userID)
	return err
}

func (s *Store) RestoreUser(ctx context.Context, userID string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE users SET left_at=NULL, status='active' WHERE id=$1`, userID)
	return err
}

func (s *Store) UpdateUser(ctx context.Context, u *UserWithRoles) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE users SET name=$2, mobile=$3, department_id=$4, department_path=$5, position=$6, "group"=$7
		WHERE id=$1
	`, u.ID, u.Name, u.Mobile, u.DepartmentID, u.DepartmentPath, u.Position, u.Group)
	return err
}

// ============== Department ==============

func (s *Store) ListDepartments(ctx context.Context) ([]*Department, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, COALESCE(parent_id,0), name, path, order_idx FROM wecom_departments ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Department
	for rows.Next() {
		d := &Department{}
		if err := rows.Scan(&d.ID, &d.ParentID, &d.Name, &d.Path, &d.Order); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) UpsertDepartment(ctx context.Context, d *Department) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO wecom_departments (id, parent_id, name, path, order_idx, synced_at)
		VALUES ($1,$2,$3,$4,$5,NOW())
		ON CONFLICT (id) DO UPDATE SET
			parent_id=EXCLUDED.parent_id, name=EXCLUDED.name,
			path=EXCLUDED.path, order_idx=EXCLUDED.order_idx, synced_at=NOW()
	`, d.ID, d.ParentID, d.Name, d.Path, d.Order)
	return err
}

// ============== Audit ==============

func (s *Store) LogAudit(ctx context.Context, actorID, targetUser, action, reason string, detail any) error {
	detailJSON := []byte("{}")
	if detail != nil {
		b, _ := json.Marshal(detail)
		detailJSON = b
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO permission_audit (actor_id, target_user, action, detail, reason, ts)
		VALUES ($1,$2,$3,$4,$5,NOW())
	`, actorID, targetUser, action, detailJSON, reason)
	return err
}

func (s *Store) ListAudit(ctx context.Context, targetUser string, limit int) ([]*AuditEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT id, COALESCE(actor_id,''), COALESCE(target_user,''), action, detail::text, COALESCE(reason,''), ts
		FROM permission_audit`
	args := []any{}
	if targetUser != "" {
		args = append(args, targetUser)
		q += " WHERE target_user=$1"
	}
	q += " ORDER BY ts DESC LIMIT $" + strconv.Itoa(len(args)+1)
	args = append(args, limit)
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AuditEntry
	for rows.Next() {
		a := &AuditEntry{}
		if err := rows.Scan(&a.ID, &a.ActorID, &a.TargetUser, &a.Action, &a.Detail, &a.Reason, &a.TS); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ExpireOverdue deletes expired user_roles (called by hourly cron).
func (s *Store) ExpireOverdue(ctx context.Context) (int, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM user_roles WHERE expires_at IS NOT NULL AND expires_at < NOW()`)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

var _ = time.Second
