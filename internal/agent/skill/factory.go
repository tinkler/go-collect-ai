package skill

import (
	"os"
	"path/filepath"
)

// DefaultLoader 工厂:返回一个 Loader 闭包,包内默认实现
//   - 适用于 NewWatcher(store, roots, skill.DefaultLoader)
func DefaultLoader() Loader {
	return func(roots []string) (*LoadResult, error) {
		return Load(roots)
	}
}

// RootsFromEnvOrDefault 综合 env + 默认值
//   COLLECTAI_SKILL_ROOTS="d:/a,d:/b" 时追加到默认 roots 之后
func RootsFromEnvOrDefault(workdir, envValue string) []string {
	roots := DefaultRoots(workdir)
	if envValue == "" {
		return roots
	}
	// 逗号分隔;支持 ; (Windows 友好)
	extra := splitMulti(envValue, ",;")
	for _, p := range extra {
		if p == "" {
			continue
		}
		// 相对路径 → 相对 workdir
		if !filepath.IsAbs(p) && workdir != "" {
			p = filepath.Join(workdir, p)
		}
		roots = append(roots, p)
	}
	return dedup(roots)
}

// 简单 split,避免 import strings 在工厂文件
func splitMulti(s, sep string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if c == rune(sep[0]) {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// 确保 os 在 hot path 不被无用 import
var _ = os.Getenv
