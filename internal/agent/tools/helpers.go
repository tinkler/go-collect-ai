package tools

import "strings"

// trimSpace 去首尾空白
func trimSpace(s string) string {
	return strings.TrimSpace(s)
}

// orDefault 空时返回 fallback
func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// keysOf map key 列表(白名单给 LLM 看,提示哪些合法)
func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
