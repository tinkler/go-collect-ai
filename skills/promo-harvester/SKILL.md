---
name: promo-harvester
description: |
  企微群消息自动采集堆头费/快讯/活动——监听老板在指定企微群的对话 (例: 汇一"堆头 5000/月, 到 12 月底" / "端架费 3000 到月底" / "汇一 8 月 DM 单" / "汇一 9 月快讯"), LLM 抽取 (supplier, kind, amount, period_start, period_end, note) 五元组, 调 record_promotion_fee 写入;同时识别"取消"意图 (例: "汇一堆头取消" / "端架下架" / "DM 没了"), 调 cancel_promotion_fee 撤销。Use this skill when the user asks about 堆头费/端架费/快讯/DM/海报/促销费从企微群/飞书/微信自动收集 / 取消堆头/端架下架 / 自动识别促销费, or when the user says "汇一堆头 5000 到 12 月底" / "汇一快讯" / "端架取消" / "DM 没了" / "汇一活动" / "堆头 8000, 9-1 到 9-30".
license: Internal-Project
metadata:
  version: "1.0.0"
  author: collect-ai
  category: promo-fee-harvest
  depends_on: "supplier-policy (查 has_duitou / has_duanjia 联动) + purchase-alert (写入后下次 Apply 跑 has_duitou / flash_promo 规则)"
compatibility: requires Python 3.x (scripts/extract_promos.py)
triggers:
  - 堆头费
  - 端架费
  - 快讯
  - DM
  - 海报
  - 特价活动
  - 促销费
  - 堆头
  - 端架
  - 堆头取消
  - 端架下架
  - DM 没了
  - 企微群采集
  - 飞书群采集
  - 微信群采集
  - 群消息识别
  - 自动入账
  - 取消堆头
---

# Promo Harvester (企微群消息自动采集促销费)

> **目标**:把"老板在企微群说的堆头/端架/快讯/DM"和"取消意图"自动结构化、写入 `promotion_fee` 表,让 purchase-alert skill 下次跑 7 规则时命中 `has_duitou` / `flash_promo`。
>
> **之前**:老板必须在群发消息后,手填 `record_promotion_fee` tool;漏填率高 → 高库存误报 / 堆头/快讯不被识别。
>
> **现在**:LLM 听群消息 → 自动抽取五元组 → 调 `record_promotion_fee` 落库;识别"取消" → 调 `cancel_promotion_fee` 撤销。

---

## When to use this skill

**适用**:
- 企微/飞书/微信群里老板或采购说"X 供应商签了堆头 Y 元 Z 期间"
- 群里说"X 供应商的 DM 发了 / 海报贴了 / 端架下了"
- 群里说"X 供应商的堆头取消 / 端架下架 / DM 没了 / 活动结束"

**不适用**:
- 销售数据 (走 supplier-payment skill)
- 商品促销 (走 restock-strategy skill)
- OCR 解析采购收货单 (走 ocr-purchase skill)
- 老板记录供应商基础政策 (走 supplier-policy skill)

---

## 输入 (群消息内容)

LLM 接收一段对话,典型例子:

```
老板: 汇一签了堆头 5000/月, 到 12-31
员工: 好的
老板: 汇一 8 月有 DM 单
员工: 收到
老板: 端架取消, 汇一的下周撤了
员工: 明白
```

或者单条:

```
汇一堆头 8000, 9-1 到 9-30
```

---

## How to use this skill (LLM 工作流)

### 步骤 0: 加载识别规则 + 撤销规则

调 `invoke_skill` action=`read_file`:
- `references/wecom-msg-patterns.md` → 5 种 kind 的识别模式
- `references/revoke-patterns.md` → 7 种"取消"短语模式

### 步骤 1: 抽取 (supplier, kind, amount, period_start, period_end, note) 五元组

| 字段 | 抽取来源 | 缺省 |
|---|---|---|
| supplier | 老板话里第一个供应商名 | 必填, 缺则跳过 (无法入库) |
| kind | "堆头" / "端架" / "DM" / "快讯" / "海报" / "特价" 等关键词 | 必填, 缺则推断 (默认"堆头") |
| amount | 数字 + 元 / ¥ / ￥ (支持 5k=5000) | 必填, 缺则问老板 |
| period_start | 日期 / "本月" / "下月" / "9-1" / "9月1日" | 缺则 today |
| period_end | 日期 / "月底" / "12-31" / "12月底" | 缺则 today + 30d |
| note | 来源消息的上下文, 群里说话的人的 ID | 默认 "wecom 群消息" |

### 步骤 2: 二次确认 (任何写操作必走)

```
识别: 汇一 堆头 ¥5000/月, 9-1 到 12-31

预览:
  record_promotion_fee(
    supplier="汇一", kind="堆头",
    amount=5000,
    period_start="2026-09-01", period_end="2026-12-31",
    note="wecom 群消息, 老板 9-3 13:00 说")
对吗?
```

老板回 OK → 真写。

### 步骤 3: 落库 (record / cancel)

- **record 路径**:`record_promotion_fee` (UPSERT 幂等, 同 supplier+kind+period_end 覆盖)
- **cancel 路径**:`cancel_promotion_fee` (W4.2 新, 见 references/revoke-patterns.md)

### 步骤 4: 下游联动 (重要!)

- 写完一条堆头 (汇一 has_duitou=true) → **调 `query_supplier_policy` 检查**:
  - 还没设 `has_duitou=true`? → 提示老板要不要同步 set
  - 已设? → 不动
- 写完一条快讯 (汇一 kind=快讯) → 不需要动 supplier_policy (flash_promo 规则直接查 promotion_fee 表)
- 撤销堆头/快讯 → 调 `query_promotion_fee` 确认是否还有同 supplier 同 kind 的 active 记录,如果有 → 不删旧(老板可能误操作),只标记 `note="已撤销"` 让运营人工 review

### 步骤 5: 跟 supplier-policy / purchase-alert 的协同

- **supplier-policy**:`has_duitou=true` 是"该 supplier 签了堆头协议" 的总开关;promo-harvester 写堆头记录会**触发提示**让老板同步 set 这个 key
- **purchase-alert**:下次 `CreateSession` 触发 Apply,自动调 `query_promotion_fee` 查当前 active 记录,跑 `has_duitou` 规则 (highlight_dui) + `flash_promo` 规则 (highlight_others)

---

## 关键 kind 字段枚举

允许的 `kind` 值(白名单):

| kind | 中文名 | 典型场景 | 影响的 purchase-alert 规则 |
|---|---|---|---|
| `堆头` | 堆头陈列 | 超市主通道大型陈列 | has_duitou (highlight_dui) |
| `端架` | 端架陈列 | 货架两端 | flash_promo (highlight_others, 默认配置) |
| `DM` | DM 单 | 邮报/宣传单 | flash_promo |
| `快讯` | 快讯 | 临时促销信息 | flash_promo |
| `海报` | 海报 | 店内海报 | flash_promo |
| `特价` | 特价 | 临时特价 | flash_promo |
| `其他` | 其他 | 兜底 | flash_promo |

> 严格按白名单写,避免乱填 (e.g. "堆头费" 应规范为 "堆头")

---

## "取消"识别模式

详见 `references/revoke-patterns.md`。7 种短语模式:

1. "X 堆头取消" / "X 堆头下架"
2. "X 端架撤了" / "X 端架下架"
3. "X DM 没了" / "X 海报撤了"
4. "X 快讯结束"
5. "X 活动结束"
6. "X 促销费取消"
7. "X 没堆头了" / "X 不做了" (整条撤销, 慎用)

调 `cancel_promotion_fee(supplier, kind, period_end=today)` 把今天之后到期的对应记录 `period_end` 改成 today (标记"已结束")。

---

## Scripts

### `scripts/extract_promos.py`

**作用**:从一段群消息文本中,正则提取 (supplier, kind, amount, period) 四元组候选,LLM 再判断真伪。

**入参**:
```json
{
  "message": "汇一签了堆头 5000/月, 到 12-31. 端架费 3000 到月底.",
  "known_suppliers": ["汇一", "榄菊", "金龙鱼"],
  "today": "2026-09-03"
}
```

**出参**:
```json
{
  "candidates": [
    {
      "supplier": "汇一", "kind": "堆头", "amount": 5000,
      "period_start": null, "period_end": "2026-12-31",
      "raw_text": "堆头 5000/月, 到 12-31"
    },
    {
      "supplier": "汇一", "kind": "端架", "amount": 3000,
      "period_start": null, "period_end": "2026-09-30",
      "raw_text": "端架费 3000 到月底"
    }
  ]
}
```

> **何时调**:LLM 不确定日期/金额时跑脚本,正则给候选,LLM 用语义判断修正。

---

## 关键设计要点

1. **白名单 kind**:`kind` 严格 7 个白名单,LLM 写错会被 record_promotion_fee 拒绝
2. **upsert 幂等**:`(supplier, kind, period_end)` 唯一,二次写入覆盖
3. **下游即时生效**:写完一条 promotion_fee,下次 purchase-alert Apply 立即识别
4. **跟 supplier-policy 联动**:堆头记录会提示让老板同步设 `has_duitou=true`
5. **不替代人工**:二次确认必须走; 复杂的 (金额模糊, 时间跨年) 走 dry_run 预览

---

## 不要做

- ❌ 不要自动写入 (跳过二次确认)
- ❌ 不要把"堆头费"作为 kind (规范化为"堆头", fee 在 amount 字段)
- ❌ 不要用同一条消息推多轮 (老板发一次, 触发一次 skill, 等老板确认)
- ❌ 不要删旧记录 (用 cancel 把 period_end 改 today, 保留历史)
- ❌ 不要把 supplier_policy 跟 promotion_fee 混了 (前者是 K-V 政策, 后者是 N 条时间窗记录)

---

## 相关文档

- `references/wecom-msg-patterns.md` — 5 种 kind 的识别模式 + 真实案例
- `references/revoke-patterns.md` — 7 种"取消"短语模式
- `skills/supplier-policy/SKILL.md` — has_duitou / has_duanjia key
- `skills/purchase-alert/SKILL.md` — has_duitou / flash_promo 规则
- `internal/agent/tools/payment.go` — record_promotion_fee / list_promotion_fee / cancel_promotion_fee 实现
