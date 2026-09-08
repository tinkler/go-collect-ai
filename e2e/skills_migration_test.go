// e2e/skills_migration_test.go — 验证 3 个新迁移的 skill 全部可加载 + 可 invoke
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

// TestE2E_MigratedSkills 验证 3 个迁移的 skill:
//   1) settlement-suggestion  2) supplier-policy  3) restock-strategy
//   - 加载所有 skill
//   - 校验 description 含必要关键词
//   - invoke_skill(load) 拿正文
//   - invoke_skill(read_file) 读 references
//   - invoke_skill(run_script) 跑 scripts
func TestE2E_MigratedSkills(t *testing.T) {
	root := repoRoot(t)
	skillsDir := filepath.Join(root, "skills")

	res, err := skill.Load([]string{skillsDir})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Skills) < 4 { // seasonal-buying + 3 new = 4
		t.Fatalf("至少 4 个 skill,got %d(errors: %s)", len(res.Skills), res.FormatErrors())
	}

	store := skill.NewStore()
	store.Replace(res.Skills)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 1) settlement-suggestion
	t.Run("settlement-suggestion", func(t *testing.T) {
		validateSkillKeywords(t, store, "settlement-suggestion",
			"堆头费", "端架费", "促销费", "供应商结算", "付款建议", "settlement")

		// 跑 calc_share.py
		runScriptAndCheck(t, ctx, store, "settlement-suggestion", "scripts/calc_share.py",
			`{"amount": 5000, "period_start": "2026-01-15", "period_end": "2026-03-15", "month": "2026-02"}`,
			func(out string) {
				if !strings.Contains(out, "overlap_days") {
					t.Errorf("应含 overlap_days,got %s", out)
				}
			},
		)

		// 跑 assess_investment.py
		runScriptAndCheck(t, ctx, store, "settlement-suggestion", "scripts/assess_investment.py",
			`{"month_share": 1500, "month_forecast": 10000, "supplier_tier": "strategic"}`,
			func(out string) {
				if !strings.Contains(out, "investment_weight") {
					t.Errorf("应含 investment_weight,got %s", out)
				}
			},
		)

		// 读 reference
		readFileAndCheck(t, ctx, store, "settlement-suggestion", "references/coefficient_defaults.md",
			func(c string) {
				if !strings.Contains(c, "investment_weight") {
					t.Errorf("coefficient_defaults.md 应含 investment_weight")
				}
			},
		)
	})

	// 2) supplier-policy
	t.Run("supplier-policy", func(t *testing.T) {
		validateSkillKeywords(t, store, "supplier-policy",
			"供应商政策", "防回扣", "自采", "不让退", "黑名单", "反回扣")

		// 跑 parse_policy_utterance.py
		runScriptAndCheck(t, ctx, store, "supplier-policy", "scripts/parse_policy_utterance.py",
			`{"utterance": "汇一是自采供应商,堆头他们出,榄菊不让退", "known_suppliers": ["汇一", "榄菊"]}`,
			func(out string) {
				if !strings.Contains(out, "is_self_procure") || !strings.Contains(out, "allow_return") {
					t.Errorf("应拆出 is_self_procure + allow_return,got %s", out)
				}
			},
		)

		// 跑 check_concentration.py
		runScriptAndCheck(t, ctx, store, "supplier-policy", "scripts/check_concentration.py",
			`{"supplier_share": {"汇一": 0.5, "金龙鱼": 0.3, "福临门": 0.2}}`,
			func(out string) {
				if !strings.Contains(out, "hhi") {
					t.Errorf("应含 hhi,got %s", out)
				}
			},
		)

		// 读 reference
		readFileAndCheck(t, ctx, store, "supplier-policy", "references/key_semantics.md",
			func(c string) {
				if !strings.Contains(c, "is_self_procure") {
					t.Errorf("key_semantics.md 应含 is_self_procure")
				}
			},
		)
	})

	// 3) restock-strategy
	t.Run("restock-strategy", func(t *testing.T) {
		validateSkillKeywords(t, store, "restock-strategy",
			"补货", "库存预警", "P0", "fill_rate", "备货周期", "优先级")

		// 跑 compute_suggest_qty.py
		runScriptAndCheck(t, ctx, store, "restock-strategy", "scripts/compute_suggest_qty.py",
			`{"daily_avg": 30, "lead_days": 7, "safety_days": 1.5, "fill_rate": 0.95, "ceil_unit": 12}`,
			func(out string) {
				if !strings.Contains(out, "final_qty") {
					t.Errorf("应含 final_qty,got %s", out)
				}
			},
		)

		// 跑 days_until_stockout.py
		runScriptAndCheck(t, ctx, store, "restock-strategy", "scripts/days_until_stockout.py",
			`{"inv_snapshot": 12, "daily_avg": 30}`,
			func(out string) {
				if !strings.Contains(out, "P0") {
					t.Errorf("12/30=0.4 应是 P0,got %s", out)
				}
			},
		)

		// 跑 supplier_fill_rate.py
		runScriptAndCheck(t, ctx, store, "restock-strategy", "scripts/supplier_fill_rate.py",
			`{"delivered_qty": 950, "ordered_qty": 1000}`,
			func(out string) {
				if !strings.Contains(out, "fill_rate") {
					t.Errorf("应含 fill_rate,got %s", out)
				}
			},
		)

		// 读 reference
		readFileAndCheck(t, ctx, store, "restock-strategy", "references/priority_semantics.md",
			func(c string) {
				if !strings.Contains(c, "P0") || !strings.Contains(c, "P3") {
					t.Errorf("priority_semantics.md 应含 P0/P3")
				}
			},
		)
	})

	// 4) 全部 4 个 skill(包括 seasonal-buying)都能 load
	if got := store.Count(); got < 4 {
		t.Errorf("应有 >= 4 个 skill,got %d", got)
	}
	t.Logf("✅ 共 %d 个 skill 加载成功", store.Count())
	for _, sk := range store.List() {
		t.Logf("   - %s [%s] scripts=%d refs=%d", sk.Manifest.Name, sk.Source, len(sk.Scripts), len(sk.References))
	}
}

// === helpers ===

func validateSkillKeywords(t *testing.T, store *skill.Store, name string, keywords ...string) {
	t.Helper()
	sk, ok := store.Get(name)
	if !ok {
		t.Fatalf("skill %q 未加载", name)
	}
	desc := sk.Manifest.Name + " " + sk.Manifest.Description
	for _, kw := range keywords {
		if !strings.Contains(desc, kw) {
			t.Errorf("[%s] description 缺关键词 %q", name, kw)
		}
	}
}

func runScriptAndCheck(t *testing.T, ctx context.Context, store *skill.Store, skillName, path, args string, check func(string)) {
	t.Helper()
	out, err := skill.RunInvokeForTest(ctx, store, skill.InvokeSkillReq{
		SkillName: skillName,
		Action:    "run_script",
		Path:      path,
		Args:      json.RawMessage(args),
	})
	if err != nil {
		t.Fatalf("[%s] run_script %s: %v\noutput: %s", skillName, path, err, out.Output)
	}
	check(out.Output)
}

func readFileAndCheck(t *testing.T, ctx context.Context, store *skill.Store, skillName, path string, check func(string)) {
	t.Helper()
	out, err := skill.RunInvokeForTest(ctx, store, skill.InvokeSkillReq{
		SkillName: skillName,
		Action:    "read_file",
		Path:      path,
	})
	if err != nil {
		t.Fatalf("[%s] read_file %s: %v", skillName, path, err)
	}
	check(out.Content)
}

// 兼容 _test.go 已有 repoRoot
var _ = repoRoot
var _ = os.Getenv
