# AGENTS.md — collect-ai 项目规则

> 面向所有 AI 编程 agent(Mavis / Claude Code / Cursor / Codex 等)的工作约定。
> 违反这些规则 = 引入技术债,review 时会被打回。

---

## 1. 核心规则:LLM 推理逻辑必须外置成 Skill

**当任务涉及以下任何一种"推理 / 判定 / 分类"时,严禁把算法写进 Go 代码。** 必须先建一个 Skill,再让 LLM 调它。

| 触发场景 | 严禁 | 必须做 |
|---|---|---|
| 季节 / 节假日 / 档期 判定 | 写 Go 函数 `IsHoliday(d time.Time)` | 建 `skills/seasonal-buying/` |
| 供应商分类 / 风险等级 | 写 Go 函数 `ClassifySupplier(s)` | 建 `skills/supplier-policy/` |
| 价格建议 / 促销分摊 | 写 Go 函数 `SuggestPrice(item)` | 建 `skills/settlement-suggestion/` |
| 缺货 / 备货倍数 判定 | 写 Go 函数 `ShouldRestock(item, stock)` | 建 `skills/restock-strategy/` |
| 客户分群 / 复购周期 | 写 Go 函数 `CustomerSegment(c)` | 建 `skills/customer-segment/` |
| 文案 / 标题 / 描述 生成 | 写 Go 函数 `GenTitle(ctx)` | 建 `skills/copywriting/` |
| 任何"老板话 → 决策" 的映射 | 写 Go 白名单 / 规则 | 建 `skills/<name>/`,把白名单搬到 `references/`,把规则搬到 SKILL.md 正文 |

**判别问题**:把"我希望系统做什么"翻译成一句 — 如果答案里出现"判断"、"分类"、"建议"、"根据…决定"、"取决于",**99% 是要建 skill,不是写 Go**。

### 1.1 例外(继续用 Go tool,不建 skill)

- 纯数据 CRUD(insert / update / select / delete)
- 强一致性要求(账目、库存扣减、付款)
- 高频低延迟路径(单条 < 10ms,不能接受 LLM 调用)
- 跟 LLM 无关的纯算法(图片压缩、CSV 解析、时间格式化)

### 1.2 已有 skill(不要重复建)

- `skills/seasonal-buying/` — 应季采购(2026-09-02 从 `internal/agent/tools/calendar.go` 迁出)
- 未来 W3+ 会迁:`settlement-suggestion` / `supplier-policy` / `restock-strategy`

**建新 skill 前,先 `ls skills/` 看下有没有能复用的**。

---

## 2. Skill 命名 + 目录规范

- 目录名 = `name` 字段 = kebab-case,小写字母数字 + 单个连字符
- 必填文件:`SKILL.md`(YAML frontmatter + Markdown body)
- 可选目录:`scripts/` / `references/` / `assets/`
- 一句话定位:**Skill = 知识 + 工作流,不是可执行函数**

**反面例子**(会被 review 打回):
- `skills/CalculateDiscount/` (大写)
- `skills/seasonal buying/` (空格)
- `skills/seasonal--buying/` (连续连字符)
- 名字等于目录名不一致

---

## 3. description 写作(决定 LLM 触不触发)

`description` 是 model-driven activation 的**唯一信号**。LLM 看到任务时,自己决定调不调这个 skill。

**强制要求**:
- 1-1024 字符(校验器强制)
- **至少 10 字符**(校验器强制)
- 包含**触发关键词**(用户可能说的词,中英都要)
- 用祈使语气:"Use this skill when..."
- **不要**总结工作流(LLM 会偷懒跳过正文)

**模板**:
```yaml
description: <一句做啥>.<二句何时用 + 关键词>. Use this skill when the user mentions <关键词 1/2/3/4>.
```

**反面例子**(会被 loader 拒收):
- `description: 季节判定` (过短)
- `description: 帮助商家` (无关键词,LLM 触发不到)
- `description: 这个 skill 会先做 A,然后做 B,最后 C` (总结工作流)

---

## 4. Go 代码里的硬编码红线

`internal/agent/tools/` 下,凡是"白名单 / 公式 / 决策表"必须是**数据定义**(常量、enum),不能是**算法实现**。

| 允许 | 严禁 |
|---|---|
| `var SpecialDateType = struct{...}{...}` (枚举字面值) | `func ClassifyDate(d time.Time) string {...}` (判定) |
| `var allowedTypes = map[string]bool{...}` (白名单 KV) | `func ShouldRestock(s, d Daily) bool {...}` (决策) |
| `const MaxLeadDays = 30` (业务上限常量) | `func ComputeMultiplier(holiday string) float64 {...}` (计算) |
| 结构体定义 + JSON schema | 业务规则 / 启发式 |

**任何"if/else for/while + 业务判断"在 Go 工具里出现 = 提交前要先建 skill 把它迁出去**。

---

## 5. 热更新与重启

- **改 skill**(SKILL.md / scripts / references)→ 不需要重启 collect-ai,fsnotify 200ms 内自动 reload
- **改 Go 代码**→ 必须重启(go build + 重启进程)
- **改 invoke_skill tool 本身**(`internal/agent/skill/runtime.go`)→ 必须重启

调试 skill 时,**优先改文件而不是改 Go**,这样不打断线上服务。

---

## 6. 跟 LLM 的接口

- LLM 通过 `invoke_skill` tool 调 skill
- tool 接受 3 种 action:`load` / `run_script` / `read_file`
- skill 内部有 python/node/bash 脚本时,Go 端 spawn 子进程跑,timeout 30s
- **不要**让 LLM 直接读 SKILL.md(它要调 tool,不是用 Read 工具)
- **不要**在 skill 里写"调 read_file 后再做 X"这种 step-by-step — 这是 LLM 自己的事,SKILL.md 只描述目标和约束

---

## 7. 测试

每次新建/修改 skill,必须跑:

```bash
go test ./internal/agent/skill/...
go vet ./internal/agent/...
go test ./internal/agent/...
```

并在 SKILL.md 的 metadata 里更新 version。

---

## 8. 跨项目规则(影响 collect-ai 的全局原则)

- **凡是要上线的代码必须有单测**(沿用项目原有约定)
- **不进数据库的纯函数优先用 skill,放不进 skill 的再考虑 Go**
- **不要在 Go 里 import 任何 skill 用的 Python 库** — skill 跟 Go 是边界清晰的:Go 调 Python 进程,Python 不知道 Go 存在
- **不要把 LLM API key / 业务配置写进 skill** — 用 env 变量,SKILL.md 只引用 env 名

---

## 9. 违反本规则的后果

提交 review 时会按以下优先级打回:

1. **P0 阻断**:在 Go 里写"业务判断 / 分类 / 决策"函数 → 必须先建 skill 再合
2. **P1 必改**:新 skill 缺 description / description < 10 字符 → loader 直接拒,无法注册
3. **P2 建议**:新 skill 没 metadata.version / 没写触发词 → 提醒补,不一定阻断

---

## 10. 何时该用 `npx skills` 装社区 skill

| 场景 | 建议 |
|---|---|
| React / Next.js / Node 最佳实践 | `npx skills add vercel-labs/agent-skills` |
| Python 异步 / 类型注解 | `npx skills add <某个 Python skill repo>` |
| 数据库 schema 评审 | `npx skills add <sql-review repo>` |
| 商超领域逻辑(季节、定价、品类) | **自己建 skill**,不放进社区(领域私有) |

社区 skill 装到 `~/.agents/skills/`,本项目**默认自动扫描**,无需配置。

---

## 11. 相关文档

- `docs/skill-system.md` — 架构、端到端流程、迁移 checklist
- `skills/<name>/SKILL.md` — 每个 skill 的 usage
- `internal/agent/skill/types.go` — Manifest 字段定义
- `internal/agent/skill/validate.go` — 字段校验规则

---

## 12. Cube 数据源统一规则(2026-09-02 沉淀)

**目标**: 任何从 cube 获取的数据,无论给前端(外部)还是给 LLM tool / cron / 内部 service(内部),**必须经过 `business.Gateway` / `business.Executor` 统一出入口**,业务字段名(对外)跟物理 cube 字段名(对内)严格分离。

### 12.1 入口边界 — 谁允许 import `parser/agent`

| 包 | 允许? | 理由 |
|---|---|---|
| `cmd/server/main.go` | ✅ 唯一持 `*agent.Client` 实例 | 注入 Gateway / SupplierPayment / AgentRunner |
| `internal/business/gateway.go` | ✅ 定义 `CubeClient` interface | 隔离业务层跟具体 client |
| `internal/parser/agent/client.go` | ✅ 自己实现 `business.CubeClient` | 编译期 `var _ business.CubeClient = (*Client)(nil)` |
| `internal/api/handler/handler.go` | ⚠️ 渐进式: 仅 `Ping()` / `GetDataSource()` | 进一步可改 `Gateway.Ping()` / `Gateway.DS()` |
| `internal/business/executor.go` | ❌ 持 `CubeClient` interface | 已脱钩 |
| `internal/restock/**` | ❌ 持 `*business.Gateway` | 已脱钩(2026-09-02) |
| `internal/supplierpayment/cube.go` | ❌ 持 `business.CubeClient` | 已脱钩(2026-09-02) |
| `internal/agent/runner.go` | ❌ 持 `*business.Gateway` (W2+ cube tool 用) | 已脱钩 |
| **其他业务代码** | ❌ **严禁直接 import `parser/agent`** | 必须经 Gateway / Executor |

**Vibe coding 校验**:
```bash
# 任何改动后跑这行,只应输出 main.go / handler.go(渐进)/ 注释
rg "parser/agent" --type go -l | grep -vE "(cmd/server/main\.go|internal/api/handler/handler\.go|internal/parser/agent/client\.go)"
```

### 12.2 字段名边界 — 业务名 vs 物理名

| 场景 | 用 | 严禁 |
|---|---|---|
| HTTP handler 返回给前端 | 业务字段名 `barcode` / `product_name` / `stock_qty` | 物理 `t_bd_item_info.item_no` |
| LLM tool 入参 / 出参(对 LLM) | 业务字段名 + 业务语义 | 物理 cube 字段名(LLM 不知道 cube) |
| cron 任务 / 内部 service 之间 | 业务字段名(走 Executor / Gateway) | 直接 import parser/agent 调 |
| Gateway 内部(翻译层) | 物理字段名 (transient) | 暴露给上层 |
| 业务专用 cube(restock / supplierpayment) 内部 | 物理字段名封装在 RawQuery 内 | 透传到上层 |

**Vibe coding 校验**:
- handler.go 里的 `gin.H{...}` 字典 key 必须是 `barcode` / `product_name` 等业务字段名,不能是 `t_bd_item_info.item_no`
- 任何新 struct 字段 json tag,产品/供应商/客户主数据相关 → 业务字段名

### 12.3 Mapping 单一来源 — 严禁在 Go 里硬编码物理字段

| 改动类型 | 改哪里 | 严禁 |
|---|---|---|
| 加新 datasource(kingdee / yonyou) | `configs/mappings.yaml` 加 `sources.<ds>` 段 | 改 `internal/business/mapping.go` 的 `registerXxx()` 函数 |
| 改某 ds 下的物理字段名 | `configs/mappings.yaml` | 改 Go 代码 |
| 加新 entity(restock_window_sales 等) | (W2) `configs/mappings.yaml` + `mapping_yaml.go` 校验 | 散落到 handler/restock 各处硬编码物理字段 |
| 临时实验某 ds 物理字段 | 也走 yaml(改完一并提交) | `registerXxx()` 加 hardcode 后忘删 |

**Vibe coding 校验**:
- 任何含 `products.` / `sales.` / `t_bd_item_info.` 等物理前缀的字符串,只允许出现在:
  1. `configs/mappings.yaml` 数据
  2. `internal/business/mapping.go` 的 `registerXxx()` fallback(2026-09-02 之前的过渡)
  3. `internal/business/mapping_yaml_test.go` 单测
  4. `internal/parser/agent/client.go` 自身(cube 通信层)
  5. 业务专用 cube 内部(restock / supplierpayment 在 `RawQuery` 调用前)
- **新增**物理字段名出现在其他位置 = 立即按 12.1 规则 review

```bash
# 检查 handler / service / 工具 是否有硬编码物理字段名泄漏
rg "t_bd_item_info\.|siss_saleflow\.|v_prom_saleflow\." --type go \
  internal/api internal/restock internal/supplierpayment internal/agent/tools
# 应只输出 raw query 调用点 (supplierpayment 允许、restock 允许),不应有 handler 写死物理字段
```

### 12.4 Executor 优先 — 业务逻辑封装

| 想做的事 | 调 | 严禁 |
|---|---|---|
| handler 查商品列表 | `BizExecutor.SearchProducts(supplier, limit)` | 自己写 `ToPhysicalQuery` + `Execute` + `ToBusinessResponse` |
| handler 按品牌反查供应商 | `BizExecutor.SearchProductsByBrand(brand, limit)` | 同上 |
| handler 列所有供应商 | `BizExecutor.DistinctSuppliers(limit)` | 同上 |
| handler 通用 cube 查询(任意 bizFields + filter) | `BizExecutor.Query(entity, bizFields, filters, limit)` | 同上 |
| 业务专用 cube(restock / supplierpayment) | `Gateway.RawQuery` / `Gateway.RawQueryWithTime` | `*agent.Client` 直接调 |
| 取当前 ds 用的物理 cube 名 | `BizExecutor.CubeOf(entity)` | `src.Cube` 手取 |

**新增 Executor 方法规则**:
- 业务名 + 业务语义(`SearchProductsByBrand` 不是 `QueryProductsByNameContains`)
- 内部统一调 `e.query()` 私有方法,不要每个新方法自己拼 `ToPhysicalQuery + Execute + ToBusinessResponse`
- ds-specific 逻辑(如 erp suppliers 没 measure)在 Executor 内加注释,不放 handler

### 12.5 trpc-agent-go 工具边界

| 工具类型 | 怎么接 cube |
|---|---|
| 写 PG 表的工具(policy/calendar/fee/payment) | ❌ 不接 Gateway,直接 `*pgxpool.Pool` |
| 查 cube 的工具(W2+ `QueryProductsTool` 等) | ✅ `r.gateway.Query("products", ...)` |
| 写操作类工具(改 cube 不存在,都是写 PG) | N/A |

**`agent.NewRunner` 当前签名**:
```go
NewRunner(ctx, cfg, pool, gateway)  // gateway 必传,W2+ cube tool 用
```

### 12.6 数据源隔离 (W1 现状)

- 启动后数据源固定(`cfg.DataSource`),不再运行时切换
- 前端 / LLM 不需要知道当前数据源,也不允许切换
- `?datasource=` 运行时覆盖 API 已删(2026-08-31)
- 加新 ds 步骤:cube-agent-server 建 cube → `configs/mappings.yaml` 加 sources → 重启

### 12.7 违反本规则的后果(P0 阻断)

1. **业务代码直接 `import "parser/agent"`** → P0 阻断,必须改走 Gateway
2. **handler / API 返回物理字段名给前端** → P0 阻断,数据契约破坏
3. **`configs/mappings.yaml` 没改却在 Go 里 hardcode 物理字段** → P0 阻断,破坏配置化
4. **handler 自己拼 `ToPhysicalQuery + Execute` 而不用 Executor** → P0 阻断,破坏单一收编
5. **Executor 新方法没走 `e.query()` 私有方法,自己拼** → P1 必改,后续维护成本

---

**最后一条规则**:改完本文件,跑一下 `go test ./internal/agent/skill/...` 确认 skill 系统没坏(虽然本文件不影响代码,但习惯性验证是好事)。
