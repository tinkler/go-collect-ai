# 智能采购模块 Review 指南

> 适用: `feat/agentic-purchase` 分支(18 commit, 36 文件, 7950 行新增)
> 范围: W1 智能采购工具 + W2 企微/H5 Agent 桥 + W3 OCR/规则/cron + W4 对账 + W5 cube

---

## 0. 一句话全景

```
企微群消息 / H5 调 Agent
  → trpc-agent-go Runner (DeepSeek 兼容)
  → Function Tool 调 PG / cube
  → 落库 supplier_policy / special_calendar / promotion_fee / alerts
  → 规则引擎(规则表 + 季节 LLM + 应季判定)
  → H5 显示 alerts / 企微推 cron 提醒
  → D 模块周建议 + 月分摊 + 现金检查
```

---

## 1. 推荐 Review 路径(由浅入深)

| 阶段 | 文件 | 时长 | 关注什么 |
|---|---|---|---|
| **1. 看方案** | `docs/agent-purchase-plan.md` | 15 min | 整体设计意图 + 6 大模块边界 |
| **2. 看入口** | `cmd/server/main.go` | 15 min | 装配顺序 + 8 个 Service 注入 + 6 个 cron goroutine |
| **3. 看 Schema** | `internal/store/pg.go` | 10 min | 7 张新表的 DDL + 索引策略 |
| **4. 看核心算法** | `internal/parser/matcher/matcher.go` | 20 min | 6 段位匹配 + 字节/rune 老 bug 修复 |
| **5. 看 Agent 桥** | `internal/agent/wecom_bridge.go` | 20 min | per-chat worker + 5 层降级 + 频控 |
| **6. 看规则引擎** | `internal/purchasealert/rules.go` + `service.go` | 20 min | 4 规则 + 季节链 + 5 层降级 |
| **7. 看 D 模块** | `internal/supplierpayment/cron.go` + `cube.go` | 20 min | 三维度算法 + cube 接入 |
| **8. 看 cron 调度** | `cmd/server/main.go` 末段 `runDaily/runWeekly/runMonthly` | 10 min | 4 个对齐式 ticker |
| **9. 看测试** | `**/*_test.go` | 30 min | 120+ case, 重点看 testhelper |
| **10. 看降级总图** | 本文件 §4 | 10 min | 任何新需求先看降级链 |

**总: ~3 小时**

---

## 2. Review 清单 (11 项)

### 2.1 架构层
- [ ] **入口零散落**:`cmd/server/main.go` 是唯一入口,8 个 Service 都在 main 注入
- [ ] **循环依赖**:不存在 — `internal/agent` 依赖 `parser/agent` 的类型别名
- [ ] **类型别名**:`parseragent` (parser/agent) vs `agent` (internal/agent) — 必须看清哪个是哪个

### 2.2 降级层
- [ ] **5 层降级**全打通?每一层:
  1. LLM 不可用 → tools-only / 友好提示
  2. Cube 不可用 → Noop 占位
  3. DB 不可用 → 跳过 Skip (测试) / pool nil error (生产)
  4. 数据缺失 → 0.0 / 0
  5. 错误传播 → log + 不崩
- [ ] **cron 在 LLM 不可用时**:D 模块 cron 不依赖 LLM,只依赖 cube + PG,降级更好做

### 2.3 并发层
- [ ] **per-chat worker**:`wecom_bridge.go` lazy init `map[string]chan`,sync.Mutex 保护
- [ ] **串行**:同 chat 顺序处理(`buffered channel`),跨 chat 并行
- [ ] **频控**:25/min/chat 滑窗,超出返"消息太多"
- [ ] **写冲突**:Apply idempotent + ListAlerts 跳过(已存在) → 重复跑安全

### 2.4 安全 / RBAC
- [ ] **新权限位**:`cash:write` / `cash:read` / `payment:read` / `agent:write` 已加进 router
- [ ] **agent/agent:write**:需要 owner 角色(加进 role_permissions)
- [ ] **secret 隔离**:无 secret 进 commit(BigModel key 在 .env, 没入 git)

### 2.5 性能
- [ ] **DB 索引**:每个 WHERE 列都建了索引(`idx_*`)
- [ ] **partial index**:`(session_id) WHERE acked_at IS NULL` 高频查询
- [ ] **cached response**:`promo_weight` 6h LRU 缓存(季节分类)
- [ ] **大查询**:`siss_saleflow` 单 supplier 30 天 → cube 端应该有按 supplier 索引

### 2.6 算法
- [ ] **L3 Jaccard 阈值 0.6**:有 `TestL3_ThresholdBoundary` 验过
- [ ] **三维度系数范围**:inv 0.8-1.5 / promo 0.9-1.3 / sell 0.7-1.2 — clamp 严防越界
- [ ] **byte vs rune**:`len(s)` 字节 vs `kLen(s)` rune — matcher 修复后,后续代码警惕

### 2.7 错误处理
- [ ] **panic 防护**:`defer recover` 在 cron 入口(`runDaily/runWeekly/runMonthly`)
- [ ] **log 而非 panic**:所有工具失败都 `log.Printf` + 返 error
- [ ] **错误不丢**:agent.Runner error → handler 返 200 + 友好文本(不 500)

### 2.8 兼容性
- [ ] **零业务代码删除**:只删了 L4/L5 老 bug 的 21 行
- [ ] **现有 API 不破**:`GET /sessions/:id` 老路径保留(alerts 是新字段)
- [ ] **降级开关**:`PROMOTION_ALERT_CHAT_ID` 空 → cron 写库但不推群

### 2.9 测试
- [ ] **真 PG 集成测试**:用 `.env` DSN,测试 7 个包
- [ ] **mock + LLM-free**:`wecom_bridge_test.go` / `cube_test.go` 不依赖 LLM
- [ ] **t-% 数据隔离**:测试用 `t-` 前缀,setup 前清场,defer cleanup
- [ ] **Skip 而非 Fail**:PG 不可达 → Skip,默认 `go test` 仍能跑

### 2.10 部署
- [ ] **5 个新 env 变量**:`COLLECTAI_AGENT_*` / `PROMOTION_ALERT_CHAT_ID` / `OWNER_CHAT_ID` / `COLLECTAI_CUBE_QUERIER`
- [ ] **空 env = 禁用**(devMode 友好):所有 cron 都有"未配置则不启动"分支
- [ ] **Migrate 幂等**:启动自动建表,`CREATE TABLE IF NOT EXISTS` + 索引

### 2.11 文档
- [ ] **方案文档**:`docs/agent-purchase-plan.md` 是入口(术语纠正已加)
- [ ] **本 review 指南**:`docs/review-guide.md` 持续维护
- [ ] **commit message**:每个 commit 带 W1/W2/W3.x 标签,便于追溯

---

## 3. 文件布局

### 3.1 顶层结构

```
collect-ai/
├── cmd/server/main.go                # 唯一入口: 8 Service 装配 + 6 cron goroutine
├── docs/
│   ├── agent-purchase-plan.md        # 方案设计 (28KB, 7 大模块)
│   ├── review-guide.md               # 本文件
│   ├── wecom-sop.md                  # 已有
│   └── auth.md                       # 已有
├── go.mod / go.sum                    # 依赖 (trpc-agent-go v1.11.2)
├── internal/
│   ├── agent/                        # ★ W1+W2+W2.5: 智能采购 Agent
│   │   ├── runner.go                 #    Runner 配置 + LLM 加载
│   │   ├── wecom_bridge.go           #    W2 企微对话桥 (per-chat worker)
│   │   ├── wecom_bridge_test.go      #    12 单测
│   │   └── tools/                    #    W1+W4: 10 个 Function Tool
│   │       ├── policy.go             #      remember/query_supplier_policy
│   │       ├── calendar.go           #      record/query_special_calendar
│   │       ├── fee.go                #      record/list_promotion_fee
│   │       ├── payment.go            #      D 模块 4 工具
│   │       ├── helpers.go            #      trimSpace / orDefault
│   │       ├── tools_test.go         #      30+ 单测
│   │       ├── payment_test.go       #      D 模块 6 单测
│   │       └── testhelper_test.go    #      PG fixture
│   ├── parser/
│   │   ├── matcher/                  # ★ W3.1: 6 段位匹配 (L1-L5+L3 新)
│   │   │   ├── matcher.go            #    + 修 byte/rune 老 bug
│   │   │   └── matcher_test.go       #    9 单测
│   │   ├── agent/                    # 已有: cube HTTP 客户端 (parseragent 别名)
│   │   └── bigmodel/                 # 已有: 智谱 GLM 客户端
│   ├── purchasealert/                # ★ W3.2+W3.5: 4 规则 + LLM 应季判定
│   │   ├── rules.go                  #    BlockEntry / NoReturn / Offseason / HolidayLead / LLMSeason
│   │   ├── service.go                #    Apply 编排
│   │   ├── season_classifier.go      #    Keyword + LLM + Caching + Chained
│   │   ├── rules_test.go             #    13 单测
│   │   └── season_classifier_test.go #    11 单测
│   ├── promotionalert/               # ★ W3.3: 堆头费到期预警
│   │   ├── cron.go                   #    RunOnce + Push
│   │   ├── cron_test.go              #    7 单测
│   │   └── testhelper_test.go        #    PG fixture
│   ├── supplierpayment/               # ★ W4+W4.3+W5: D 模块全栈
│   │   ├── cron.go                   #    4 个 cron 任务
│   │   ├── cube.go                   #    CubeQuerier (Noop/Real) + 系数计算
│   │   ├── cron_test.go              #    6 单测
│   │   ├── cube_test.go              #    7 单测
│   │   └── testhelper_test.go        #    PG fixture
│   ├── store/                        # 扩展 (新表 + Repo)
│   │   ├── pg.go                     #    Migrate 6 新表
│   │   ├── cash.go                   #    W4 Cash/Pay/Forecast/Share Repo
│   │   ├── session.go / template.go  #    已有
│   │   └── ...                       #    已有
│   ├── restock/
│   │   └── wecom.go                  # 扩展: 加 OnAgentMessage 钩子 (W2)
│   ├── api/
│   │   ├── handler/handler.go        # 扩展: AlertSvc, CashRepo, PayRepo, AgentRunner 注入
│   │   │                            #       AgentChat 端点 (W2.5)
│   │   └── router.go                 # 扩展: cash/balance + payments/pending + agent/chat
│   ├── model/types.go                # 扩展: Session.Alerts + AlertItem (避免循环)
│   ├── auth/, rbac/, wxsign/, config/ # 已有 (未动)
│   └── business/                     # 已有 (未动)
└── e2e/, scripts/, uploads/           # 已有 (未动)
```

### 3.2 新增文件按模块归类

| 模块 | 新文件 (10) | 行数 |
|---|---|---|
| **W1 agent** | runner.go, tools/{policy,calendar,fee,helpers}.go | 938 |
| **W1 tools tests** | tools_test.go, testhelper_test.go | 511 |
| **W2 agent** | wecom_bridge.go + test | 687 |
| **W2.5 api** | (集成进 handler/router) | 114 |
| **W2 restock** | (扩展 wecom.go) | 12 |
| **W3.1 matcher** | matcher_test.go | 314 |
| **W3.2 purchasealert** | rules.go, service.go + tests | 535 |
| **W3.3 promotionalert** | cron.go + tests | 545 |
| **W3.5 purchasealert** | season_classifier.go + tests | 499 |
| **W4 store** | cash.go | 355 |
| **W4 agent tools** | payment.go + tests | 659 |
| **W4.3+W5 supplierpayment** | cron.go, cube.go + tests | 1019 |
| **docs** | agent-purchase-plan.md, review-guide.md | 870 |
| **config** | go.mod / go.sum (间接) | 142 |

---

## 4. 降级路径总图(必看)

### 4.1 LLM 不可用
- `agent.Runner.Enabled()=false`
- W1: Function Tool 仍可独立调(LLM 缺失不阻塞工具)
- W2 Bridge: 返"智能助理暂未配置"
- W2.5 Handler: 200 + reply="智能助理暂未配置"
- W3.5: LLMSeasonRule 自动跳过(classifier=nil)

### 4.2 Cube 不可用
- `COLLECTAI_CUBE_QUERIER` 默认 noop → NoopCubeQuerier 返 0.5/0.8 占位
- RealCubeQuerier 调用超/错 → 返 0.5 + log,业务继续
- W4.2/4.3 三维度算法:inv 真实算 / promo+sell 退化但 clamp 严防越界

### 4.3 DB 不可用
- 测试: `t.Skipf("PG 不可达")` 不报错
- 生产: `pool.Ping` 失败 → `log.Fatalf` (启动硬性)
- 运行时: query 失败 → Service 返 error,handler 返 500

### 4.4 企微断连
- 长连接自动重连(指数退避 2-60s)
- 推送失败 → `log.Printf("SendText err")` 不崩
- 频控撞顶 → 返"消息太多"提示用户

### 4.5 现金日报缺失
- DailyCashCheck: cb == nil → cash=0 → 算 shortBy 仍合理
- 没录入 → 用户提示录入(不崩)

### 4.6 5 层 fallback 总图
```
LLM 错  →  工具不调   →  返 "没听懂"
Cube 错 →  Noop 占位  →  clamp 范围 (0.7-1.3)
DB 错   →  pool.Exec 返 err  →  handler 500 + log
wcm 错  →  log + skip  →  下一轮重试
数据缺 →  0.0/0       →  业务算降级
```

---

## 5. 关键设计决策 (Why)

### 5.1 为什么不用 GraphAgent (C 模块)?
- 场景是 row × rule 简单 1:N 关系,纯 Go 循环+map dispatch 即可
- LLM 介入点只 1 个(应季判定),用单条 Chained 装饰器更轻
- 失败降级更可控(Graph 节点失败难定位)
- **保留余地**:W3.5 + W4.2 都注释"W5 接 GraphAgent" — 后续真复杂化再上

### 5.2 为什么 Cube 用 Noop 占位而非 nil?
- devMode 必须能跑通(无 cube-agent-server)
- devMode 调试系数变化时,Noop 提供稳定基线
- 业务即使 cube 接入失败,Noop 返 0.5 让 cron 仍产生建议
- 真实切换只需 `COLLECTAI_CUBE_QUERIER=real`,0 代码改动

### 5.3 为什么 cron 自实现不引 robfig/cron?
- W3.3 + W4.3 用了 4 个 cron,如果每个都装一份配置字符串,运维难
- 跟 restock 模块一致(`internal/restock/` 自实现)
- 对齐到整点的逻辑只需 3 个 helper:`runDaily/runWeekly/runMonthly`
- 单元测试容易(mock time)

### 5.4 为什么 matcher L3 不在 L1 之后?
- L1 是 barcode 全等,L3 是 barcode 后缀 + name 模糊,语义不同
- L3 必须在 L2 (name exact) 之后 — 否则 OCR barcode 缺失时,L1 永远失败,L3 直接进
- 命中优先级 L1 > L2 > L3 > L4 > L5,确保"全等" 永远优先 "模糊"

### 5.5 为什么 supplier_payment_suggestion 不用 ON CONFLICT?
- W4.3 cron 每周一跑,可能同一 supplier 多次跑
- 不去重是有意 — 历史建议留作审计,UI 用 `status='pending'` 过滤
- 用户 ack 后改 status,新建议照写,实现简单 append-only

---

## 6. 测试覆盖总览

| 包 | case 数 | 时长 | 关键测试 |
|---|---|---|---|
| `agent/tools` | 36+ | 1.4s | 6 工具 happy / 错 / dry_run / pool nil |
| `agent` (wecom_bridge) | 12 | 6.6s | 白名单 / 串行 / 频控 / 降级 / 截断 |
| `parser/matcher` | 9 | 1.4s | L3 命中 / 阈值 / 排序 / 老 bug 修 |
| `purchasealert` | 24 | 1.4s | 4 规则 + 季节链 4 分类器 + Chained 组合 |
| `promotionalert` | 7 | 0.2s | 扫描 / 分组 / max 5 / ChatID 空 |
| `supplierpayment` | 13 | 0.7s | 4 cron 任务 + cube 7 case + 阈值 clamp |
| `auth`, `config`, `parser/bigmodel` | (既有) | cached | 配置 + LLM 客户端 |
| **合计** | **~120+** | ~12s | |

**测试基础设施**(3 套类似):
- `agent/tools/testhelper_test.go` + `promotionalert/testhelper_test.go` + `supplierpayment/testhelper_test.go`
- 共享模式:runtime.Caller 找 .env → 读 PG_DSN → 装 pgxpool → setup 前清 `t-%` → defer cleanup

---

## 7. 店端验证 SOP (用户必看)

```bash
# 1. 推送分支
git push -u origin feat/agentic-purchase

# 2. .env 配置 (W1-W5 全开)
cat >> .env <<EOF
COLLECTAI_AGENT_ENABLED=true
COLLECTAI_AGENT_CHAT_IDS=wrXxx
COLLECTAI_LLM_API_KEY=sk-xxx
COLLECTAI_LLM_BASE_URL=https://api.deepseek.com
COLLECTAI_LLM_MODEL=deepseek-chat
PROMOTION_ALERT_CHAT_ID=wrYyy
OWNER_CHAT_ID=wrZzz
COLLECTAI_CUBE_QUERIER=noop  # 改 real 接真实 cube
EOF

# 3. 启动 + 验证
go run ./cmd/server

# 4. 场景跑通 (按时间顺序)
# a) 群里对 Agent 说"汇一是自采,堆头他们出"
# b) H5 打开含"榄菊"的采购单 → alerts.block_entry
# c) H5 POST /api/v1/agent/chat 同 a) 验证双入口
# d) H5 GET /api/v1/payments/pending → 周一后有建议
# e) 等 21:00 → 堆头费推 office 群
# f) 等 22:00 → 现金不足推 owner 群 (如缺 cash_balance)
# g) 每月 1 号 → promotion_fee_share 入库
```

---

## 8. 后续工作清单(不在本期 PR)

| 优先级 | 任务 | 范围 |
|---|---|---|
| P1 | W2.5 H5 chat UI SSE 流式 | 前端 |
| P1 | cash_balance RPA 接入 | 影刀 / 金蝶 ODBC |
| P2 | cube 真实接入 (需 cube-agent-server 端确认 siss_saleflow / v_prom_saleflow) | main.go env 切换 |
| P2 | 合同 PDF RAG (方案 §11) | Knowledge 入库 |
| P3 | A2A 互通(方案 §11) | 跨 agent 协作 |
| P3 | 现金日报 UI | H5 表单 |
| P3 | alerts H5 UI 标红 | 前端 |

---

## 9. 关键 commit 索引

| hash | 内容 | 改动量 |
|---|---|---|
| `73da87c` | 方案设计 | 619 行 |
| `1a050e9` | W1 智能采购工具 | 1713 行 |
| `e19ad31` | W2 企微桥 | 687 行 |
| `9ed7c97` | W3.1 matcher L3 + 修老 bug | 431 行 |
| `f3ede05` | W3.2 规则引擎 | 811 行 |
| `5562b32` | W3.3 堆头费 cron | 589 行 |
| `4787fba` | W3.5 LLM 应季判定 | 577 行 |
| `1f18d1f` | W4 D 模块数据 + 4 tool | 1176 行 |
| `a114d4b` | W4.3 D 模块 cron | 1126 行 |
| `4516717` | W2.5 H5 端 HTTP 触发 | 114 行 |
| `3d228ca` | W5 cube 接入 | 418 行 |

**共计 18 commit, 36 文件, 7950 行新增, 421 删除**

---

## 10. Reviewer 速读建议 (30 分钟版)

```
0. 看本文件 (5 min)
1. cat docs/agent-purchase-plan.md | head -100 (5 min)
2. 看 cmd/server/main.go:80-200 装配部分 (5 min)
3. 看 internal/agent/wecom_bridge.go 5 层降级 (10 min)
4. 看 internal/purchasealert/rules.go + service.go 4 规则 (5 min)
```

**30 分钟拿到 80% 的架构理解,关键设计一眼可见**
