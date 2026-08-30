package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims JWT claims
//   跟 API 契约一致: sub/tid/role/iat/exp
//   不放 PII (手机 / 邮箱) — 只放 id + role + tenant
type Claims struct {
	UserID   string `json:"sub"`
	TenantID string `json:"tid"`
	Role     string `json:"role"`
	jwt.RegisteredClaims
}

// Signer JWT 签发 / 验签器
//   HS256, secret 长度 ≥32 (HS256 最低要求)
type Signer struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
}

// NewSigner 构造 signer
//   accessTTL  = 15 min  (短期, Authorization 头)
//   refreshTTL = 7 days  (长期, httpOnly cookie)
func NewSigner(secret string, accessTTLSec, refreshTTLSec int) *Signer {
	if accessTTLSec <= 0 {
		accessTTLSec = 900
	}
	if refreshTTLSec <= 0 {
		refreshTTLSec = 604800
	}
	return &Signer{
		secret:     []byte(secret),
		accessTTL:  time.Duration(accessTTLSec) * time.Second,
		refreshTTL: time.Duration(refreshTTLSec) * time.Second,
	}
}

// AccessTTL 暴露给外部 (handler 用作 expires_in)
func (s *Signer) AccessTTL() time.Duration { return s.accessTTL }

// RefreshTTL 暴露给外部 (handler 用作 cookie MaxAge)
func (s *Signer) RefreshTTL() time.Duration { return s.refreshTTL }

// SignAccess 签 access token
//   sub = user_id, tid = tenant_id, role
func (s *Signer) SignAccess(userID, tenantID, role string) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID:   userID,
		TenantID: tenantID,
		Role:     role,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.accessTTL)),
			Issuer:    "collect-ai",
			Subject:   userID,
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString(s.secret)
}

// SignRefresh 签 refresh token (Opaque, 不放敏感信息)
//   内部是 random UUID, 不需要解析, 直接 bcrypt 比对
//   这里复用 SignAccess 模式产生 "rt.<jwt>" 字符串, 便于识别
func (s *Signer) SignRefresh(userID string) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(s.refreshTTL)
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(exp),
		Issuer:    "collect-ai-refresh",
		Subject:   userID,
		ID:        randHex(16), // 唯一 jti
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString(s.secret)
	if err != nil {
		return "", time.Time{}, err
	}
	return "rt." + signed, exp, nil
}

// ParseAccess 验签 + 返回 claims
//   失败返回 error
func (s *Signer) ParseAccess(tokenStr string) (*Claims, error) {
	c := &Claims{}
	tok, err := jwt.ParseWithClaims(tokenStr, c, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.secret, nil
	})
	if err != nil {
		return nil, err
	}
	if !tok.Valid {
		return nil, errors.New("token invalid")
	}
	if c.UserID == "" {
		return nil, errors.New("missing sub")
	}
	return c, nil
}

// ParseRefresh 验签 refresh token (不解析业务 claims, 只验签 + 有效期)
func (s *Signer) ParseRefresh(tokenStr string) (userID string, jti string, err error) {
	// 去掉 "rt." 前缀
	raw := tokenStr
	if len(raw) > 3 && raw[:3] == "rt." {
		raw = raw[3:]
	}
	c := &jwt.RegisteredClaims{}
	tok, perr := jwt.ParseWithClaims(raw, c, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.secret, nil
	})
	if perr != nil {
		return "", "", perr
	}
	if !tok.Valid {
		return "", "", errors.New("refresh invalid")
	}
	return c.Subject, c.ID, nil
}
