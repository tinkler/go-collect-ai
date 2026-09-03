# Skill 系统架构(Anthropic Agent Skills spec)

> **目的**:把"季节判定 / 供应商分类 / 风险评估"这类**高频推理逻辑**从硬编码(Go 代码 / 业务表白名单)迁到 **Skill**——遵循 Anthropic 2025-12 开源的 Agent Skills 规范——**支持热更新、零停机迭代**。
>
> **背景**:v1 (trpc-agent-go 接入) 阶段所有"应季 / 供应商策略 / 促销费分摊"逻辑都硬编码在 `internal/agent/tools/{calendar,payment,policy,fee}.go` 里。改一次要重新 build + 部署 + 重启 collect-ai 进程,周转时间以小时计。
>
> **本文档适用版本**:v2 (2026-09-02 起,feat/skill-system 分支)

---

## 1. 设计目标

| 目标 | 做法 |
|---|---|
| **标准化** | 严格遵循 Anthropic Agent Skills 规范(`SKILL.md` + frontmatter + `scripts/` / `references/` / `assets/`) |
| **生态兼容** | 兼容 `npx skills` (Vercel Labs) 装的所有 skill 落点(`~/.agents/skills/`)和 Anthropic 官方 (`~/.claude/skills/`) |
| **热更新** | fsnotify 监听 skill 目录,改完即生效(无需重启 Runner / collect-ai) |
| **推理外置** | LLM 自主决定调哪个 skill,描述(description)是唯一触发信号;不再把判定算法写进 Go |
| **白名单准入** | 校验 name / description 符合 spec,防止 description 太短导致 LLM 触发不到 |
| **可降级** | skill 加载失败不阻塞 Runner;关闭 `COLLECTAI_AGENT_SKILLS_ENABLED=false` 走纯 6 个 tool 模式 |

---

## 2. 目录结构

```
collect-ai/
├── skills/                                # 项目内 skill root(L1 优先级最高)
│   ├── seasonal-buying/                   # 示范 skill:应季采购(从 calendar.go 迁出)
│   │   ├── SKILL.md                       # 必填:frontmatter + Markdown body
│   │   ├── scripts/
│   │   │   └── compute_window.py          # L3: 纯计算脚本
│   │   └── references/
│   │       ├── chinese_holidays_2026.md   # L3: 事实表
│   │       └── season_taxonomy.md         # L3: 季节分类词典
│   ├── settlement-suggestion/             # (待迁移) 从 payment.go 迁出
│   └── supplier-policy/                   # (待迁移) 从 policy.go 迁出
│
├── internal/agent/skill/                  # Go 端实现
│   ├── types.go                           # Manifest / Skill 类型
│   ├── validate.go                        # Anthropic spec 字段校验
│   ├── loader.go                          # 扫描 roots + 解析 SKILL.md
│   ├── factory.go                         # DefaultLoader / RootsFromEnvOrDefault
│   ├── store.go                           # 线程安全 registry + L1Prompt
│   ├── watcher.go                         # fsnotify 热更新
│   ├── runtime.go                         # invoke_skill tool (trpc-agent-go)
│   ├── os_helpers.go                      # statDir / SourceFromRoot
│   ├── *_test.go                          # 单元测试
│
├── docs/skill-system.md                   # 本文件
└── AGENTS.md                              # rules: vibe coding 必须走 skill
```

**外部 skill 落点**(自动扫描,无需配置):

| 路径 | 来源 | 优先级 |
|---|---|---|
| `<cwd>/skills/` | 项目内,git 同步 | 最高 |
| `~/.claude/skills/` | Anthropic 官方 Claude Code | 次高 |
| `~/.agents/skills/` | Vercel `npx skills` 装 | 次高 |

---

## 3. Skill 规范(对齐 Anthropic 官方)

### 3.1 目录结构

```text
<skill-name>/
  SKILL.md                # 必填
  scripts/                # 可选:LLM 调 invoke_skill run_script 跑
  references/             # 可选:LLM 调 invoke_skill read_file 读
  assets/                 # 可选:模板/数据/图片
```

### 3.2 SKILL.md 格式

```markdown
---
name: <skill-name>          # 必填,1-64 字符,小写字母数字+连字符
description: <text>         # 必填,1-1024 字符,含触发关键词
license: <text>             # 可选
compatibility: <text>       # 可选,运行时要求
metadata:                   # 可选,自定义 K-V
  version: "1.0.0"
  author: ...
allowed-tools: <list>       # 可选,实验性
triggers:                   # Vercel 扩展,可选
  - <natural language phrase>
---

# Skill 名称

## When to use this skill
...

## How to use this skill
...
```

### 3.3 字段校验(Go 端)

| 字段 | 规则 | 失败行为 |
|---|---|---|
| `name` | 1-64 字符,小写字母数字+单个连字符,不能以连字符开头/结尾,无连续连字符,**必须等于父目录名** | 整个 skill 加载失败,记录到 `LoadResult.Errors` |
| `description` | 1-1024 字符,**至少 10 字符** | 同上 |
| `compatibility` | ≤ 500 字符 | 同上 |
| `triggers` | 每个非空 | 同上 |

错误**不阻塞**其它 skill 加载 — 一个写坏的 skill 不应让整个 Runner 失能。

### 3.4 description 的写法(决定 LLM 触不触发)

> **description 是 model-driven activation 的唯一信号** — LLM 看到任务时,自己决定调不调这个 skill。

**好的 example**:

```yaml
description: 判定"应季采购窗口"——根据当前日期、节假日日历、季节切换、促销档期,自动给老板建议"从哪一天开始备货,备多少倍"。Use this skill when the user asks about 应季/换季/节假日备货/节前预警/中秋/春节/618/双11/夏季饮料/冬季火锅底料/雪糕季/开学季.
```

要点:
- 写**做什么** + **何时用** + **触发关键词**(关键词 = 用户可能说的词)
- 用祈使语气("Use this skill when...")
- 至少 10 字符,推荐 100-300 字符
- 不要总结工作流(LLM 会偷懒跳过正文)

**差的 example**:
- `description: 季节判定` (过短,无触发词)
- `description: 这个 skill 干很多事情` (无关键词)
- `description: Use this skill when... 然后它会做 A 然后 B 然后 C` (总结工作流,LLM 会跳过正文)

---

## 4. 三层渐进披露(Anthropic 设计)

| 层 | 内容 | 加载时机 | Token 成本 |
|---|---|---|---|
| **L1 目录层** | 所有 skill 的 `name` + `description` + `triggers` | 启动时,拼到 system prompt | ~50-150 tokens/skill |
| **L2 指令层** | 完整 `SKILL.md` body | LLM 调 `invoke_skill(action=load)` | < 5000 tokens/skill |
| **L3 资源层** | `scripts/` / `references/` / `assets/` 内容 | LLM 调 `invoke_skill(action=run_script \| read_file)` | 视文件大小 |

**收益**:即使装了 50 个 skill,L1 也只占 5-7K tokens(占满 context 不到 5%)。

---

## 5. invoke_skill tool(暴露给 LLM)

```go
// internal/agent/skill/runtime.go
type InvokeSkillReq struct {
    SkillName string          `json:"skill_name"`           // 必填
    Action    string          `json:"action"`               // "load" | "run_script" | "read_file"
    Input     string          `json:"input,omitempty"`      // load 时拼到正文后
    Path      string          `json:"path,omitempty"`       // run_script / read_file 的相对路径
    Args      json.RawMessage `json:"args,omitempty"`       // run_script 的 stdin JSON
    ScriptTimeoutSec int      `json:"script_timeout_sec"`   // 默认 30,最大 120
}
```

**安全护栏**:
- 路径穿越阻断(`..` / 绝对路径)
- 脚本超时 30s(可调到 120s,硬上限)
- stdout 截断 20K 字符
- body 截断 50K 字符(避免 context 爆炸)

**降级**:LLM 调 `invoke_skill` 失败时,直接看到 error 字符串,可以自我修正(比如换一个 skill_name 重试)。

---

## 6. 热更新机制

```
┌────────────────────────────────────────────────────────────┐
│ Watcher (goroutine)                                        │
│   ↓ fsnotify 监听                                          │
│     - CREATE/REMOVE 子目录   → 新增/移除 skill              │
│     - WRITE/RENAME SKILL.md  → reload 这个 skill           │
│     - 防抖 200ms                                            │
│   ↓ 触发 reload                                            │
│     Loader(roots) → LoadResult → Store.Replace(skills)     │
└────────────────────────────────────────────────────────────┘
```

- **新增 skill**:放一个新目录,例如 `mkdir skills/settlement-suggestion/`,放 `SKILL.md`,200ms 后 LLM 就能看到
- **修改 skill description**:改完 SKILL.md,200ms 后下次 Run 立刻生效
- **删除 skill**:`rm -rf skills/old-skill/`,200ms 后从 registry 消失
- **修改 Go 端代码**:**仍然需要重启** — fsnotify 只监听 skill 目录,不动 Go 代码

**注意**:LLM Agent 的 `llmagent.WithInstruction(...)` 是构造期固定的。L1 拼到 instruction 是在 `NewRunner` 时一次完成。热更新后,LLM 看到的"旧 L1"会持续到下次 Runner 重建。

**Workaround**(已实现):新一次 `Runner.Run()` 调用时,从 Store 重新拼 L1。如果要更严格,可以在 `Runner.Run()` 里做 message processor 注入(后续 W2+ 优化)。

---

## 7. 跟现有 6 个 tool 怎么协同

| Tool (Go 端,硬编码) | Skill (LLM 端,推理) | 关系 |
|---|---|---|
| `record_special_date` | `seasonal-buying` | tool 存数据,skill 决定"该不该存 + 存什么" |
| `query_upcoming_dates` | `seasonal-buying` | tool 查数据,skill 解读数据 + 给老板建议 |
| `record_promotion_fee` | (待迁)`settlement-suggestion` | tool 记账,skill 算"分摊到哪些商品" |
| `remember_supplier_policy` | (待迁)`supplier-policy` | tool 存白名单 K-V,skill 教 LLM "哪些字段是策略类的" |
| `query_supplier_policy` | (待迁)`supplier-policy` | tool 查 K-V,skill 解读"汇一自采=老板自己定价" |
| `list_promotion_fee` | (待迁)`settlement-suggestion` | tool 列表,skill 算"过期预警 + 续约建议" |
| **(新增)** `invoke_skill` | — | skill 的"入口"tool,LLM 调它来 load / run / read |

**6 个 tool 不会被替换**,它们是"事实记录层";skill 是"判断层"。

---

## 8. 怎么新建一个 Skill

### 8.1 最快路径(用社区模板)

```bash
# Claude 官方模板
npx create-skill@latest my-skill

# 或者从 Vercel 社区找现成的
npx skills add <owner>/<repo> --skill <name>

# Vercel skill 自动落到 ~/.agents/skills/<name>/
# 项目内 skill 落 <repo>/skills/<name>/
```

### 8.2 项目内手写

```bash
mkdir -p skills/<skill-name>/scripts skills/<skill-name>/references

# 写 SKILL.md
$EDITOR skills/<skill-name>/SKILL.md

# 写脚本(可选)
$EDITOR skills/<skill-name>/scripts/<name>.py

# 写事实表(可选)
$EDITOR skills/<skill-name>/references/<name>.md
```

> ⚠️ **name 必须等于目录名**(`seasonal-buying` ↔ `skills/seasonal-buying/`),否则加载失败。

### 8.3 description 模板(复制后改)

```yaml
---
name: <kebab-name>
description: <一句话做啥>.<二句话何时用 + 触发词列表>. Use this skill when the user mentions <关键词 1/2/3/4>.
license: Internal-Project
metadata:
  version: "0.1.0"
  author: <你>
  category: <分类>
  migrated_from: <如果是从 Go 工具迁来的,注明原文件>
compatibility: requires Python 3.x
triggers:
  - <用户可能说的中文短语>
  - <英文短语>
---

# <Skill 名>

## When to use this skill
- 场景 1
- 场景 2

## How to use this skill
### 步骤 1:...
### 步骤 2:...
### 步骤 3:落库(可选)

## Scripts
- `scripts/<name>.py` — 简述

## References
- `references/<name>.md` — 简述

## Common Patterns
### 模式 A:...
### 模式 B:...

## Guidelines
- description 是唯一触发信号,不要在这里总结工作流
- 老板的话优先于事实表
- dry_run 二次确认

## Keywords
<关键词 1>, <关键词 2>, ...
```

---

## 9. 怎么迁移现有硬编码 → Skill

**适用场景**:
- 算法是"老板的话 → 结构化决策"(季节判定、风险分类、价格建议)
- 算法依赖**事实表 / 政策**且会变(节假日、调价、品类边界)
- 算法 LLM 调一下会更准(比硬编码白名单灵活)

**不适用**(继续用 Go tool):
- 纯数据读写 CRUD(insert / update / select)
- 强一致性要求(账目、库存、付款)
- 高频低延迟路径(单条记录 < 10ms)

**迁移 checklist**:

1. [ ] 找到 `internal/agent/tools/<name>.go` 里"判断 / 计算"的部分(白名单 / 公式)
2. [ ] 抽到 `skills/<skill-name>/SKILL.md` 描述里("何时用" + 触发词)
3. [ ] 事实表 / 政策抽到 `references/`
4. [ ] 纯计算抽到 `scripts/<name>.py`(由 invoke_skill 调)
5. [ ] Go 工具**保留**作为"事实记录层",只去掉"判断"那部分
6. [ ] 跑 `go test ./internal/agent/skill/...` 验证
7. [ ] 更新 `AGENTS.md` rules(标注哪个 skill 替代了哪个硬编码)

---

## 10. 端到端调用链路

```
用户问: "下个月要过节了,要不要备货?"
   ↓ wecom OnMessage
Runner.Run(ctx, userID, sessionID, message)
   ↓
trpc-agent-go LLM Agent
   ↓ 1) 看到 system prompt 里的 L1 列表,有 seasonal-buying,description 匹配
LLM 调 invoke_skill(skill_name="seasonal-buying", action="load", input="下个月...")
   ↓
Go runtime:读 SKILL.md 全文 + 列出 scripts/references,返 JSON
   ↓
LLM 拿到正文,看到步骤 1: 调 scripts/compute_window.py
LLM 调 invoke_skill(skill_name="seasonal-buying", action="run_script", path="scripts/compute_window.py", args={"today": "2026-09-02"})
   ↓
Go runtime:python 进程跑脚本,拿 stdout JSON
   ↓
LLM 拿到 next_event,看到步骤 3: 调 query_upcoming_dates tool(确认老板没记过类似)
LLM 调 query_upcoming_dates(type="holiday", days_ahead=30)
   ↓
Go tool:查 PG 表
   ↓
LLM 综合判断,给老板一段 200 字内的回复 + 建议 dry_run 落库
   ↓
Runner.Run() 返回事件流
   ↓
wecom 推回老板群
```

---

## 11. 监控 / 调试

### 启动日志

```
[agent] skills 加载: 3 个 skill 从 3 个 root
[agent]   - seasonal-buying [project] (1 scripts, 2 refs)
[agent]   - react-best-practices [user-agents] (0 scripts, 0 refs)
[agent]   - web-design-guidelines [user-agents] (0 scripts, 0 refs)
[agent] Runner ready: model=deepseek-chat base=https://api.deepseek.com tools=7 skills=3
[skill-watcher] 启动,监听了 3 个 root
```

### 热更新日志

```
[skill-watcher] reload: 4 skill(s) now active
[skill-watcher] reload warnings:
  - /path/to/bad/SKILL.md: name "BadName" 不合法: 仅允许小写字母数字+单个连字符
```

### HTTP 端点(W2+ 待加)

```
GET  /v1/agent/skills          列出当前所有 skill(name + description + 路径)
POST /v1/agent/skills/reload   强制 reload(不等 fsnotify)
GET  /v1/agent/skills/{name}   读某个 skill 的 SKILL.md
```

---

## 12. 已知限制

| 限制 | 原因 | 计划 |
|---|---|---|
| L1 在 `NewRunner` 时一次拼到 instruction | trpc-agent-go v1.11 的 `llmagent.WithInstruction` 构造期固定 | W2 加 message processor 拦截 Run() 调用,重新注入最新 L1 |
| 脚本只能用 Python / Node / Bash | 沙箱复杂度 | W3+ 考虑 WASM (wazero) |
| skills 目录里不能放 Go 代码 | 防止 LLM 调 Go 函数绕过 RBAC | 维持现状 |
| 跨平台:Windows + Linux 都跑过 | fsnotify 在两个平台都 OK | macOS 没测,理论上 OK |
| skill 名只能用英文 + 连字符 | Anthropic spec 强约束 | 维持现状 |

---

## 13. 后续(W2+)

- [ ] W2:wecom 接入后,看 LLM 调 invoke_skill 的实际命中率,优化 description
- [ ] W2:加 `/v1/agent/skills` HTTP 端点,给运营平台用
- [ ] W3:从 `payment.go` 迁 `settlement-suggestion` skill(分摊/账期/续约预警)
- [ ] W3:从 `policy.go` 迁 `supplier-policy` skill(策略分类/反回扣/风险评估)
- [ ] W4:加 skill **评测体系**(参照 Anthropic skill-creator:Grader + Comparator + Analyzer)
- [ ] W5:加 skill **自我进化** — LLM 把自己常用的"工作流"沉淀成新 skill
