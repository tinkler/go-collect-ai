# Skill 评测 JSON Schema 规范(7 个数据结构)

> 7 个 JSON 互相引用,形成完整的数据管道。对齐 Anthropic skill-creator references/schemas.md。

## 1. `cases.json` — 测试定义

```json
{
  "skill": "seasonal-buying",
  "version": "1.0.0",
  "cases": [
    {
      "id": "case-001",
      "prompt": "下个月要过节了,要不要备货?",
      "context": {
        "today": "2026-09-02",
        "user_role": "owner"
      },
      "expected_skill": "seasonal-buying",
      "expected_output_keywords": ["中秋", "23 天", "5 倍", "建议"],
      "must_call_tools": ["invoke_skill"],
      "must_not_call_tools": ["remember_supplier_policy"]
    }
  ]
}
```

字段说明:
- `expected_skill` — 期望 LLM 选中的 skill 名(测试**触发准确率**)
- `expected_output_keywords` — 输出必须包含的关键词(测试**响应相关性**)
- `must_call_tools` / `must_not_call_tools` — 路由白名单(测试**工具选择**)
- `context.today` — 注入当前日期(模拟 LLM 看到的 now)

## 2. `assertions.json` — 断言定义

```json
{
  "skill": "seasonal-buying",
  "version": "1.0.0",
  "assertions": [
    {
      "id": "assert-001",
      "case_id": "case-001",
      "type": "contains",
      "check": "skill_loaded",
      "value": "seasonal-buying"
    },
    {
      "id": "assert-002",
      "case_id": "case-001",
      "type": "json_field",
      "check": "next_event.name",
      "value": "中秋节"
    },
    {
      "id": "assert-003",
      "case_id": "case-001",
      "type": "regex",
      "check": "next_event.days_until",
      "value": "^[12][0-9]$"
    }
  ]
}
```

类型:
- `contains` — 输出中包含某字符串
- `exact` — 严格相等
- `regex` — 正则匹配
- `json_field` — JSON 路径取值后再比较

## 3. `grading.json` — Grader 输出

```json
{
  "skill": "seasonal-buying",
  "version": "1.0.0",
  "run_at": "2026-09-02T18:00:00Z",
  "summary": {
    "total": 8,
    "passed": 6,
    "failed": 2,
    "pass_rate": 0.75
  },
  "cases": [
    {
      "id": "case-001",
      "status": "pass",
      "assertion_results": [...],
      "evidence": "LLM 调 invoke_skill(load) 拿到 next_event=中秋节,days_until=23,断言全部通过"
    }
  ]
}
```

## 4. `comparison.json` — Comparator 输出

```json
{
  "skill": "seasonal-buying",
  "v1": "1.0.0",
  "v2": "1.0.1",
  "blind_runs": [
    {
      "case_id": "case-001",
      "a": {"score": 8, "content_match": "..."},
      "b": {"score": 9, "content_match": "..."},
      "winner": "b",
      "rationale": "B 多了 days_until 字段,信息更完整"
    }
  ]
}
```

## 5. `analysis.json` — Analyzer 输出

```json
{
  "skill": "seasonal-buying",
  "generated_at": "2026-09-02T18:30:00Z",
  "issues": [
    {
      "case_id": "case-003",
      "category": "description_too_vague",
      "priority": "high",
      "suggestion": "description 缺 '春节' 关键词,LLM 看到 '下个月要过年' 时没触发"
    }
  ],
  "metrics": {
    "avg_response_time_ms": 1850,
    "total_tokens": 12500
  }
}
```

## 6. `history.json` — 版本历史

```json
{
  "skill": "seasonal-buying",
  "runs": [
    {"version": "1.0.0", "date": "2026-09-02", "pass_rate": 0.75, "issues": 4},
    {"version": "1.0.1", "date": "2026-09-03", "pass_rate": 0.88, "issues": 2}
  ]
}
```

## 7. `eval_viewer.html` — 人工 review 页面(可选)

```html
<!-- 简单 HTML,逐 case 显示 LLM 输出 + Grader 评分 + 人工填 feedback -->
<div class="case" id="case-001">
  <h3>Case 001: 下个月要过节了</h3>
  <p><b>Prompt:</b> 下个月要过节了,要不要备货?</p>
  <p><b>LLM 输出:</b> ...</p>
  <p><b>Grader:</b> ✅ pass</p>
  <textarea name="feedback-001">老板可能更想看备货量,不是节前天数</textarea>
</div>
```

## 更新历史

- 2026-09-02 v1.0 初版
