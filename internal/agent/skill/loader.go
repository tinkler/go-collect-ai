package skill

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// LoadResult 一次扫描的产出
type LoadResult struct {
	Skills   []*Skill
	Errors   []LoadError // 解析/校验失败的项(不阻塞其它 skill 加载)
	Roots    []string    // 实际扫描过的根目录
	ScannedAt time.Time
}

// LoadError 单个 skill 加载失败的原因
type LoadError struct {
	Path string
	Err  error
}

func (e LoadError) Error() string {
	return fmt.Sprintf("%s: %v", e.Path, e.Err)
}

// Load 扫描多个 root,加载所有合法 skill
//   - 每个 root 下的"直接子目录"被视为一个 skill(<root>/<name>/SKILL.md)
//   - 一个 root 解析失败不影响其它 root
//   - 单个 skill 解析失败被收集到 result.Errors,不阻塞整体
func Load(roots []string) (*LoadResult, error) {
	result := &LoadResult{
		Roots:     roots,
		ScannedAt: time.Now(),
	}

	seen := make(map[string]string) // name -> root 路径,检测重名

	for _, root := range roots {
		if root == "" {
			continue
		}
		// 不存在不报错(可能后续 npx skills 装上)
		if _, err := os.Stat(root); err != nil {
			if !os.IsNotExist(err) {
				result.Errors = append(result.Errors, LoadError{Path: root, Err: err})
			}
			continue
		}

		entries, err := os.ReadDir(root)
		if err != nil {
			result.Errors = append(result.Errors, LoadError{Path: root, Err: err})
			continue
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			// 跳过隐藏目录(.git, .claude, etc)
			if strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			skillDir := filepath.Join(root, entry.Name())
			skillPath := filepath.Join(skillDir, "SKILL.md")

			if _, err := os.Stat(skillPath); err != nil {
				// 没 SKILL.md 的子目录不当作 skill
				continue
			}

			s, err := LoadOne(skillDir, SourceFromRoot(root))
			if err != nil {
				result.Errors = append(result.Errors, LoadError{Path: skillDir, Err: err})
				continue
			}

			if prev, dup := seen[s.Manifest.Name]; dup {
				result.Errors = append(result.Errors, LoadError{
					Path: skillDir,
					Err:  fmt.Errorf("重名 skill %q,先前在 %s", s.Manifest.Name, prev),
				})
				continue
			}
			seen[s.Manifest.Name] = skillDir
			result.Skills = append(result.Skills, s)
		}
	}

	// 排序保证输出稳定(便于 L1 prompt 顺序一致)
	sort.Slice(result.Skills, func(i, j int) bool {
		return result.Skills[i].Manifest.Name < result.Skills[j].Manifest.Name
	})

	return result, nil
}

// LoadOne 解析单个 skill 目录
//   source: "project" / "user-claude" / "user-agents" / "extra:<path>"
func LoadOne(skillDir, source string) (*Skill, error) {
	skillPath := filepath.Join(skillDir, "SKILL.md")
	raw, err := os.ReadFile(skillPath)
	if err != nil {
		return nil, fmt.Errorf("read SKILL.md: %w", err)
	}

	manifest, body, err := parseFrontmatter(raw)
	if err != nil {
		return nil, fmt.Errorf("parse frontmatter: %w", err)
	}

	dirName := filepath.Base(skillDir)
	if err := Validate(manifest, dirName); err != nil {
		return nil, err
	}

	// 枚举子目录
	scripts := listDirFiles(filepath.Join(skillDir, "scripts"))
	refs := listDirFiles(filepath.Join(skillDir, "references"))
	assets := listDirFiles(filepath.Join(skillDir, "assets"))

	return &Skill{
		Manifest:   *manifest,
		Root:       skillDir,
		SKILLPath:  skillPath,
		Body:       body,
		Scripts:    scripts,
		References: refs,
		Assets:     assets,
		LoadedAt:   time.Now(),
		Source:     source,
	}, nil
}

// parseFrontmatter 解析 SKILL.md 的 YAML frontmatter + Markdown body
// 形如:
//
//	---
//	name: foo
//	description: ...
//	---
//	# Body markdown...
func parseFrontmatter(raw []byte) (*Manifest, string, error) {
	const sep = "\n---\n"
	// 兼容文件以 "---\n" 开头
	text := string(raw)
	if !strings.HasPrefix(text, "---") {
		return nil, "", fmt.Errorf("SKILL.md 必须以 YAML frontmatter (---) 开头")
	}
	// 去掉前导 ---
	rest := strings.TrimPrefix(text, "---")
	// rest 现在是 "\nkey: val\n---\nbody"
	endIdx := strings.Index(rest, sep)
	if endIdx < 0 {
		// 兼容 "\r\n---\r\n" (CRLF)
		crlf := "\r\n---\r\n"
		if i := strings.Index(rest, crlf); i >= 0 {
			fm := strings.TrimSpace(rest[:i])
			body := rest[i+len(crlf):]
			return decodeFM(fm, body)
		}
		return nil, "", fmt.Errorf("未找到 frontmatter 结束标记 '---'")
	}
	fm := strings.TrimSpace(rest[:endIdx])
	body := rest[endIdx+len(sep):]
	return decodeFM(fm, body)
}

func decodeFM(fm, body string) (*Manifest, string, error) {
	var m Manifest
	if err := yaml.Unmarshal([]byte(fm), &m); err != nil {
		return nil, "", err
	}
	return &m, strings.TrimSpace(body), nil
}

func listDirFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // 目录不存在是合法的
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// DefaultRoots 给一组默认扫描根(按优先级排序,先去重的胜出)
//   1. 项目内 ./skills/             (仓库内,git 同步,首选)
//   2. 全局 ~/.claude/skills/       (Anthropic 官方 Claude Code 落点)
//   3. 全局 ~/.agents/skills/       (Vercel add-skill 落点)
//   4. 可被 COLLECTAI_SKILL_ROOTS env 追加(逗号分隔)
func DefaultRoots(workdir string) []string {
	var roots []string

	// 1. 项目内
	if workdir == "" {
		if wd, err := os.Getwd(); err == nil {
			workdir = wd
		}
	}
	if workdir != "" {
		roots = append(roots, filepath.Join(workdir, "skills"))
	}

	// 2 & 3. 用户主目录下的两个标准落点
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots,
			filepath.Join(home, ".claude", "skills"),
			filepath.Join(home, ".agents", "skills"),
		)
	}

	return dedup(roots)
}

func dedup(in []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// FormatErrors 把 LoadResult.Errors 拼成一段可读文本(日志用)
func (r *LoadResult) FormatErrors() string {
	if len(r.Errors) == 0 {
		return ""
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "skill 加载发现 %d 个错误:\n", len(r.Errors))
	for _, e := range r.Errors {
		fmt.Fprintf(&b, "  - %s\n", e.Error())
	}
	return b.String()
}
