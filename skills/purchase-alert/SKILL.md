---
name: purchase-alert
description: |
  采购收货单后续推理引擎——对刚 OCR 解析完的供货单,LLM 跑 8 类规则,产出 alerts (含 category 决定前端 icon 段位)。
  8 规则: 限入场 / 不让退 / 反季 / 节假日 lead / 高库存 / 堆头陈列 / 快讯 / 未审批退货单。
  规则是"活的"——比如供应商堆头费支持大 + 不催款,LLM 可判定"高库存"规则降级 (不报警)。
  未审批退货单规则:查 supplier 是否有未审批退货单(走 cube t_rm_returnflow),有则提醒收货人将退货退出。
  Go 端零业务判断,仅提供 query/insert tool;LLM 决定跑哪些规则、怎么判定、降级不降级、总结栏怎么拼。
  Use this skill when the user mentions 采购收货单推理 / 供货单 alert / 限制商品 / 难消化 / 高库存 / 堆头陈列 / 快讯 / 节假日备货 / 应季采购 / 限入场 / 不让退 / 退货单 / 未审批退货 / 退货率, or when alert_category is needed for frontend icons.
license: Internal-Project
metadata:
  version: "1.2.0"
  author: collect-ai
  category: purchase-receipt-post-analysis
  migrated_from: "internal/purchasealert/rules.go (Phase W4.1 之前的 4 规则 + W4.1 加的 3 规则, 2026-09-03)"
  supersedes: "Go-side rule engine (W4.2 删除)"
  new_in_v1.2: "规则 8 pending_return 激活 + cube 端 supplier_returns 已接 — 2026-09-04"
  v1.1_added: "规则 8 pending_return (未审批退货单) 接口 + 工具 9 query_return_order 接口 — 2026-09-03"
compatibility: requires Go service with 9 tools (see references/query-tools.md)
triggers:
  - 采购收货单推理
  - 供货单 alert
  - 限制商品
  - 难消化
  - 高库存
  - 堆头陈列
  - 快讯
  - 节假日备货
  - 应季采购
  - 限入场
  - 不让退
  - 退货单
  - 未审批退货
  - 退货率
  - 采购策略分析
  - purchase alert
  - supplier block entry
  - flash promo detection
  - pending return
---

# Purchase Alert(采购收货单后续推理)

> **目标**:把"采购收货单解析后,跑 8 类规则产出 alert + 总结栏"这件事,从"Go 写死 if/else"升级为"LLM 调 query tool 跑规则, 可灵活降级/扩展"。
>
> **之前**(W4.1,2026-09-02 之前):
> - `internal/purchasealert/rules.go` 7 个 rule struct,每个写死 if/else
> - block_entry / no_return / offseason / holiday_lead / high_stock / has_duitou / flash_promo 全是硬规则
> - 改一条规则要改 Go + 重启 collect-ai
> - 没法处理"活规则":供应商堆头费支持大 + 不催款 → 高库存是否报警?
>
> **现在**(W4.2,2026-09-03 之后):
> - Go 端只提供 8 个 query/insert tool
> - LLM 决定跑哪些规则、怎么判定、是否降级
> - 加新规则 = 改 SKILL.md 的 references,无需改 Go
> - 改阈值 = 改 references/app-settings.md (跟 LLM 读) 或 PG 表 (跟 LLM 不读)
> - 活规则: LLM 看到 supplier_policy.has_duitou=true AND 不催款,自动判定 high_stock 降级 (warn → info 或不报)

---

## When to use this skill

**适用**: OCR 解析完一张新的采购收货单 (parse_session) 后,异步跑 8 类规则,产出 alert + 总结栏。

**不适用**:
- OCR 解析本身 (走 ocr-purchase skill)
- 库存预警 (走 restock-strategy skill)
- 供应商政策录入 (走 supplier-policy skill)
- 应季备货建议 (走 seasonal-buying skill,本 skill 输出可作为输入)
- 堆头费结算 (走 settlement-suggestion skill)

---

## How to use this skill (LLM 工作流)

### 步骤 0: 调 `run_analysis.py` 拿预聚合数据 (可选但推荐)

> **重要**: `run_script` 走的是**子进程 stdin**,必须用 `args` 字段传 JSON,不要用 `input`(那是给 `load` action 拼到 SKILL.md 末尾的)。

调 `invoke_skill` action=`run_script` path=`scripts/run_analysis.py`:

```json
{
  "skill_name": "purchase-alert",
  "action": "run_script",
  "path": "scripts/run_analysis.py",
  "args": {
    "session_id": "3051fc81-...",
    "supplier_name": "汇一",
    "rows": [{"row_id": 1, "matched_name": "...", "qty": 5, ...}]
  }
}
```

跑完会返回 4 类预查数据(supplier_policy / promotion_fee / calendar / app_settings)+ 空 candidate_alerts。
LLM 拿这个结果跑步骤 3 判定。如果不调这步直接跑也行,只是要自己调 8 个 tool 查同样的数据。

**注意**:
- `args` 字段是 JSON 对象,不是字符串
- 漏传 `args` 脚本收到空 stdin 会 `json.load` 异常 → exit 1 → 跑不动

### 输入

LLM 接收一段 JSON,描述待分析的 session:

```json
{
  "session_id": "9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d",
  "supplier_name": "汇一",
  "mode": "purchase",
  "image_hashes": ["a1b2c3..."],
  "rows": [
    {
      "row_id": 1,
      "seq": 1,
      "image_index": 0,
      "raw_barcode": "6901234567890",
      "matched_barcode": "6901234567890",
      "matched_name": "可口可乐 330ml",
      "matched_supp": "汇一",
      "qty": 5,
      "stock_qty": 47.0,
      "is_new": false
    },
    {
      "row_id": 2,
      "seq": 2,
      "matched_name": "蒙牛纯牛奶",
      "matched_supp": "汇一",
      "qty": 3,
      "stock_qty": 2.0,
      "is_new": true
    }
  ]
}
```

### 步骤 1: 加载 8 规则判定文档

读 `references/7-rules.md` 拿到 8 规则 (前 7 条 + 1 条 W4.4 新加的 pending_return):
- 每条的"必报条件"、"降级条件"、"不报条件"
- 每条的 severity / category 默认值
- 每条跟供应商政策的联动(活的规则)

### 步骤 2: 加载可用 tool 列表

读 `references/query-tools.md`,记 9 个 tool:
- `query_supplier_policy(supplier, key?)` — 查 KV
- `query_promotion_fee(supplier, now)` — 查 active promos
- `query_special_calendar(now, lead_days)` — 查接下来 N 天节假日
- `query_app_settings(key)` — 查阈值/分类配置
- `query_sku_stock(item_no)` — 查 SKU 当前库存
- `query_sku_sales(item_no, days)` — 查 SKU 30/60/90 天销量
- `query_return_order(supplier, status?, days?)` — 查供应商退货单 (W4.4 新, 数据源未接入时返 not_available=true, 规则自动降级)
- `insert_purchase_alert(session_id, row_id, rule, severity, category, message)`
- `update_analysis_status(session_id, status, error?)`

### 步骤 3: 跑每条规则

按 row 循环, 对每行调必要 tool 查数据, 决定是否报 + 报什么 category + 报什么 message。

#### 规则判定流程图

```
row 进来
  ↓
[1] 调 query_supplier_policy(supplier) → 拿到 policy 列表
  ↓
[2] block_entry=true?
  ├─ yes → 报 block (🔴 红色感叹号)  ← 必报, 无降级
  └─ no  → 继续
  ↓
[3] allow_return=false?
  ├─ yes → 报 warn (🟠 橙色感叹号)  ← 必报
  └─ no  → 继续
  ↓
[4] 反季词命中 (冰品/暖手宝/火锅/...)? 查 references/7-rules.md §反季
  ├─ 是反季 → 报 info (⚪ 灰感叹号)  ← 必报
  └─ 否 → 继续
  ↓
[5] 查 stock_qty → > high_stock_threshold?
  ├─ yes → 查 [6] 看是否降级
  │     ↓
  │     6a. has_duitou=true AND 当前在期 → 降级: 改 info (堆头期允许压库存)
  │     6b. 供应商累计促销费 > 10000 AND 不催款 → 降级: 不报
  │     6c. 其他 → 报 warn (🟠 橙色感叹号) (必报)
  └─ no  → 继续
  ↓
[7] 查 promotion_fee(kind in others_kinds)?
  ├─ yes → 报 highlight_others (🟢 绿色"其它"标志)
  └─ no  → 继续
  ↓
[8] (此行没命中任何 rule-specific alert, 跳过)
```

#### 总结栏 (session 级, row_id=0)

- holiday_lead: 调 query_special_calendar(now, 90), 找 lead_days 窗口内的节假日, 报 info
- has_duitou: 遍历 session 内所有 supplier, 命中 has_duitou=true AND 当前 promotion_fee 在期, 报 highlight_dui (合并多条为 1 条)
- block_entry (session 级汇总): 任何 supplier 被 block_entry, 整单 1 条 info 提示
- **pending_return (W4.4 新, 规则 8)**: 对 session 内每个 supplier 调 `query_return_order(supplier, status="pending", days=30)`,如果有 ≥1 单未审批退货,报 1 条 warn (整 supplier 1 条,不按 row 报)。`not_available=true` (cube 数据源未接入) → **规则降级, 不报**。详见 `references/7-rules.md` §pending_return

### 步骤 4: 输出 alerts 数组

LLM 把所有命中的 alert 收集成数组, 调 `insert_purchase_alert` 多次落库 (或单次批量)。

每条 alert 形如:
```json
{
  "session_id": "9b1deb4d-...",
  "row_id": 1,
  "rule": "high_stock",
  "severity": "warn",
  "category": "warn",
  "message": "商品 [可口可乐 330ml] 当前库存 47,超过阈值 50,本次采购需谨慎(可能压库存)。注:供应商 [汇一] 当前签了堆头 ¥5000/月,允许阶段性压库存,降为提示。"
}
```

### 步骤 5: 更新 session 状态

调 `update_analysis_status(session_id, 'done')`。

如果中途出错 (tool 调失败、PG 写不进),调 `update_analysis_status(session_id, 'failed', error_msg)`,LLM 退出。

---

## 输出契约 (Go 端期望)

LLM 必须输出一个 alerts 数组 (JSON),每个元素含 6 个必填字段:
- `session_id` (从输入取)
- `row_id` (0 = session 级, >0 = 行内)
- `rule` (block_entry / no_return / offseason / holiday_lead / high_stock / has_duitou / flash_promo)
- `severity` (block / warn / info)
- `category` (block / warn / info / highlight_dui / highlight_others)
- `message` (中文, 1-2 句, 含商品名 + 数量 + 关键阈值/政策)

Go 端会用 `insert_purchase_alert` tool 落库,然后用 `update_analysis_status` 收尾。

---

## 关键的"活规则"示例

### 案例 1: 供应商堆头期 + 高库存 → 降级

场景: 汇一当前签了堆头 ¥5000/月(10-15 到期),采购可口可乐 47 件,阈值 50。

- 严格执行 8 规则 → 报 high_stock warn
- **降级**: 查 promotion_fee has_duitou=true + 在期 → 改报 high_stock info (堆头期允许)
- **message 备注**: "供应商 [汇一] 当前签了堆头 ¥5000/月(至 10-15),允许阶段性压库存,降为提示。"

### 案例 2: 供应商累计促销费大 + 不催款 + 高库存 → 不报

场景: 汇一近 3 月累计促销费 ¥30000,从不催款,采购可口可乐 100 件,阈值 50。

- 严格执行 8 规则 → 报 high_stock warn
- **降级**: 调 query_supplier_payment / query_promotion_fee 累计 + 不催款判定 → 完全不报
- **记录但不显示**: 仍然记 1 条 internal note (severity=info,category=info) 给运营看

### 案例 3: 同一商品多供应商比价

场景: 同时采购可口可乐 5 件 (汇一) + 5 件 (康师傅直采),汇一 ¥2.5,康师傅 ¥2.3。

- 不在 8 规则内 → **不报**(超 scope)
- 但是这是反回扣信号 → 触发 supplier-policy skill 的反回扣判定(另一路径)
- 本 skill **只**负责采购单内的 alert, 不做反回扣

---

## 性能预算

- 一次 session 跑 8 规则: ~8-15 tool calls (查一次 supplier_policy 可复用给所有 row)
- 单次 tool 调用: 100-500ms (PG) / 1-2s (cube)
- LLM 推理: ~5-10s
- **总预算**: < 30s,跟 W4.1 异步超时一致

如果某次 tool 调超时, 跳过该规则不报(不阻断其他规则),但要记 1 条 internal alert (severity=info, message="规则 X 跑超时, 请人工 review")。

---

## 加新规则的流程 (本 skill 自我扩展)

1. 在 `references/7-rules.md` 加一节
2. 在本 SKILL.md 的"步骤 3"判定流程图加分支
3. 如需新 tool,在 `references/query-tools.md` 加,并在 Go 端 `internal/agent/tools/purchase_alert.go` 实现
4. 跑单测验证: 加新 case 到 `rules_test.go` (Go 端会保留 1 个 LlmRuleStub rule 来测调度)
5. 提交, skill 系统 fsnotify 200ms 内 reload,无需重启 collect-ai

---

## 不要做

- ❌ 在 Go 端写业务判断 (AGENTS.md §4 红线)
- ❌ 把 8 规则写死成 enum (用 references 文档,LLM 读)
- ❌ 在 SKILL.md 里写具体供应商名 (汇一/榄菊 → 用 {supplier} 占位)
- ❌ 在 SKILL.md 里写具体商品名 (可口可乐 → 用 {item_name} 占位)
- ❌ 跨 session 复用 alert 决策 (每个 session 独立跑,即使同一 supplier)

---

## 相关文档

- `references/7-rules.md` — 8 规则的判定 + 决策表 + 活的规则
- `references/query-tools.md` — 9 个 tool 的调用格式
- `references/app-settings.md` — 阈值/分类配置 (LLM 也读 PG 表)
- `references/icon-mapping.md` — category → 前端 icon 段位
- `references/examples.md` — 真实案例 (汇一/榄菊/...)
- `docs/w4.1-purchase-receipt-frontend-contract.md` — 前端契约
- `AGENTS.md` §1 / §4 — skill 外置红线
