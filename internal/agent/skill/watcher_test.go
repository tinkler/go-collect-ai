package skill

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatcher_HotReloadOnAdd(t *testing.T) {
	root := t.TempDir()

	store := NewStore()
	store.SetRoots([]string{root})

	// 构造一个 loader 闭包:扫 root 下子目录
	loader := func(roots []string) (*LoadResult, error) {
		return Load(roots)
	}

	w, err := NewWatcher(store, []string{root}, loader)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Stop()

	// 初始为 0
	if got := store.Count(); got != 0 {
		t.Errorf("初始 Count = %d, want 0", got)
	}

	// 落一个新 skill
	skillDir := filepath.Join(root, "added-later")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: added-later\ndescription: 后加的 skill,用于测试热更新\n---\n# body\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	// 等热更新触发(防抖 200ms + 一些 buffer)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if store.Count() >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := store.Count(); got != 1 {
		t.Fatalf("热更新后 Count = %d, want 1", got)
	}
	if _, ok := store.Get("added-later"); !ok {
		t.Errorf("added-later 应该在 store 里")
	}
}

func TestWatcher_HotReloadOnModify(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "demo")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	initial := "---\nname: demo\ndescription: 初始描述,用于热更新测试\n---\n# body\n"
	skillPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	store := NewStore()
	store.SetRoots([]string{root})
	loader := func(roots []string) (*LoadResult, error) { return Load(roots) }
	w, err := NewWatcher(store, []string{root}, loader)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	// 等首次加载
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if store.Count() == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := store.Count(); got != 1 {
		t.Fatalf("首次加载后 Count = %d, want 1", got)
	}

	// 修改 SKILL.md 触发 WRITE 事件
	updated := "---\nname: demo\ndescription: 修改后的描述,版本 2,用于验证热更新\n---\n# body v2\n"
	if err := os.WriteFile(skillPath, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}

	// 等热更新
	deadline = time.Now().Add(2 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		sk, ok := store.Get("demo")
		if ok {
			got = sk.Manifest.Description
			if got != "" && got != "初始描述,用于热更新测试" {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got != "修改后的描述,版本 2,用于验证热更新" {
		t.Errorf("热更新后 description = %q, want 修改后", got)
	}
}

func TestWatcher_HotReloadOnRemove(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "soon-gone")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: soon-gone\ndescription: 即将被删的 skill,用于测试热移除\n---\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	store := NewStore()
	store.SetRoots([]string{root})
	loader := func(roots []string) (*LoadResult, error) { return Load(roots) }
	w, err := NewWatcher(store, []string{root}, loader)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if store.Count() == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := store.Count(); got != 1 {
		t.Fatalf("首次加载后 Count = %d, want 1", got)
	}

	// 删除整个 skill 目录
	if err := os.RemoveAll(skillDir); err != nil {
		t.Fatal(err)
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if store.Count() == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := store.Count(); got != 0 {
		t.Errorf("删除后 Count = %d, want 0", got)
	}
}
