package auth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// LoginResult 登录结果
type LoginResult struct {
	AccessToken string
	User        *User
	ExpiresIn   int
}

// Service 业务编排
//   - 持有 Store (pg) + Signer (jwt) + WeComClient
//   - 业务方法: WeComCallback, DevLogin, Refresh, Logout, Me
type Service struct {
	store *Store
	sign  *Signer
	wecom *WeComClient
}

// NewService
func NewService(store *Store, sign *Signer, wecom *WeComClient) *Service {
	return &Service{store: store, sign: sign, wecom: wecom}
}

// WeComCallback 企微 code → 登录 (签 access + refresh + 写 session)
//   1. 调 wecom.Code2UserID(code) 拿 userid
//   2. upsert user
//   3. 签 access + refresh
//   4. bcrypt(refresh) 写 session
//   5. 返回 {access, user}
func (s *Service) WeComCallback(ctx context.Context, code string) (*LoginResult, string, error) {
	if code == "" {
		return nil, "", &AuthError{Code: CodeMissingCode, Msg: "code is required", Status: 400}
	}
	userID, name, err := s.wecom.Code2UserID(ctx, code)
	if err != nil {
		log.Printf("[auth] wecom callback failed: %v", err)
		return nil, "", &AuthError{Code: CodeWeComAPIError, Msg: "wechat work api error: " + err.Error(), Status: 502}
	}
	u, err := s.store.UpsertUserByExternalID(ctx, userID, name)
	if err != nil {
		return nil, "", fmt.Errorf("upsert user: %w", err)
	}
	return s.issueTokens(ctx, u)
}

// DevLogin dev 模式直接查 user 登录
//   - userID 不存在 → 404
//   - DevMode=false 时挂载层就 404, 这里不重复检查
func (s *Service) DevLogin(ctx context.Context, userID string) (*LoginResult, string, error) {
	u, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	if u == nil {
		return nil, "", &AuthError{Code: CodeNotFound, Msg: "user not found: " + userID, Status: 404}
	}
	if u.Status != "active" {
		return nil, "", &AuthError{Code: CodeForbidden, Msg: "user disabled", Status: 403}
	}
	return s.issueTokens(ctx, u)
}

// Refresh 用 refresh token 换新 access + 新 refresh
//   rotation: 旧 session 软删, 发新 session
//   失败:
//   - token 验签失败 / 过期 → REFRESH_EXPIRED 401
//   - session 不存在 / 已撤销 → REFRESH_EXPIRED 401
//   - bcrypt 比对失败 → REFRESH_EXPIRED 401 (一律 REFRESH_EXPIRED, 不区分防探测)
func (s *Service) Refresh(ctx context.Context, refreshTokenStr string) (*LoginResult, string, error) {
	if refreshTokenStr == "" {
		return nil, "", &AuthError{Code: CodeRefreshExpired, Msg: "missing refresh", Status: 401}
	}
	userID, _, err := s.sign.ParseRefresh(refreshTokenStr)
	if err != nil {
		return nil, "", &AuthError{Code: CodeRefreshExpired, Msg: "invalid refresh", Status: 401}
	}
	// 找未撤销未过期的 session, bcrypt 比对 (VerifyRefresh 内部用 bcryptCompare)
	sess, u, err := s.store.VerifyRefresh(ctx, userID, refreshTokenStr)
	if err != nil {
		return nil, "", fmt.Errorf("verify refresh: %w", err)
	}
	if sess == nil || u == nil {
		return nil, "", &AuthError{Code: CodeRefreshExpired, Msg: "session not found or expired", Status: 401}
	}
	// rotation: 撤销旧 session
	if err := s.store.RevokeSessionByID(ctx, sess.ID); err != nil {
		log.Printf("[auth] revoke old session failed: %v (continue)", err)
	}
	return s.issueTokens(ctx, u)
}

// Logout 软删 user 的所有 session
func (s *Service) Logout(ctx context.Context, userID string) error {
	if userID == "" {
		return errors.New("user_id required")
	}
	return s.store.RevokeSessionsForUser(ctx, userID)
}

// Me 拿当前 user
func (s *Service) Me(ctx context.Context, userID string) (*User, error) {
	u, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, &AuthError{Code: CodeNotFound, Msg: "user not found", Status: 404}
	}
	return u, nil
}

// GetLastPage 拿 user 最后访问的页 (无记录返回 "")
//   2026-08-31: 登录后自动跳回
func (s *Service) GetLastPage(ctx context.Context, userID string) (string, error) {
	if userID == "" {
		return "", errors.New("user_id required")
	}
	return s.store.GetLastPage(ctx, userID)
}

// SetLastPage 写最后访问页
//   2026-08-31: 前端每次打开页面异步上报
//   handler 已校验白名单, 这里不再重复
func (s *Service) SetLastPage(ctx context.Context, userID, page string) error {
	if userID == "" {
		return errors.New("user_id required")
	}
	return s.store.SetLastPage(ctx, userID, page)
}

// ============== 内部 ==============

// issueTokens 签 access + refresh, 写 session
//   返回: LoginResult, refreshTokenPlain
func (s *Service) issueTokens(ctx context.Context, u *User) (*LoginResult, string, error) {
	access, err := s.sign.SignAccess(u.ID, u.TenantID, u.Role)
	if err != nil {
		return nil, "", fmt.Errorf("sign access: %w", err)
	}
	refreshPlain, exp, err := s.sign.SignRefresh(u.ID)
	if err != nil {
		return nil, "", fmt.Errorf("sign refresh: %w", err)
	}
	hash, err := bcryptHash(refreshPlain)
	if err != nil {
		return nil, "", fmt.Errorf("hash refresh: %w", err)
	}
	if _, err := s.store.CreateSession(ctx, u.ID, hash, exp); err != nil {
		return nil, "", fmt.Errorf("create session: %w", err)
	}
	// 兜底: 确保 exp 是未来
	if !exp.After(time.Now()) {
		exp = time.Now().Add(s.sign.RefreshTTL())
	}
	return &LoginResult{
		AccessToken: access,
		User:        u,
		ExpiresIn:   int(s.sign.AccessTTL().Seconds()),
	}, refreshPlain, nil
}

// AuthError 业务错误 (handler 映射成 JSON 响应)
type AuthError struct {
	Code   string
	Msg    string
	Status int
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("[%s] %s", e.Code, e.Msg)
}

// IsAuthError 提取业务错误
func IsAuthError(err error) (*AuthError, bool) {
	var ae *AuthError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}
