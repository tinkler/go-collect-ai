# 促销费 Kind 分类词典(2026-Q3)

> **作用**:LLM 解释 / 录入促销费时,需要这张表做依据。
> `record_promotion_fee` / `list_promotion_fee` 工具的 `kind` 字段必须从白名单选。

## 白名单(共 5 种)

| kind | 语义 | 典型金额 | 是否计入 supplier_investment |
|---|---|---|---|
| `堆头` | 货架端头陈列费,按月收 | ¥1000 ~ ¥10000/月 | ✅ 是 |
| `端架` | 货架侧端陈列费,比堆头便宜 | ¥500 ~ ¥5000/月 | ✅ 是 |
| `进场费` | 新品首次进店一次性收费 | ¥500 ~ ¥5000/次 | ❌ 否(一次性) |
| `dm` | DM 海报 / 宣传单张 | ¥200 ~ ¥2000/期 | ⚠️ 视情况 |
| `其它` | 店主与供应商临时约定的费用 | 不定 | ⚠️ 视情况 |

## 关键判定规则

- **重复识别**:老板说"端架费" → kind=`端架`;说"端架陈列费" → `端架`;说"货架侧" → `端架`;说"货架端头" → `堆头`
- **DM 费**:如果是长期(每月都有)→ 计入 supplier_investment;如果是单次活动 → 不计
- **其它** 默认不计入 supplier_investment,但 LLM 看到金额异常大(> 月供 20%)应该问老板一句"这比是单次还是长期"

## 与 investment_weight 的关系

```
monthly_investment = sum(month_share for kind in [堆头, 端架, dm(长期), 其它(长期)])
ratio = monthly_investment / base_forecast
investment_weight = lookup(ratio)  # 见 coefficient_defaults.md
```

## 编辑权限

老板和采购经理可加新 kind(比如"促销员费"、"陈列架押金")。改完 SKILL.md 即可(热更新)。

## 更新历史

- 2026-09-02 v1.0 初版,从 internal/agent/tools/fee.go 迁出
