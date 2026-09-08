# 真实案例(汇一/榄菊/...)

> 配合 SKILL.md + 7-rules.md 使用。给 LLM 跑规则时参考真实业务场景。

---

## 案例 1: 汇一 9 月初采购,堆头期 + 高库存 (降级 A)

### 输入
```json
{
  "session_id": "sess-001",
  "supplier_name": "汇一",
  "rows": [
    {
      "row_id": 1, "matched_name": "可口可乐 330ml", "matched_supp": "汇一",
      "qty": 5, "stock_qty": 47.0
    },
    {
      "row_id": 2, "matched_name": "雪碧 330ml", "matched_supp": "汇一",
      "qty": 3, "stock_qty": 12.0
    }
  ]
}
```

### 期望输出 alerts
```json
[
  {
    "rule": "high_stock", "severity": "info", "category": "info",
    "row_id": 1,
    "message": "商品 [可口可乐 330ml] 库存 47,接近阈值 50。但供应商 [汇一] 当前签了堆头 ¥5000/月(至 09-30),允许阶段性压库存,降为提示。"
  },
  {
    "rule": "has_duitou", "severity": "info", "category": "highlight_dui",
    "row_id": 0,
    "message": "本期堆头陈列: [汇一] 堆头 ¥5000/月(至 09-30)"
  }
]
```

### LLM 推理步骤
1. 调 `query_supplier_policy("汇一")` → 返 `{has_duitou: true, is_self_procure: true}`
2. 调 `query_promotion_fee("汇一", now)` → 返 1 条 kind="堆头" amount=5000 在 09-01~09-30
3. 调 `query_app_settings("high_stock_threshold")` → 返 50
4. row 1: stock_qty=47, 接近阈值 50, **降级 A 命中** → 改 info
5. row 2: stock_qty=12 < 50 → 不报
6. 总结栏: has_duitou + 堆头在期 → 报 highlight_dui
7. 共 2 条 alert

---

## 案例 2: 榄菊 不让退 + block (同时两个硬规则)

### 输入
```json
{
  "session_id": "sess-002",
  "supplier_name": "榄菊",
  "rows": [
    { "row_id": 1, "matched_name": "蚊香", "matched_supp": "榄菊", "qty": 10, "stock_qty": 30.0 }
  ]
}
```

### 期望输出
```json
[
  {
    "rule": "block_entry", "severity": "block", "category": "block",
    "row_id": 1,
    "message": "供应商 [榄菊] 已被限制入场(block_entry=true),本单据不审请勿入库"
  },
  {
    "rule": "no_return", "severity": "warn", "category": "warn",
    "row_id": 1,
    "message": "供应商 [榄菊] 不支持退货(allow_return=false),请确认本次采购数量 10"
  }
]
```

### LLM 推理
- 2 个硬规则同时命中,**都报**(不只报最高)
- 同一行 row_id=1,前端会显示 2 个 icon (🔴 + 🟠)

---

## 案例 3: 反季 + 中秋前 30 天 (降级)

### 输入
```json
{
  "session_id": "sess-003",
  "supplier_name": "稻香村",
  "rows": [
    { "row_id": 1, "matched_name": "稻香村月饼礼盒", "matched_supp": "稻香村", "qty": 50 }
  ]
}
```

### 期望输出
```json
[
  {
    "rule": "offseason", "severity": "info", "category": "info",
    "row_id": 1,
    "message": "商品 [稻香村月饼礼盒] 含应季词 [月饼],但当前 [秋] 季节性命中,降为提示 (距中秋 25 天,在 lead 窗口内)。"
  },
  {
    "rule": "holiday_lead", "severity": "info", "category": "info",
    "row_id": 0,
    "message": "距 [中秋节] 还有 25 天(lead_days=7),建议提前备货"
  }
]
```

### LLM 推理
- "月饼" 应季 autumn,但当前是 9 月(autumn),**季节匹配**
- 节假日 lead 窗口内 → offseason 降级为"提示"而非"反季"
- 同时 holiday_lead 报 1 条 session 级

---

## 案例 4: 汇一 累计促销费 ¥30000 + 不催款 + 高库存 (降级 B,不报)

### 输入
```json
{
  "session_id": "sess-004",
  "supplier_name": "汇一",
  "rows": [
    { "row_id": 1, "matched_name": "加多宝 310ml", "matched_supp": "汇一", "qty": 100, "stock_qty": 80.0 }
  ]
}
```

### 期望输出
```json
[
  {
    "rule": "high_stock_internal", "severity": "info", "category": "info",
    "row_id": 1,
    "message": "[internal] 商品 [加多宝 310ml] 库存 80 超阈值 50,但供应商 [汇一] 近 90 天累计促销费 ¥30000 且不催款,跳过 high_stock 报警(注:不影响前端展示)。"
  }
]
```

### LLM 推理
1. stock_qty=80 > 50 → 触发 high_stock
2. 降级 A (堆头期) 未命中 (has_duitou=false 或 无 active promo)
3. 降级 B (累计促销费 + 不催款) 命中:
   - `query_supplier_payment("汇一", days=90)` (W4.2 实现) 返 ¥30000, urge_status="not_urge"
4. **不报外部 alert**, 只记 1 条 internal note

---

## 案例 5: 新 SKU (is_new=true) + 没有任何命中

### 输入
```json
{
  "session_id": "sess-005",
  "supplier_name": "新供应商",
  "rows": [
    { "row_id": 1, "matched_name": "新品A", "matched_supp": "新供应商", "qty": 1, "is_new": true, "stock_qty": 0 }
  ]
}
```

### 期望输出
```json
[]
```

### LLM 推理
- 新供应商,无 supplier_policy → block_entry/no_return 不报
- 新品A 不含应季词 → offseason 不报
- stock_qty=0 < 50 → high_stock 不报
- 无 promotion_fee → has_duitou/flash_promo 不报
- 90 天内无节假日 → holiday_lead 不报
- 0 条 alert,前端的 alerts/summary 都是空数组

---

## 案例 6: 同一 session 多 supplier 总结栏

### 输入
```json
{
  "session_id": "sess-006",
  "rows": [
    { "row_id": 1, "matched_supp": "汇一", "matched_name": "可口可乐", "qty": 5, "stock_qty": 47 },
    { "row_id": 2, "matched_supp": "康师傅", "matched_name": "康师傅红烧牛肉面", "qty": 10, "stock_qty": 5 }
  ]
}
```

汇一: has_duitou=true, 当前有堆头 ¥5000 (9-1~9-30)
康师傅: 无 has_duitou, 但 kind=快讯 in-期 (9-3~9-10)

### 期望输出
```json
[
  {
    "rule": "has_duitou", "severity": "info", "category": "highlight_dui",
    "row_id": 0,
    "message": "本期堆头陈列: [汇一] 堆头 ¥5000/月(至 09-30)"
  },
  {
    "rule": "flash_promo", "severity": "info", "category": "highlight_others",
    "row_id": 2,
    "message": "商品 [康师傅红烧牛肉面] 供应商 [康师傅] 正在做 快讯,注意陈列位置"
  }
]
```

### LLM 推理
- 总结栏: 汇一命中 → 1 条
- 行内: 康师傅 命中 flash_promo → row_id=2 报 1 条
- 汇一的 row 1 不报 (高库存接近但没超, 且有堆头降级, info)
- 共 2 条 alert

---

## 案例 7: block_entry 永不降级 (即使堆头期 + 大额促销)

### 输入
```json
{
  "session_id": "sess-007",
  "rows": [
    { "row_id": 1, "matched_supp": "问题供应商", "matched_name": "x", "qty": 1 }
  ]
}
```

问题供应商: block_entry=true, 但 has_duitou=true, 累计促销费 ¥50000

### 期望输出
```json
[
  {
    "rule": "block_entry", "severity": "block", "category": "block",
    "row_id": 1,
    "message": "供应商 [问题供应商] 已被限制入场(block_entry=true),本单据不审请勿入库"
  }
]
```

### LLM 推理
- block_entry 是硬阻断,**永不降级**(参考 7-rules.md 规则 1)
- 即使该 supplier 给了 ¥50000 促销费,也不能解除限入场
- 想要解除,先去 supplier-policy skill 走"撤销"流程
