# Skill 评测体系(Anthropic skill-creator 风格)

> 4 个 Agent Skills 的"训练/测试集" + 3 Agent 协作(Grader + Comparator + Analyzer)。
> 参照 Anthropic 官方 skill-creator 6 阶段方法论,本目录是 W4 沉淀产物。

## 目录结构

```
skills/_eval/
  README.md                    # 本文件
  schemas.md                   # 7 个 JSON 数据结构
  grader.py                    # 评分:断言通过/失败 + 证据
  comparator.py                # 盲 A/B 对比:对两个版本打分
  analyzer.py                  # 揭盲后:WHY 赢家赢了 + 改进建议
  run_eval.py                  # 一键:Grader → Comparator → Analyzer
  description_optimizer.py     # 基于失败用例的 description 微调
  report_template.md           # 评测报告模板
```

## 4 个 skill 的 eval 数据

每个 skill 在自己的目录下放 `eval/`:

```
skills/<skill-name>/
  eval/
    cases.json         # {id, prompt, context, expected_skill, expected_output_keywords}
    assertions.json    # {id, type: "exact"|"contains"|"regex"|"json_field", check: ..., value: ...}
    results/           # 跑出来后的结果(grader 输出、comparator 输出)
```

## 跑评测

```bash
# 跑单个 skill
python skills/_eval/run_eval.py seasonal-buying

# 跑全部
python skills/_eval/run_eval.py --all

# 仅 Grader(快)
python skills/_eval/grader.py seasonal-buying

# A/B 对比(对比 v1 和 v2)
python skills/_eval/comparator.py seasonal-buying --v1 v1.0.0 --v2 v2.0.0

# 揭盲后给改进建议
python skills/_eval/analyzer.py seasonal-buying --latest
```

## 评测指标

| 指标 | 含义 | 目标 |
|---|---|---|
| 触发准确率 | LLM 调 invoke_skill 时,**正确的 skill** 被选中的比例 | >= 90% |
| 断言通过率 | 期望输出与实际输出匹配的比例 | >= 80% |
| 响应延迟 | invoke_skill(load) 返回耗时 | < 2s |
| token 消耗 | L1 system prompt + skill body + output 总 token | 报告趋势 |

## 3 Agent 协作流程

```
  cases.json + assertions.json
            │
            ▼
   ┌─────────────────┐
   │  Grader Agent   │  跑 N 个 case,每个用 LLM 触发 skill,断言输出
   └─────────────────┘
            │ grading.json
            ▼
  ┌──────────────────┐
  │ Comparator Agent │  盲 A/B:把 v1 和 v2 输出打乱,逐项评分
  └──────────────────┘
            │ comparison.json
            ▼
  ┌──────────────────┐
  │  Analyzer Agent  │  揭盲,看 WHY 赢家赢 / 输家输,生成改进建议
  └──────────────────┘
            │ analysis.json
            ▼
   review.md (人审)
```

## 编辑权限

老板 / 运营经理可以加 case(覆盖更多老板话),但**不能改 assertion**(改完容易让所有 case 都"通过",产生 false confidence)。

## 更新历史

- 2026-09-02 v1.0 初版,从 Anthropic skill-creator 方法论适配
