// Package rbac - HTTP handler
package rbac

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	Store *Store
}

func NewHandler(store *Store) *Handler {
	return &Handler{Store: store}
}

// ============== Roles ==============

// ListRoles GET /admin/roles
func (h *Handler) ListRoles(c *gin.Context) {
	roles, err := h.Store.ListRoles(c.Request.Context())
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	// 附加权限点
	for _, r := range roles {
		perms, _ := h.Store.GetRolePermissions(c.Request.Context(), r.ID)
		r.Permissions = perms
	}
	c.JSON(200, gin.H{"roles": roles, "count": len(roles)})
}

// GetRole GET /admin/roles/:id
func (h *Handler) GetRole(c *gin.Context) {
	id := c.Param("id")
	role, err := h.Store.GetRole(c.Request.Context(), id)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	if role == nil {
		c.JSON(404, gin.H{"error": "role not found"})
		return
	}
	role.Permissions, _ = h.Store.GetRolePermissions(c.Request.Context(), id)
	c.JSON(200, role)
}

// CreateRole POST /admin/roles
func (h *Handler) CreateRole(c *gin.Context) {
	var req struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Scope       string   `json:"scope"`
		Description string   `json:"description"`
		Permissions []string `json:"permissions"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	if req.ID == "" || req.Name == "" {
		c.JSON(400, gin.H{"error": "id 和 name 必填"})
		return
	}
	if req.Scope == "" {
		req.Scope = "platform"
	}
	r := &Role{ID: req.ID, Name: req.Name, Scope: req.Scope, Description: req.Description}
	if err := h.Store.CreateRole(c.Request.Context(), r); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	if len(req.Permissions) > 0 {
		_ = h.Store.SetRolePermissions(c.Request.Context(), req.ID, req.Permissions)
	}
	actor := c.GetString("auth_user_id")
	_ = h.Store.LogAudit(c.Request.Context(), actor, "", "create_role", req.Description, gin.H{"role_id": req.ID, "permissions": req.Permissions})
	c.JSON(200, gin.H{"ok": true, "role": r})
}

// UpdateRole PUT /admin/roles/:id
func (h *Handler) UpdateRole(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Name        string   `json:"name"`
		Scope       string   `json:"scope"`
		Description string   `json:"description"`
		Permissions []string `json:"permissions"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	if err := h.Store.UpdateRole(c.Request.Context(), id, req.Name, req.Scope, req.Description); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	if req.Permissions != nil {
		_ = h.Store.SetRolePermissions(c.Request.Context(), id, req.Permissions)
	}
	actor := c.GetString("auth_user_id")
	_ = h.Store.LogAudit(c.Request.Context(), actor, "", "update_role", "", gin.H{"role_id": id, "permissions": req.Permissions})
	c.JSON(200, gin.H{"ok": true})
}

// DeleteRole DELETE /admin/roles/:id
func (h *Handler) DeleteRole(c *gin.Context) {
	id := c.Param("id")
	if err := h.Store.DeleteRole(c.Request.Context(), id); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	actor := c.GetString("auth_user_id")
	_ = h.Store.LogAudit(c.Request.Context(), actor, "", "delete_role", "", gin.H{"role_id": id})
	c.JSON(200, gin.H{"ok": true})
}

// ============== Permissions ==============

// ListPermissions GET /admin/permissions
func (h *Handler) ListPermissions(c *gin.Context) {
	perms, err := h.Store.ListPermissions(c.Request.Context())
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"permissions": perms, "count": len(perms)})
}

// ============== Users ==============

// ListUsers GET /admin/users
func (h *Handler) ListUsers(c *gin.Context) {
	search := c.Query("search")
	status := c.DefaultQuery("status", "active")
	limit, _ := strconv.Atoi(c.Query("limit"))
	users, err := h.Store.ListUsersWithRoles(c.Request.Context(), search, status, limit)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"users": users, "count": len(users)})
}

// GetUser GET /admin/users/:id
func (h *Handler) GetUser(c *gin.Context) {
	id := c.Param("id")
	u, err := h.Store.GetUserWithRoles(c.Request.Context(), id)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	if u == nil {
		c.JSON(404, gin.H{"error": "user not found"})
		return
	}
	c.JSON(200, u)
}

// UpdateUser PUT /admin/users/:id
func (h *Handler) UpdateUser(c *gin.Context) {
	id := c.Param("id")
	var req UserWithRoles
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	req.ID = id
	if err := h.Store.UpdateUser(c.Request.Context(), &req); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	actor := c.GetString("auth_user_id")
	_ = h.Store.LogAudit(c.Request.Context(), actor, id, "update_user", "", req)
	c.JSON(200, gin.H{"ok": true})
}

// MarkLeft DELETE /admin/users/:id (soft delete)
func (h *Handler) MarkLeft(c *gin.Context) {
	id := c.Param("id")
	if err := h.Store.MarkLeft(c.Request.Context(), id); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	actor := c.GetString("auth_user_id")
	_ = h.Store.LogAudit(c.Request.Context(), actor, id, "left", "", nil)
	c.JSON(200, gin.H{"ok": true})
}

// RestoreUser POST /admin/users/:id/restore
func (h *Handler) RestoreUser(c *gin.Context) {
	id := c.Param("id")
	if err := h.Store.RestoreUser(c.Request.Context(), id); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	actor := c.GetString("auth_user_id")
	_ = h.Store.LogAudit(c.Request.Context(), actor, id, "restore", "", nil)
	c.JSON(200, gin.H{"ok": true})
}

// ============== Grant / Revoke ==============

// GrantRole POST /admin/user-roles
func (h *Handler) GrantRole(c *gin.Context) {
	var req GrantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	if req.UserID == "" || req.RoleID == "" {
		c.JSON(400, gin.H{"error": "user_id 和 role_id 必填"})
		return
	}
	if req.ScopeType == "" {
		req.ScopeType = "all"
	}
	actor := c.GetString("auth_user_id")
	ur := &UserRole{
		UserID: req.UserID, RoleID: req.RoleID,
		ScopeType: req.ScopeType, ScopeID: req.ScopeID,
		IsPrimary: req.IsPrimary, GrantedBy: actor, ExpiresAt: req.ExpiresAt,
	}
	if err := h.Store.GrantRole(c.Request.Context(), ur); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	_ = h.Store.LogAudit(c.Request.Context(), actor, req.UserID, "grant", req.Reason, req)
	c.JSON(200, gin.H{"ok": true, "granted": ur})
}

// RevokeRole DELETE /admin/user-roles
func (h *Handler) RevokeRole(c *gin.Context) {
	userID := c.Query("user_id")
	roleID := c.Query("role_id")
	scopeType := c.DefaultQuery("scope_type", "all")
	scopeID := c.DefaultQuery("scope_id", "")
	if userID == "" || roleID == "" {
		c.JSON(400, gin.H{"error": "user_id 和 role_id 必填"})
		return
	}
	if err := h.Store.RevokeRole(c.Request.Context(), userID, roleID, scopeType, scopeID); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	actor := c.GetString("auth_user_id")
	_ = h.Store.LogAudit(c.Request.Context(), actor, userID, "revoke", "", gin.H{"role_id": roleID, "scope_type": scopeType, "scope_id": scopeID})
	c.JSON(200, gin.H{"ok": true})
}

// ============== Departments ==============

// ListDepartments GET /admin/departments
func (h *Handler) ListDepartments(c *gin.Context) {
	depts, err := h.Store.ListDepartments(c.Request.Context())
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"departments": depts, "count": len(depts)})
}

// ============== Audit ==============

// ListAudit GET /admin/audit?target_user=xxx&limit=50
func (h *Handler) ListAudit(c *gin.Context) {
	target := c.Query("target_user")
	limit, _ := strconv.Atoi(c.Query("limit"))
	entries, err := h.Store.ListAudit(c.Request.Context(), target, limit)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"entries": entries, "count": len(entries)})
}

// ============== Stats ==============

// Stats GET /admin/stats
func (h *Handler) Stats(c *gin.Context) {
	ctx := c.Request.Context()
	var totalUsers, activeUsers, totalRoles, totalGrants int
	_ = h.Store.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&totalUsers)
	_ = h.Store.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE left_at IS NULL`).Scan(&activeUsers)
	_ = h.Store.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM roles`).Scan(&totalRoles)
	_ = h.Store.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_roles WHERE expires_at IS NULL OR expires_at > now()`).Scan(&totalGrants)
	c.JSON(200, gin.H{
		"total_users":   totalUsers,
		"active_users":  activeUsers,
		"total_roles":   totalRoles,
		"total_grants":  totalGrants,
		"now":           time.Now().Unix(),
	})
}
