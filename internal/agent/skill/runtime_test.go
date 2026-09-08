package skill

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInvokeSkill_Load(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "demo", "SKILL.md"), `---
name: demo
description: 测试用 demo skill,演示 load 动作
---
# Demo
body content
`)

	res, _ := Load([]string{root})
	store := NewStore()
	store.Replace(res.Skills)

	out, err := runInvoke(context.Background(), store, InvokeSkillReq{
		SkillName: "demo",
		Action:    "load",
		Input:     "今天要备什么",
	})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(out.Body, "body content") {
		t.Errorf("body 应包含正文,got %q", out.Body)
	}
	if !strings.Contains(out.Body, "今天要备什么") {
		t.Errorf("body 应包含 input 追加,got %q", out.Body)
	}
	if out.Action != "load" {
		t.Errorf("Action = %q, want load", out.Action)
	}
}

func TestInvokeSkill_Unknown(t *testing.T) {
	store := NewStore()
	_, err := runInvoke(context.Background(), store, InvokeSkillReq{
		SkillName: "ghost",
		Action:    "load",
	})
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("err = %q, want 含 ghost", err.Error())
	}
}

func TestInvokeSkill_ReadFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "demo", "SKILL.md"), `---
name: demo
description: 测试 read_file 动作
---
# body
`)
	writeFile(t, filepath.Join(root, "demo", "references", "facts.md"), "# facts\n- 事实 1\n- 事实 2\n")

	res, _ := Load([]string{root})
	store := NewStore()
	store.Replace(res.Skills)

	out, err := runInvoke(context.Background(), store, InvokeSkillReq{
		SkillName: "demo",
		Action:    "read_file",
		Path:      "references/facts.md",
	})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(out.Content, "事实 1") {
		t.Errorf("content 应含事实 1,got %q", out.Content)
	}
}

func TestInvokeSkill_ReadFilePathTraversalBlocked(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "demo", "SKILL.md"), `---
name: demo
description: 测试路径穿越防护
---
# body
`)

	res, _ := Load([]string{root})
	store := NewStore()
	store.Replace(res.Skills)

	_, err := runInvoke(context.Background(), store, InvokeSkillReq{
		SkillName: "demo",
		Action:    "read_file",
		Path:      "../etc/passwd",
	})
	if err == nil {
		t.Fatal("应拦截路径穿越")
	}
}

// TestInvokeSkill_ReadFileRejectsScriptsDir 验证 read_file 不允许读 scripts/ 下的脚本
// (防止 LLM 用 read_file 误读 .py 源码, 脚本必须用 run_script 跑)
func TestInvokeSkill_ReadFileRejectsScriptsDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "demo", "SKILL.md"), `---
name: demo
description: 测试 read_file 拒绝 scripts/ 下的脚本
---
# body
`)
	scriptDir := filepath.Join(root, "demo", "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(scriptDir, "compute.py"), "print(1)")

	res, _ := Load([]string{root})
	store := NewStore()
	store.Replace(res.Skills)

	_, err := runInvoke(context.Background(), store, InvokeSkillReq{
		SkillName: "demo",
		Action:    "read_file",
		Path:      "scripts/compute.py",
	})
	if err == nil {
		t.Fatal("read_file 读 scripts/ 应被拒绝")
	}
	if !strings.Contains(err.Error(), "scripts/") {
		t.Errorf("err 应说明 scripts/ 限制,got %q", err.Error())
	}
}

func TestInvokeSkill_RunScript(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "demo", "SKILL.md"), `---
name: demo
description: 测试 run_script 动作
---
# body
`)
	scriptDir := filepath.Join(root, "demo", "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 写个 echo.py:读 stdin,回写
	script := `import json, sys
data = json.loads(sys.stdin.read() or "{}")
print(json.dumps({"echo": data.get("x", 0) * 2, "ok": True}))
`
	if err := os.WriteFile(filepath.Join(scriptDir, "echo.py"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}

	res, _ := Load([]string{root})
	store := NewStore()
	store.Replace(res.Skills)

	out, err := runInvoke(context.Background(), store, InvokeSkillReq{
		SkillName: "demo",
		Action:    "run_script",
		Path:      "scripts/echo.py",
		Args:      json.RawMessage(`{"x": 21}`),
	})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(out.Output, `"echo": 42`) {
		t.Errorf("output 应含 echo=42,got %q", out.Output)
	}
}

func TestInvokeSkill_RunScriptPathTraversalBlocked(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "demo", "SKILL.md"), `---
name: demo
description: 测试脚本路径穿越
---
# body
`)

	res, _ := Load([]string{root})
	store := NewStore()
	store.Replace(res.Skills)

	_, err := runInvoke(context.Background(), store, InvokeSkillReq{
		SkillName: "demo",
		Action:    "run_script",
		Path:      "../evil.py",
	})
	if err == nil {
		t.Fatal("应拦截脚本路径穿越")
	}
}

func TestInvokeSkill_ActionValidation(t *testing.T) {
	store := NewStore()
	_, err := runInvoke(context.Background(), store, InvokeSkillReq{
		SkillName: "demo",
		Action:    "explode",
	})
	if err == nil {
		t.Fatal("非法 action 应报错")
	}
}
