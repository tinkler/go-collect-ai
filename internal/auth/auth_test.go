package auth

import (
	"testing"
	"time"
)

// Test JWT roundtrip
func TestSignParseAccess_Roundtrip(t *testing.T) {
	s := NewSigner("this-is-a-32-char-secret-key-1234", 900, 604800)
	tok, err := s.SignAccess("u_owner", "t_dev", "owner")
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	c, err := s.ParseAccess(tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.UserID != "u_owner" {
		t.Errorf("UserID = %q, want u_owner", c.UserID)
	}
	if c.TenantID != "t_dev" {
		t.Errorf("TenantID = %q, want t_dev", c.TenantID)
	}
	if c.Role != "owner" {
		t.Errorf("Role = %q, want owner", c.Role)
	}
	// 过期时间应该是 iat + 15min
	if c.ExpiresAt == nil {
		t.Fatal("ExpiresAt is nil")
	}
	expected := time.Unix(c.IssuedAt.Unix(), 0).Add(15 * time.Minute)
	actual := time.Unix(c.ExpiresAt.Unix(), 0)
	if !actual.Equal(expected) {
		t.Errorf("ExpiresAt = %v, want ~%v", actual, expected)
	}
}

func TestParseAccess_InvalidToken(t *testing.T) {
	s := NewSigner("correct-secret-32-chars-aaaaaaaaaa", 900, 604800)
	if _, err := s.ParseAccess("not-a-jwt"); err == nil {
		t.Error("expected error for invalid token")
	}
	if _, err := s.ParseAccess(""); err == nil {
		t.Error("expected error for empty token")
	}
	// 用错 secret 签的 token
	s2 := NewSigner("wrong-secret-32-chars-bbbbbbbbbb", 900, 604800)
	tok, _ := s2.SignAccess("u_x", "t_x", "owner")
	if _, err := s.ParseAccess(tok); err == nil {
		t.Error("expected error for token signed with different secret")
	}
}

func TestSignRefresh_Roundtrip(t *testing.T) {
	s := NewSigner("test-secret-32-chars-aaaaaaaaaa", 900, 604800)
	rt, exp, err := s.SignRefresh("u_owner")
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if rt == "" {
		t.Fatal("empty refresh token")
	}
	if !exp.After(time.Now()) {
		t.Error("refresh exp should be in future")
	}
	userID, jti, err := s.ParseRefresh(rt)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if userID != "u_owner" {
		t.Errorf("userID = %q", userID)
	}
	if jti == "" {
		t.Error("jti is empty")
	}
}

func TestBcryptHashAndCompare(t *testing.T) {
	plain := "rt.test-refresh-token-12345"
	hash, err := bcryptHash(plain)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash == plain {
		t.Error("hash should not equal plain")
	}
	if !bcryptCompare(hash, plain) {
		t.Error("compare should succeed for same plain")
	}
	if bcryptCompare(hash, "wrong") {
		t.Error("compare should fail for wrong plain")
	}
}

// Test HasPerm 含通配符
func TestHasPerm_Wildcard(t *testing.T) {
	// 直接操作内部缓存 (测试用, 不依赖 DB)
	rolePermsMu.Lock()
	rolePerms = map[string]map[string]bool{
		"owner":   {"*": true},
		"manager": {"session:read": true, "session:create": true},
		"cashier": {"session:read": true},
		"nobody":  {},
	}
	rolePermsMu.Unlock()

	cases := []struct {
		role, perm string
		want       bool
	}{
		{"owner", "anything", true},     // 通配
		{"owner", "session:create", true}, // 通配
		{"manager", "session:read", true},
		{"manager", "plan:read", false}, // 没这个
		{"cashier", "session:read", true},
		{"cashier", "session:create", false}, // cashier 不能 create
		{"nobody", "session:read", false},
	}
	for _, tc := range cases {
		got := HasPerm(tc.role, tc.perm)
		if got != tc.want {
			t.Errorf("HasPerm(%q, %q) = %v, want %v", tc.role, tc.perm, got, tc.want)
		}
	}
}

func TestRefreshCookieName(t *testing.T) {
	if refreshCookieName != "refresh" {
		t.Errorf("cookie name = %q, want refresh", refreshCookieName)
	}
}

func TestRandHex(t *testing.T) {
	a := randHex(8)
	b := randHex(8)
	if a == b {
		t.Error("two calls should produce different hex")
	}
	if len(a) != 16 {
		t.Errorf("len(a) = %d, want 16", len(a))
	}
}
