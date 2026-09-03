# Query/Insert Tools(8 个)

> 配合 SKILL.md 使用。LLM 跑规则时,**调这些 tool 查数据 + 落库**。Go 端实现见 `internal/agent/tools/purchase_alert.go`。

---

## 工具 1: query_supplier_policy

### 用途
查某 supplier 的所有政策 (key-value) 或查单个 key。

### 入参
```json
{
  "supplier": "汇一",       // 必填
  "key": "block_entry"      // 可选, 不传返该 supplier 的所有 policy
}
```

### 出参
```json
// 单 key 模式
{
  "key": "block_entry",
  "value": true,
  "source": "feishu",
  "updated_at": "2026-09-01T10:30:00Z"
}

// 全部模式
{
  "policies": [
    {"key": "is_self_procure", "value": true, ...},
    {"key": "has_duitou", "value": true, ...},
    {"key": "allow_return", "value": false, ...}
  ]
}
```

### 找不到
- 单 key: 返 `null`
- 全部: 返 `{policies: []}` (不报错)

### Go 端实现
读 `supplier_policy` 表,按 `supplier_name, key` 查。

---

## 工具 2: query_promotion_fee

### 用途
查 supplier 当前在期内的所有 promotion_fee (堆头/端架/快讯 等)。

### 入参
```json
{
  "supplier": "汇一",       // 必填
  "now": "2026-09-03T12:00:00Z"   // 可选, 默认服务器时间
}
```

### 出参
```json
{
  "promos": [
    {
      "id": 5,
      "kind": "堆头",
      "amount": 5000.0,
      "period_start": "2026-09-01",
      "period_end": "2026-10-15",
      "note": "汇一堆头 9-10 月"
    }
  ]
}
```

### 找不到
- `{promos: []}`

### Go 端实现
读 `promotion_fee` 表,`period_start <= now <= period_end`。

---

## 工具 3: query_special_calendar

### 用途
查接下来 N 天的节假日/促销/季节切换日。

### 入参
```json
{
  "now": "2026-09-03T12:00:00Z",   // 可选
  "lead_days": 90                   // 必填, 查 [now, now+lead_days]
}
```

### 出参
```json
{
  "events": [
    {
      "id": 3,
      "date": "2026-09-25",
      "type": "holiday",       // holiday / promo / season_start / season_end / blackout
      "name": "中秋节",
      "lead_days": 7,
      "note": ""
    }
  ]
}
```

### Go 端实现
读 `special_calendar` 表,`date >= now AND date < now + lead_days`。

---

## 工具 4: query_app_settings

### 用途
查系统配置 (阈值/分类白名单)。

### 入参
```json
{
  "key": "high_stock_threshold"  // 必填
}
```

合法 key:
- `high_stock_threshold` (number) — 高库存阈值
- `low_movement_threshold_30d` (number) — 难消化阈值(预留,W4.2 用)
- `duitou_kinds` (JSONB array) — 堆头 kind 白名单
- `others_kinds` (JSONB array) — 快讯 kind 白名单

### 出参
```json
{
  "key": "high_stock_threshold",
  "value": 50,
  "updated_at": "2026-09-01T10:00:00Z"
}
```

### 找不到
- 返 `null`

### Go 端实现
读 `app_settings` 表。

---

## 工具 5: query_sku_stock

### 用途
查 SKU 当前库存(覆盖 row.stock_qty 字段,防止 SkuMatcher 时是旧值)。

### 入参
```json
{
  "item_no": "000123"        // 或
  "barcode": "6901234567890" // 二选一
}
```

### 出参
```json
{
  "item_no": "000123",
  "item_name": "可口可乐 330ml",
  "stock_qty": 47.0,
  "branch_no": "0001",
  "as_of": "2026-09-03T12:00:00Z"
}
```

### 找不到
- 返 `null`

### Go 端实现
走 `business.Gateway` 调 cube `t_im_branch_stock` 表 (数据源 hbpos/erp)。

---

## 工具 6: query_sku_sales

### 用途
查 SKU 30/60/90 天销量(难消化规则用,W4.2 启用)。

### 入参
```json
{
  "item_no": "000123",
  "days": 30                  // 30 / 60 / 90
}
```

### 出参
```json
{
  "item_no": "000123",
  "item_name": "可口可乐 330ml",
  "days": 30,
  "total_qty": 12.0,
  "total_money": 30.0,
  "daily_avg": 0.4
}
```

### 找不到
- 返 `null`

### Go 端实现
走 `business.Gateway` 调 cube `siss_saleflow` view (30 天 / 60 天 / 90 天窗口)。

---

## 工具 7: insert_purchase_alert

### 用途
LLM 决定报 alert 后,落库到 `purchase_session_alert` 表。

### 入参
```json
{
  "session_id": "9b1deb4d-...",
  "row_id": 0,                          // 0 = session 级, >0 = 行内
  "rule": "high_stock",                 // 见 7-rules.md
  "severity": "warn",                   // block / warn / info
  "category": "warn",                   // block / warn / info / highlight_dui / highlight_others
  "message": "商品 [可口可乐] 库存 47,..."  // 中文
  "dedup_key": "high_stock:row_1:2026-09-03"  // 可选, 防止同 session+row+rule 重复插入
}
```

### 出参
```json
{
  "alert_id": 11,
  "created_at": "2026-09-03T12:00:00Z"
}
```

### Go 端实现
INSERT `purchase_session_alert` (session_id, row_id, rule, severity, category, message)。
如果 `dedup_key` 存在,先 SELECT 查重,有就 UPDATE 替换。

---

## 工具 8: update_analysis_status

### 用途
更新 session 的 analysis_status,前端轮询用。

### 入参
```json
{
  "session_id": "9b1deb4d-...",
  "status": "done",                     // pending / running / done / failed
  "error": ""                            // 可选, status=failed 时填原因
}
```

### 出参
```json
{
  "session_id": "9b1deb4d-...",
  "analysis_status": "done",
  "analysis_at": "2026-09-03T12:00:05Z"
}
```

### Go 端实现
UPDATE `parse_session SET analysis_status, analysis_at, analysis_error`。

---

## 工具 9: `query_return_order` (W4.4 新, 等 cube 数据源)

**用途**: purchase-alert skill 跑 "未审批退货单" 规则 (规则 8, `pending_return`)。
**数据源**: cube 端 `t_rm_returnflow` (HBPoS) 或其它数据源, **业务字段名** (bill_no / supplier_name / item_no / item_name / qty / return_money / status / create_date / reason)。**严禁直接 import parser/agent**, 必须经 `business.Gateway.RawQuery` 或 `BizExecutor.SearchReturnsBySupplier`。

### 入参

```json
{
  "supplier": "汇一",      // 必填, 过滤供应商
  "status": "pending",    // 可选, 过滤状态: pending|approved|rejected, 留空查所有
  "days": 30              // 可选, 窗口天数, 默认 30, 取值 7/30/60/90
}
```

### 出参

```json
{
  "supplier_name": "汇一",
  "status": "pending",
  "days": 30,
  "count": 2,
  "total_money": 856.50,
  "returns": [
    {
      "bill_no": "TH202609030001",
      "supplier_name": "汇一",
      "item_no": "6901234567890",
      "item_name": "可口可乐 330ml",
      "qty": 12,
      "return_money": 36.00,
      "status": "pending",
      "create_date": "2026-09-01",
      "reason": "近效期"
    }
  ],
  "not_available": false,
  "hint": ""
}
```

### 降级语义 (重要!)

- `not_available=true` + `hint` 字段有提示 → **cube 数据源未接入 / 未配置 / 调用失败**, LLM **必须降级, 不报 pending_return 规则** (避免误报)。
- `not_available=true` 触发场景:
  1. Go 端 Fn 注入为 nil (cube 数据源压根没接)
  2. `configs/mappings.yaml` 没配 `entities.returns` 段
  3. cube 端 `t_rm_returnflow` cube 不存在
  4. cube 查询超时 / 物理表 schema 不对
- 任何时候 LLM 看到 `not_available=true` → 跳过规则 8, 不报 alert, 继续跑规则 1-7。
- (W4.4 真实接入后, `not_available=false` 才是常态)

### Go 端实现

- 文件: `internal/agent/tools/purchase_alert.go` `QueryReturnOrder(QueryReturnOrderFn)`
- 签名: `func(ctx, supplier, status, days) ([]ReturnOrder, string, error)`
- 第二个 string 返回 = hint, 非空时 LLM 走降级路径
- **未 wire** 阶段: runner.go 不注册此 tool (跟其他 5 个 purchase_alert tool 保持一致), **等 mvs_1b47f8887e3c416195f506869c7d4bd8 加完 cube + mapping.yaml 后一次性 wire 6 个 tool 进去**。
- **wire 时**: 在 `cmd/server/main.go` 实现 `QueryReturnOrderFn`, 内部调 `gateway.RawQuery("t_rm_returnflow", ...)` 或 `executor.SearchReturnsBySupplier(...)`。**严禁直接 import parser/agent**。

---

## 工具调用建议顺序

LLM 跑一个 session 的 8 规则:
1. **批量前置查询(并行)**: `query_supplier_policy(supplier)` (拿全 keys) + `query_promotion_fee(supplier, now)` + `query_special_calendar(now, 90)` + `query_app_settings("high_stock_threshold")` + `query_app_settings("duitou_kinds")` + `query_app_settings("others_kinds")`
2. **逐行循环**: 命中规则时,只调必要的补充查询(比如 high_stock 降级 B 才查 `query_sku_sales`)
3. **总结栏 (session 级, row_id=0)**:
   - holiday_lead: `query_special_calendar` (复用步骤 1 结果)
   - has_duitou: 复用 promotion_fee
   - **pending_return (W4.4 新)**: 对 session 内每个 supplier 调 `query_return_order(supplier, status="pending", days=30)`, 命中 → 1 条 warn, **不报 row**。
4. **批量落库**: 把 alerts 收齐,`insert_purchase_alert` 多次调用
5. **收尾**: `update_analysis_status(session_id, "done")`

总 tool calls: ~12-18 次(前置 6 + 5-7 次 insert + 0-2 次 pending_return + 1 次 update),**所有 query 互不依赖,可并发**。
