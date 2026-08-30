package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// ctx keys (私有, 防外部覆盖)
const (
	CtxUserID   = "auth_user_id"
	CtxTenantID = "auth_tenant_id"
	CtxRole     = "auth_role"
)

// ============== Cookie helpers ==============

const refreshCookieName = "refresh"

// SetRefreshCookie 写 refresh cookie
//   Domain: cfg.CookieDomain (dev=127.0.0.1)
//   Secure: prod=true, dev=false
//   SameSite=Lax: 跨站 GET 允许带, 跨站 POST 不带 (符合前端 H5 同源调用)
func SetRefreshCookie(c *gin.Context, domain string, secure bool, maxAgeSec int, token string) {
	if domain == "" {
		domain = "127.0.0.1"
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(
		refreshCookieName,
		token,
		maxAgeSec, // maxAge, 秒, -1 = session cookie
		"/",       // path
		domain,    // domain
		secure,    // secure
		true,      // httpOnly
	)
}

// ClearRefreshCookie 删 cookie
//   设 MaxAge=-1 + Value="" 让浏览器立刻删
func ClearRefreshCookie(c *gin.Context, domain string) {
	if domain == "" {
		domain = "127.0.0.1"
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(
		refreshCookieName,
		"",
		-1, // 立刻过期
		"/",
		domain,
		false, // secure (dev). prod 由 caller 决定, 但 logout 本地删即可
		true,  // httpOnly
	)
}

// GetRefreshCookie 读 cookie (无则空字符串)
func GetRefreshCookie(c *gin.Context) string {
	v, err := c.Cookie(refreshCookieName)
	if err != nil {
		return ""
	}
	return v
}

// ============== AuthMiddleware ==============

// AuthMiddleware 验 access token, 塞 ctx
//   - 缺 / 错 / 过期 token → 401
//   - 通过: ctx[CtxUserID/CtxTenantID/CtxRole]
func AuthMiddleware(svc *Service, sign *Signer) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := c.GetHeader("Authorization")
		if raw == "" {
			apiError(c, 401, CodeUnauthorized, "missing Authorization header", nil)
			return
		}
		// 期望 "Bearer <token>"
		const prefix = "Bearer "
		if !strings.HasPrefix(raw, prefix) {
			apiError(c, 401, CodeUnauthorized, "Authorization must be Bearer", nil)
			return
		}
		tok := strings.TrimSpace(raw[len(prefix):])
		if tok == "" {
			apiError(c, 401, CodeUnauthorized, "empty bearer token", nil)
			return
		}
		claims, err := sign.ParseAccess(tok)
		if err != nil {
			apiError(c, 401, CodeUnauthorized, "invalid or expired token", nil)
			return
		}
		c.Set(CtxUserID, claims.UserID)
		c.Set(CtxTenantID, claims.TenantID)
		c.Set(CtxRole, claims.Role)
		c.Next()
	}
}

// RequirePerm 权限检查
//   - 必须先经过 AuthMiddleware (否则 401)
//   - role 缺 perm → 403
func RequirePerm(perm string) gin.HandlerFunc {
	return func(c *gin.Context) {
		role, ok := c.Get(CtxRole)
		if !ok {
			apiError(c, 401, CodeUnauthorized, "auth required", nil)
			return
		}
		roleStr, _ := role.(string)
		if roleStr == "" {
			apiError(c, 401, CodeUnauthorized, "auth required", nil)
			return
		}
		if !HasPerm(roleStr, perm) {
			userID, _ := c.Get(CtxUserID)
			userIDStr, _ := userID.(string)
			apiError(c, 403, CodeForbidden,
				"user "+userIDStr+" lacks perm "+perm,
				map[string]any{"required": perm, "user_id": userIDStr, "role": roleStr},
			)
			return
		}
		c.Next()
	}
}

// ============== ctx getters (handler 用) ==============

// UserIDFromCtx 拿 ctx 里的 user_id
func UserIDFromCtx(c *gin.Context) string {
	v, _ := c.Get(CtxUserID)
	s, _ := v.(string)
	return s
}

// TenantIDFromCtx
func TenantIDFromCtx(c *gin.Context) string {
	v, _ := c.Get(CtxTenantID)
	s, _ := v.(string)
	return s
}

// RoleFromCtx
func RoleFromCtx(c *gin.Context) string {
	v, _ := c.Get(CtxRole)
	s, _ := v.(string)
	return s
}
