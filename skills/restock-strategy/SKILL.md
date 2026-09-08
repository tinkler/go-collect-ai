---
name: restock-strategy
description: 补货决策辅助——给 restock_task(库存预警)判断紧急度(P0/P1/P2/P3 含义)、按品类给出建议备货周期(食品 7 天/日化 14 天/季节性 3 天)、根据供应商 fill_rate 调整补货量、解释"为什么这条要发"给员工听。Use this skill when the user asks about 补货/备货/库存预警/紧急补货/补货周期/补货量/供应商交付率/fill_rate/补货优先级/要不要发/补货窗口, or when the user says "这个要发吗" / "P0 是什么" / "为什么是 P1" / "这个供应商靠谱吗" / "补多少合适" / "今天不急吧".
license: Internal-Project
metadata:
  version: "1.0.0"
  author: collect-ai
  category: restock
  migrated_from: "internal/restock/service.go (旧 computePriority/ShouldEscalate 在 2026-09-02 重构时移除,本 skill 接管其语义解释层)"
compatibility: requires Python 3.x
triggers:
  - 补货
  - 备货
  - 库存预警
  - 紧急补货
  - 补货周期
  - 补货量
  - 供应商交付率
  - fill_rate
  - 补货优先级
  - 备货窗口
  - 库存
  - 要不要发
  - 补多少
---

# Restock Strategy(补货决策辅助)

> **目标**:把"看到 restock_task 时怎么判断 + 怎么跟员工解释"这件事,从硬编码的 cron 逻辑 + ETL,**升级成 LLM + 行业经验表 + 现有 restock 工具协同**。
>
> **之前**:`internal/restock/service.go` 旧 `computePriority` / `ShouldEscalate` 在 2026-09-02 重构时**已移除**。现在 restock 是"3 次 cron 从 cube 拉数据 → 写 display_suggest"的纯 ETL。**真正需要"判定"的部分(紧急度、给员工解释、补货量)目前是真空**。
>
> **现在**:本 skill 接管这些 LLM 推理任务。

## When to use this skill

适用:

1. 员工/老板看到 `restock_task` 字段,问"这个要不要发" / "P0 是啥意思"
2. 老板问"补货量算得对不对" / "为什么这个 SKU 备这么多"
3. 老板问"这家供应商 fill_rate 多少" / "补货量要调整吗"
4. 老板问"补货周期多久合适" / "食品要不要每周补"
5. 系统检测到 priority 自动升级(P2→P1→P0),LLM 解释为什么

不适用:

- 单纯 ETL(cron 拉 cube → 写 PG,LLM 不介入)
- 补货量硬算(由 `display_suggest` 字段直接给,不需 LLM)
- 应季备货(走 `seasonal-buying` skill)

## How to use this skill(LLM 工作流)

### 步骤 0:加载行业经验

调 `invoke_skill` action=`read_file`:
- `references/priority_semantics.md` — P0/P1/P2/P3 的精确含义
- `references/category_lead_days.md` — 各品类的默认补货周期

### 步骤 1:理解 restock_task

每个 restock_task 字段含义:

| 字段 | 含义 |
|---|---|
| `priority` | P0(立即)/ P1(2h 内)/ P2(今日)/ P3(预防) |
| `inv_snapshot` | 当前库存 |
| `daily_avg` | 加权日均(昨日×0.4 + 7日均×0.4 + 30日均×0.2) |
| `rop` | Reorder Point = max(daily_avg × 1.5, 5) |
| `suggest_qty` | LLM 建议补货量(可能含 fill_rate 调整) |
| `supplier_id` / `supplier_name` | 供应商 |
| `branch` / `item_no` | 门店 + SKU |

### 步骤 2:评估"要不要现在发"

```
触发: stock ≤ ROP
优先级: 库存可支撑天数 = inv_snapshot / daily_avg
  < 0.5 天 → P0(立即)
  < 1.5 天 → P1(2h 内)
  < 3   天 → P2(今日)
  ≥ 3   天 → P3(预防)
```

**升级**(静默):
- P2 24h 未响应 → 自动升 P1
- P1 12h 未响应 → 自动升 P0

### 步骤 3:评估"补多少"

公式(本 skill 推荐,**可改**):

```
base_qty = daily_avg × lead_days + safety_stock
fill_rate_adj = base_qty / supplier.fill_rate
final_qty = round(fill_rate_adj, ceil_unit)
```

其中:
- `lead_days` 从 `category_lead_days.md` 取(食品 7 / 日化 14 / 季节性 3)
- `safety_stock` = `daily_avg × 1.5`
- `supplier.fill_rate` 从 `supplier_reliability` 表(0~1,默认 0.95)

调 `scripts/compute_suggest_qty.py` 一键算。

### 步骤 4:给员工解释(单条 ≤ 200 字)

```
"汇一瓶装水 500ml 缺货预警:
 当前库存 12 瓶,日均卖 30,只剩 0.4 天
 优先级 P0(立即处理,不能再等)
 建议补 60 瓶(7 天量 + 安全库存,按供应商 95% 交付率)
 供应商: 汇一,账期 30 天"
```

要素:
- 当前库存 + 日均 + 可支撑天数 → 让员工**理解**为什么紧急
- 优先级标签 + 建议补货量 → 行动指引
- 供应商 + 账期 → 让员工填单

## Scripts

### `scripts/compute_suggest_qty.py`

**作用**:根据 daily_avg / lead_days / safety_days / fill_rate 算 final 补货量。

**入参(stdin JSON)**:

```json
{
  "daily_avg": 30.0,
  "lead_days": 7,
  "safety_days": 1.5,
  "fill_rate": 0.95,
  "ceil_unit": 12
}
```

**出参(stdout JSON)**:

```json
{
  "base_qty": 255.0,
  "fill_rate_adjusted": 268.42,
  "final_qty": 276,
  "rationale": "30×7 + 30×1.5 = 255,按 95% 交付率上调到 268.42,向上取 12 的倍数到 276"
}
```

### `scripts/days_until_stockout.py`

**作用**:算"按当前 daily_avg,库存还能撑几天"。

**入参(stdin JSON)**: `{"inv_snapshot": 12, "daily_avg": 30.0}`

**出参(stdout JSON)**:

```json
{ "days_until_stockout": 0.4, "priority": "P0" }
```

### `scripts/supplier_fill_rate.py`

**作用**:算某 supplier 近 30/60/90 天的 fill_rate(基于 parse_session)。

**入参(stdin JSON)**:

```json
{
  "delivered_qty": 950,
  "ordered_qty": 1000,
  "window_days": 30
}
```

**出参(stdout JSON)**:

```json
{ "fill_rate": 0.95, "tier": "reliable", "adjustment_factor": 1.053 }
```

## References

- `references/priority_semantics.md` — P0/P1/P2/P3 精确定义 + 升级规则
- `references/category_lead_days.md` — 食品/日化/季节性 各类目补货周期
- `references/escalation_rules.md` — 静默升级逻辑 + 何时不升级
- `references/restock_task_schema.md` — restock_task 字段完整说明

## Common Patterns

### 模式 A:员工问"这个 P0 是什么意思"

```
用户: P0 是什么
LLM:
  1. 调 invoke_skill(action=load, skill_name=restock-strategy, input=...)
  2. 读 references/priority_semantics.md
  3. 告诉员工: P0 = 立即处理(库存撑不到半天),P1 = 2 小时内,P2 = 今天,P3 = 预防
```

### 模式 B:员工问"这个补货量对吗"

```
用户: 汇一瓶装水建议补 60 瓶,够吗
LLM:
  1. 调 query_upcoming_dates / 拿 SKU 的 inv_snapshot + daily_avg
     (可能需要 cube 数据,不在 skill 内,需要 W2 cube tool)
  2. 调 compute_suggest_qty(...)
  3. 告诉员工: 按日均 30 + 7 天 lead + 95% 交付率,建议 276 瓶,60 偏少
```

### 模式 C:老板问"为什么这条 P1 升到 P0 了"

```
用户: 这条为啥自动升 P0 了
LLM:
  1. 查 restock_task 的 history(从 restock_task.status_history 字段)
  2. 读 references/escalation_rules.md
  3. 告诉老板: P1 12 小时没人处理,系统自动升 P0(避免真的断货)
```

## Guidelines

- **优先级解释优先**:员工对 P0/P1/P2/P3 经常困惑,要主动解释
- **补货量有依据**:不要瞎调,要么用默认公式,要么老板明确说"多备点"
- **fill_rate 是经验值**:新供应商用默认 0.95,跑 30 天后用真实数据
- **静默升级不等于忽略**:升级触发后,LLM 应该**主动**告诉员工/老板,而不是只改字段

## Keywords

补货, 备货, 库存预警, 紧急补货, 补货周期, 补货量, 供应商交付率, fill_rate, 优先级, P0, P1, P2, P3, 备货窗口, 静默升级, 库存, ROP, 触发, 预警, 缺货, 补货单
