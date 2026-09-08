# 8 规则判定详细

> 配合 SKILL.md 主入口使用。LLM 跑规则时,**先读本文件**拿到每条的判定逻辑, 再调 query tool 查数据, 最后决定报/不报/降级。

> **W4.4 (2026-09-03) 新增规则 8: pending_return (未审批退货单)** — 详细见文末。

---

## 规则 1: block_entry(限入场)

### 必报条件
- `query_supplier_policy(supplier, "block_entry")` 返 `true`

### 判定
```python
policy = query_supplier_policy(supplier, "block_entry")
if policy and policy.value == True:
    alert = {
        "rule": "block_entry",
        "severity": "block",      # 最严重
        "category": "block",      # 🔴 红色感叹号
        "row_id": row.row_id,     # 必填 (按 supplier 命中该 supplier 提供的所有行)
        "message": f"供应商 [{supplier}] 已被限制入场(block_entry=true),本单据不审请勿入库"
    }
    emit(alert)
```

### 降级
**永远不降级**。block_entry 是硬阻断,任何业务关系都改不了,只能先去 supplier-policy skill 撤销。

### 不报条件
- `block_entry` 不存在 / `false`

---

## 规则 2: no_return(不让退)

### 必报条件
- `query_supplier_policy(supplier, "allow_return")` 返 `false`

### 判定
```python
policy = query_supplier_policy(supplier, "allow_return")
if policy and policy.value == False:
    alert = {
        "rule": "no_return",
        "severity": "warn",
        "category": "warn",       # 🟠 橙色感叹号
        "row_id": row.row_id,
        "message": f"供应商 [{supplier}] 不支持退货(allow_return=false),请确认本次采购数量 {row.qty}"
    }
    emit(alert)
```

### 降级
**永远不降级**。`allow_return=false` 是合同条款,除非改合同。

### 不报条件
- `allow_return` 不存在 / `true`

---

## 规则 3: offseason(反季)

### 必报条件
- row.matched_name 或 raw_name 含"应季词", 但当前日期不在该词对应的季节

### 应季词表(参考,不全;遇到新词让 LLM 自己判断)
| 词 | 适合季节 |
|---|---|
| 冰品 / 冰棍 / 冰激凌 / 冰淇淋 / 冰粉 | summer |
| 凉席 / 风扇 / 空调 / 西瓜 | summer |
| 电热 / 暖手宝 / 棉衣 / 毛毯 / 火锅 | winter |
| 月饼 / 螃蟹(中秋) | autumn (中秋前 30 天) |
| 圣诞礼盒 / 圣诞树 | winter (12-15 到 12-25) |
| 防晒霜 / 太阳镜 | summer (5-9 月) |

### 判定流程
```python
name = row.matched_name or row.raw_name
cur_season = current_season()  # 简单按月份: 3-5 春, 6-8 夏, 9-11 秋, 12-2 冬
hits = [w for w in SEASON_WORDS if w in name]
if hits:
    for word in hits:
        if cur_season not in SEASON_WORDS[word]:
            # 反季
            emit({
                "rule": "offseason",
                "severity": "info",
                "category": "info",     # ⚪ 灰普通感叹号
                "row_id": row.row_id,
                "message": f"商品 [{name}] 含应季词 [{word}],但当前是 [{cur_season}],可能是反季补货"
            })
            break
```

### 降级
- 节假日 lead 期间 (如 中秋前 30 天) 含"月饼" 不算反季 → **降级: 不报**
- 季节切换窗口 (春分/秋分前后 7 天) 容忍度放宽 → **降级: 改 info**

### 不报条件
- 词不在应季词表 → 跳过
- 词在应季词表且 cur_season 匹配 → 跳过

---

## 规则 4: holiday_lead(节假日备货窗口)

### 必报条件
- `query_special_calendar(now, 90)` 返至少 1 个 `type=holiday`
- 该节假日的 `date - now` 在 `[0, lead_days]` 范围内

### 判定
```python
holidays = query_special_calendar(now, lead_days=90)
# 找最近的、且在 lead_days 窗口内的
candidates = [h for h in holidays if h.type == "holiday" and 0 <= (h.date - now).days <= h.lead_days]
if candidates:
    nearest = min(candidates, key=lambda h: h.date)
    days_left = (nearest.date - now).days
    emit({
        "rule": "holiday_lead",
        "severity": "info",
        "category": "info",     # ⚪ 灰普通感叹号 (总结栏展示)
        "row_id": 0,            # session 级
        "message": f"距 [{nearest.name}] 还有 {days_left} 天(lead_days={nearest.lead_days}),建议提前备货"
    })
```

### 降级
- 距节假日 > 14 天 且 lead_days < 14 → 改 info, message 去掉"建议提前备货"
- 多个节假日都在窗口内 → 只报最近的 1 条

### 不报条件
- 90 天内无节假日

---

## 规则 5: high_stock(高库存) — **活的规则**

### 必报条件(初判)
- `row.stock_qty > app_settings.high_stock_threshold` (默认 50)
- 注意: row.stock_qty 是 input 字段,如果为 null 跳过

### 降级条件(活的规则,这是关键)
- **降级 A**: 该 supplier 的 `has_duitou=true` AND `query_promotion_fee(supplier, now)` 命中堆头期内 (kind in duitou_kinds)
  - 改: `category=info` (灰感叹号), message 备注 "供应商签了堆头 ¥X/月(至 MM-DD),允许阶段性压库存"
- **降级 B**: 该 supplier 近 90 天累计促销费 > ¥10000 AND 催款状态="不催"
  - 改: 不报 (完全跳过)
- **降级 C**: 该 supplier 在 7 规则的某个其他降级条件命中 (如节日大促期)
  - 改: 改 category=info

### 判定流程
```python
if row.stock_qty is None or row.stock_qty <= high_stock_threshold:
    return  # 不报

# 尝试降级
downgraded = False

# 降级 A: 堆头期
if query_supplier_policy(supplier, "has_duitou"):
    promos = query_promotion_fee(supplier, now)
    duitou = [p for p in promos if p.kind in duitou_kinds and p.period_start <= now <= p.period_end]
    if duitou:
        emit({
            "rule": "high_stock",
            "severity": "info",  # 降级: warn → info
            "category": "info",
            "row_id": row.row_id,
            "message": f"商品 [{row.matched_name}] 库存 {row.stock_qty:.0f} 超阈值 {high_stock_threshold:.0f},但供应商 [{supplier}] 当前签了堆头 {duitou[0].kind} ¥{duitou[0].amount:.0f}/月(至 {duitou[0].period_end.strftime('%m-%d')}),允许阶段性压库存,降为提示。"
        })
        downgraded = True

# 降级 B: 累计促销费大 + 不催款
if not downgraded:
    pay_history = query_supplier_payment(supplier, days=90)
    if pay_history.total_amount > 10000 and pay_history.urge_status == "not_urge":
        # 完全不报, 但 internal note
        emit({
            "rule": "high_stock_internal",
            "severity": "info",
            "category": "info",
            "row_id": row.row_id,
            "message": f"[internal] 商品 [{row.matched_name}] 库存 {row.stock_qty:.0f} 超阈值,但供应商 [{supplier}] 近 90 天累计促销费 ¥{pay_history.total_amount:.0f} 且不催款,跳过 high_stock 报警(注:不影响前端展示)"
        })
        return  # 不报外部 alert

# 默认: 必报
if not downgraded:
    emit({
        "rule": "high_stock",
        "severity": "warn",
        "category": "warn",     # 🟠 橙色感叹号
        "row_id": row.row_id,
        "message": f"商品 [{row.matched_name}] 当前库存 {row.stock_qty:.0f},超过阈值 {high_stock_threshold:.0f},本次采购需谨慎(可能压库存)"
    })
```

### 不报条件
- stock_qty ≤ threshold
- 降级 B 命中 (完全跳过)

---

## 规则 6: has_duitou(堆头陈列,总结栏)

### 必报条件(session 级,row_id=0)
- 该 supplier 的 `has_duitou=true`
- `query_promotion_fee(supplier, now)` 命中 kind in duitou_kinds 且在期内
- 同 session 内至少 1 个 row 的 matched_supp == supplier

### 判定
```python
session_suppliers = {r.matched_supp for r in rows if r.matched_supp}
dui_hits = []
for sup in session_suppliers:
    if query_supplier_policy(sup, "has_duitou"):
        promos = query_promotion_fee(sup, now)
        dui_promos = [p for p in promos if p.kind in duitou_kinds and p.period_start <= now <= p.period_end]
        if dui_promos:
            # 合并该 supplier 的所有堆头为 1 条
            parts = [f"{p.kind} ¥{p.amount:.0f}(至 {p.period_end.strftime('%m-%d')})" for p in dui_promos]
            dui_hits.append(f"[{sup}] {', '.join(parts)}")

if dui_hits:
    emit({
        "rule": "has_duitou",
        "severity": "info",
        "category": "highlight_dui",   # 🟢 绿色"贴切"标志
        "row_id": 0,                   # session 级
        "message": f"本期堆头陈列: {', '.join(dui_hits)}"
    })
```

### 降级
**不降级**。这是亮点提示,不是告警。

### 不报条件
- 没有任何 supplier 同时签了 has_duitou=true 且当前 promotion_fee 在期内

---

## 规则 7: flash_promo(快讯/端架,行内)

### 必报条件
- `query_promotion_fee(supplier, now)` 命中 kind in others_kinds 且在期内
- others_kinds 默认: `[端架, 快讯, DM, 特价, 海报]`

### 判定
```python
promos = query_promotion_fee(supplier, now)
flash = [p for p in promos if p.kind in others_kinds and p.period_start <= now <= p.period_end]
if flash:
    kinds_str = ', '.join(set(p.kind for p in flash))
    emit({
        "rule": "flash_promo",
        "severity": "info",
        "category": "highlight_others",  # 🟢 绿色"其它"标志
        "row_id": row.row_id,
        "message": f"商品 [{row.matched_name}] 供应商 [{supplier}] 正在做 {kinds_str},注意陈列位置"
    })
```

### 降级
**不降级**。亮点提示。

### 不报条件
- 无 in-期 in-kind 的 promotion_fee

---

## 规则优先级与冲突解决

如果同一行同时命中多个 rule:
- severity 取最高: block > warn > info
- 但**所有命中的都报**,不只是最高 (前端按 row_id 关联多个 alert, 显示多个 icon)

如果同一 supplier 的 has_duitou 和 flash_promo 都在期内 (kind 既是堆头又是快讯 — 不可能,但如果 kind 配置重叠):
- has_duitou 用 duitou_kinds 判
- flash_promo 用 others_kinds 判
- 两集合**互斥**(运行期保证)

---

## 阈值调整(给运营的)

改 `app_settings` 表:
- `high_stock_threshold` (默认 50) — 数字大 → 报得少
- `duitou_kinds` (默认 ["堆头"]) — JSONB 数组
- `others_kinds` (默认 ["端架","快讯","DM","特价","海报"]) — JSONB 数组

改完后 LLM 下次跑自动 pick 新值,无需重启。

---

## 规则 8: pending_return(未审批退货单) — W4.4 新增, 2026-09-04 激活

> **目的**: 提醒收货人:**这批货供应商有未处理的退货单, 先确认退货已退出仓库, 再决定是否收货。** 防止重复收货 + 防止错收已退货品。

### 数据源 (W4.4 已落地)
- 走 cube 端 `supplier_returns` (HBPoS `t_pm_sheet_master WHERE trans_no='RO' AND sale_way='A'`)
  - 物理 cube: cube-agent-server `plugins/supplier_returns/plugin.yaml` (3484 行 / 2026-09-03 实测)
  - 物理主表: `t_pm_sheet_master` (近 1 年, `oper_date >= DATEADD(YEAR, -1, GETDATE())`)
- 业务字段 (mapping.yaml `entities.returns` 段):
  - `bill_no` ← `sheet_no` (退货单号, 主表 PK)
  - `supplier_id` ← `supcust_no` (供应商编号)
  - `supplier_name` ← `sup_name` (供应商名, cube SQL LEFT JOIN `t_bd_supcust_info`)
  - `status` ← `approve_flag` (HBPoS: '0'=未审核 / '1'=已审核; 业务翻译: pending='0' / approved='1')
  - `return_money` ← `sheet_amt` (单据金额)
  - `create_date` ← `oper_date` (制单时间)
  - `branch_no` ← `branch_no` (门店编号)
- 走 `business.Executor.SearchReturnsBySupplier` (e.query() 私有方法) → mapping 翻译 → cube 端
- **没有 reason/item_no/item_name 字段** (cube 端 2026-09-04 确认无, 退货明细在 supplier_return_items cube 后续再加)

### 状态值翻译 (ValueMap, mapping.yaml 写)
- 业务侧 (LLM 用): `pending` / `approved` / `rejected`
- 物理侧 (HBPoS approve_flag): `'0'` (未审核) / `'1'` (已审核) / **不存在**
- mapping ValueMap 翻译: `pending → '0'`, `approved → '1'`, `rejected` 不在 HBPoS → 永远 0 行
- LLM 永远只看业务值, 物理值藏在 mapping 层

### 必报条件
- `query_return_order(supplier, status="pending", days=30)` 返 `count >= 1` AND `not_available=false`

### 不报条件 (本规则的"降级"语义)
- **`not_available=true` (任何原因) → 规则降级, 不报** (不报比误报好)
  - 触发场景: cube 数据源未接入 / mapping 缺失 / cube 端 schema 不对 / 查询超时
- `count == 0` (该 supplier 近 30 天没有未审批退货单) → 不报
- `status` 不是 `pending` (已审批) → 不在本规则 scope, 业务上不需要提醒

### 判定
```python
result = query_return_order(supplier, status="pending", days=30)

# 关键: not_available=true → 整条规则降级, 不报 (跟 high_stock 降级不同, 这里是"数据缺失"型降级)
if result.not_available:
    log("[rule 8 pending_return] 降级: cube 数据源未接入, 不报")
    return  # 跳过本规则, 继续规则 1-7

# 正常路径: count >= 1 → 报 1 条 session 级 warn (整 supplier 1 条, 不按 row 报)
if result.count >= 1:
    # 按 create_date 升序排序, 找最早一单
    earliest = sorted(result.returns, key=lambda r: r.create_date)[0]
    alert = {
        "rule": "pending_return",
        "severity": "warn",            # 第二严重 (跟 no_return / high_stock warn 同级)
        "category": "warn",            # 🟠 橙色感叹号
        "row_id": 0,                   # session 级 (跟 has_duitou / block_entry 汇总同级)
        "message": (
            f"供应商 [{supplier}] 近 30 天有 {result.count} 单未审批退货单"
            f"(总额 ¥{result.total_money:.0f}),请收货人确认退货已退出仓库后,再决定是否收货。"
            f"最早一单: {earliest.bill_no} ({earliest.create_date})"
        )
    }
    emit(alert)  # 整 session 1 条 (不按 row 报, 即使 session 内多个 row 同 supplier)
```

### 活规则 (跟 supplier_policy 联动)
- **若 supplier_policy.allow_return=false (合同不让退)** → 报 **block** (最高严重, 提醒业务"这家供应商不让退, 有未审批退货单 = 异常"), 而非 warn
  - 判定: `if not_available=false AND count >= 1 AND policy.allow_return == false: severity="block", category="block"`
- **若 supplier_policy.block_entry=true (限入场)** → 规则 1 已报 block_entry, 规则 8 仍报 warn (不重复, 但 1 条不同维度的提示: "限入场的供应商还有未处理的退货, 财务上需先平账")

### 跟其他规则的关系
- 独立于规则 1-7, **不影响** high_stock / no_return / offseason 判定
- 是 **session 级** alert (row_id=0), 跟 has_duitou / block_entry 汇总同级
- 一个 session 内同一 supplier 无论多少 row, **只报 1 条** (合并去重)

### 性能预算
- 一次 tool call: 500ms-2s (cube)
- session 级调用 N 次 (N = session 内不同 supplier 数, 通常 1-3 个)
- 增加总时长: ~2-6s (跟 30s 总预算兼容)

### 误报案例 / False Positive
- 某 supplier 长期退货率低, 突然 1 单 pending → 仍报 (低频事件更需要提醒)
- status="pending" 但实际是 "已审批未通知供应商" 的中间态 → 报 (这是业务该知道的状态)

