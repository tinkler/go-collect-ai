// e2e/skill_e2e_test.go — 端到端验证 skill 系统
//   1) Load 加载项目内 skills/seasonal-buying/
//   2) invoke_skill(load) → 拿到 SKILL.md 全文
//   3) invoke_skill(run_script) → 跑 compute_window.py → 拿到 next_event
//   4) 验证 description / 触发词 / 路径 / 工具联动
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tinkler/collect-ai/internal/agent/skill"
)

// repoRoot 找项目根(含 go.mod)
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := wd
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("找不到 go.mod(从 %s 向上)", wd)
	return ""
}

func TestE2E_SkillSeasonalBuying(t *testing.T) {
	root := repoRoot(t)
	skillsDir := filepath.Join(root, "skills")

	// 1) 加载
	res, err := skill.Load([]string{skillsDir})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Skills) == 0 {
		t.Fatalf("未加载到任何 skill,errors: %s", res.FormatErrors())
	}

	var sb *skill.Skill
	for _, s := range res.Skills {
		if s.Manifest.Name == "seasonal-buying" {
			sb = s
			break
		}
	}
	if sb == nil {
		t.Fatalf("找不到 seasonal-buying skill,got: %+v", res.Skills)
	}

	// 1.1) 校验 description
	desc := sb.Manifest.Description
	for _, kw := range []string{"应季", "中秋", "春节", "备货", "节假日", "618", "双11"} {
		if !strings.Contains(desc, kw) {
			t.Errorf("description 缺关键词 %q: %q", kw, desc)
		}
	}

	// 1.2) 校验 scripts / references
	if len(sb.Scripts) == 0 {
		t.Error("应有 scripts")
	}
	if len(sb.References) == 0 {
		t.Error("应有 references")
	}

	// 2) 模拟 LLM 调 invoke_skill(load)
	store := skill.NewStore()
	store.Replace(res.Skills)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	loadOut, err := skill.RunInvokeForTest(ctx, store, skill.InvokeSkillReq{
		SkillName: "seasonal-buying",
		Action:    "load",
		Input:     "下个月要过节了,要不要备货?",
	})
	if err != nil {
		t.Fatalf("load invoke: %v", err)
	}
	if !strings.Contains(loadOut.Body, "Seasonal Buying") {
		t.Errorf("load 返 body 不含 SKILL.md 正文: %s", loadOut.Body[:min(200, len(loadOut.Body))])
	}
	if !strings.Contains(loadOut.Body, "下个月要过节了") {
		t.Error("load 返 body 应追加 input")
	}
	if !strings.Contains(loadOut.Body, "compute_window.py") {
		t.Error("load 返 body 应提到 compute_window.py")
	}

	// 3) 调 run_script
	runOut, err := skill.RunInvokeForTest(ctx, store, skill.InvokeSkillReq{
		SkillName: "seasonal-buying",
		Action:    "run_script",
		Path:      "scripts/compute_window.py",
		Args:      json.RawMessage(`{"today": "2026-09-02"}`),
	})
	if err != nil {
		t.Fatalf("run_script invoke: %v (output: %s)", err, runOut.Output)
	}
	// stdout 是 JSON,parse 后看 next_event
	var win struct {
		Today         string `json:"today"`
		NextEvent     struct {
			Name      string  `json:"name"`
			Date      string  `json:"date"`
			DaysUntil int     `json:"days_until"`
			Multi     float64 `json:"recommended_multiplier"`
		} `json:"next_event"`
		ActiveSeasons []struct {
			Name string `json:"name"`
		} `json:"active_seasons"`
	}
	if err := json.Unmarshal([]byte(runOut.Output), &win); err != nil {
		t.Fatalf("parse script output: %v\nraw: %s", err, runOut.Output)
	}
	if win.Today != "2026-09-02" {
		t.Errorf("today = %q, want 2026-09-02", win.Today)
	}
	if win.NextEvent.Name == "" {
		t.Error("next_event.name 空")
	}
	if win.NextEvent.DaysUntil <= 0 {
		t.Errorf("days_until 应 > 0,got %d", win.NextEvent.DaysUntil)
	}
	t.Logf("✅ next_event = %s(%s, %d 天, 倍数 %.1f)",
		win.NextEvent.Name, win.NextEvent.Date, win.NextEvent.DaysUntil, win.NextEvent.Multi)
	t.Logf("✅ active_seasons = %+v", win.ActiveSeasons)

	// 4) 验证 hot-reload:加一个新 skill,200ms 后 L1 应包含它
	hotDir := t.TempDir()
	hotSkills := filepath.Join(hotDir, "skills")
	if err := os.MkdirAll(filepath.Join(hotSkills, "hot-added"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(hotSkills, "hot-added", "SKILL.md"),
		[]byte("---\nname: hot-added\ndescription: e2e 测试热更新用的临时 skill\n---\n# hot\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	store2 := skill.NewStore()
	store2.SetRoots([]string{hotSkills})
	w, err := skill.NewWatcher(store2, []string{hotSkills}, func(roots []string) (*skill.LoadResult, error) {
		return skill.Load(roots)
	})
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Stop()

	// 等热加载
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := store2.Get("hot-added"); ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, ok := store2.Get("hot-added"); !ok {
		t.Fatal("热更新未生效")
	}
	t.Logf("✅ 热更新 OK,当前 skill 数 = %d", store2.Count())
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
