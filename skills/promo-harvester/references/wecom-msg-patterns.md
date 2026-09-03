# 企微群消息识别模式 (Promo Harvester)

> 配合 SKILL.md 使用。LLM 听群消息时按本表识别 5 种 kind 的写法,提取 (supplier, kind, amount, period_start, period_end, note) 五元组。

---

## kind: 堆头

### 典型写法
- "X 签了堆头 Y 元 / 月, Z 期间"
- "X 堆头 Y 元 到 Z"
- "X 堆头 Y, Z 起"
- "X 堆头协议 Y/月 到 Z"
- "X 堆头费 Y 元" → kind="堆头", amount=Y (fee 字段在 amount 里)

### 真实案例

老板: "汇一签了堆头 5000/月, 到 12-31"
- 抽取: supplier=汇一, kind=堆头, amount=5000, period_end=2026-12-31, period_start=默认 today
- 备注: note="wecom 群消息, 老板 9-3 说"

老板: "汇一 9-1 到 9-30 堆头 8000"
- 抽取: supplier=汇一, kind=堆头, amount=8000, period_start=2026-09-01, period_end=2026-09-30

老板: "X 堆头他们出 5000" → 这是 supplier-policy 范畴,不是 promo-harvester
- 识别: kind=堆头但无 amount/period → 跳过 (让 LLM 走 supplier-policy skill)

老板: "汇一堆头到下月底" → kind=堆头, amount=缺 (问老板)
- 识别: 缺 amount → 走 dry_run 预览,问老板补金额

### 数量词解析
- "5k" / "5K" = 5000
- "1w" / "1W" = 10000
- "1.5w" = 15000
- "5,000" = 5000
- "5000" = 5000
- "5千" = 5000
- "1万" = 10000

### 期间解析
- "到 12-31" → period_end=2026-12-31
- "到 12 月底" → period_end=YYYY-12-31 (year 推断为今年或明年,如果 12 月已过则明年)
- "到月底" → period_end=YYYY-MM-月底 (今天所在月)
- "9-1 到 9-30" → period_start=2026-09-01, period_end=2026-09-30
- "9月" → period_start=2026-09-01, period_end=2026-09-30
- "本月" → period_start=today, period_end=月底
- "下月" → period_start=下月1号, period_end=下月月底
- "3 个月" → period_start=today, period_end=today+90d
- 缺 period_start → 默认 today
- 缺 period_end → 默认 today+30d,问老板

---

## kind: 端架

### 典型写法
- "X 端架费 Y 元 Z"
- "X 端架 Y, Z 期间"
- "X 端架下架了" → 撤销 (走 cancel_promotion_fee)

### 真实案例

老板: "汇一 端架 3000, 9-1 到 9-30"
- 抽取: supplier=汇一, kind=端架, amount=3000, period_start=2026-09-01, period_end=2026-09-30

老板: "X 端架 2000, 8 月"
- 抽取: supplier=X, kind=端架, amount=2000, period=8 月整月

### 注意事项
- 端架 = 货架两端陈列
- 默认归到 `flash_promo` 规则 (highlight_others, 绿其它)
- 如果老板同时说"X 端架供应商出" → 这是 supplier-policy (has_duanjia=true) 不是 promo-harvester

---

## kind: DM (宣传单)

### 典型写法
- "X DM 单 Y 元"
- "X 8 月 DM"
- "X DM 发了"

### 真实案例

老板: "汇一 8 月有 DM 单, 1500"
- 抽取: supplier=汇一, kind=DM, amount=1500, period=8 月整月

老板: "X DM 2000, 9-15 到 9-20"
- 抽取: supplier=X, kind=DM, amount=2000, period=9-15 到 9-20 (5 天短期)

### 注意事项
- DM 单 = 邮报 / 宣传单
- 短期 (几天) 也算,prompt 仍归 flash_promo
- kind="DM" 不要写成"DM单" / "宣传单" / "邮报"

---

## kind: 快讯

### 典型写法
- "X 快讯 Y 元"
- "X 9 月快讯"
- "X 临时快讯, Y 元 Z 期间"

### 真实案例

老板: "汇一快讯 500, 9-3 到 9-5"
- 抽取: supplier=汇一, kind=快讯, amount=500, period=3 天

老板: "X 这周末快讯, 1000 元"
- 抽取: amount=1000, period=本周末 (推断 3 天)

---

## kind: 海报

### 典型写法
- "X 海报 Y 元"
- "X 海报张贴 Z"
- "X 9 月海报 800"

### 真实案例

老板: "汇一 海报 500, 9-1 到 9-30"
- 抽取: supplier=汇一, kind=海报, amount=500, period=9 月整月

---

## kind: 特价 (临时降价活动)

### 典型写法
- "X 特价 Y 元"
- "X 临时特价, 9-1 到 9-3"
- "X 9 月特价活动"

### 真实案例

老板: "X 特价 1000, 9-1 到 9-3 (3 天)"
- 抽取: supplier=X, kind=特价, amount=1000, period=3 天

---

## kind: 其他 (兜底)

老板说"X 搞活动, Y 元, 9 月" 但没说具体类型:
- 抽取: kind="其他", amount=Y, period=9 月
- 标注: note="老板未指定 kind, 默认 '其他'"

或者: 走 dry_run,问老板具体类型

---

## 多条消息的处理

老板可能发一条消息含多个 promotion,例如:

老板: "汇一堆头 5000, 端架 3000, 都到 12-31"
- 拆 2 条:
  1. supplier=汇一, kind=堆头, amount=5000, period_end=2026-12-31
  2. supplier=汇一, kind=端架, amount=3000, period_end=2026-12-31

每条都走 dry_run 二次确认,逐条 record_promotion_fee。

---

## 复杂句式

老板: "汇一那个堆头继续, 5000/月, 续到 12 月底"
- 识别: "继续" 表示**不是新签,是续约** → 走 record_promotion_fee (覆盖同 supplier+kind+period_end)
- 抽取: supplier=汇一, kind=堆头, amount=5000, period_end=2026-12-31

老板: "汇一那个堆头结束, 8 月底到期"
- 识别: 撤销意图 → 走 cancel_promotion_fee(supplier=汇一, kind=堆头, period_end=today)
- 如果当前是 9 月, period_end=8-31 已过期,无需撤销,但要告诉老板已过期

老板: "汇一这周到 9-10 临时促销"
- 识别: kind 缺, amount 缺, 期间有 → 走 dry_run 问

---

## 跟 supplier-policy 的边界

| 老板的话 | 走哪个 skill |
|---|---|
| "汇一堆头他们出" | supplier-policy (has_duitou=true, 合同条款) |
| "汇一签了堆头 5000/月" | promo-harvester (record_promotion_fee, 时间窗) |
| "汇一不让进" | supplier-policy (block_entry=true) |
| "汇一活动结束" | promo-harvester (cancel_promotion_fee) |
| "汇一活动取消" | promo-harvester (cancel_promotion_fee) |

> 关键区别:
> - supplier-policy = 政策 (合同/规则,无时间窗)
> - promotion_fee = 记录 (每次活动, 有时间窗)
