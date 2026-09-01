# 智能采购收货系统 方案设计

> 调研产物:基于 `trpc-group/trpc-agent-go` (Go 1.21+, Apache-2.0, 1.8k stars) 集成 collect-ai 现状
> 范围:调研 + 设计,不出代码
> 编写日期: 2026-09-01

---

## 0. 调研结论(先看这部分)

### 0.1 trpc-agent-go 关键能力

| 能力 | 包路径 | 用途 |
|---|---|---|
| LLMAgent | `agent/llmagent` | LLM + Function Tool 调用,主流用 |
| ChainAgent / ParallelAgent / CycleAgent | `agent/chainagent` 等 | 多 Agent 编排 |
| GraphAgent | `agent/graph` | 有状态图工作流,支持条件路由/分支,**关键** |
| Function Tool | `tool/function` | Go 函数直接注册为 tool,JSON schema 强类型 |
| MCP Tool | `tool/mcptool` | MCP 协议 tool 桥 |
| Memory Service | `memory` | 跨 session 长期记忆(可 Redis 持久化) |
| Knowledge / RAG | `knowledge` | 文档检索 |
| Session | `session` | 会话状态/事件流 |
| Runner | `runner` | 统一执行器,支持流式 SSE + OpenTelemetry |
| AG-UI / A2A | `server/agui`, `server/a2a` | 协议化前端/Agent 互通 |
| Prompt Caching | `model/openai` | 自动成本优化 90% |

- OpenAI 兼容接口 → 可直接用 DeepSeek(用户环境已有)
- 现有 `internal/parser/bigmodel.LlmClient`(智谱 glm-4-flash)可作为对照基线,新模块用 DeepSeek
- trpc-agent-go 不抢 HTTP / DB / WS,collect-ai 现有 `agent.Client` / `wecom` / pgxpool 全部可作为 **Function Tool** 注册进去

### 0.2 collect-ai 现状(本次要扩展的基线)

| 已有 | 文件 / 位置 | 与新方案的衔接 |
|---|---|---|
| 5 级级联匹配(barcode exact / name exact / no-space exact / Levenshtein / substring) | `internal/parser/matcher/matcher.go` | 需在 L2 之后插入 **L3:条码前缀缺失→后 4 位 + 名称模糊** |
| OCR + LLM 解析 | `internal/parser/parser.go` | 已支持 mode=purchase |
| 采购订单 session | `parse_session` + `parse_row` (PostgreSQL) | 直接复用,session 加新字段 |
| 企业微信智能机器人 长连接 SDK | `internal/restock/wecom.go` (自实现,无外部依赖) | **完全复用**,只新增 `OnMessage` 的 LLM 分发 |
| 业务字段映射 | `internal/business/{executor,mapping}.go` | 已是业务名→物理 cube 字段 翻译,Agent 工具直接调它 |
| HTTP 风格的 `agent.Client` | `internal/parser/agent/client.go` | **不是**真正的 LLM Agent 框架,仅是 cube-agent-server 的 HTTP 客户端 → 包装成 Function Tool |
| 补货体系 | `internal/restock/` | 数据模型(任务/反馈/采购计划) 可借鉴 |
| 权限 + RBAC | `internal/rbac/`, `internal/auth/` | Agent 工具调用前必须过 `permInventoryView` 之类的权限检查 |

### 0.3 可行性结论

**可行,推荐集成。** 三个具体证据:

1. **wecom 现有 WS 客户端**已经有 `OnMessage(chatID, userID, text)` 回调,直接喂给 LLM Agent 即可;不需要再加一层
2. **trpc-agent-go 是 OpenAI 兼容**,DeepSeek 现成可用;`bigmodel.LlmClient` 和 `agent.Client` 都是普通 Go 函数,符合 Function Tool 的接口
3. **GraphAgent** 完美匹配"OCR → 多级匹配 → 业务规则检查 → 提醒"的流程,有状态、条件路由都可原生表达

**建议集成度:中度**
- 100% 替换现有 LLM 调用不划算(收益低、迁移风险大)
- 90% 走 collect-ai 现有逻辑,只在**新功能**(企微对话/智能提醒/语义判定)用 trpc-agent-go

---

## 1. 整体架构

### 1.1 集成后架构图(伪图)

```
                 ┌────────────────────────────┐
                 │  企业微信智能机器人(长连接) │
                 └──────────┬─────────────────┘
                            │ OnMessage(text, user, chat)
                            ▼
   ┌────────────────────────────────────────────────────┐
   │ collect-ai 服务 (Go, 现有)                          │
   │                                                     │
   │  ┌──────────────────────────────────────────────┐   │
   │  │  internal/agent/  (NEW)                      │   │
   │  │                                              │   │
   │  │  ┌────────────────┐    ┌─────────────────┐   │   │
   │  │  │ PurchaseAgent  │    │ AlertGraphAgent │   │   │
   │  │  │ (LLMAgent)     │    │ (GraphAgent)    │   │   │
   │  │  └───────┬────────┘    └────────┬────────┘   │   │
   │  │          │ Function Tools       │            │   │
   │  │          ▼                       ▼            │   │
   │  │  ┌─────────────────────────────────────────┐ │   │
   │  │  │  tools/  (NEW)                          │ │   │
   │  │  │  - RememberSupplierPolicy               │ │   │
   │  │  │  - QuerySupplierPolicy                  │ │   │
   │  │  │  - RecordSpecialDate                    │ │   │
   │  │  │  - QuerySpecialDate                     │ │   │
   │  │  │  - RecordPromotionFee                   │ │   │
   │  │  │  - CubeQuery    (包装 agent.Client)    │ │   │
   │  │  │  - CheckPurchaseAlert (规则)            │ │   │
   │  │  │  - WeComSend     (包装 wecom.Send)     │ │   │
   │  │  └─────────────────────────────────────────┘ │   │
   │  └──────────────────────────────────────────────┘   │
   │                                                     │
   │  ┌─ 现有模块(全部复用,不破坏)───────────────────┐    │
   │  │ parser/ business/ restock/ store/ rbac/      │    │
   │  │ auth/ wxsign/ config/                        │    │
   │  └────────────────────────────────────────────┘    │
   └────────────┬────────────────────┬──────────────────┘
                │ HTTP               │ pgx
                ▼                    ▼
      ┌─────────────────┐   ┌──────────────────────┐
      │ cube-agent-     │   │ PostgreSQL           │
      │ server (已有)   │   │  (现有)              │
      └─────────────────┘   └──────────────────────┘
                │
                ▼
      ┌─────────────────┐
      │ hbpos / erp     │
      │ SQL Server      │
      └─────────────────┘
```

### 1.2 集成原则

| 原则 | 落实 |
|---|---|
| 不改 HTTP API 路由 | 已有 `/api/v1/parse/*` `/api/v1/restock/*` 全保留,Agent 只在内部被调用 |
| 不改 PG schema 主键 | 新表加 `_id` 自增,老表加可空字段 |
| 不破坏现有 LLM 调用 | `bigmodel.LlmClient` 继续给 parser 用,Agent 走 DeepSeek |
| 复用现有 wecom 客户端 | 不另写 WS,`WeCom` 已经是 publish-subscribe 模式 |
| 复用现有 RBAC | Agent 工具调用前 `auth.HasPerm(ctx, "supplier:write")` |
| 复用现有 cube 调用 | `business.Executor` 已经是 业务字段名 → cube,工具直接调它 |
| 单进程,不引入新服务 | `internal/agent/` 作为子包,跟 restock 一样在 server 进程跑 |

### 1.3 不在本期范围

- 不动 wecom 协议 / 频控
- 不动 cube YAML 定义(只新增查询维度,如 supplier_policy 是 PG 不是 cube)
- 不引入新数据库
- 不引入 MCP server(本期是 Function Tool 起步,MCP 是下一阶段的事)
- 不动权限模型(RBAC 不改,只调 `auth.HasPerm`)

---

## 2. 模块 A:企微对话 Agent(决策信息收集)

### 2.1 目标

> 用户场景:店长老张在企微群@机器人:"汇一是自采供应商,堆头费他们自己出;榄菊不能退货,以后别进了;中秋节前 3 天要备 5 倍量"

Agent 自动抽取结构化字段,写入 PG,**确认 + 落库**,不要求店长老张会填表。

### 2.2 trpc-agent-go 怎么用

```text
Runner.Run(ctx, userID="u_owner", sessionID="wecom-chat-abc",
  Message("汇一是自采供应商,堆头费他们自己出"))
      ↓
LLMAgent (model=DeepSeek, instruction="你是商超采购助理...")
      ↓
function_calling → tools.RememberSupplierPolicy
      ↓
JSON schema 校验 → 写 supplier_policy 表
      ↓
LLM 二次回复:"已记下:汇一-自采-堆头自付。还有别的吗?"
      ↓
SSE 流式回发,WeCom 客户端发 aibot_send_msg 帧
```

**关键设计**:
- `llmagent.WithTools(...)` 注册 6 个工具(下方 2.3)
- `llmagent.WithMemoryService(memorysvc.NewInMemoryService())` 跨消息上下文
- `runner.Runner` 单例,服务启动时 NewRunner 一次,所有消息复用
- `OnMessage` 回调里 `runner.Run(...)` 拿 event channel,逐 chunk 发回

### 2.3 Function Tools 设计(本模块 6 个)

| 工具 | 输入 schema | 副作用 | 权限 |
|---|---|---|---|
| `remember_supplier_policy` | `{supplier, attrs:{is_self_procure, allow_return, has_duitou, has_duanjia, note}}` | UPSERT supplier_policy | `supplier:write` |
| `query_supplier_policy` | `{supplier}` | SELECT | `supplier:read` |
| `record_special_date` | `{date, type:holiday/promo/blackout, name, lead_days}` | INSERT special_calendar | `calendar:write` |
| `query_upcoming_dates` | `{type, days_ahead}` | SELECT | `calendar:read` |
| `record_promotion_fee` | `{supplier, kind:堆头/端架/陈列, amount, period_start, period_end, note}` | INSERT promotion_fee | `promotion:write` |
| `list_promotion_fee` | `{supplier?, period_start?, period_end?}` | SELECT | `promotion:read` |

所有工具实现统一签名:
```go
func(ctx context.Context, req MyReq) (MyResp, error)
```
- 通过 `function.WithName(...)` `function.WithDescription(...)` 注册
- JSON schema 自动从 struct tag(jsonschema)生成,无需手写
- 内部一律调 PG,**不直接拼 SQL**,用 `pgx.NamedArgs` + 准备好的常量语句
- 失败要返回 error,LLM 收到后会自我修正("数据库写失败,请重试")

### 2.4 关键对话流(确认机制)

LLM 第一次抽取的结构化字段,**不直接写库**,而是返回给用户做"自然语言确认":

```
User:  "榄菊以后不让退了"
Agent: "记一下:榄菊 不允许退货(allow_return=false),影响后续新单入场。
        对吗?有补充吗?"
User:  "对,以后拉黑榄菊"
Agent: "已记:榄菊 block_entry=true。
        当前 supplier_policy 现有:
        - 汇一: 自采=true, 堆头自付=true
        - 榄菊: allow_return=false, block_entry=true
        还有别的供应商要更新吗?"
```

**实现**:Agent 的 system prompt 强制要求"重要操作前确认",tool 内部用 `dry_run: bool` 字段,确认时再 dry_run=false 落库。

### 2.5 频率与频控

- 企微 30/min/会话、1000/h/会话
- Agent 单次回复必须 < 200 字(避免频控)
- 工具调用链超过 3 步就强制结束对话("我先去查,稍后给你完整答复" → 走后台异步)
- 复杂多意图(用户说一堆) → 用 `ParallelAgent` fan-out,但首版简化为顺序

### 2.6 Memory 用法

`memorysvc.NewInMemoryService()` 装在 Runner 上:
- 每条用户消息写入会话 memory
- 下次同 userID 进来 → LLM 自动看到历史("上次你说过汇一自采...")
- 短期够用,长期(年)再换 Redis 后端

---

## 3. 模块 B:OCR 采购订单多级匹配升级

### 3.1 目标

用户提出的 3 段位 + 现有 2 段位 → 重组为 5 段位,显化每档:

| 段位 | 触发 | 命中条件 | 现有 | 改动 |
|---|---|---|---|---|
| **L1** | barcode 完整 | `sku.barcode = ocr.barcode`(全等) | ✅ 已实现 | 不动 |
| **L2** | name 完整 | `sku.name = ocr.name`(去空格、大小写) | ✅ 已实现 | 不动 |
| **L3(新)** | barcode 前缀缺失 | `RIGHT(sku.barcode, 4) = RIGHT(ocr.barcode, 4)` AND 名称模糊匹配 ≥ 0.6 相似度 | ❌ | **新增** |
| L4 | 名称相似 | Levenshtein 距离 ≤ `fuzzy` | ✅ 已实现 | 不动 |
| L5 | 子串 | 任一端 ≥ 4 字符,长度差 ≤ 3 | ✅ 已实现 | 不动 |

### 3.2 实现思路

`internal/parser/matcher/matcher.go` 现有 `Match` 流程中,**在 L2 之后插入 L3**:

```go
// 伪代码,不动现有结构,只插一段
// (现有 2) name exact → applyMatch, return
// ↓ 新增 ↓
// (新 3) barcode 缺失前缀补偿:
//   a) 把 sku 的 barcode 索引按 "后 4 位" 桶化(byBarcodeSuffix4)
//   b) 截取 ocr.barcode 的后 4 位(或全部,看长度)
//   c) 同桶的 sku → 计算 name 相似度(用 levenshtein 或 Jaccard)
//   d) 相似度 ≥ 0.6 → applyMatch(status="修正(条码后缀+名称模糊)")
// (现有 3-5) 不动
```

**关键点**:
- 桶化用 `map[string][]model.SkuRecord` 一遍预计算
- 名称相似度建议用 **Jaccard 字符 2-gram** 或 **Levenshtein/rune 数**;选 Jaccard 因为中文 L2 短,L1 反而太严
- L3 的 `status` 字段要明确("修正(条码后缀+名称模糊)"),前端显示要跟其他 status 一致
- 失败 L4/L5 兜底逻辑不变

### 3.3 cube 数据来源

- 已有 `business.Registry.ToPhysicalQuery("products", ds, bizFields, ...)` 接口
- 新增 `matched_by` 字段(SkuRow)记录每行的命中段位:"L1" / "L2" / "L3" / "L4" / "L5" / "新SKU"
- 入库:`parse_row.status` 已经有(`OK` / `修正(名称)` / `修正(模糊)` / `新SKU`),可加 `L3 修正(条码后缀+名称模糊)`,**不破坏老 status 字符串**

### 3.4 trpc-agent-go 用量

- **本模块基本不用** trpc-agent-go —— 多级匹配是纯规则,引入 LLM 反而慢+贵
- 唯一用 LLM 的地方:**OCR 错误纠正**。OCR 识别错的 barcode ("0" vs "O","1" vs "l")时,可让 LLM 看图重新抽(可选 Phase 3)

### 3.5 验证脚本

- `scripts/test_matcher_l3.py`:用历史 50 个采购单,跑 L1~L5,统计每段命中率
- 验收:L1+L2 ≥ 80% 命中,L3 ~ 5-10% 命中,L4+L5 ~ 5-10% 命中,新 SKU ≤ 5%

---

## 4. 模块 C:采购订单 session 智能提醒

### 4.1 目标

每次店长 / 采购员在 H5 / 企微打开采购订单时,后端实时给两类提醒:
1. **限入场 / 黑名单**:`supplier_policy.block_entry=true` 或 `allow_return=false` 的供应商,新单据标红
2. **季节不宜补货**:`item_clsname` 命中"应季/反季"模式 + special_calendar 命中"holiday" 提前 lead_days,推"需提前 X 天备货"
3. **节假日流量高峰预警**:节假日 lead_days 范围内 + 商品 is_seasonal,推"这类商品节前需求翻倍"

### 4.2 trpc-agent-go 怎么用:**GraphAgent**

```text
LoadSession(id)
   ↓
GraphAgent.Run(state={session, rows, ctx})
   ↓
[Node: 加载供应商政策]      → map[supplier]supplier_policy
   ↓
[Node: 加载节假日窗口]      → []special_calendar_in_window
   ↓
[Node: 分类每行]            → rules_engine.Classify(row, policy, calendar)
   ↓
[ConditionalEdge: 类型路由]
   ├─ "blocked"    → [Node: 生成限入场警告]    → append alert
   ├─ "offseason"  → [Node: 生成季节警告]      → append alert
   ├─ "holiday"    → [Node: 生成节日备货建议]  → append alert
   └─ "normal"     → pass
   ↓
[Node: 聚合 + 排序]         → alerts sorted by severity
   ↓
[Node: 写回 parse_session_alert 表 + (可选) 推 wecom]
```

**为什么用 GraphAgent**:
- 多条件路由(每个 row 走不同分支)是 Graph 的强项
- 状态在 graph.State 里流动,LLM 只在"季节语义判定"那一步介入
- 失败回退容易:Graph 节点失败可降级为"无提醒",不阻塞主流程

### 4.3 规则引擎(纯 Go,非 LLM)

`internal/purchasealert/rules.go`(NEW):

```go
type Rule interface {
    Classify(row SkuRow, ctx RuleContext) []Alert
}
type RuleContext struct {
    SupplierPolicies map[string]SupplierPolicy
    Calendar         []SpecialDate
    Today            time.Time
}

var DefaultRules = []Rule{
    &BlockEntryRule{},        // 限入场
    &NoReturnRule{},           // 不允许退货 → 警告需确认
    &OffseasonRule{},          // 季节不匹配
    &HolidayLeadRule{},        // 节假日 lead_days
}
```

LLM 不参与分类,只参与 **"该商品是否应季"** 这种语义模糊的判断(走"分类每行" Node 内部的 sub-LLM call)。

### 4.4 提醒落库 + 推送

- 新表 `purchase_session_alert(id, session_id, row_id, rule, severity, message, created_at, acked_at)`
- H5 端:`GET /api/v1/parse/sessions/{id}` 响应里带 `alerts: []`
- 推企微:**只在 alert 数量 ≥ 3 或 severity=block 时推**,避免噪音
- 老板在企微里点 ack → 更新 `acked_at`(类似 restock 的按钮反馈)

### 4.5 LLM 用量控制

- 季节语义判定是**唯一** LLM 介入点
- 走 `llmagent.New(...)` + 1 个 tool(`classify_season(item_name, today)`),每次最多 200 tokens
- 6h 缓存(`map[item_no]season` in-memory + LRU 1000 条)
- 失败降级:LLM 不可用 → 全部按"非应季"处理,只走纯规则(够安全)

---

## 5. 模块 D:对账 + 堆头费 + 借款建议

### 5.1 目标

月底对账时,系统主动告诉老板:
- **堆头费/端架费** 应分摊到哪些供应商,金额多少
- **哪些供应商的堆头费即将到期** (剩 ≤ 7 天)
- **下月预计采购额** 多少,对应**建议借款额** (采购额 × 1.5 buffer + 7 天回款延迟)
- **现金日报** 显示可用资金 < 建议借款,推 owner 群

### 5.2 Function Tools(本模块 4 个)

| 工具 | 作用 | 算法 |
|---|---|---|
| `compute_promotion_fee_share(supplier, month)` | 算该供应商当月堆头+端架费分摊 | 简单求和 `promotion_fee` 表 |
| `upcoming_promotion_expiry(days_ahead)` | 查未来 N 天到期的堆头费 | `period_end - today ≤ N` |
| `forecast_purchase_amount(supplier, days)` | 预测 N 天内采购额 | cube: `近 30 天 supplier 采购额 × (N/30)` |
| `suggest_loan_amount(days, buffer_factor)` | 借款建议 | `forecast_purchase × buffer × (1 + 7/30 cash_lag)` |

### 5.3 数据流

```
1) 每日 21:00 cron → 算 forecast_purchase_amount(30) → 写 supplier_forecast 表
2) 每日 21:00 cron → 查 upcoming_promotion_expiry(7) → 如有 → 推 office 群
3) 每周一 09:00 cron → 算 suggest_loan_amount(30, 1.5) → 写 loan_suggestion 表
4) 每日 22:00 cron → 拉现金日报(新数据源)→ 算 cash_available → 如 < suggest → 推 owner
5) 每月 1 号 02:00 → 跑上月 promotion_fee_share → 写 promotion_fee_share 表
```

### 5.4 LLM 介入点(可选,Phase 4)

- 借款建议输出时,LLM 给一句"为什么这个数"(可读性),默认 1 句
- 老板问"为什么这个供应商采购预测这么高" → LLM 拉近 30 天趋势 + 节假日 → 自然语言解释

### 5.5 现金日报数据源

> ⚠️ 风险点:目前 collect-ai 没有现金日报数据源

**建议**:
- 短期:老板手动在 H5 页面录入"昨日现金结余" → 写 cash_balance 表
- 中期:影刀 RPA 抓管家婆/金蝶的现金日报 → 写 cash_balance(新增 internal/rpa/ 模块)
- 长期:cube 加 `t_cash_daily` 数据源(从金蝶 ODBC 取数,数据治理层负责)

---

## 6. 数据模型(新增表)

```sql
-- A 模块 + C 模块用
CREATE TABLE supplier_policy (
    id BIGSERIAL PRIMARY KEY,
    supplier_name TEXT NOT NULL,
    key TEXT NOT NULL,                  -- 'is_self_procure' | 'allow_return' | 'has_duitou' | 'has_duanjia' | 'block_entry' | 'block_reason' | 'note'
    value JSONB NOT NULL,               -- 任意 JSON 值
    source TEXT NOT NULL,               -- 'wecom_agent' | 'manual' | 'import'
    chat_id TEXT,                       -- 来源企微群
    message_id TEXT,                    -- 企微消息 ID(幂等)
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (supplier_name, key)         -- 同一供应商同一属性只有 1 行
);
CREATE INDEX ON supplier_policy(supplier_name);

-- A 模块
CREATE TABLE special_calendar (
    id BIGSERIAL PRIMARY KEY,
    date DATE NOT NULL,
    type TEXT NOT NULL,                 -- 'holiday' | 'promo' | 'blackout' | 'season_start' | 'season_end'
    name TEXT NOT NULL,                 -- '中秋节' | '国庆' | '春节' | '618' | ...
    lead_days INT NOT NULL DEFAULT 0,  -- 提前备货天数
    note TEXT,
    source TEXT NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (date, type, name)
);
CREATE INDEX ON special_calendar(date);

-- A 模块
CREATE TABLE promotion_fee (
    id BIGSERIAL PRIMARY KEY,
    supplier_name TEXT NOT NULL,
    kind TEXT NOT NULL,                 -- '堆头' | '端架' | '陈列' | 'DM' | '条码费'
    amount NUMERIC(12,2) NOT NULL,
    period_start DATE NOT NULL,
    period_end DATE NOT NULL,
    note TEXT,
    source TEXT NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW()
);
CREATE INDEX ON promotion_fee(supplier_name, period_end);

-- C 模块
CREATE TABLE purchase_session_alert (
    id BIGSERIAL PRIMARY KEY,
    session_id UUID NOT NULL REFERENCES parse_session(id) ON DELETE CASCADE,
    row_id BIGINT REFERENCES parse_row(id) ON DELETE CASCADE,
    rule TEXT NOT NULL,                 -- 'block_entry' | 'no_return' | 'offseason' | 'holiday_lead'
    severity TEXT NOT NULL,             -- 'block' | 'warn' | 'info'
    message TEXT NOT NULL,
    acked_at TIMESTAMPTZ,
    acked_by TEXT,
    created_at TIMESTAMPTZ DEFAULT NOW()
);
CREATE INDEX ON purchase_session_alert(session_id, created_at DESC);

-- D 模块
CREATE TABLE supplier_forecast (
    id BIGSERIAL PRIMARY KEY,
    supplier_name TEXT NOT NULL,
    forecast_date DATE NOT NULL,
    horizon_days INT NOT NULL,          -- 7 / 30 / 90
    amount NUMERIC(12,2) NOT NULL,
    basis TEXT,                          -- 算法说明
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE loan_suggestion (
    id BIGSERIAL PRIMARY KEY,
    horizon_days INT NOT NULL,
    buffer_factor NUMERIC(4,2) NOT NULL,
    amount NUMERIC(12,2) NOT NULL,
    basis JSONB NOT NULL,               -- 各项 forecast 明细
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE cash_balance (
    id BIGSERIAL PRIMARY KEY,
    balance_date DATE NOT NULL UNIQUE,
    amount NUMERIC(14,2) NOT NULL,
    source TEXT NOT NULL,               -- 'manual' | 'rpa' | 'cube'
    created_at TIMESTAMPTZ DEFAULT NOW()
);
```

**老表加字段**(可选,均 nullable):
- `parse_row.matched_by TEXT` — 记录 L1~L5 / 新SKU
- `parse_session.alerts_count INT` — 冗余,加快列表显示

---

## 7. trpc-agent-go 使用范围一览

| 模块 | 用 trpc-agent-go 吗? | 怎么用 | 备注 |
|---|---|---|---|
| A 企微对话 Agent | ✅ 核心 | LLMAgent + 6 Function Tools + Memory | 必做 |
| B OCR 多级匹配 | ❌ 不直接用 | 纯规则 | LLM 仅可选"OCR 错误纠正"(远期) |
| C 智能提醒 | ✅ GraphAgent | 规则节点 + 1 个 LLM 子调用(应季判定) | 必做 |
| D 对账 + 借款 | ✅ 部分 | Function Tools(4 个)+ 1 个解释 LLM | 可选 |
| 现有 restock | ❌ 不动 | 现有 bigmodel.LlmClient 继续 | 不破坏 |
| 现有 parser | ❌ 不动 | 现有 bigmodel.LlmClient 继续 | 不破坏 |

**Runner 实例**:
- 全进程复用 1 个 `runner.NewRunner("collect-ai-agent", agent, ...)`
- 通过 `Runner.Run(ctx, userID, sessionID, message)` 调用
- LLM 切换 DeepSeek: `openai.New("deepseek-chat", openai.WithVariant(openai.VariantDeepSeek))`

---

## 8. 实施路径(4 周)

| 周 | 交付 | 验收 |
|---|---|---|
| **W1** | (1) 引入 trpc-agent-go 依赖<br>(2) `internal/agent/` 子包骨架<br>(3) supplier_policy / special_calendar / promotion_fee 三张表 migration<br>(4) 模块 A 的 6 个工具实现 + 单元测试 | go build 通过;每个工具单测覆盖正常+异常路径 |
| **W2** | (5) PurchaseAgent (LLMAgent) + Runner<br>(6) 接入 wecom OnMessage → runner.Run → 文本回复<br>(7) 真实对话测试:"汇一自采+堆头" / "榄菊不让退" | E2E:用户在群里说一句话,supplier_policy 表新增/更新,Agent 文字确认 |
| **W3** | (8) matcher.go 加 L3 (条码后缀+名称模糊)<br>(9) 模块 C 规则引擎 + GraphAgent<br>(10) purchase_session_alert 表 + H5 端显示<br>(11) cron:堆头费到期预警 | 50 单历史跑 L1~L5 命中率统计;新单打开有 alert 显示 |
| **W4** | (12) D 模块 4 个工具 + cron<br>(13) 现金日报手动录入入口<br>(14) OpenTelemetry + Langfuse 接入(可选)<br>(15) 端到端冒烟 | 借款建议出数;堆头费到期推 office 群;现金 < 借款推 owner |

**W1 风险最高**(trpc-agent-go 首次引入),W2-W4 都是叠加。

---

## 9. 风险 & 缓解

| 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|
| LLM 抽取结构化字段不准 | 中 | 高 | JSON schema 强校验 + 必走"二次确认"对话;每次写库前 dry_run 预览 |
| trpc-agent-go 跟现有 `bigmodel.LlmClient` 行为不一致 | 低 | 中 | A/C/D 走新框架,parser 走老的,两条线独立;e2e 测试覆盖两路 |
| 企微频控撞顶(30/min) | 中 | 中 | Agent 输出限 200 字,关键写库,长回答走异步分页;不抢群频控,只在 office 群推严重告警 |
| 现金日报数据源缺 | 高 | 中 | 短期手动录入;中期 RPA;长期 cube 接入 |
| 季节判定 LLM 误判 | 中 | 低 | 6h 缓存 + 失败降级为"非应季"(保守策略) |
| trpc-agent-go 升级 breaking change | 中 | 中 | 锁版本,引用本地 vendor 或固定 tag |
| LLM 费用超预算 | 低 | 低 | 默认 deepseek-chat(便宜),只在"二次确认解释"用更大的 |
| 数据迁移(老 supplier 已有政策) | 中 | 中 | 一次性 import 脚本;先 dry_run 看 diff 再 apply |

---

## 10. 验证标准(Definition of Done)

- [ ] 模块 A:用户在企微说"汇一自采+堆头自付",`supplier_policy` 表新增 2 行,Agent 文字确认收到
- [ ] 模块 B:历史 50 个采购单,跑通 L1-L5,新 SKU 比例 < 5%,L3 命中率有数据
- [ ] 模块 C:含黑名单供应商的 session 打开,H5 显示红色 alert;P0 alert 推 office 群
- [ ] 模块 D:cron 跑完,loan_suggestion 表有数,堆头费到期前 7 天有企微推
- [ ] 端到端:从企微对话 → 数据库写 → H5 显示 → 推送,全链路 < 5s
- [ ] RBAC:无权限用户调 Agent 工具,返回 403,LLM 收到错误自我修正
- [ ] 重启恢复:服务重启后 Runner state / chat bindings / memory 不丢
- [ ] 观测:OpenTelemetry trace 覆盖 Runner → Tool → PG 全链路

---

## 11. 后续(本期不做)

- **A2A**:采购 Agent 跟补货 Agent 互通(补货 Agent 调用采购 Agent 拿"限制入场"清单)
- **AG-UI**:H5 升级为 AG-UI 协议前端,采购助理 Agent 流式 UI(选品助手、对话式采购)
- **MCP**:采购 Agent 通过 MCP 调用外部(影刀 RPA / 钉钉审批 / 财务系统)
- **Knowledge / RAG**:把供应商合同、堆头费合同 PDF 入库,Agent 自动检索
- **Evolution**:Agent Self-Evolution,把高频"季节判定""供应商分类"沉淀为 SKILL.md
- **多店**:现在按单店设计,集团化时 supplier_policy 加 store_id 维度

---

## 附录 A:关键文件清单(本期将动到的)

| 文件 | 动作 |
|---|---|
| `go.mod` | + `trpc.group/trpc-go/trpc-agent-go` |
| `internal/agent/` (NEW) | agent / tools / runner / memory |
| `internal/purchasealert/` (NEW) | 规则引擎 + Graph 编排 |
| `internal/parser/matcher/matcher.go` | + L3 段位 |
| `internal/parser/matcher/matcher_test.go` (NEW) | 5 段位单测 |
| `migrations/2026_09_xx_agent.sql` (NEW) | 新表 DDL |
| `cmd/server/main.go` | + 启动 Agent Runner + 注册 cron |
| `docs/agent-purchase-plan.md` (本文件) | 持续维护 |
| `scripts/test_matcher_l3.py` (NEW) | 命中率统计 |

## 附录 B:参考资料

- trpc-agent-go: <https://github.com/trpc-group/trpc-agent-go> (v0.x, Apache-2.0)
- 官方文档: <https://trpc-group.github.io/trpc-agent-go/>
- DeepSeek API: OpenAI 兼容,`base_url=https://api.deepseek.com`
- 企微智能机器人 长连接协议: <https://developer.work.weixin.qq.com/document/path/101833>
- 现有 wecom SDK: `internal/restock/wecom.go` (collect-ai 内部)
- 现有 cube-agent-server: 详见 `docs/` 仓库,本文件不展开
