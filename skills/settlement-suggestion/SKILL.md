---
name: settlement-suggestion
description: 供应商结算建议——堆头费/端架费等促销费在账期内的分摊比例计算、付款建议金额(三维度算法:供应商投入×产品促销×产品动销)、促销费到期预警。Use this skill when the user asks about 堆头费/端架费/促销费分摊/供应商结算/付款建议/账期/对账/分摊比例/续约/促销费到期/支付建议, or when the user says "汇一这个月结多少" / "618 堆头费分摊到哪些 SKU" / "这个促销费下月到期提醒我" / "提单金额建议多少".
license: Internal-Project
metadata:
  version: "1.0.0"
  author: collect-ai
  category: supplier-finance
  migrated_from: "internal/agent/tools/payment.go (compute_promotion_fee_share / upcoming_promotion_expiry / forecast_purchase_amount / suggest_supplier_payment)"
compatibility: requires Python 3.x
triggers:
  - 堆头费
  - 端架费
  - 促销费
  - 供应商结算
  - 付款建议
  - 账期
  - 对账
  - 分摊
  - 续约
  - 促销费到期
  - 支付建议
  - 提单金额
  - 结算单
  - settlement
---

# Settlement Suggestion(供应商结算建议)

> **目标**:把"促销费分摊 + 付款建议 + 到期预警"这件事,从硬编码的"白名单 + 公式",升级成 LLM + 行业经验表 + 现有 4 个 tool 协同的**推理流程**。
>
> **之前**:三维度算法 `amount = base × investment × promo × sellthru` 全部硬编码在 `payment.go:suggest_supplier_payment`。系数区间(0.8~1.5/0.9~1.3/0.7~1.2)固定,改一次要重新 build。
>
> **现在**:行业经验表(默认系数)放 references/,LLM 根据老板的"这家供应商重要 / 这批货卖得快 / 这个档期动销好"等微调,最终调 4 个 tool 拿数据 + 写建议。

## When to use this skill

适用:

1. 老板问"这个月要结多少" / "提单金额建议多少"
2. 老板问"618 堆头费要分摊到哪些 SKU" / "对账"
3. 老板问"哪些促销费下月到期" / "续约"
4. 老板问"这家供应商值不值得这么大投入" / "投入产出"
5. 老板问"供应商账期能不能拉到 60 天"

不适用:

- 单纯进销存 CRUD(走 `record_promotion_fee` / `list_promotion_fee` tool)
- 询问历史某次结算(走 PG 查询,不调本 skill)

## How to use this skill(LLM 工作流)

### 步骤 0:加载行业经验

调 `invoke_skill` action=`read_file` path=`references/coefficient_defaults.md`。
拿到默认系数表(`investment_weight` / `promo_weight` / `sellthrough_weight` 的中位值和上下限)。

如果老板话里提到特殊场景(战略供应商、新品推广、清仓),用 `references/scenario_adjustments.md` 里的调整表覆盖默认。

### 步骤 1:判定"老板想算哪种"

| 老板的话 | 走哪个 tool |
|---|---|
| "这个月要结多少" / "提单金额建议" | `suggest_supplier_payment` |
| "堆头费分摊到哪些 SKU" / "对账" | `compute_promotion_fee_share` |
| "促销费下月到期" / "续约" | `upcoming_promotion_expiry` |
| "未来 30 天采购额" | `forecast_purchase_amount` |

如果老板说"全算一下" / "对账 + 建议金额",4 个 tool 都调一遍。

### 步骤 2:拿基础数据

按上表调对应 tool。注意:

- `compute_promotion_fee_share` 必填 `month`(`YYYY-MM`),默认当月
- `upcoming_promotion_expiry` 必填 `days_ahead`(建议 30/60/90)
- `forecast_purchase_amount` 必填 `days`(7/30/90)
- `suggest_supplier_payment` 必填 `supplier` + `period_days`(7/15/30/60)

如果 tool 报错,直接告诉老板"查不到,先看是不是没记账",**不要**自己硬算。

### 步骤 3:解释给老板(单条 ≤ 200 字)

拿到数据后,用以下结构回话:

```
汇一 (supplier):
  建议金额: ¥12345 (base ¥10000 × 投入 1.2 × 促销 1.0 × 动销 1.03)
  账期: 30 天,buffer 1.5
  当前投入: 堆头 ¥2000/月,端架 ¥0
  下月到期: 9/25 端架费 ¥500 (剩 23 天)
```

要素:
- 必须有"建议金额"或"分摊结果",**不能只说"算好了"**
- 系数要展开(base × 投入 × 促销 × 动销),让老板看到"为什么是这个数"
- 关联预警(下月到期 / 异常高)放最后

### 步骤 4:落库(可选)

如果老板确认(要写建议单 / 要发对账单):

- 当前 `suggest_supplier_payment` 是 dry_run only(`Action: "dry_run"`)
- 写对账单 → 调业务侧"对账单" service(走 HTTP,不调本 skill)
- 写建议备注 → 调 `note` 字段进 supplier_policy(走 `remember_supplier_policy`)

## Scripts

### `scripts/calc_share.py`(纯计算,无 IO)

**作用**:给定促销费的 `amount / period_start / period_end` 和目标 `month`,算当月分摊。

**入参(stdin JSON)**:

```json
{
  "amount": 5000.0,
  "period_start": "2026-01-15",
  "period_end": "2026-03-15",
  "month": "2026-02"
}
```

**出参(stdout JSON)**:

```json
{
  "overlap_days": 28,
  "total_days": 60,
  "month_share": 2333.33
}
```

**逻辑**:1+月份天数,overlap = [max(ps, monthStart), min(pe+1, monthEnd))。`month_share = amount * overlap / total`。

> **何时调**:tool `compute_promotion_fee_share` 不可用(比如 PG 挂了),或老板要"先粗算一下"。

### `scripts/assess_investment.py`(评估供应商投入比例)

**作用**:把"这个月分摊的促销费 / 这个月预测采购额"映射到 `investment_weight`(0.8 ~ 1.5)。

**入参(stdin JSON)**:

```json
{ "month_share": 1500.0, "month_forecast": 10000.0, "supplier_tier": "strategic" }
```

**出参(stdout JSON)**:

```json
{
  "ratio": 0.15,
  "investment_weight": 1.025,
  "rationale": "ratio 15% 在 0.8~1.5 区间内,中位映射到 1.025"
}
```

> **何时调**:tool `suggest_supplier_payment` 不可用,或 LLM 想先"干跑"看看。

## References

- `references/coefficient_defaults.md` — 三维度系数默认值 + 区间
- `references/scenario_adjustments.md` — 战略/新品/清仓场景的系数覆盖
- `references/fee_kind_taxonomy.md` — 5 种促销费 kind 各自语义
- `references/payment_cycle_guide.md` — 常见账期(7/15/30/60)+ buffer 怎么选

## Common Patterns

### 模式 A:老板问"汇一这个月要结多少"

```
用户: 汇一这个月要结多少
LLM:
  1. 调 invoke_skill(action=load, skill_name=settlement-suggestion, input=...)
  2. 调 suggest_supplier_payment(supplier="汇一", period_days=30)
  3. 读 references/coefficient_defaults.md 看一下默认系数
  4. 用 tool 返的 amount 直接告诉老板
  5. (可选)调 upcoming_promotion_expiry(supplier="汇一", days_ahead=30) 补充到期预警
  6. 回答: 汇一 30 天建议 ¥12345,投入 1.2,促销 1.0,动销 1.03。下月 9/25 端架 ¥500 到期,剩 23 天。
```

### 模式 B:老板问"618 堆头费分摊到哪些商品"

```
用户: 618 堆头费怎么分摊
LLM:
  1. 调 invoke_skill(action=load, skill_name=settlement-suggestion, input=...)
  2. 调 compute_promotion_fee_share(supplier=..., month="2026-06")
     (老板没说 supplier → 问清楚再调,或者按 kind=堆头 过滤所有 supplier)
  3. 把 items 展开,告诉老板每笔的 month_share
```

### 模式 C:批量预警"哪些促销费下月到期"

```
用户: 下月哪些促销费到期
LLM:
  1. 调 upcoming_promotion_expiry(days_ahead=30)
  2. 按 supplier 聚合,给老板一段汇总 + 每条 days_left
```

## Guidelines

- **系数有依据**:不要瞎调,要么用默认(读 references),要么老板明确说"调高到 1.3"
- **写库二次确认**:任何写对账单 / 写建议单 / 调 supplier_policy 写 note 之前,先告诉老板你要写什么
- **过期预警分两级**:≤7 天 立即推;8-30 天 进"周一看"清单
- **场景优先级**:老板的具体要求 > 默认系数;老板说"按你说的来" > 默认系数 + 场景调整

## Keywords

堆头费, 端架费, 促销费, 堆头, 端架, 进场费, 节庆费, DM 费, 海报费, 分摊, 对账, 结算, 账期, 提单, 建议金额, settlement, share, 付款, 续约, 投入产出
