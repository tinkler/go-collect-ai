# 老板自然语言 → Policy 映射规则

> **作用**:LLM 听到老板一段话时,套用本表拆成 (supplier, key, value) 三元组。
> 拆完再用 `references/key_semantics.md` 二次校验 value 类型。

## 规则 1:实体识别(供应商名)

老板的话里,第一个**专有名词**(汇一/榄菊/金龙鱼/福临门等)通常是供应商名。

**消歧技巧**:
- "汇一" 后面跟"自采" → 供应商=汇一
- "榄菊" 后面跟"不让退" → 供应商=榄菊
- 多个供应商,每个动作单独拆:
  - "汇一自采,榄菊不让退" → 2 条 policy

## 规则 2:动作 → key 映射

### 自采 / 直采类
| 老板话 | key | value | 置信度 |
|---|---|---|---|
| "自采" | is_self_procure | true | 0.95 |
| "我自己进货" | is_self_procure | true | 0.95 |
| "自营" | is_self_procure | true | 0.90 |
| "直采" | is_self_procure | true | 0.85 |
| "厂方直发" | is_self_procure | true | 0.85 |
| "从他那进" | (不写自采) | - | - |

### 退货类
| 老板话 | key | value | 置信度 |
|---|---|---|---|
| "不让退" | allow_return | false | 0.95 |
| "不能退" | allow_return | false | 0.95 |
| "无理由不退" | allow_return | false | 0.95 |
| "售出概不退换" | allow_return | false | 0.95 |
| "可以退" | allow_return | true | 0.90 |
| "7 天无理由" | allow_return | true | 0.90 |
| "临期可退" | allow_return | true | 0.90 |

### 堆头类
| 老板话 | key | value | 置信度 |
|---|---|---|---|
| "堆头他们出" | has_duitou | true | 0.95 |
| "供应商承担堆头" | has_duitou | true | 0.95 |
| "我们出堆头" | has_duitou | false | 0.95 |
| "店方出" | has_duitou | false | 0.85 |

### 端架类
| 老板话 | key | value | 置信度 |
|---|---|---|---|
| "端架他们出" | has_duanjia | true | 0.95 |
| "供应商承担端架" | has_duanjia | true | 0.95 |
| "我们出端架" | has_duanjia | false | 0.95 |

### 黑名单类
| 老板话 | key | value | 备注 |
|---|---|---|---|
| "以后不进" | block_entry | true | 同时写 block_reason="永久" |
| "拉黑" | block_entry | true | 同时写 block_reason="永久" |
| "黑名单" | block_entry | true | 同时写 block_reason="永久" |
| "临时不让进" | block_entry | true | block_reason="临时" |
| "暂时别进了" | block_entry | true | block_reason="临时" |

### 备注类
| 老板话 | key | value |
|---|---|---|
| 任何补充说明 | note | 原文 |
| "汇一 2026-09 起调价到 5.5" | note | "2026-09 起调价到 5.5" |

## 规则 3:复合句拆分

```
"汇一是自采供应商,堆头费他们出,以后不进货 XX"
```

LLM 应该拆成:
- supplier=汇一, key=is_self_procure, value=true
- supplier=汇一, key=has_duitou, value=true
- supplier=XX, key=block_entry, value=true, block_reason="永久"

**每条都 dry_run=true 念给老板**。

## 规则 4:置信度低时问老板

如果某动作的映射不在上表,或者语义模糊(比如"汇一要换"→ 换什么?),LLM 应该:
1. 拒绝自己映射
2. 问老板:"你说'汇一要换'是要换 supplier 还是 policy?"

## 规则 5:不写法的"话"

以下情况**不**写 policy:
- "汇一最近货不好" → 进 note,不改 key
- "汇一老板态度差" → 不写(主观评价,易起争议)
- "汇一欠我钱" → 不写(应进应收应付表)
- "汇一价格比 XX 便宜" → 不写(数字会变,改 block_reason)

## 编辑权限

老板和运营可加新映射(比如方言 / 行业黑话)。改完即生效。

## 更新历史

- 2026-09-02 v1.0 初版
