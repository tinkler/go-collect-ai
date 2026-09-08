// Package auth 实现企微 OAuth 免登 + JWT 双 token + RBAC 权限中间件
//
// 设计原则:
//   - access token 短期 (15min), 放在 Authorization: Bearer 头
//   - refresh token 长期 (7d), 放在 httpOnly cookie, bcrypt 存 DB
//   - rotation: 每次 refresh 换新 + 旧失效, 防重放
//   - RBAC: 内存缓存 role → perm 映射, 启动时加载一次
//   - WeCom corp_secret 严禁进 log / 前端
//
// 调用顺序: handler → service (sign/verify) → store (pgx) + wecom (HTTP)
package auth

import (
	"github.com/gin-gonic/gin"
)

// 错误码 (与 API 契约一致)
const (
	CodeMissingCode      = "MISSING_CODE"      // 400 缺 code
	CodeBadRequest       = "BAD_REQUEST"       // 400 通用
	CodeUnauthorized     = "UNAUTHORIZED"      // 401 无 / 失效 token
	CodeRefreshExpired   = "REFRESH_EXPIRED"   // 401 refresh 过期 / 失效
	CodeForbidden        = "FORBIDDEN"         // 403 权限不够
	CodeNotFound         = "NOT_FOUND"         // 404
	CodeWeComAPIError    = "WECOM_API_ERROR"   // 502 企微调用失败
	CodeInternal         = "INTERNAL"          // 500
	CodeUserNotFound     = "USER_NOT_FOUND"    // 404
)

// apiError 统一错误响应
//   { "code": "...", "message": "...", "detail": {...} }
func apiError(c *gin.Context, status int, code, msg string, detail map[string]any) {
	body := gin.H{"code": code}
	if msg != "" {
		body["message"] = msg
	}
	if detail != nil {
		body["detail"] = detail
	}
	c.AbortWithStatusJSON(status, body)
}
