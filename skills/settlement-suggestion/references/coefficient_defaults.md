# 三维度算法默认系数(2026-Q3 版)

> **作用**:LLM 在调用 `suggest_supplier_payment` 之前,先看这张表,决定要不要**调整** tool 算出来的系数。
>
> **不要**直接覆盖 tool 返的数 — 调整要写回"basis"给老板看到。

## 三维度系数定义

`amount = base_forecast × investment_weight × promo_weight × sellthrough_weight`

| 系数 | 含义 | 默认值 | 区间 | 数据来源 |
|---|---|---|---|---|
| `investment_weight` | 供应商投入度(堆头/端架/月费) | 1.0 | 0.8 ~ 1.5 | `promotion_fee_share / base_forecast` 比例 → 线性映射 |
| `promo_weight` | 产品促销力度(档期/折扣) | 1.0 | 0.9 ~ 1.3 | W5 升级:从 cube 拉 `v_prom_saleflow` 占比 |
| `sellthrough_weight` | 产品动销率(周转) | 1.0 | 0.7 ~ 1.2 | W5 升级:从 cube 拉近 30 天 sellthrough |

## investment_weight 详细映射(可改)

```python
# scripts/assess_investment.py 用同一张表
ratio = month_share / month_forecast  # 0% ~ 10%
if ratio < 0.5%:
    inv_weight = 0.8   # 基本不投入
elif ratio < 2%:
    inv_weight = 0.95
elif ratio < 5%:
    inv_weight = 1.0
elif ratio < 10%:
    inv_weight = 1.2
else:
    inv_weight = 1.5  # 投入>10%,可能过度依赖供应商
```

| 比例 | 系数 | 含义 |
|---|---|---|
| < 0.5% | 0.8 | 这家供应商基本不投,给的建议金额应该**少** |
| 0.5% - 2% | 0.95 | 正常合作 |
| 2% - 5% | 1.0 | 平均水平 |
| 5% - 10% | 1.2 | 重投入,建议金额上调 |
| > 10% | 1.5 | 严重依赖,风险高,**建议老板重新谈判** |

## promo_weight(暂固定 1.0)

W5 升级前没有 cube 促销数据,LLM 不应该调。如果老板话里明确说"这家产品在做买一送一",LLM 可以设 1.1-1.2 并在 basis 里说明。

## sellthrough_weight(暂固定 1.0)

同上。W5 升级后改成从 `t_rm_saleflow` 算近 30 天动销率 vs 品类均值。

## 编辑权限

老板和采购经理可改:
- 区间上下限(0.8 ~ 1.5 → 改成 0.7 ~ 1.7)
- 比例阈值(< 5% → 改成 < 8%)
- 临时把某 supplier 的 investment_weight 固定到 1.3(老板的话优先)

改完本文件,LLM 在下一轮对话里就能看到新值(热更新)。

## 更新历史

- 2026-09-02 v1.0 初版,从 internal/agent/tools/payment.go 迁出
