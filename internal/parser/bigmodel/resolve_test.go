package bigmodel

import "testing"

func TestResolveOcrModel(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "hand_write"},
		{"hand_write", "hand_write"},
		{"layout_parsing", "layout_parsing"},
		{"unknown_thing", "unknown_thing"}, // 客户端不强制白名单, 透传给 API
	}
	for _, c := range cases {
		if got := resolveOcrModel(c.in); got != c.want {
			t.Errorf("resolveOcrModel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolveLlmModel(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "glm-4-flash"},
		{"glm-4-flash", "glm-4-flash"},
		{"glm-4-plus", "glm-4-plus"},
	}
	for _, c := range cases {
		if got := resolveLlmModel(c.in); got != c.want {
			t.Errorf("resolveLlmModel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNewOcrClient_Default(t *testing.T) {
	c := NewOcrClient("sk-test", "https://x", 30)
	if c.apiKey != "sk-test" {
		t.Errorf("apiKey not stored: %q", c.apiKey)
	}
	if c.baseURL != "https://x" {
		t.Errorf("baseURL not stored: %q", c.baseURL)
	}
	if c.timeout.Seconds() != 30 {
		t.Errorf("timeout = %v, want 30s", c.timeout)
	}
}

func TestNewOcrClient_EmptyBaseURL(t *testing.T) {
	c := NewOcrClient("sk-test", "", 30)
	if c.baseURL != "https://open.bigmodel.cn/api/paas/v4" {
		t.Errorf("空 baseURL 应自动用官方, got %q", c.baseURL)
	}
}

func TestNewLlmClient_Default(t *testing.T) {
	c := NewLlmClient("sk-test", "https://x", 60)
	if c.timeout.Seconds() != 60 {
		t.Errorf("timeout = %v, want 60s", c.timeout)
	}
}
