# "取消"短语识别模式 (Promo Harvester)

> 配合 SKILL.md 使用。LLM 听群消息时按本表识别 7 种"取消"意图,调 `cancel_promotion_fee` 撤销。

---

## 7 种取消短语模式

### 模式 1: 显式 "X 堆头取消"

老板: "汇一堆头取消"
- 解析: supplier=汇一, kind=堆头, action=cancel
- 调 `cancel_promotion_fee(supplier="汇一", kind="堆头", period_end=today)`

老板: "汇一堆头下架"
- 同上 (下架=取消)

老板: "汇一那个堆头不做了"
- 同上

### 模式 2: 端架撤了

老板: "汇一端架撤了"
- 解析: supplier=汇一, kind=端架, action=cancel
- 调 `cancel_promotion_fee(supplier="汇一", kind="端架", period_end=today)`

老板: "X 端架下架"
- 同上

老板: "X 端架到期撤了"
- 识别: "到期撤了" 通常表示 period_end 已到,不需要 cancel
- 处理: 调 `query_promotion_fee` 看是否还有 active 的端架,有则 cancel,无则告诉老板已无

### 模式 3: DM / 海报 没了

老板: "汇一 DM 没了"
- 解析: supplier=汇一, kind=DM, action=cancel
- 调 `cancel_promotion_fee(supplier="汇一", kind="DM", period_end=today)`

老板: "X 海报撤了"
- kind=海报, action=cancel

老板: "X 9 月 DM 没了"
- 解析: kind=DM 但 period 限 9 月 → cancel period_end=9-30 (而不是 today)
- 调 `cancel_promotion_fee(supplier="X", kind="DM", period_end="2026-09-30")`

### 模式 4: 快讯结束

老板: "X 快讯结束"
- kind=快讯, action=cancel
- 调 `cancel_promotion_fee(supplier="X", kind="快讯", period_end=today)`

### 模式 5: 活动结束

老板: "汇一活动结束"
- kind 模糊 (可能是任何 active 记录) → 走 dry_run
- 调 `query_promotion_fee(supplier="汇一", today)` 拿全部 active 记录
- 给老板列清单 ("汇一当前有 2 条 active: 堆头 5000 (9-1~12-31), 快讯 1000 (9-3~9-5)")
- 老板确认全撤 → 调 cancel_promotion_fee 每条

### 模式 6: 促销费取消 (整组)

老板: "X 促销费取消"
- 解析: 整组撤销 (所有 active 记录)
- 调 query 拿全部,逐条 cancel
- 慎用: 先 dry_run 列表,老板确认

### 模式 7: 没堆头了 (语义模糊)

老板: "汇一没堆头了"
- 含义 1: 整条取消 (调 cancel 全部 active 堆头)
- 含义 2: 改 has_duitou=false (走 supplier-policy)
- 含义 3: 当前没有堆头 (跟"取消"无关,只是状态描述)

→ 走 dry_run:
```
汇一当前有 1 条 active 堆头: 5000 (9-1~12-31)
这是要:
A) 整条取消 (调 cancel_promotion_fee)
B) 改 has_duitou=false (调 supplier-policy,以后不签新堆头)
C) 只是状态描述 (啥也不做)
```

老板选 A → cancel; 选 B → supplier-policy; 选 C → 关闭对话

---

## "到期" vs "取消" 的区别

老板: "汇一堆头 8 月底到期" → 这不是"取消",是"自然到期"
- 处理: 不调 cancel (period_end=2026-08-31 已经过期,无需操作)
- 但要 调 query 看是否还有其它 active 记录

老板: "汇一堆头 8 月底结束 (提前撤)" → 这是"提前取消"
- 调 cancel_promotion_fee(supplier="汇一", kind="堆头", period_end="2026-08-31")

老板: "汇一堆头 8 月底撤" → "撤" 通常表示提前取消
- 调 cancel

---

## 群消息中混合 "新增" 和 "取消" 的处理

老板: "汇一撤了堆头, 改快讯 1000 到月底"
- 拆 2 步:
  1. 撤销: cancel_promotion_fee(supplier="汇一", kind="堆头", period_end=today)
  2. 新增: record_promotion_fee(supplier="汇一", kind="快讯", amount=1000, period_end="2026-09-30")
- 都走 dry_run 预览

老板: "汇一那堆头下架, 同时 8 月有 DM"
- 拆 2 步:
  1. 撤销: cancel_promotion_fee
  2. 新增: record_promotion_fee (DM)
- 调 query 看有没有 active 堆头,有则 cancel

---

## 跨年期间的处理

老板 12 月说: "汇一堆头到 1 月底"
- 解析: period_end=2027-01-31 (跨年)
- 调 record_promotion_fee(supplier="汇一", kind="堆头", period_start=2026-XX-XX, period_end=2027-01-31)

老板 1 月说: "汇一去年 12 月堆头撤了"
- 解析: 撤销去年 12 月的 (实际 period_end=2026-12-31 已自然到期)
- 调 query 确认: 如果有 active 记录(period_end >= today),cancel;否则告诉老板已自然到期

---

## 误识别保护

如果 LLM 不确定"是否取消",走 dry_run 问老板:
```
识别: 汇一堆头 → 不确定是新增还是取消
  当前 DB: 1 条 active 堆头 5000 (9-1~12-31)
是:
  A) 新增一条堆头 (在原有基础上再加)
  B) 撤销现有的堆头
  C) 啥也不做 (老板在描述现状)
```

老板回 A → record_promotion_fee (会因同 supplier+kind+period_end 唯一而覆盖,LLM 要先 query 拿 period_end)
老板回 B → cancel_promotion_fee
老板回 C → 关闭

---

## cancel_promotion_fee tool 行为 (W4.2 新)

```python
cancel_promotion_fee(
    supplier="汇一",     # 必填
    kind="堆头",         # 必填
    period_end="2026-09-03"  # 可选, 默认 today
)
```

实际行为:
- 查 supplier+kind 当前 period_end >= today 的记录
- 把这些记录的 period_end 改成传入值 (标记"已结束")
- **不真删**:保留历史,运营可查
- 返: cancelled_count + cancelled_ids

如果 supplier+kind 当前无 active 记录 → 返 `cancelled_count=0, action="not_found"`,告诉老板本来就没堆头。

---

## 二次确认模板

任何 cancel 必走 dry_run 预览:
```
识别: 汇一堆头取消
预览: 汇一 当前 active 堆头 1 条: 5000 (9-1~12-31)
       → 把 period_end 改为 2026-09-03 (今天)
对吗?
```

老板回 OK → 真调 cancel (非 dry_run 模式)。
