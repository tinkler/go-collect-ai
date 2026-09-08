package skill

import (
	"sort"
	"sync"
)

// Store 线程安全的 skill 注册表
//   - Read 走 atomic snapshot(无锁,但一致性读)
//   - Write 走 copy-on-write(替换整个 map)
// 热更新场景:Watcher's goroutine 拿到 fsnotify 事件后,重新 Load → 替换
type Store struct {
	mu     sync.RWMutex
	skills map[string]*Skill
	// roots 当前生效的扫描根(便于 reload 时复用)
	roots []string
}

// NewStore 构造空 store
func NewStore() *Store {
	return &Store{
		skills: make(map[string]*Skill),
	}
}

// Replace 原子替换整个 skill 集合
func (s *Store) Replace(skills []*Skill) {
	m := make(map[string]*Skill, len(skills))
	for _, sk := range skills {
		m[sk.Manifest.Name] = sk
	}
	s.mu.Lock()
	s.skills = m
	s.mu.Unlock()
}

// SetRoots 记录当前扫描根
func (s *Store) SetRoots(roots []string) {
	s.mu.Lock()
	s.roots = append([]string(nil), roots...)
	s.mu.Unlock()
}

// Roots 读当前 roots
func (s *Store) Roots() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.roots))
	copy(out, s.roots)
	return out
}

// Get 按 name 查 skill(同时返回是否存在)
func (s *Store) Get(name string) (*Skill, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sk, ok := s.skills[name]
	return sk, ok
}

// List 列出当前所有 skill(name 排序,保证 L1 prompt 稳定)
func (s *Store) List() []*Skill {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Skill, 0, len(s.skills))
	for _, sk := range s.skills {
		out = append(out, sk)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Manifest.Name < out[j].Manifest.Name
	})
	return out
}

// Names 列出所有 skill 名(给 L1 prompt 用)
func (s *Store) Names() []string {
	skills := s.List()
	names := make([]string, 0, len(skills))
	for _, sk := range skills {
		names = append(names, sk.Manifest.Name)
	}
	return names
}

// L1Prompt 拼出 Anthropic 风格的 L1 system prompt 片段
// 包含每个 skill 的 name + description + 路径 + triggers
// 严格控制 token 数(~80-150 tokens/skill)
func (s *Store) L1Prompt() string {
	skills := s.List()
	if len(skills) == 0 {
		return ""
	}
	var b []byte
	b = append(b, "## Available Skills (Agent Skills spec, model-driven activation)\n\n"...)
	b = append(b, "You have the following skills available. Read each skill's `SKILL.md` body only when its `description` matches the current task — they are loaded on demand (progressive disclosure).\n\n"...)
	for _, sk := range skills {
		b = append(b, "### "...)
		b = append(b, sk.Manifest.Name...)
		b = append(b, '\n')
		b = append(b, "Description: "...)
		b = append(b, sk.Manifest.Description...)
		b = append(b, '\n')
		if sk.Root != "" {
			b = append(b, "Path: "...)
			b = append(b, sk.Root...)
			b = append(b, '\n')
		}
		if len(sk.Manifest.Triggers) > 0 {
			b = append(b, "Triggers: "...)
			for i, t := range sk.Manifest.Triggers {
				if i > 0 {
					b = append(b, " | "...)
				}
				b = append(b, t...)
			}
			b = append(b, '\n')
		}
		b = append(b, '\n')
	}
	b = append(b, "To invoke a skill, call the `invoke_skill` tool with `skill_name=<name>` and `input=<user message>`. The tool returns the skill's full SKILL.md body and an enumerated list of its scripts/references/assets, so you can continue the task with that context.\n"...)
	return string(b)
}

// Count 当前 skill 数(供监控/测试用)
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.skills)
}
