package auth

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// Handler 鉴权 HTTP handler
type Handler struct {
	svc          *Service
	sign         *Signer
	cookieDomain string
	cookieSecure bool
}

// NewHandler
func NewHandler(svc *Service, sign *Signer, cookieDomain string, cookieSecure bool) *Handler {
	return &Handler{
		svc:          svc,
		sign:         sign,
		cookieDomain: cookieDomain,
		cookieSecure: cookieSecure,
	}
}

// userView user 响应 (跟契约一致: id/name/role/tenant_id/avatar/group)
func userView(u *User) gin.H {
	avatar := ""
	return gin.H{
		"id":        u.ID,
		"name":      u.Name,
		"role":      u.Role,
		"tenant_id": u.TenantID,
		"avatar":    avatar,
		"group":     u.Group, // 2026-08-30: 'floor' / 'office' / ''
	}
}

// WeComCallback POST /auth/wecom/callback
//   body: { "code": "..." }
//   resp: { access_token, token_type, expires_in, user }
func (h *Handler) WeComCallback(c *gin.Context) {
	var req struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		apiError(c, 400, CodeBadRequest, "invalid json: "+err.Error(), nil)
		return
	}
	if req.Code == "" {
		apiError(c, 400, CodeMissingCode, "code is required", nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	result, refresh, err := h.svc.WeComCallback(ctx, req.Code)
	if err != nil {
		writeAuthError(c, err)
		return
	}
	SetRefreshCookie(c, h.cookieDomain, h.cookieSecure, int(h.sign.RefreshTTL().Seconds()), refresh)
	c.JSON(http.StatusOK, gin.H{
		"access_token": result.AccessToken,
		"token_type":   "Bearer",
		"expires_in":   result.ExpiresIn,
		"user":         userView(result.User),
	})
}

// DevLogin POST /auth/dev-login?as_user=u_xxx
//   仅 DevMode=true 时挂载
func (h *Handler) DevLogin(c *gin.Context) {
	userID := c.Query("as_user")
	if userID == "" {
		apiError(c, 400, CodeBadRequest, "as_user is required", nil)
		return
	}
	result, refresh, err := h.svc.DevLogin(c.Request.Context(), userID)
	if err != nil {
		writeAuthError(c, err)
		return
	}
	SetRefreshCookie(c, h.cookieDomain, h.cookieSecure, int(h.sign.RefreshTTL().Seconds()), refresh)
	c.JSON(http.StatusOK, gin.H{
		"access_token": result.AccessToken,
		"token_type":   "Bearer",
		"expires_in":   result.ExpiresIn,
		"user":         userView(result.User),
	})
}

// Refresh POST /auth/refresh
//   用 cookie 里的 refresh token 换新 access
func (h *Handler) Refresh(c *gin.Context) {
	rt := GetRefreshCookie(c)
	if rt == "" {
		apiError(c, 401, CodeRefreshExpired, "no refresh cookie", nil)
		return
	}
	result, refresh, err := h.svc.Refresh(c.Request.Context(), rt)
	if err != nil {
		// 失败时清 cookie, 防无限重试
		ClearRefreshCookie(c, h.cookieDomain)
		writeAuthError(c, err)
		return
	}
	// rotation: 设新 cookie
	SetRefreshCookie(c, h.cookieDomain, h.cookieSecure, int(h.sign.RefreshTTL().Seconds()), refresh)
	resp := gin.H{
		"access_token": result.AccessToken,
		"token_type":   "Bearer",
		"expires_in":   result.ExpiresIn,
	}
	if result.User != nil {
		resp["user"] = userView(result.User)
	}
	c.JSON(http.StatusOK, resp)
}

// Logout POST /auth/logout
//   撤销该 user 的所有 session, 清 cookie
func (h *Handler) Logout(c *gin.Context) {
	userID := UserIDFromCtx(c)
	if userID != "" {
		if err := h.svc.Logout(c.Request.Context(), userID); err != nil {
			log.Printf("[auth] logout revoke failed: %v", err)
			// 不阻断, cookie 还是要清
		}
	}
	ClearRefreshCookie(c, h.cookieDomain)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Me GET /auth/me
func (h *Handler) Me(c *gin.Context) {
	userID := UserIDFromCtx(c)
	u, err := h.svc.Me(c.Request.Context(), userID)
	if err != nil {
		writeAuthError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": userView(u)})
}

// writeAuthError 把 AuthError 映射成 JSON 响应, 其它 500
func writeAuthError(c *gin.Context, err error) {
	if ae, ok := IsAuthError(err); ok {
		apiError(c, ae.Status, ae.Code, ae.Msg, nil)
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		apiError(c, 504, CodeInternal, "upstream timeout", nil)
		return
	}
	log.Printf("[auth] internal error: %v", err)
	apiError(c, 500, CodeInternal, "internal error", nil)
}
