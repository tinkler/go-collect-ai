package skill

import (
	"fmt"
	"regexp"
	"strings"
)

// nameRe 严格对齐 Anthropic 规则:
//   - 1-64 字符
//   - 仅小写字母数字 + 单个连字符
//   - 不能以连字符开头/结尾
//   - 无连续连字符
var nameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

const (
	maxNameLen        = 64
	maxDescriptionLen = 1024
	maxCompatibility  = 500
)

// Validate 校验 manifest 是否符合 Anthropic spec
//   - name: 必填、长度、字符集、必须等于父目录名
//   - description: 必填、长度
//   - compatibility: 可选、长度
func Validate(m *Manifest, dirName string) error {
	if m == nil {
		return fmt.Errorf("manifest is nil")
	}

	// name
	m.Name = strings.TrimSpace(m.Name)
	if m.Name == "" {
		return fmt.Errorf("name 必填")
	}
	if len(m.Name) > maxNameLen {
		return fmt.Errorf("name 过长(%d > %d)", len(m.Name), maxNameLen)
	}
	if !nameRe.MatchString(m.Name) {
		return fmt.Errorf("name %q 不合法: 仅允许小写字母数字+单个连字符,不能以连字符开头/结尾,不能含连续连字符", m.Name)
	}
	if m.Name != dirName {
		return fmt.Errorf("name %q 必须等于父目录名 %q", m.Name, dirName)
	}

	// description
	m.Description = strings.TrimSpace(m.Description)
	if m.Description == "" {
		return fmt.Errorf("description 必填")
	}
	if len(m.Description) > maxDescriptionLen {
		return fmt.Errorf("description 过长(%d > %d)", len(m.Description), maxDescriptionLen)
	}
	if len(m.Description) < 10 {
		return fmt.Errorf("description 过短(%d 字符),至少 10 字符;description 是 LLM 触发判定的唯一信号,要写清楚\"做什么 + 何时用 + 关键词\"", len(m.Description))
	}

	// compatibility
	if len(m.Compatibility) > maxCompatibility {
		return fmt.Errorf("compatibility 过长(%d > %d)", len(m.Compatibility), maxCompatibility)
	}

	// triggers(Vercel 扩展)宽松校验
	for i, t := range m.Triggers {
		m.Triggers[i] = strings.TrimSpace(t)
		if m.Triggers[i] == "" {
			return fmt.Errorf("triggers[%d] 为空", i)
		}
	}

	return nil
}
