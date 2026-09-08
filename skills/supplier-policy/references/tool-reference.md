# 工具参考: supplier-policy skill 用的 4 个 tool

> 配合 SKILL.md 使用。LLM 跑撤销/录入/查询时按本文件调 tool。

---

## 1. query_supplier_policy (读)

### 用途
查某 supplier 全部 policy 或按 key 过滤。

### 入参
```json
{
  "supplier": "汇一",          // 必填
  "key": "block_entry"        // 可选, 不传返全部
}
```

### 出参
```json
{
  "policies": [
    {
      "key": "is_self_procure",
      "value": true,
      "source": "user_chat",
      "chat_id": "c1",
      "updated_at": "2026-09-01T10:00:00Z"
    }
  ],
  "count": 1
}
```

### 找不到
- 返 `{policies: [], count: 0}` (不报错)

---

## 2. remember_supplier_policy (UPSERT 写)

### 用途
录一条 policy (新建或覆盖现有同 supplier+key)。

### 入参
```json
{
  "supplier": "汇一",
  "key": "is_self_procure",   // 必填, 必须是 list_supplier_keys 列出的 7 个之一
  "value": true,              // bool / string / number / object
  "dry_run": true,            // 二次确认模式
  "source": "user_chat",      // 可选, 默认 user_chat
  "chat_id": "c1",            // 可选, 企微群溯源
  "message_id": "m1"          // 可选, 幂等用
}
```

### 出参
```json
{
  "supplier": "汇一",
  "key": "is_self_procure",
  "value": true,
  "action": "created",         // "dry_run" | "created" | "updated" | "unchanged"
  "previous_value": null,
  "updated_at": "2026-09-03T12:00:00Z"
}
```

### 关键行为
- 同 (supplier, key) 二次写入会**覆盖** (UPSERT)
- 7 个 key 白名单: `is_self_procure` / `allow_return` / `has_duitou` / `has_duanjia` / `block_entry` / `block_reason` / `note`
- 不在白名单 → 报错 (LLM 应调 `list_supplier_keys` 查)
- `dry_run=true` 只预览不入库 (二次确认用)

---

## 3. delete_supplier_policy (撤销 W4.2 新增)

### 用途
删某 supplier 的 1 条或全部 policy。

### 入参
```json
{
  "supplier": "汇一",          // 必填
  "key": "block_entry",       // 可选: 不传=删全部, 传=只删这一个 key
  "dry_run": true             // 二次确认模式
}
```

### 出参
```json
{
  "supplier": "汇一",
  "action": "deleted",         // "dry_run" | "deleted" | "not_found"
  "deleted_count": 1,
  "deleted_keys": ["block_entry"]
}
```

### 关键行为
- `key` 不传 → 删该 supplier 全部 policy (整条清空,慎用)
- `key` 传 → 只删这一个 key
- 找不到 → `action="not_found"`,不报错
- `dry_run=true` 返待删清单不入库
- DELETE 后下游 (purchase-alert 7 规则) 立即生效:下次跑 Apply 不再命中该 key

### 撤销决策 (W4.2 SKILL.md 主流程)
- "解除 X 黑名单" / "不限制 X 了" → DELETE 单 key (`block_entry`)
- "X 政策全清" / "跟 X 没关系了" → DELETE 整条
- "X 以后可以退了" → 不是 DELETE (因为 value 翻转为 true,不是删 allow_return row) → 走 UPSERT
- "X 没堆头了" → 不是 DELETE → UPSERT `has_duitou=false`

---

## 4. list_supplier_keys (W4.2 新增,白名单查询)

### 用途
列 7 个允许的 key + 含义 + value 类型 + 示例。LLM 解读老板话时参照,避免写错 key。

### 入参
```json
{}
```

### 出参
```json
{
  "keys": [
    {
      "key": "is_self_procure",
      "value_type": "bool",
      "description": "是否自采供应商(老板自己进货,不入批发流程)",
      "examples": "true=自采, false=他采"
    },
    {
      "key": "allow_return",
      "value_type": "bool",
      "description": "是否支持退货(合同条款, 影响 no_return 规则)",
      "examples": "true=可退, false=不退 (no_return alert 触发)"
    },
    {
      "key": "has_duitou",
      "value_type": "bool",
      "description": "是否签了堆头陈列",
      "examples": "true=签了, 影响 has_duitou 规则 + high_stock 降级 A"
    },
    {
      "key": "has_duanjia",
      "value_type": "bool",
      "description": "是否签了端架陈列",
      "examples": "true=签了"
    },
    {
      "key": "block_entry",
      "value_type": "bool",
      "description": "是否限制入场(老板不想再跟这家做生意, 硬阻断, 永不降级)",
      "examples": "true=黑名单, false=正常"
    },
    {
      "key": "block_reason",
      "value_type": "string",
      "description": "限入场原因 (跟 block_entry=true 配套)",
      "examples": "返点高 / 价格乱 / 老板不信任"
    },
    {
      "key": "note",
      "value_type": "string",
      "description": "自由备注 (老板说的原话/上下文, 给以后的人工看)",
      "examples": "老板 2026-09-03 飞书群消息记录"
    }
  ]
}
```

---

## 调用顺序建议

LLM 跑 supplier-policy 一次任务:

### 录入 (UPSERT):
1. `list_supplier_keys` → 确认 7 个 key 含义
2. `query_supplier_policy(supplier)` → 拿现状 (看是否已存在)
3. `remember_supplier_policy(dry_run=true)` → 预览
4. 老板确认 OK
5. `remember_supplier_policy(dry_run=false)` → 真写

### 撤销单 key:
1. `query_supplier_policy(supplier, key=...)` → 确认存在 + 拿当前 value
2. `delete_supplier_policy(supplier, key=..., dry_run=true)` → 预览
3. 老板确认
4. `delete_supplier_policy(supplier, key=..., dry_run=false)` → 真删

### 撤销整条 (慎用):
1. `query_supplier_policy(supplier)` → 拿全部 key
2. 给老板看清单 ("汇一有 3 条 policy, 你确定全清吗?")
3. 老板确认
4. `delete_supplier_policy(supplier, dry_run=false)` → 真删
