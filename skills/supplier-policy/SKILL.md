---
name: supplier-policy
description: 供应商政策"决策记忆"——把老板的自然语言(汇一自采/榄菊不让退/汇一堆头他们出/以后不进货/拉黑)准确映射到 7 个 policy key,支持 录入 / 撤销(整条) / 部分撤销(单个 key) 三种操作;同时识别反回扣危险信号(同品多供应商价差大/临时换供应商/堆头费异常高/临时涨价/私下返点),给出供应商分级与建议。Use this skill when the user asks about 供应商政策/供应商分级/防回扣/反回扣/自采/不让退/堆头谁出/不进某供应商/黑名单/拉黑/供应商评估/供应商分类/临时换供应商/报价异常/私下返点 / 解除黑名单 / 不限制X了 / X政策清空, or when the user says "汇一是自采的" / "榄菊不让退" / "以后不进货 X" / "拉黑" / "这家供应商怎么样" / "这个价格不对" / "临时换供应商" / "私下给我返点" / "汇一以后可以退了" / "解除榄菊黑名单" / "汇一政策全清".
license: Internal-Project
metadata:
  version: "1.1.0"
  author: collect-ai
  category: supplier-management
  migrated_from: "internal/agent/tools/policy.go (remember_supplier_policy / query_supplier_policy) + 7 个 key 白名单"
  extended_in: "W4.2 (2026-09-03) — 加 delete_supplier_policy + list_supplier_keys 工具 + 撤销/部分撤销工作流"
compatibility: requires Python 3.x
triggers:
  - 供应商政策
  - 供应商分级
  - 防回扣
  - 反回扣
  - 自采
  - 不让退
  - 堆头谁出
  - 黑名单
  - 不进货
  - 供应商评估
  - 供应商分类
  - 战略供应商
  - 临时换供应商
  - 报价异常
  - 私下返点
  - 解除黑名单
  - 撤销政策
  - 不限制
  - 可以退了
  - 政策清空
---

# Supplier Policy(供应商政策 + 防回扣)

> **目标**:把"老板的话 → 7 个 policy key 的解读 + 防回扣风险识别"这件事,从"LLM 自由发挥"升级成"LLM + 行业经验表 + 现有 2 个 tool 协同"的**推理流程**。
>
> **之前**:`policy.go` 只有白名单 + UPSERT 读写,**判定"老板的话对应哪个 key"完全靠 LLM 自己**。风险:
> - LLM 自由发挥导致 key 写错(把"不让退"写成 `block_return`,实际是 `allow_return=false`)
> - 反回扣危险信号没人管,等老板自己发现
> - 供应商分级没有标准,谁都说自己"重要"
>
> **现在**:
> 1. 老板自然语言 → 7 个 key 的映射表(本 skill)
> 2. 反回扣危险信号清单(references/)
> 3. 供应商分级标准(references/)
> 4. LLM 调 `query_supplier_policy` 拿当前值,对比政策变动

## When to use this skill

适用:

1. 老板说"汇一是自采供应商" / "榄菊不让退" / "汇一堆头他们出"
2. 老板说"以后不进 XX 供应商" / "把 XX 拉黑"
3. 老板问"汇一现在政策是什么" / "所有供应商政策列一下"
4. 老板说"防回扣" / "这家供应商有问题" / "价格不对"
5. 员工反馈"供应商临时涨价" / "换供应商了"

不适用:

- 询问商品质量(走 SKU 维度,不调本 skill)
- 询问结算金额(走 `settlement-suggestion` skill)

## How to use this skill(LLM 工作流)

### 步骤 0:加载语义映射

调 `invoke_skill` action=`read_file` path=`references/key_semantics.md`。
拿到 7 个 key 的精确定义、合法 value 类型、典型误用。

### 步骤 1:把老板的话拆成 (supplier, key, value)

参考映射规则:

| 老板的话 | key | value |
|---|---|---|
| "自采" / "我自己进货" / "自营" | `is_self_procure` | `true` |
| "不让退" / "不能退" / "无理由不退" | `allow_return` | `false` |
| "可以退" / "7 天无理由" | `allow_return` | `true` |
| "堆头他们出" / "供应商承担堆头" | `has_duitou` | `true` |
| "没堆头" / "我们承担堆头" | `has_duitou` | `false` |
| "端架他们出" | `has_duanjia` | `true` |
| "以后不进" / "拉黑" / "黑名单" | `block_entry` | `true` |
| "临时不让进" | `block_entry` | `true` + `block_reason`=`临时` |
| "价格不对" / "临时涨价" / "有回扣嫌疑" | (不直接写 key) | 进 step 4 风险评估 |

**注意**:一个供应商可能有多个 key 同时被提到,比如"汇一是自采供应商,堆头他们出" → 拆 2 条 policy。

### 步骤 2:二次确认

每个 (key, value) 组合都要用 `remember_supplier_policy` 的 `dry_run=true` 模式预览:

```
"汇一" is_self_procure=true (新建)
"汇一" has_duitou=true (新建)
对吗?两个都记吗?
```

老板回 OK → 把 `dry_run=false` 真写。

### 步骤 3:落库

按确认结果调 `remember_supplier_policy`,每次一个 key。

### 步骤 4:反回扣风险评估(关键!)

**每次**录入或查询供应商政策时,LLM 都要自检以下信号:

| 信号 | 危险等级 | LLM 动作 |
|---|---|---|
| 同一 SKU 多个供应商报价差 > 30% | 🔴 高 | 立即告诉老板,问要不要查 |
| 老板说"这个供应商" 临时换到新供应商 | 🟡 中 | 查 `block_entry` 旧供应商,问是否有关联 |
| 老板拒绝透露某供应商身份 | 🟡 中 | 不深究,记入 `note` |
| 老板说"堆头费异常高" (> 月供 20%) | 🔴 高 | 套用 settlement-skill 评估,问老板 |
| 老板多次说"换个供应商" | 🟡 中 | 建议老板保留"试用供应商"policy |
| 老板说"私下给我返点" | 🔴 高 | **拒绝**,提示合规风险 |

参考 `references/anti_kickback_signals.md` 完整清单。

### 步骤 5:供应商分级(可选)

老板问"这家供应商怎么样"时,套用 `references/supplier_tiering.md` 给 A/B/C/D 分级。

---

## 决策记忆操作 (W4.2 新增)

本 skill 不仅是"录入",还支持 **3 种操作** + **1 个查询**:

| 操作 | 老板的话示例 | 调 tool | 关键差异 |
|---|---|---|---|
| **录入 (create/update)** | "汇一是自采" / "汇一以后可以退了" | `remember_supplier_policy` dry_run=true → confirm → dry_run=false | 同 key 二次写入会**覆盖** (UPSERT) |
| **撤销单 key (partial revoke)** | "解除榄菊黑名单" / "汇一以后又可以退了" | `delete_supplier_policy(supplier, key=block_entry)` | 删一条,其它 key 保留 |
| **撤销整条 (full revoke)** | "汇一政策全清" / "跟汇一没关系了" | `delete_supplier_policy(supplier)` (不传 key) | 删该 supplier 全部 policy |
| **查询 (read)** | "汇一现在什么政策" | `query_supplier_policy(supplier)` | 读现状,准备后续操作 |

### 撤销 / 部分撤销决策流程图

```
老板说 "汇一以后可以退了" / "解除榄菊黑名单"
  ↓
[1] 调 query_supplier_policy(supplier) → 拿到现状
  ↓
[2] 语义解析意图:
    - "以后可以退了"  → 跟"之前不能退"对立 → 这是 UPDATE allow_return=true
    - "解除黑名单"    → 这是 DELETE block_entry (单 key)
    - "政策全清"      → 这是 DELETE 整条
    - "不限制X了"     → 这是 DELETE block_entry (单 key)
  ↓
[3] 二次确认 (任何 DELETE 必走):
    "汇一 删 1 条: allow_return=false → 删 (即变回 true 默认值, 不会被 block_entry 命中)
     对吗?"
  ↓
[4a] UPDATE 路径 (录入):
    → remember_supplier_policy(supplier="汇一", key="allow_return", value=true)
  ↓
[4b] DELETE 单 key 路径:
    → delete_supplier_policy(supplier="汇一", key="block_entry")
  ↓
[4c] DELETE 整条 路径 (整条清空):
    → delete_supplier_policy(supplier="汇一")  // key 留空
  ↓
[5] 落库 → 后续 CreateSession 触发 Apply → 相关 alert 重新评估
```

### 撤销映射表

| 老板的话 | 操作 | tool 调用 |
|---|---|---|
| "汇一以后可以退了" | UPDATE allow_return=true | `remember_supplier_policy(supplier="汇一", key="allow_return", value=true)` |
| "解除榄菊黑名单" | DELETE block_entry | `delete_supplier_policy(supplier="榄菊", key="block_entry")` |
| "汇一不限制入场了" | DELETE block_entry | `delete_supplier_policy(supplier="汇一", key="block_entry")` |
| "汇一政策全清" | DELETE 整条 | `delete_supplier_policy(supplier="汇一")` |
| "汇一跟 X 没关系了" | DELETE 整条 | `delete_supplier_policy(supplier="汇一")` |
| "汇一那个临时不允许进了,现在又让进" | DELETE block_entry (原因在 block_reason) | `delete_supplier_policy(supplier="汇一", key="block_entry")` + `delete_supplier_policy(supplier="汇一", key="block_reason")` |
| "汇一没堆头了" | UPDATE has_duitou=false | `remember_supplier_policy(supplier="汇一", key="has_duitou", value=false)` |
| "拉黑汇一" | UPDATE block_entry=true (新增) | `remember_supplier_policy(supplier="汇一", key="block_entry", value=true, source="user_chat")` |
| "汇一是自采" | UPDATE is_self_procure=true | `remember_supplier_policy(supplier="汇一", key="is_self_procure", value=true)` |

### 关键的"撤销"判定

**"以后可以退了"** 这种否定翻转的句子,优先按 UPDATE 走 (因为 value 翻转为 true/false,跟现有 key 同一行覆盖即可)。
不是 DELETE (DELETE 不会把 value 变 true,只是把 row 删了,下次跑规则会按"该 supplier 无 allow_return 政策" → 默认按规则 NoReturnRule 不命中)。

**"解除黑名单"** 这种语义明确的"删除某条",走 DELETE 单 key。

**"政策全清"** 这种"跟这家没关系了",走 DELETE 整条,慎用!先 query 现状给老板看,确认无误再删。

### 撤销后下游影响 (重点!)

- `DELETE allow_return` → 下次采购单 Apply → NoReturnRule 不命中 (假设有其它 supplier 不让退) → 不再报橙感叹号
- `DELETE block_entry` → 下次采购单 Apply → BlockEntryRule 不命中 → 红感叹号消失
- `DELETE has_duitou` → 下次 Apply → HasDuitouRule 不命中 (总结栏绿贴切标志消失) + HighStockRule 降级 A 不命中
- `DELETE 整条` → 该 supplier 完全"无政策" (跟新供应商等价)

### 二次确认模板 (DELETE 必走)

```
[1] 先 query 拿现状 (避免误删):
    query_supplier_policy("汇一")  → 拿到 [is_self_procure=true, has_duitou=true, allow_return=false]

[2] dry_run 预览删除清单:
    delete_supplier_policy("汇一", key="allow_return", dry_run=true)
    → 返 {action: "dry_run", deleted_count: 1, deleted_keys: ["allow_return"]}

[3] 老板确认 "OK"

[4] 真删:
    delete_supplier_policy("汇一", key="allow_return", dry_run=false)
    → 返 {action: "deleted", deleted_count: 1, deleted_keys: ["allow_return"]}
```

---

## W4.2 关键工具变更

| 工具 | 行为 | 何时调 |
|---|---|---|
| `remember_supplier_policy` | UPSERT (不变) | 录入 / 覆盖 / UPDATE 翻转 |
| `query_supplier_policy` | 读 (不变) | 撤销前必查 |
| `delete_supplier_policy` | **新增** | 撤销单 key / 撤销整条 |
| `list_supplier_keys` | **新增** | 拿 7 个 key 白名单 + 含义,避免 LLM 写错 key |

(详见 `references/tool-reference.md`, 4 个 tool 的完整调用格式)


## Scripts

### `scripts/parse_policy_utterance.py`

**作用**:把老板的一段话解析成 (supplier, key, value) 三元组列表。

**入参(stdin JSON)**:

```json
{
  "utterance": "汇一是自采供应商,堆头费他们出,榄菊不让退",
  "known_suppliers": ["汇一", "榄菊", "金龙鱼"]  // 可选,帮助实体消歧
}
```

**出参(stdout JSON)**:

```json
{
  "items": [
    {"supplier": "汇一", "key": "is_self_procure", "value": true, "confidence": 0.95},
    {"supplier": "汇一", "key": "has_duitou", "value": true, "confidence": 0.85},
    {"supplier": "榄菊", "key": "allow_return", "value": false, "confidence": 0.95}
  ],
  "warnings": ["堆头费他们出 = has_duitou=true,但 '他们' 指代模糊,默认是供应商"]
}
```

> **何时调**:LLM 不确定老板的话怎么拆时,跑脚本先看"字面拆解",LLM 再用语义判断修正。

### `scripts/check_concentration.py`(赫芬达尔指数)

**作用**:算某品类 / 全店的供应商集中度。HHI > 0.25 视为高集中(风险高)。

**入参(stdin JSON)**:

```json
{
  "supplier_share": {"汇一": 0.5, "金龙鱼": 0.3, "福临门": 0.2}
}
```

**出参(stdout JSON)**:

```json
{
  "hhi": 0.38,
  "tier": "high",
  "warning": "HHI 0.38 > 0.25,供应商集中度过高,议价能力风险"
}
```

> **何时调**:老板问"我家供应商是不是太集中",或反回扣自检时。

## References

- `references/key_semantics.md` — 7 个 key 的精确语义 + 合法 value + 误用警告
- `references/anti_kickback_signals.md` — 反回扣危险信号清单(红/黄/绿)
- `references/supplier_tiering.md` — A/B/C/D 供应商分级标准
- `references/utterance_patterns.md` — 老板自然语言 → key 映射规则(更全)

## Common Patterns

### 模式 A:老板说"汇一是自采供应商,堆头他们出"

```
用户: 汇一是自采供应商,堆头他们出
LLM:
  1. 调 invoke_skill(action=load, skill_name=supplier-policy, input=...)
  2. 读 references/key_semantics.md + references/utterance_patterns.md
  3. 拆成 2 条:is_self_procure=true + has_duitou=true
  4. 对每条调 remember_supplier_policy(dry_run=true) 预览
  5. 念给老板: 记下: 汇一 is_self_procure=true (新建);汇一 has_duitou=true (新建)。对吗?
  6. 老板确认 → 调 remember_supplier_policy(dry_run=false) 两次
```

### 模式 B:老板说"榄菊不让退"

```
用户: 榄菊不让退
LLM:
  1. 调 invoke_skill(action=load)
  2. 读 references/key_semantics.md 看 allow_return 语义
  3. 拆:allow_return=false
  4. dry_run=true → 念给老板 → 确认 → 真写
```

### 模式 C:老板问"汇一现在什么政策"

```
用户: 汇一现在什么政策
LLM:
  1. 调 query_supplier_policy(supplier="汇一")
  2. 拿到所有 key+value,展开说给老板
```

### 模式 D:反回扣自检(老板说"价格不对")

```
用户: 这个价不对吧,XX 供应商上周报 5 块,这周怎么变 7 块
LLM:
  1. 调 invoke_skill(action=load, skill_name=supplier-policy, input=...)
  2. 读 references/anti_kickback_signals.md
  3. 命中信号"临时涨价 > 30%" → 危险等级 🔴 高
  4. 调 query_supplier_policy(supplier="XX") 看是否在黑名单
  5. 问老板: 这家供应商近期有政策变动吗?要不要查下别的供应商报价?
```

## Guidelines

- **必须二次确认**:每个 (key, value) 都要 dry_run=true 念给老板,不能跳过
- **白名单为准**:不在 7 个 key 里的(如"delivery_speed")必须拒,告诉老板"只能记这 7 种"
- **反回扣信号优先**:遇到红信号,**先告诉老板**,再继续后续动作
- **不复用 query result 写**:query 拿到的值不要直接当新值,要让老板重新确认

## Keywords

供应商政策, 供应商分级, 防回扣, 反回扣, 自采, 不让退, 堆头, 端架, 进场, 黑名单, 拉黑, 议价, 报价, 异常, 集中度, HHI, 战略供应商, 试用供应商, 临时换, 涨价, 降本
