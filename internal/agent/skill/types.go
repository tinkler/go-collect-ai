// Package skill 实现 Anthropic Agent Skills 规范的 Go 端加载与运行时。
//
// 标准来源:
//   - Anthropic Agent Skills spec (2025-12 开源):
//     https://www.anthropic.com/engineering/equipping-agents-for-the-real-world-with-agent-skills
//   - Vercel npx skills CLI 安装的 skill 落点:
//     ~/.agents/skills/  (统一存储) → symlink 到各 Agent 配置目录
//     ~/.claude/skills/  (Claude Code 项目内)
//     ./.skills/         (项目内)
//
// 目录约定(对齐 Anthropic + Vercel 双方):
//
//	<skill-name>/
//	  SKILL.md            # 必填:YAML frontmatter (name + description) + Markdown body
//	  scripts/            # 可选:可执行脚本(LLM 经 invoke_skill 调,stdin/stdout JSON)
//	  references/         # 可选:按需加载的参考文档
//	  assets/             # 可选:静态资源
//
// 加载模型(对齐 Anthropic 三层渐进披露):
//   L1 启动时:把所有 skill 的 name + description 注入 system prompt
//   L2 触发时:LLM 调 invoke_skill → 读 SKILL.md 全文 + 列出 scripts/references/assets
//   L3 资源层:经 invoke_skill 显式 require 的脚本/参考文件
package skill

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Manifest 一个 Skill 的元数据(从 SKILL.md frontmatter 解析)
// 字段命名对齐 Anthropic 官方 spec;Vercel 扩展字段(triggers/compatibility)
// 用独立标签读取,不参与核心校验。
type Manifest struct {
	// Name 唯一标识,严格遵循 Anthropic 规则
	//   1-64 字符,小写字母数字 + 连字符,不能以连字符开头/结尾,无连续连字符
	//   必须等于父目录名
	Name string `yaml:"name" json:"name"`

	// Description 1-1024 字符,描述做什么 + 何时使用(必须含触发关键词)
	// description 是 model-driven activation 的唯一信号,要用心写
	Description string `yaml:"description" json:"description"`

	// License 可选,许可证名或路径
	License string `yaml:"license,omitempty" json:"license,omitempty"`

	// Compatibility 可选,运行时要求(<= 500 字符)
	Compatibility string `yaml:"compatibility,omitempty" json:"compatibility,omitempty"`

	// Metadata 可选,自定义 K-V(版本/作者/分类)
	Metadata map[string]any `yaml:"metadata,omitempty" json:"metadata,omitempty"`

	// AllowedTools 可选,实验性(Claude Code only),我们不强制使用
	AllowedTools []string `yaml:"allowed-tools,omitempty" json:"allowed_tools,omitempty"`

	// Triggers Vercel 扩展:触发短语列表(自然语言)
	// 我们兼容读取;真正触发仍走 description 的 LLM 自主判定
	Triggers []string `yaml:"triggers,omitempty" json:"triggers,omitempty"`
}

// Skill 内存中的完整 Skill 表达
type Skill struct {
	// Manifest frontmatter
	Manifest Manifest

	// Root 绝对路径(磁盘上)
	Root string

	// SKILLPath SKILL.md 绝对路径
	SKILLPath string

	// Body SKILL.md 去除 frontmatter 后的 Markdown 正文(L2 加载)
	Body string

	// Scripts 目录下所有脚本相对路径(L3 资源)
	Scripts []string

	// References references/ 下的文件相对路径
	References []string

	// Assets assets/ 下的文件相对路径
	Assets []string

	// LoadedAt 解析时间(用于调试/审计)
	LoadedAt time.Time

	// Source 来源标签: "project" / "user-claude" / "user-agents" / "extra:<path>"
	Source string
}

// SkillsForPrompt 拼成一段 L1 system prompt 注入(L1: name + description + 路径)
// 严格控制 token 数:每个 skill ~80 tokens
func (s Skill) SkillsForPrompt() string {
	var b strings.Builder
	fmt.Fprintf(&b, "### %s\n", s.Manifest.Name)
	fmt.Fprintf(&b, "  描述: %s\n", s.Manifest.Description)
	if s.Root != "" {
		fmt.Fprintf(&b, "  路径: %s\n", s.Root)
	}
	if len(s.Manifest.Triggers) > 0 {
		fmt.Fprintf(&b, "  触发: %s\n", strings.Join(s.Manifest.Triggers, " | "))
	}
	return b.String()
}

// NameFromPath 从目录路径提取 skill 名(取最后一段)
func NameFromPath(root string) string {
	return filepath.Base(root)
}
