package auth

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
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

// allowedPages 允许记录为 last_page 的白名单 (2026-08-31)
//   防止前端被 XSS 后写入任意路径 (虽然前端只走 api, 但后端兜底)
//   格式: "file.html" 或 "subdir/file.html"
var allowedPages = map[string]bool{
	"index.html":                        true, // 欢迎页 (默认)
	"purchase.html":                     true, // 采购收货单
	"scan.html":                         true, // 扫商品
	"restock.html":                      true, // 陈列补货
	"admin/permissions.html":            true, // 权限管理
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
//   resp: { access_token, token_type, expires_in, user, last_page }
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
	lastPage, _ := h.svc.GetLastPage(ctx, result.User.ID)
	c.JSON(http.StatusOK, gin.H{
		"access_token": result.AccessToken,
		"token_type":   "Bearer",
		"expires_in":   result.ExpiresIn,
		"user":         userView(result.User),
		"last_page":    lastPage, // 2026-08-31: 登录后跳回
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
	lastPage, _ := h.svc.GetLastPage(c.Request.Context(), result.User.ID)
	c.JSON(http.StatusOK, gin.H{
		"access_token": result.AccessToken,
		"token_type":   "Bearer",
		"expires_in":   result.ExpiresIn,
		"user":         userView(result.User),
		"last_page":    lastPage, // 2026-08-31: 登录后跳回
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

// GetLastPage GET /auth/last-page
//   拿当前 user 最后访问的页; 无记录返回空字符串
//   200: { "last_page": "purchase.html" | "" }
func (h *Handler) GetLastPage(c *gin.Context) {
	userID := UserIDFromCtx(c)
	if userID == "" {
		apiError(c, 401, CodeUnauthorized, "auth required", nil)
		return
	}
	page, err := h.svc.GetLastPage(c.Request.Context(), userID)
	if err != nil {
		log.Printf("[auth] get last page failed: %v", err)
		apiError(c, 500, CodeInternal, "fetch last page failed", nil)
		return
	}
	c.JSON(http.StatusOK, gin.H{"last_page": page})
}

// SetLastPage POST /auth/last-page
//   body: { "page": "purchase.html" }
//   2026-08-31: 前端每次打开页面异步上报, 用于登录后自动跳转
//   白名单校验: 只允许白名单内的页 (防 XSS 写入任意路径)
func (h *Handler) SetLastPage(c *gin.Context) {
	userID := UserIDFromCtx(c)
	if userID == "" {
		apiError(c, 401, CodeUnauthorized, "auth required", nil)
		return
	}
	var req struct {
		Page string `json:"page"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		apiError(c, 400, CodeBadRequest, "invalid json: "+err.Error(), nil)
		return
	}
	page := strings.TrimSpace(req.Page)
	if page == "" {
		// 空值 = 清空记录, 让下次登录回 index.html
		page = ""
	} else if !allowedPages[page] {
		apiError(c, 400, CodeBadRequest, "page not in whitelist: "+page, map[string]any{"page": page})
		return
	}
	if err := h.svc.SetLastPage(c.Request.Context(), userID, page); err != nil {
		log.Printf("[auth] set last page failed: %v", err)
		apiError(c, 500, CodeInternal, "save last page failed", nil)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "last_page": page})
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
