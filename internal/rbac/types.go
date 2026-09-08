// Package rbac 角色权限管理 (2026-08-30)
//
// 数据模型:
//   roles              - 角色 (内置 6 个 + 任意自定义)
//   permissions        - 权限点字典
//   role_permissions   - 角色-权限多对多
//   user_roles         - 用户-角色多对多 (含数据范围)
//   permission_audit   - 审计日志
//   wecom_departments  - 企微部门缓存
package rbac

import "time"

// Role 角色
type Role struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Scope       string    `json:"scope"`        // 'platform' / 'store' / 'dept'
	Description string    `json:"description"`
	IsBuiltin   bool      `json:"is_builtin"`
	CreatedAt   time.Time `json:"created_at"`
	// 展开字段 (可选)
	Permissions []string `json:"permissions,omitempty"`
	UserCount   int      `json:"user_count,omitempty"`
}

// Permission 权限点
type Permission struct {
	ID          string `json:"id"`        // 'session:create'
	Domain      string `json:"domain"`    // 'session'
	Action      string `json:"action"`    // 'create'
	Description string `json:"description"`
}

// UserRole 用户角色绑定
type UserRole struct {
	UserID    string     `json:"user_id"`
	RoleID    string     `json:"role_id"`
	ScopeType string     `json:"scope_type"` // 'all' / 'store' / 'dept'
	ScopeID   string     `json:"scope_id"`
	IsPrimary bool       `json:"is_primary"`
	GrantedBy string     `json:"granted_by"`
	GrantedAt time.Time  `json:"granted_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// 展开
	RoleName string `json:"role_name,omitempty"`
	UserName string `json:"user_name,omitempty"`
}

// UserWithRoles 用户 + 角色列表 (管理界面用)
type UserWithRoles struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Role           string     `json:"role"`           // 旧字段, 保留兼容
	Group          string     `json:"group"`
	Mobile         string     `json:"mobile"`
	DepartmentID   *int64     `json:"department_id,omitempty"`
	DepartmentPath string     `json:"department_path"`
	DepartmentName string     `json:"department_name"`
	Position       string     `json:"position"`
	ExternalID     string     `json:"external_id"`
	Source         string     `json:"source"`
	Status         string     `json:"status"`
	HiredAt        *time.Time `json:"hired_at,omitempty"`
	LeftAt         *time.Time `json:"left_at,omitempty"`
	SyncAt         *time.Time `json:"sync_at,omitempty"`
	Roles          []UserRole `json:"roles"`
	PrimaryRole    string     `json:"primary_role"` // 展示用
}

// GrantRequest 授权请求 body
type GrantRequest struct {
	UserID    string     `json:"user_id"`
	RoleID    string     `json:"role_id"`
	ScopeType string     `json:"scope_type"`
	ScopeID   string     `json:"scope_id"`
	IsPrimary bool       `json:"is_primary"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Reason    string     `json:"reason"`
}

// Department 部门
type Department struct {
	ID       int64  `json:"id"`
	ParentID int64  `json:"parent_id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Order    int    `json:"order"`
}

// AuditEntry 审计
type AuditEntry struct {
	ID         int64     `json:"id"`
	ActorID    string    `json:"actor_id"`
	TargetUser string    `json:"target_user"`
	Action     string    `json:"action"`
	Detail     string    `json:"detail"`
	Reason     string    `json:"reason"`
	TS         time.Time `json:"ts"`
}
