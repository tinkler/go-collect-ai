package skill

import "os"

func readDir(dir string) ([]os.DirEntry, error) {
	return os.ReadDir(dir)
}

func statDir(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

// SourceFromRoot 推断 source 标签(给热重载后的 Skill 标来源)
//   <cwd>/skills           → "project"
//   <home>/.claude/skills  → "user-claude"
//   <home>/.agents/skills  → "user-agents"
//   其它                   → "extra:<path>"
func SourceFromRoot(root string) string {
	home, _ := os.UserHomeDir()
	switch {
	case home != "" && (root == home+"/.claude/skills" || root == home+"\\.claude\\skills"):
		return "user-claude"
	case home != "" && (root == home+"/.agents/skills" || root == home+"\\.agents\\skills"):
		return "user-agents"
	default:
		// 简化:不再细分 project vs extra
		return "extra"
	}
}
