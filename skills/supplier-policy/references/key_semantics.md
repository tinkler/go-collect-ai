# Supplier Policy 7 Key 语义详解(2026-Q3)

> **作用**:LLM 在解析老板自然语言 / 解释已存 policy 时,必须看这张表,避免写错 key 或误用 value。
>
> **白名单来源**:`internal/agent/tools/policy.go:allowedPolicyKeys`

## 7 个 Key 详解

### 1. `is_self_procure` (是否自采)

| 维度 | 内容 |
|---|---|
| 语义 | 老板自己进货(不通过这个供应商的渠道) |
| value type | `boolean` |
| true 含义 | 这个"供应商"其实是店老板自己的进货渠道,不算严格意义的供应商 |
| false 含义 | 正常供应商关系 |
| 典型话 | "自采" / "我自己进货" / "自营" / "直采" / "厂方直发" |
| 反面例子 | 不应该记 `supplier` 为"自采"(那是个动作,不是供应商名) |

### 2. `allow_return` (是否允许退货)

| 维度 | 内容 |
|---|---|
| 语义 | 残次 / 临期 / 滞销 商品是否能退给供应商 |
| value type | `boolean` |
| true 含义 | 可以退(进价回滚或换货) |
| false 含义 | 不可以退(只能自己消化) |
| 典型话 | true: "可以退" / "7 天无理由" / "临期可退";false: "不让退" / "不能退" / "无理由不退" / "售出概不退换" |
| 关联 | 配合 `block_entry` 看:不让你进 ≠ 不让退 |

### 3. `has_duitou` (是否供应商承担堆头费)

| 维度 | 内容 |
|---|---|
| 语义 | 货架端头陈列费,谁出钱 |
| value type | `boolean` |
| true 含义 | 供应商承担(常见合作模式) |
| false 含义 | 店老板自己承担(自己争取位置) |
| 典型话 | true: "堆头他们出" / "供应商出堆头";false: "我们出堆头" / "店方出" |
| 误用 | ❌ 不能用来记"堆头费金额",金额走 `promotion_fee` 表 |

### 4. `has_duanjia` (是否供应商承担端架费)

| 维度 | 内容 |
|---|---|
| 语义 | 货架侧端陈列费,谁出钱 |
| value type | `boolean` |
| true 含义 | 供应商承担 |
| false 含义 | 店老板自己承担 |
| 典型话 | true: "端架他们出";false: "我们出端架" |

### 5. `block_entry` (是否黑名单 / 拒绝进货)

| 维度 | 内容 |
|---|---|
| 语义 | 是否**永久**或**临时**拒绝该供应商的商品进店 |
| value type | `boolean` |
| true 含义 | 不进这家货 |
| false 含义 | 正常合作(默认值) |
| 典型话 | true: "不进" / "拉黑" / "黑名单" / "以后不进货" / "临时不让进" |
| 关联 | 临时黑名单应同时写 `block_reason`=`临时` + 用 `note` 写原因 |

### 6. `block_reason` (黑名单原因)

| 维度 | 内容 |
|---|---|
| 语义 | 解释为什么 block,帮助后续员工判断 |
| value type | `string` |
| 典型值 | "临时" / "质量问题" / "价格太高" / "老板个人原因" / "已换供应商" |
| 关联 | 仅当 `block_entry=true` 时有意义 |

### 7. `note` (备注)

| 维度 | 内容 |
|---|---|
| 语义 | 任意补充信息,自由文本 |
| value type | `string` (单条) 或 `array of string` (多条) |
| 用途 | 老板的话原文 / 特殊规则 / 备忘 |
| 示例 | "汇一 2026-09-01 起调价,新价格 5.5 元/瓶" |

## 写入时的硬规则(LLM 必须遵守)

1. **7 个 key 之一**:不在白名单里直接拒,告诉老板"只能记这 7 种:[is_self_procure, allow_return, has_duitou, has_duanjia, block_entry, block_reason, note]"
2. **value 类型匹配**:
   - `is_self_procure` / `allow_return` / `has_duitou` / `has_duanjia` / `block_entry` 必须是 `true` 或 `false`
   - `block_reason` 必须是 `string`
   - `note` 可以是 `string` 或 `string[]`
3. **同一 (supplier, key) 覆盖**:再次写入会覆盖旧值,旧值会通过 `previous_value` 字段返回
4. **dry_run 强制**:每次写入都先 dry_run=true,等老板点头再 dry_run=false

## 编辑权限

老板可加新 key(比如 `delivery_days`、`price_floor`)。需要:
1. 改 `internal/agent/tools/policy.go:allowedPolicyKeys`
2. 重启 collect-ai(Go 端硬编码,不像 skill 可热更新)
3. 同步更新本文件

## 更新历史

- 2026-09-02 v1.0 初版
