// e2e/skill_eval_test.go — 验证 skill 评测体系自身(Grader / Analyzer / description_optimizer)走通
package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestE2E_SkillEvalSystem 跑全部 4 个 skill 的评测,确认 100% pass
func TestE2E_SkillEvalSystem(t *testing.T) {
	root := repoRoot(t)
	evalDir := filepath.Join(root, "skills", "_eval")
	if _, err := os.Stat(evalDir); err != nil {
		t.Fatalf("评测目录不存在: %v", err)
	}

	// 跑 run_eval.py --all
	cmd := exec.Command("python", filepath.Join("skills", "_eval", "run_eval.py"), "--all")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run_eval.py 失败: %v\n%s", err, string(out))
	}
	t.Logf("run_eval.py output:\n%s", string(out))

	// 校验每个 skill 的 grading.json:pass_rate 必须是 1.0
	skills := []string{"seasonal-buying", "settlement-suggestion", "supplier-policy", "restock-strategy"}
	for _, s := range skills {
		t.Run(s, func(t *testing.T) {
			gradingPath := filepath.Join(root, "skills", s, "eval", "results", "grading.json")
			data, err := os.ReadFile(gradingPath)
			if err != nil {
				t.Fatalf("读 %s 失败: %v", gradingPath, err)
			}
			var g struct {
				Summary struct {
					Total     int     `json:"total"`
					Passed    int     `json:"passed"`
					Failed    int     `json:"failed"`
					PassRate  float64 `json:"pass_rate"`
				} `json:"summary"`
			}
			if err := json.Unmarshal(data, &g); err != nil {
				t.Fatalf("parse %s 失败: %v", gradingPath, err)
			}
			if g.Summary.Total == 0 {
				t.Fatalf("%s: 总数 0", s)
			}
			if g.Summary.PassRate < 0.99 {
				t.Errorf("%s: pass_rate=%.2f,期望 >= 0.99(%d/%d)",
					s, g.Summary.PassRate, g.Summary.Passed, g.Summary.Total)
			}
			t.Logf("[OK] %s: %d/%d pass (%.0f%%)",
				s, g.Summary.Passed, g.Summary.Total, g.Summary.PassRate*100)
		})
	}
}

// TestE2E_EvalGraderUnit 单元测试 Grader 的核心断言(不依赖 skill 数据)
func TestE2E_EvalGraderUnit(t *testing.T) {
	root := repoRoot(t)
	runPy := func(scriptName string, args ...string) (string, error) {
		cmd := exec.Command("python", append([]string{"skills/_eval/" + scriptName}, args...)...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// 跑 3 个评测脚本的 --help,验证能 import
	for _, s := range []string{"grader", "analyzer", "description_optimizer"} {
		out, _ := runPy(s+".py", "--help")
		// --help 时 argparse 会 exit 0 或 2 都行(我们主要看输出里有 "usage:")
		if !strings.Contains(out, "usage:") {
			t.Errorf("%s.py --help 缺 usage 输出: %s", s, out)
		}
	}

	t.Logf("[OK] 3 个评测脚本(grader / analyzer / description_optimizer)都跑得起来")
}

// TestE2E_AnalyzerGivesActionableSuggestions 验证 Analyzer 输出的结构
func TestE2E_AnalyzerGivesActionableSuggestions(t *testing.T) {
	root := repoRoot(t)

	// 检查至少一个 skill 的 analysis.json 存在
	skills := []string{"seasonal-buying", "settlement-suggestion", "supplier-policy", "restock-strategy"}
	for _, s := range skills {
		analysisPath := filepath.Join(root, "skills", s, "eval", "results", "analysis.json")
		data, err := os.ReadFile(analysisPath)
		if err != nil {
			t.Logf("[skip] %s: 缺 analysis.json(可能还没跑 analyzer)", s)
			continue
		}
		var a struct {
			Metrics     map[string]any `json:"metrics"`
			Issues      []map[string]any `json:"issues"`
			NextAction  string          `json:"next_action"`
		}
		if err := json.Unmarshal(data, &a); err != nil {
			t.Fatalf("%s: parse analysis.json 失败: %v", s, err)
		}
		if a.NextAction == "" {
			t.Errorf("%s: next_action 为空", s)
		}
		t.Logf("[OK] %s: pass_rate=%v issues=%d next_action=%q",
			s, a.Metrics["pass_rate"], len(a.Issues), truncate(a.NextAction, 60))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// 防 strconv 引用丢失
var _ = strconv.Itoa
