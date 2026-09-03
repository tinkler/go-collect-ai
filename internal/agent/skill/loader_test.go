package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile 辅助:mkdir + write
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestLoad_ValidSkill(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "seasonal-buying", "SKILL.md"), `---
name: seasonal-buying
description: 应季采购窗口判定,给老板节前预警和备货倍数建议(节假日/季节/促销档期)。
---
# Seasonal Buying
body
`)
	res, err := Load([]string{root})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Skills) != 1 {
		t.Fatalf("want 1 skill, got %d (errors: %s)", len(res.Skills), res.FormatErrors())
	}
	sk := res.Skills[0]
	if sk.Manifest.Name != "seasonal-buying" {
		t.Errorf("name = %q, want seasonal-buying", sk.Manifest.Name)
	}
	if !strings.Contains(sk.Body, "Seasonal Buying") {
		t.Errorf("body 没解析出来: %q", sk.Body)
	}
}

func TestValidate_RejectsBadName(t *testing.T) {
	cases := []struct {
		name, skillName, dirName, want string
	}{
		{"空", "", "foo", "name 必填"},
		{"大写", "Foo", "foo", "不合法"},
		{"首字符连字符", "-foo", "-foo", "不合法"},
		{"尾连字符", "foo-", "foo-", "不合法"},
		{"连续连字符", "foo--bar", "foo--bar", "不合法"},
		{"64 字符", strings.Repeat("a", 65), strings.Repeat("a", 65), "过长"},
		{"目录名不一致", "foo", "bar", "必须等于父目录名"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &Manifest{Name: c.skillName, Description: "够长的描述字符串用于测试"}
			err := Validate(m, c.dirName)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %q, want contains %q", err.Error(), c.want)
			}
		})
	}
}

func TestValidate_DescriptionChecks(t *testing.T) {
	// 空
	m := &Manifest{Name: "ok", Description: ""}
	if err := Validate(m, "ok"); err == nil {
		t.Error("空 description 应报错")
	}
	// 过长
	m2 := &Manifest{Name: "ok", Description: strings.Repeat("x", 1025)}
	if err := Validate(m2, "ok"); err == nil {
		t.Error("过长 description 应报错")
	}
	// 过短
	m3 := &Manifest{Name: "ok", Description: "短的"}
	if err := Validate(m3, "ok"); err == nil {
		t.Error("过短 description 应报错(>=10 字符)")
	}
}

func TestLoad_DuplicateName(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	skill := `---
name: seasonal-buying
description: 应季采购窗口判定,给老板节前预警和备货倍数建议。
---
# body
`
	writeFile(t, filepath.Join(rootA, "seasonal-buying", "SKILL.md"), skill)
	writeFile(t, filepath.Join(rootB, "seasonal-buying", "SKILL.md"), skill)
	res, err := Load([]string{rootA, rootB})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Skills) != 1 {
		t.Errorf("重名应只保留 1 个,got %d", len(res.Skills))
	}
	if len(res.Errors) == 0 {
		t.Error("应有 1 个重名错误")
	}
}

func TestLoad_InvalidSkillNotBlocking(t *testing.T) {
	root := t.TempDir()
	// 1 个非法
	writeFile(t, filepath.Join(root, "bad", "SKILL.md"), `---
name: BadName
description: ok
---
`)
	// 1 个合法
	writeFile(t, filepath.Join(root, "good", "SKILL.md"), `---
name: good
description: 一个合法 skill,做点事
---
`)
	res, err := Load([]string{root})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Skills) != 1 || res.Skills[0].Manifest.Name != "good" {
		t.Errorf("want exactly [good], got %+v", res.Skills)
	}
	if len(res.Errors) == 0 {
		t.Error("应有 1 个错误(bad)")
	}
}

func TestLoad_EnumeratesSubdirs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "demo", "SKILL.md"), `---
name: demo
description: 演示 skill 用于测试,跑一下就好
---
# demo
body
`)
	writeFile(t, filepath.Join(root, "demo", "scripts", "compute.py"), "print(1)")
	writeFile(t, filepath.Join(root, "demo", "references", "facts.md"), "# facts")
	res, _ := Load([]string{root})
	if len(res.Skills) != 1 {
		t.Fatalf("want 1, got %d", len(res.Skills))
	}
	sk := res.Skills[0]
	if len(sk.Scripts) != 1 || sk.Scripts[0] != "scripts/compute.py" {
		t.Errorf("scripts = %v, want [scripts/compute.py] (带前缀)", sk.Scripts)
	}
	if len(sk.References) != 1 || sk.References[0] != "references/facts.md" {
		t.Errorf("references = %v, want [references/facts.md] (带前缀)", sk.References)
	}
}

func TestLoad_MissingRootIsOk(t *testing.T) {
	res, err := Load([]string{t.TempDir() + "/does-not-exist"})
	if err != nil {
		t.Fatalf("missing root shouldn't error: %v", err)
	}
	if len(res.Skills) != 0 {
		t.Errorf("want 0 skills, got %d", len(res.Skills))
	}
}

func TestStore_ReplaceAndL1Prompt(t *testing.T) {
	store := NewStore()
	store.Replace([]*Skill{
		{
			Manifest: Manifest{Name: "a-skill", Description: "干 A 事的 skill"},
			Root:     "/tmp/a-skill",
		},
		{
			Manifest: Manifest{Name: "b-skill", Description: "干 B 事的 skill"},
			Root:     "/tmp/b-skill",
		},
	})
	if store.Count() != 2 {
		t.Fatalf("Count = %d, want 2", store.Count())
	}
	prompt := store.L1Prompt()
	if !strings.Contains(prompt, "a-skill") || !strings.Contains(prompt, "b-skill") {
		t.Errorf("L1Prompt 缺 skill name: %q", prompt)
	}
	if !strings.Contains(prompt, "Available Skills") {
		t.Errorf("L1Prompt 缺 header: %q", prompt)
	}
}

func TestStore_GetMissing(t *testing.T) {
	store := NewStore()
	if _, ok := store.Get("nope"); ok {
		t.Error("Get nope 应返 ok=false")
	}
}
