# OCR 供货单解析 — Skill 化重构方案

> 状态:方案已定稿(2026-09-02),等待启动 Phase A 实施
> 关联:`AGENTS.md` §1(LLM 推理逻辑外置为 Skill)、§4(Go 端无业务判断)
> 关联:现有 `internal/parser/parser.go` / `internal/parser/bigmodel/llm.go` / `internal/store/template.go`

---

## 一、背景与现状

### 1.1 当前 OCR 解析链路

`POST /api/v1/sessions` 创建收货单:

```
handler.CreateSession (internal/api/handler/handler.go:402)
  → parser.ParseImageBytes (internal/parser/parser.go:36)
    → BigModel OCR (hand_write / layout_parsing, hardcode)
    → DefaultPurchasePrompt / DefaultInventoryPrompt (internal/parser/bigmodel/llm.go:199 / 110)
        调 LLM 拆行 + 输出 JSON
    → SkuMatcher 模糊匹配
    → 写 parse_session + parse_row
```

### 1.2 现状的"半硬编码"问题

- `template` 表(`internal/store/pg.go:77`)有 11 列:`llm_prompt / ocr_model / llm_model / use_llm / fuzzy_distance / header_keywords / footer_keywords / subtitle_keywords / is_default / updated_at / note`
- 但解析骨架(`DefaultPurchasePrompt` 200+ 行)依然硬编码在 `llm.go`,`template.llm_prompt` 只是"补充提示词"的纯文本
- 同一家供应商解析 100 次,结果**几乎一致**;人工修正完后,下次再来还是错
- 没有"自学习"机制

### 1.3 trpc-agent-go 侧已就绪

`internal/agent/skill/` 已实现完整:
- `store.go` — skill 注册表,`L1Prompt()` 拼 system prompt
- `loader.go` — 扫描 `skills/` 根目录
- `runtime.go` — `invoke_skill` tool(load / run_script / read_file)
- `watcher.go` — fsnotify 热更新
- `types.go` / `validate.go` — Manifest 校验

`internal/agent/runner.go:121 NewRunner` 已注册 6 个业务 tool + `invoke_skill` tool,LLM 是 DeepSeek。

**但 OCR 解析目前完全没走 skill 系统**。

### 1.4 已有 skill(项目内)

`skills/seasonal-buying/` `skills/restock-strategy/` `skills/supplier-policy/` `skills/settlement-suggestion/` — 全部已在用,作为本方案的对齐参考。

---

## 二、设计目标

| 目标 | 落地方式 |
|---|---|
| OCR 解析从半硬编码升级到 skill 化 | prompt 模板 + 拆行规则 + sku_hints 算法全部外置到 skill 文档 |
| per-supplier 特定解析思路(只有一种) | `supplier_parse_strategy` 表,一户一条 |
| 能自我进化 | 人工修正累计 3 次 → 异步调 `optimize-parse-strategy` skill,LLM 对比 diff + 重写 strategy |
| 通用思路自动沉淀特定策略 | 通用解析累计 5 次 → 异步调 LLM 总结,自动建 strategy |
| 去掉盘点单 | 删 `DefaultInventoryPrompt` + 删 `ModeInventory` 使用路径(enum 保留兼容) |
| 少量手写供应商 | `is_handwrite=true` 走纯启发式,不开 LLM |
| 删 template 概念 | 删 `template` 表 + TemplateRepo + 4 个端点 + `parse_session.template_id` 列 |

---

## 三、关键决策(已确认 2026-09-02)

| 决策 | 选择 | 理由 |
|---|---|---|
| `supplier_parse_strategy` 存储 | **PG 表** | 热路径一次 SELECT,Upsert 原子写,易查统计;最贴近现有架构 |
| `template` 表处理 | **立即全删** | Phase C 一次性 DROP TABLE + 删 TemplateRepo + 删 4 个端点 + 删 `parse_session.template_id` 列 |
| 手写供应商标记 | **`supplier_parse_strategy.is_handwrite` 列** | 手写本身就是"该 supplier 的解析策略 = 纯启发式",不需另起表 |
| 自优化触发阈值 | **`edit_count >= 3` 自动跑** | 1 天 1-2 次,token 可控;比 5 次响应快,比每次跑省 |

---

## 四、架构总览

```
                    POST /sessions (图片)
                          │
                          ▼
              ┌─────────────────────┐
              │ ParserOrchestrator   │  ← Go 端,薄壳
              │ (新 internal/parser/  │
              │  orchestrator.go)     │
              └──────────┬──────────┘
                         │
              ┌──────────┴──────────┐
              ▼                     ▼
   supplier_parse_strategy?    is_handwrite?
              │                     │
       ┌──────┴──────┐              ▼
       ▼             ▼        纯启发式(use_llm=false)
  有特定策略      没特定策略         不调 LLM
       │             │
       ▼             ▼
   ┌─────────┐  ┌──────────┐
   │ 读策略  │  │ 查 SKU  │ ←── 业务层(BizExecutor)
   │ L1+L2  │  │ 生成 hints│
   └────┬────┘  └────┬─────┘
        │            │
        └────┬───────┘
             ▼
   ┌─────────────────────────┐
   │ skills/ocr-purchase/    │ ←── 读 SKILL.md body
   │ SKILL.md (prompt 模板) │
   │ references/            │
   │ scripts/build_hints.py │
   └──────────┬──────────────┘
              ▼
       LLM 解析(DeepSeek)
              │
              ▼
       parse_session + parse_row 落库
              │
              ▼
   ┌──────────────────────────────────┐
   │ 异步 hook:                        │
   │   - edit_count++(PATCH 时)        │
   │   - generic_apply_count++(通用)   │
   │   - 达到阈值调 LLM 自优化         │
   └──────────────────────────────────┘
              │
              ▼
   ┌──────────────────────────────────┐
   │ skills/optimize-parse-strategy/  │ ←── LLM 调 invoke_skill
   │ SKILL.md (优化规则)              │
   │ runner.Run → invoke_skill →      │
   │ 读 references/ → 写新 strategy   │
   └──────────────────────────────────┘
```

---

## 五、核心组件

### 5.1 Skill 1: `ocr-purchase`(通用 OCR 解析供货单)

**目录结构**:
```
skills/ocr-purchase/
├── SKILL.md                     # 必填:frontmatter + Markdown body
├── references/
│   ├── purchase_layouts.md      # 8 种典型供货单版式
│   └── common_ocr_errors.md     # OCR 错字表
└── scripts/
    └── build_hints.py           # sku 列表 → L1 hints JSON(可被 invoke_skill 调)
```

**SKILL.md frontmatter**:
```yaml
---
name: ocr-purchase
description: |
  解析供应商供货单 OCR 文字行,提取 barcode/name/qty。
  这是 collect-ai 唯一负责 OCR 解析的 skill,只解析供应商供货单(不再处理盘点单)。
  Use this skill when the user mentions 供货单 / 采购单 / 进货单 / 对账单 OCR 解析 /
  解析供应商送货单 / 给汇一/榄菊/XXX 解析图片 / 供货单数量提取.
license: Internal-Project
metadata:
  version: "1.0.0"
  author: collect-ai
  category: ocr-purchase
  migrated_from: "internal/parser/bigmodel/llm.go (DefaultPurchasePrompt)"
compatibility: requires Python 3.x
triggers:
  - 供货单 OCR
  - 采购单解析
  - 进货单识别
  - 供应商送货单
  - 对账单 OCR
  - 数量提取
  - 进货单据
---
```

**SKILL.md 正文结构**:
```markdown
# OCR Purchase Parser(供货单 OCR 解析)

## 适用场景
- 只解析供应商供货单(进货单/送货单/对账单)
- 不解析盘点单(已在 2026-09-02 去掉,见 docs/ocr-purchase-skill-architecture.md)
- 不解析手写供应商(is_handwrite=true 走纯启发式,不走本 skill)

## 输入
- OCR 原始文字行(`[行N] top=T text="..."`,N 标号 + top 坐标 + 文字)
- 上下文变量:
  - `{supplier}` — 供应商名
  - `{sku_hints}` — JSON 对象,字段:
    - `barcodes`: 常见 barcode 集合(用于 LLM 拼装时校验 OCR 读错)
    - `names`: 常见商品名(用于模糊匹配锚点)
    - `units`: 该供应商常用单位(件/箱/排/袋/桶)
    - `spec_patterns`: 常见规格(200ml*1*1, 1*5*4*2 等)
  - `{strategy_body}` — 供应商特定策略正文(可能为空)
  - `{prompt_overlay}` — 供应商特定追加 prompt(可能为空)

## 输出(JSON schema)
{ "rows": [{ "barcode": "...", "name": "...", "qty": <int>, "type": "data"|"skip" }] }

## 任务
1. 行类型判定(type)
2. 拆 barcode / name / qty
3. 多 SKU 合并行拆分(按 13 位 barcode 切)
4. 数量识别陷阱(规格 vs 数量,单位列错位等)

## 默认 system prompt 模板
(用 Go 端 renderTemplate 替换 4 个变量)

## 默认 LLM 输出格式
JSON,只输出 rows 数组
```

**scripts/build_hints.py**(可选,程序可读):
```python
# 输 stdin JSON: {"supplier": "...", "skus": [{barcode, name, unit, ...}, ...]}
# 输 stdout JSON: {"barcodes": [...], "names": [...], "units": [...], "spec_patterns": [...]}
```

### 5.2 Skill 2: `optimize-parse-strategy`(LLM 调用的自优化 skill)

**目录结构**:
```
skills/optimize-parse-strategy/
├── SKILL.md
├── references/
│   ├── strategy_template.md     # strategy_body 写作模板
│   └── diff_examples.md         # 5 个真实 diff 例子
└── scripts/
    └── compute_diff.py          # 算每行 patch 字段
```

**SKILL.md frontmatter**:
```yaml
---
name: optimize-parse-strategy
description: |
  根据人工修正对比 LLM 自动解析结果,生成新的供应商特定解析策略,写入 supplier_parse_strategy 表。
  Use this skill when the user mentions 优化策略 / 解析错了 / 人工修正后自动学习 /
  调整 supplier=XXX 的解析思路 / OCR 自学习 / 策略升级 / strategy 不准.
license: Internal-Project
metadata:
  version: "1.0.0"
  author: collect-ai
  category: ocr-optimize
compatibility: requires Python 3.x
triggers:
  - 优化解析策略
  - OCR 自学习
  - 策略升级
  - 人工修正自动学习
  - 调整 supplier 解析思路
  - strategy 不准
  - 解析准确率提升
---
```

**SKILL.md 正文结构**:
```markdown
# Optimize Parse Strategy(优化解析策略)

## 适用场景
- 累计 3 次人工修正后自动调用
- 通用解析累计 5 次后自动调用(创建初始 strategy)
- 运营手动触发 `POST /suppliers/:name/strategy/optimize`

## 输入
- supplier_name
- 最近 N 次 session 的 LLM 解析结果(rows JSON)
- 同 N 次 session 的人工修正后结果(rows JSON)
- 旧 strategy_body(可空)

## 步骤
1. 调 `scripts/compute_diff.py` 算每行 patch 字段
2. 归纳易错规律(常见错误模式)
3. 读 `references/strategy_template.md` 拿写作模板
4. 读 `references/diff_examples.md` 学真实例子
5. 生成新 strategy_body + llm_prompt_overlay + sku_hints diff
6. 调 pg 工具(新增)写回 supplier_parse_strategy(版本 +1)

## 输出
- 新 strategy_version
- 本次归纳出的"易错点清单"
```

### 5.3 `ParserOrchestrator` 流程(Go 端,薄)

**新文件**:`internal/parser/orchestrator.go`

```go
package parser

import (
    "context"
    "fmt"
    "log"

    "github.com/tinkler/collect-ai/internal/agent/skill"
    "github.com/tinkler/collect-ai/internal/model"
    "github.com/tinkler/collect-ai/internal/parser/bigmodel"
    "github.com/tinkler/collect-ai/internal/store"
)

// SkuLoader 加载供应商 SKU 库(走业务层)
type SkuLoader interface {
    LoadBySupplier(ctx context.Context, supplier string, limit int) ([]model.SkuRecord, error)
}

// Orchestrator 协调 OCR + Strategy + LLM
type Orchestrator struct {
    ocr     *bigmodel.OcrClient
    llm     *bigmodel.LlmClient
    skus    SkuLoader
    strat   *store.StrategyRepo
    skills  *skill.Store
    matcher *matcher.Matcher // 模糊匹配
}

func NewOrchestrator(
    ocr *bigmodel.OcrClient, llm *bigmodel.LlmClient,
    skus SkuLoader, strat *store.StrategyRepo,
    skills *skill.Store, m *matcher.Matcher,
) *Orchestrator { ... }

func (o *Orchestrator) Parse(ctx context.Context, imgBytes []byte, fileName, supplier string) ([]model.SkuRow, []model.OcrLine, []byte, error) {
    // 1) OCR(不变)
    blocks, err := o.ocr.RecognizeBytes(fileName, imgBytes, "")  // ocrModel 留空=用默认
    if err != nil { return nil, nil, nil, fmt.Errorf("OCR 失败: %w", err) }
    lines := ParseOcrResponse(blocks)
    log.Printf("[orch] OCR → %d 行", len(lines))

    // 2) 查 strategy
    s, _ := o.strat.GetBySupplier(ctx, supplier)

    // 3) 手写 → 纯启发式(不开 LLM)
    if s != nil && s.IsHandwrite {
        log.Printf("[orch] supplier=%s is_handwrite=true, 走纯启发式", supplier)
        return o.heuristicMatch(lines, supplier), lines, imgBytes, nil
    }

    // 4) 准备 LLM 输入:特定 vs 通用
    var strategyBody, promptOverlay string
    var skuHints map[string]any
    if s != nil && s.Enabled && s.Body != "" {
        // 走特定策略
        strategyBody  = s.Body
        promptOverlay = s.LlmPromptOverlay
        skuHints      = s.SkuHints
        // 异步 +1 last_applied_at
        go o.strat.TouchApplied(ctx, supplier)
    } else {
        // 走通用策略:程序生成 L1/L2 hints
        skus, _ := o.skus.LoadBySupplier(ctx, supplier, 5000)
        skuHints = o.buildGenericHints(skus)
        // 异步 +1 generic_apply_count
        go o.strat.IncrGenericCount(ctx, supplier)
    }

    // 5) 读 ocr-purchase skill body 当 prompt 模板
    sk, ok := o.skills.Get("ocr-purchase")
    if !ok {
        return nil, lines, imgBytes, fmt.Errorf("skill ocr-purchase 未加载")
    }
    sysPrompt := renderPrompt(sk.Body, PromptVars{
        Supplier:      supplier,
        SkuHints:      skuHints,
        StrategyBody:  strategyBody,
        PromptOverlay: promptOverlay,
    })

    // 6) 调 LLM 解析
    userPrompt := buildUserPrompt(lines)  // 原 parser.go:138
    content, err := o.llm.ChatCompletion(sysPrompt, userPrompt, "")  // llmModel 留空=用默认
    if err != nil {
        log.Printf("[orch] LLM 失败, fallback 启发式: %v", err)
        return o.heuristicMatch(lines, supplier), lines, imgBytes, nil
    }
    parsed, err := bigmodel.ParseLlmJson(content)
    if err != nil {
        log.Printf("[orch] LLM JSON 解析失败, fallback 启发式: %v", err)
        return o.heuristicMatch(lines, supplier), lines, imgBytes, nil
    }
    log.Printf("[orch] LLM 解析 → %d 条", len(parsed))

    // 7) 匹配(走业务层,supplier 已确定)
    skus, _ := o.skus.LoadBySupplier(ctx, supplier, 5000)
    m := matcher.New(toSkuRecords(skus), 0)  // fuzzy 走 strategy(暂用 0)
    rows := make([]model.SkuRow, 0, len(parsed))
    for i, ocr := range parsed {
        rows = append(rows, m.Match(ocr, i+1))
    }
    return rows, lines, imgBytes, nil
}

// heuristicMatch 纯启发式(手写供应商用)
func (o *Orchestrator) heuristicMatch(lines []model.OcrLine, supplier string) []model.SkuRow {
    parsed := heuristicParse(lines)  // 原 parser.go:151
    skus, _ := o.skus.LoadBySupplier(context.Background(), supplier, 5000)
    m := matcher.New(toSkuRecords(skus), 0)
    rows := make([]model.SkuRow, 0, len(parsed))
    for i, ocr := range parsed {
        rows = append(rows, m.Match(ocr, i+1))
    }
    return rows
}

// renderTemplate 4 变量替换(Go 端, 简单 string replace, 不引入模板引擎)
func renderPrompt(body string, v PromptVars) string { ... }
```

**关键合规点(对照 AGENTS.md §1 / §4)**:
- **没有**"if/else + 业务判断"在 Go 里——选 generic vs specific 走查表
- **没有**"公式 / 启发式"——`buildGenericHints` 是数据准备,逻辑在 `references/purchase_layouts.md` + `scripts/build_hints.py`
- **没有**"LLM 调用模板"在 Go 里——prompt 模板在 `skills/ocr-purchase/SKILL.md` 正文
- 白名单 `ModeInventory / ModePurchase` 是 enum(数据),不算法

### 5.4 `supplier_parse_strategy` 表

**DDL**(`internal/store/pg.go` 加):
```sql
CREATE TABLE supplier_parse_strategy (
    supplier_name        TEXT PRIMARY KEY,        -- 一户一条
    is_handwrite         BOOLEAN NOT NULL DEFAULT FALSE,
    enabled              BOOLEAN NOT NULL DEFAULT TRUE,
    
    -- 策略主体(LLM 友好的自由文本)
    body                 TEXT NOT NULL DEFAULT '',
    
    -- 字段级 hints(机器友好,程序拼 prompt 时用)
    sku_hints            JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- 结构: {
    --   "barcodes": [...],
    --   "names": [...],
    --   "units": [...],
    --   "spec_patterns": [...],
    --   "ocr_errors": ["8→12", "排→15"]
    -- }
    
    -- 追加到默认 prompt 后(每个供应商的"额外提醒")
    llm_prompt_overlay   TEXT NOT NULL DEFAULT '',
    
    -- 元数据
    strategy_version     INT  NOT NULL DEFAULT 0,
    generic_apply_count  INT  NOT NULL DEFAULT 0,
    edit_count           INT  NOT NULL DEFAULT 0,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_edited_at       TIMESTAMPTZ,
    last_auto_optimized_at TIMESTAMPTZ,
    last_applied_at      TIMESTAMPTZ,
    note                 TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_sps_handwrite ON supplier_parse_strategy(is_handwrite) WHERE is_handwrite = TRUE;
CREATE INDEX idx_sps_generic_count ON supplier_parse_strategy(generic_apply_count) WHERE enabled = FALSE OR body = '';
```

**新 Go 文件**:`internal/store/strategy.go`(StrategyRepo)
- `GetBySupplier(ctx, name) (*Strategy, error)` — 热路径
- `Upsert(ctx, s) error`
- `IncrGenericCount(ctx, name) error` — 异步用
- `IncrEditCount(ctx, name) error` — 异步用
- `TouchApplied(ctx, name) error` — 异步用
- `ListNeedsAutoBuild(ctx, threshold) ([]string, error)` — cron 查"哪些 supplier 累计 5 次通用解析了,该建 strategy"
- `ListNeedsOptimize(ctx, threshold) ([]string, error)` — cron 查"哪些 supplier 累计 3 次人工修正了,该跑优化"

### 5.5 自优化触发链

**触发点 A:PATCH /sessions/:id/rows(人工改行 + 保存)**

```
UpdateRow (handler.go:843) 写库
  → 异步 fire-and-forget:
      o.strat.IncrEditCount(supplier_name)
      if edit_count >= 3:  // 阈值
          go o.runOptimizeSkill(ctx, sessionID, supplier)
              → 拉本次 session 的人工 rows + 上次 LLM 解析的 raw_ocr_json / raw_llm_json
              → 构造 diff
              → runner.Run(message="优化 supplier=XXX 的 strategy,这是 diff: ...")
              → LLM 调 invoke_skill("optimize-parse-strategy")
              → LLM 读 references/strategy_template.md
              → LLM 调 pg 工具(新增 pg_upsert_strategy)写新 strategy
              → strategy_version++, edit_count 重置为 0
```

**触发点 B:通用解析累计 5 次**

```
generic 路径每次 +1 generic_apply_count
  → 后台 cron(每 1 小时)查 "enabled=false 或 body='' 且 generic_apply_count >= 5"
  → 对每家调 LLM 总结最近 5 次 sessions,创建初始 strategy
  → 写回 supplier_parse_strategy(body + sku_hints 填充,version=1)
```

**触发点 C:运营手动触发(可选)**

```
POST /api/v1/suppliers/:name/strategy/optimize
  → 立即跑一次 runOptimizeSkill(同步)
  → 返 200 + 新 strategy_version
```

---

## 六、端点改动清单

| 端点 | 改动 | 备注 |
|---|---|---|
| `POST /api/v1/sessions` | 内部走 Orchestrator | 对外 API 不变;删 `template_id` / `template_name` query 参数 |
| `PATCH /api/v1/sessions/:id/rows` | 保存后异步触发优化 hook | 已有,加 hook |
| `GET /api/v1/sessions/:id` | 返 `strategy_version` 字段(从 parse_session 取) | 加列,改 Get |
| `GET /api/v1/suppliers/:name/strategy` | **新增** | 看 strategy 详情 |
| `PUT /api/v1/suppliers/:name/strategy` | **新增** | 手动覆盖 strategy_body(纠错用) |
| `POST /api/v1/suppliers/:name/strategy/optimize` | **新增** | 手动触发优化 |
| `POST /api/v1/templates/sync` | **删** | template 废了 |
| `GET /api/v1/templates` | **删** | 同上 |
| `GET /api/v1/templates/...` | **删** | 同上 |
| `DELETE /api/v1/templates/:id` | **删** | 同上 |

---

## 七、DB 迁移清单

**新增**:
```sql
CREATE TABLE supplier_parse_strategy (
    -- 见 5.4
);

-- 兼容老 parse_session 数据(如果有 template_id 引用)
ALTER TABLE parse_session ADD COLUMN IF NOT EXISTS strategy_version INT NOT NULL DEFAULT 0;
```

**删除**(Phase C 一次性):
```sql
-- 备份(可选):CREATE TABLE template_bak_20260902 AS SELECT * FROM template;
DROP TABLE IF EXISTS template CASCADE;
ALTER TABLE parse_session DROP COLUMN IF EXISTS template_id;
ALTER TABLE parse_session DROP COLUMN IF EXISTS template_name;
```

**Go 端清理**:
- 删 `internal/store/template.go`(TemplateRepo)
- 删 `internal/model/types.go` 里 `Template` / `TemplateMode`(enum 保留,值删 `ModeInventory` 可选)
- 删 `internal/api/handler/handler.go` 里 4 个 template 端点(`ListTemplates` / `UpsertTemplate` / `SyncTemplates` / `DeleteTemplate` / `resolveTemplateConfig`)
- 删 `cmd/server/main.go` 里 `templateRepo := store.NewTemplateRepo(pool)`

---

## 八、Phase 实施分阶段

### Phase A(核心流程,1-2 天)— 立即可启动

1. 删盘点单相关路径:
   - `internal/parser/bigmodel/llm.go` 删 `DefaultInventoryPrompt`(保留 enum 防存量数据)
   - `handler.go` 删 mode=inventory 分支
2. 建表 `supplier_parse_strategy` + 写 `StrategyRepo`
3. 写 `skills/ocr-purchase/SKILL.md`(从 `llm.go:DefaultPurchasePrompt` 迁)
4. 写 `skills/ocr-purchase/references/purchase_layouts.md`
5. 写 `skills/ocr-purchase/references/common_ocr_errors.md`
6. 写 `internal/parser/orchestrator.go`(薄壳)
7. 改 `handler.CreateSession` 用 Orchestrator,删 `template_id` / `template_name` 参数
8. e2e:同张图跑 3 路径对比(generic / specific / 手写)

### Phase B(自优化,1 天)

1. 写 `skills/optimize-parse-strategy/SKILL.md`
2. 写 `references/strategy_template.md` + `references/diff_examples.md`
3. 加 PATCH /sessions/:id/rows 后的异步 hook(`IncrEditCount` + 阈值触发)
4. 加后台 cron `loopHourly(查 NeedsAutoBuild, 通用 5 次建 strategy)`
5. 加 3 个新端点(strategy 查看/覆盖/手动优化)
6. e2e:人工改 3 次 → 自动优化 → 验证 strategy 升级

### Phase C(收尾,半天)

1. 删 `template` 表 + 4 个端点 + TemplateRepo + `parse_session.template_id/template_name` 列
2. 前端适配:去掉 template 选择器,展示 strategy 状态(版本/编辑次数/最后应用)
3. README + `docs/skill-system.md` 同步
4. e2e 全量回归

---

## 九、风险与回退

| 风险 | 缓解 |
|---|---|
| C# 端/飞书端还在用 `/api/v1/templates` 端点 | Phase C 一次性删前,先 grep 整个仓库确认;若有,先改消费方再删 |
| `template` 表里有历史数据(老板之前手配的提示词) | Phase C 前先备份 `CREATE TABLE template_bak_20260902 AS SELECT * FROM template;` |
| 通用 → 特定策略自动建时,LLM 总结出错的 strategy | 第一次建 strategy 后 `enabled=false`(灰度),运营 review 后手动 `PUT` 改 enabled=true |
| 自优化频次太高(token 浪费) | `edit_count >= 3` 阈值 + 优化成功后 `edit_count` 重置为 0 + 手动 PUT 覆盖可临时关掉自优化 |
| OCR skill 加载失败(缺 SKILL.md) | Orchestrator 启动时校验,缺则 fail-fast(不启动),而不是降级到硬编码(避免回退到老路) |
| 多个 LLM 优化请求并发修改同一 supplier | `supplier_parse_strategy` UPSERT 用 `version + 1 WHERE version = ?`(乐观锁),并发失败方重试 1 次 |

---

## 十、待用户决策(实施前最后确认)

无,4 个关键决策已确认(见 §三)。

可启动 Phase A 实施。
