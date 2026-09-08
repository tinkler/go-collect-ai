# freshcheck H5 前后端 API 契约

> 前端: `F:\weixinapp\supermarket-ai` (已具备 auth.js / api.js / permission.js / auth-guard.js / user-bar.js / weui 1.1.3)
> 后端: collect-ai `internal/freshcheck/http_h5.go` + `http_admin.go`
> 鉴权: 沿用 collect-ai 现有 RBAC (`session:read` / `session:create` / `inventory:view` / `plan:read` / `admin`)
> 新增 perm: `freshcheck:pool:write` (录事件) / `freshcheck:settle:read` / `freshcheck:settle:run` / `freshcheck:config:write` / `freshcheck:override` (超锁豁免)

---

## 0. 鉴权与角色映射

| 角色 | 可见页面 | perm |
|---|---|---|
| owner (店主) | 全部 | `*` |
| manager (店长) | 全部 (除 admin 后台) | `freshcheck:*` |
| buyer (采购) | 报表 + 配置 + 触发结算 | `freshcheck:settle:read`, `freshcheck:settle:run`, `freshcheck:config:write` |
| floor (卖场员工) | 入框登记 + 早晨出框检查 | `freshcheck:pool:write` |
| office (办公室) | 报表 + 异常清单 | `freshcheck:settle:read` |
| cashier (收银) | 无 | (无 freshcheck perm) |

> 跟 RBAC `role_permissions` 表对齐, W1 启动时 seed 4 个新 perm。

---

## 1. API 总览 (RESTful + JSON)

所有路径前缀 `/api/v1/freshcheck/*`。
错误码: `400` 入参错 / `403` 无 perm / `409` 业务冲突 (周期锁超线) / `500` 内部 / `503` cube 不可用。

### 1.1 入框登记 (H5 主流程)
- `POST   /pool/events/in` — 录一次入框
- `POST   /pool/events/out` — 录一次出框
- `POST   /pool/events/in/batch` — 批量入框(同一打称事件多个 SKU)
- `GET    /pool/events?date=YYYY-MM-DD&pool=xxx` — 看某日某框的事件流

### 1.2 出框检查 (早晨)
- `GET    /pool/state?pool=xxx` — 重建某框当前在框集合 (供早班查)
- `GET    /pool/diff?date=YYYY-MM-DD&pool=xxx` — 看昨日框内差值

### 1.3 周期盘点
- `GET    /period/stock?period_id=xxx` — 看某期盘点
- `POST   /period/stock` — 提交/导入盘点 (Excel + JSON 双支持)
- `GET    /period/stock/template` — 导出 Excel 模板 (含生鲜 SKU 列表)

### 1.4 结算
- `POST   /settle/run` — 触发一次结算 (body: track_code, period_end, period_stock)
- `GET    /settle/periods?track=xxx&limit=20` — 看历史结算期
- `GET    /settle/periods/:id` — 看单期 R1 报表
- `GET    /settle/periods/:id/alloc` — 看 R2 分摊明细
- `GET    /settle/periods/:id/box` — 看 R3 框内对账
- `GET    /settle/periods/:id/alerts` — 看 R5 异常清单
- `POST   /settle/periods/:id/recalc` — 重算 (admin)
- `POST   /settle/periods/:id/override` — 超锁豁免 (admin)

### 1.5 配置 (Admin)
- `GET    /config/sku-map` — 生鲜 SKU 映射
- `PUT    /config/sku-map/:item_no` — 改映射
- `GET    /config/pool-codes` — 特价码
- `PUT    /config/pool-codes/:pool_code` — 改特价码
- `GET    /config/loss-rates` — 损耗率
- `PUT    /config/loss-rates/:id` — 改损耗率
- `GET    /config/thresholds` — 业务阈值
- `PUT    /config/thresholds/:key` — 改阈值
- `GET    /config/category-tracks` — 品类轨道
- `PUT    /config/category-tracks/:fresh_category` — 改轨道

### 1.6 损耗率校准
- `GET    /loss-calibrate?status=pending` — 待办
- `POST   /loss-calibrate/:id/action` — 确认 (update_preset / investigate / discard)

### 1.7 R6 日报
- `GET    /daily-report?date=YYYY-MM-DD` — 当日 R6
- (推送: 每日 8:00 自动推送到店长企微 — W4 集成)

### 1.8 告警
- `GET    /alerts?status=open&limit=20` — 告警清单
- `POST   /alerts/:id/override` — 处置告警 (admin)

---

## 2. 详细 API 契约

### 2.1 POST /pool/events/in — 入框登记

**H5 场景**: 员工在条码秤选"1元/斤"档, 放 0.5kg 菠菜, 提交。

**Request**:
```json
{
  "pool_code": "POOL001",
  "item_no": "6977216150192",
  "event_time": "2026-09-08T10:23:15+08:00",  // 业务时间, 缺省=now (前端不传则取服务端)
  "weight_kg": 0.5,                           // 称重模式
  "piece_count": 0,                            // 计件模式
  "operator": "u_floor_01",
  "source": "h5",
  "note": ""
}
```

**Response 201**:
```json
{
  "id": 12345,
  "event_time": "2026-09-08T10:23:15+08:00",
  "recorded_at": "2026-09-08T10:23:18+08:00",
  "confidence": "high",
  "pool_state_after": {                        // 入框后该框当前在框集合
    "in_total_kg": 1.2,                         // 该框当日累计入框
    "items_in": [
      {"item_no": "...", "weight_kg": 0.5},
      ...
    ]
  }
}
```

**错误 409**: `event_time` 早于上次出框检查 → 提示"该时段已闭口, 请确认补录"
**错误 400**: `item_no` 不在 `freshcheck_sku_map` → "非生鲜 SKU"
**错误 400**: `pool_code` 不在 `freshcheck_pool_code` → "未知特价码"

### 2.2 POST /pool/events/out — 出框检查

**H5 场景**: 早晨 7:00 员工检查 1元框, 逐 SKU 填剩余量 + 去向。

**Request**:
```json
{
  "pool_code": "POOL001",
  "event_time": "2026-09-08T07:05:00+08:00",  // 早晨检查时刻
  "items": [
    {
      "item_no": "6977216150192",
      "weight_kg": 0.1,                       // 剩余
      "out_destination": "sold_out"
    },
    {
      "item_no": "6977216150193",
      "weight_kg": 0,
      "out_destination": "spoiled",
      "spoiled_weight_kg": 0.3
    },
    {
      "item_no": "6977216150194",
      "weight_kg": 0.4,
      "out_destination": "downgrade",
      "downgrade_to": "POOL005"               // 转 0.5元框
    },
    {
      "item_no": "6977216150195",
      "weight_kg": 0.2,
      "out_destination": "return_to_shelf"    // 撤回正常货架
    }
  ],
  "operator": "u_floor_01"
}
```

**Response 200**:
```json
{
  "events_created": 4,
  "missing_in_records": [                      // 兜底: 发现无入框的 SKU
    {
      "item_no": "6977216150196",
      "suggested_in_weight": 0.5,             // 估算的入框量
      "suggested_in_time": "2026-09-07T18:00:00+08:00"  // 估算的入框时间
    }
  ],
  "box_diff_kg": 0.05,                          // 框内差值 (待 Step 5)
  "needs_low_confidence": true                  // 有漏录, 全部打 low
}
```

### 2.3 GET /pool/state — 池状态重建

**Request**:
```
GET /pool/state?pool=POOL001&at=2026-09-08T10:00:00+08:00
```

**Response**:
```json
{
  "pool_code": "POOL001",
  "pool_name": "1元/斤",
  "at": "2026-09-08T10:00:00+08:00",
  "items": [
    {
      "item_no": "6977216150192",
      "item_name": "菠菜",
      "in_weight_kg": 0.5,
      "out_weight_kg": 0,
      "current_weight_kg": 0.5,                 // in - out
      "confidence": "high",
      "last_event_time": "2026-09-08T09:00:00+08:00"
    }
  ],
  "in_total_kg": 1.2,
  "out_total_kg": 0,
  "current_total_kg": 1.2
}
```

### 2.4 POST /settle/run — 触发结算

**Request**:
```json
{
  "track_code": "leaf-weekly",
  "period_end": "2026-09-08T07:00:00+08:00",
  "period_stock": [                            // 期末实盘
    {"item_no": "6977216150192", "qty": 5.0, "operator": "u_floor_01"},
    {"item_no": "6977216150193", "qty": 2.5, "operator": "u_floor_01"},
    ...
  ],
  "operator": "u_buyer_01",
  "override": false,
  "override_reason": ""
}
```

**Response 200 (success)**:
```json
{
  "period_id": 888,
  "track_code": "leaf-weekly",
  "window_start": "2026-09-01T07:00:00+08:00",
  "window_end": "2026-09-08T07:00:00+08:00",
  "window_days": 7,
  "summary": {
    "items_count": 35,
    "total_purchase_qty": 120.5,
    "total_normal_sale_amt": 1234.56,
    "total_pool_alloc_amt": 567.89,
    "total_loss_amt": 89.01,
    "total_box_loss_amt": 12.34,
    "total_gross_profit": 234.56,
    "gross_profit_rate": 0.1500
  },
  "alerts": [                                  // 触发 C1-C8 的告警
    {"rule": "C1", "severity": "warn", "entity": "POOL001", "message": "..."}
  ],
  "conservation_ok": true
}
```

**Response 409 (C6 阻断)**:
```json
{
  "error": "period_lock_exceeded",
  "message": "窗口 17 天 > 周期锁 7 天, 需 admin 豁免",
  "track_code": "leaf-weekly",
  "window_days": 17,
  "period_lock_days": 7
}
```

### 2.5 GET /settle/periods/:id — R1 报表

**Response**:
```json
{
  "period_id": 888,
  "track_code": "leaf-weekly",
  "window_start": "2026-09-01T07:00:00+08:00",
  "window_end": "2026-09-08T07:00:00+08:00",
  "settled_at": "2026-09-08T08:00:00+08:00",
  "status": "finalized",
  "items": [
    {
      "item_no": "6977216150192",
      "item_name": "菠菜",
      "begin_qty": 5.0,
      "purchase_qty": 10.0,
      "normal_sale_qty": 7.0,
      "normal_sale_amt": 35.00,
      "pool_alloc_qty": 3.0,
      "pool_alloc_amt": 3.00,                   // 1元/斤
      "end_qty": 2.0,
      "backflush_qty": 6.0,
      "loss_qty": 0.5,
      "box_loss_qty": 0.05,
      "avg_cost": 2.00,
      "sale_cost": 21.10,
      "total_revenue": 38.00,
      "gross_profit": 16.90,
      "gross_profit_rate": 0.4447,
      "confidence": "high"
    }
  ]
}
```

### 2.6 GET /settle/periods/:id/alloc — R2 分摊明细

**Response**:
```json
{
  "period_id": 888,
  "allocs": [
    {
      "pool_code": "POOL001",
      "segment_start": "2026-09-01T07:00:00+08:00",
      "segment_end": "2026-09-08T07:00:00+08:00",
      "item_no": "6977216150192",
      "item_name": "菠菜",
      "in_weight_kg": 12.0,
      "out_weight_kg": 1.0,
      "spoiled_weight_kg": 0.5,
      "weight_diff_kg": 10.5,                    // 框内差值权重
      "pool_pos_qty": 8.0,                       // POS 1元框销量
      "pool_pos_amt": 8.00,
      "share_weight": 0.3500,                    // 35% (在该框池内)
      "alloc_qty": 2.8,
      "alloc_amt": 2.80,
      "backflush_alloc_qty": 3.0,
      "deviation_qty": -0.2,
      "deviation_rate": 0.0667,                  // 6.67% (< 10% 阈值)
      "needs_review": false
    }
  ]
}
```

### 2.7 GET /settle/periods/:id/box — R3 框内对账

**Response**:
```json
{
  "period_id": 888,
  "boxes": [
    {
      "pool_code": "POOL001",
      "pool_name": "1元/斤",
      "segment_start": "2026-09-01T07:00:00+08:00",
      "segment_end": "2026-09-08T07:00:00+08:00",
      "in_total_kg": 35.0,
      "out_total_kg": 25.0,                       // 各去向合计
      "out_sold_out_kg": 20.0,
      "out_spoiled_kg": 2.0,
      "out_return_kg": 1.5,
      "out_downgrade_kg": 1.5,
      "pos_total_kg": 8.0,
      "box_loss_kg": 2.0,                         // 35 - 25 - 8
      "box_loss_amt": 2.00,
      "responsible_user": "u_floor_01",
      "responsible_at": "2026-09-08T07:05:00+08:00",
      "needs_investigate": true,
      "investigate_note": ""
    }
  ]
}
```

### 2.8 GET /alerts — 告警清单

**Request**:
```
GET /alerts?status=open&limit=20&track=leaf-weekly
```

**Response**:
```json
{
  "alerts": [
    {
      "id": 456,
      "rule_code": "C1",
      "severity": "warn",
      "entity_type": "pool",
      "entity_id": "POOL001",
      "message": "池内饱和度 18% > 15% 阈值",
      "payload": {"saturation": 0.18, "pool_pos_qty": 8.0, "sum_alloc": 6.56},
      "status": "open",
      "created_at": "2026-09-08T08:00:00+08:00"
    }
  ]
}
```

### 2.9 POST /loss-calibrate/:id/action — 校准确认

**Request**:
```json
{
  "action": "update_preset",                   // update_preset | investigate | discard
  "new_daily_rate": 0.0600,                    // 仅 update_preset 需要
  "action_note": "近 3 周实测 6%, 调高"
}
```

---

## 3. H5 页面结构 (前端在 `F:\weixinapp\supermarket-ai` 加 5 个页面)

### 3.1 入口
- `index.html` 加菜单项"生鲜免日盘"
- `js/nav.js` 加权限守卫

### 3.2 页面文件
```
F:\weixinapp\supermarket-ai\
  freshcheck-bin.html         # 入框登记 (H5 主页面, 大按钮 + 称重 input)
  freshcheck-pool.html        # 早晨出框检查 (按 SKU 列表)
  freshcheck-stock.html       # 周期盘点录入 (Excel 导入 + 简易编辑)
  freshcheck-settle.html      # 触发结算 + 看历史 (admin/buyer)
  freshcheck-report.html      # R1-R5 报表 + 异常清单 (buyer/office)
  js/freshcheck/
    api.js                    # 上面 8 类接口的 fetch 封装
    pool.js                   # 入框/出框业务逻辑
    state.js                  # 池状态重建展示
    settle.js                 # 结算触发 + 进度
    report.js                 # 表格渲染 + 导出
    stock.js                  # Excel 解析 (用 SheetJS)
  css/freshcheck.css          # 跟 weui 1.1.3 兼容
```

### 3.3 离线录入
- 入框/出框事件先存 `localStorage` 队列
- 提交时先尝试 `POST`, 失败转后台重试 (3s/30s/5min 退避)
- 重试成功: 标记 `synced=true`, 弹 toast"已补传"
- 失败超过 24h: 标 `synced=false` 红字提示, 阻断下次录入

### 3.4 RBAC 守卫
```js
// 每个页面底部加
require(['freshcheck:pool:write']);  // 没权限跳 login.html
// 按钮加 data-perm="freshcheck:settle:run"
Perm.applyTo(scope);
```

---

## 4. WebSocket / 推送 (W4)

### 4.1 R6 早报推送
- 后端: `internal/wecom/`
- 时间: 每日 8:00 cron
- 内容: 昨日漏登 / 框损 / 周期锁超线
- 渠道: 店长企微 (沿用现有 `wecom.SendAppChat`)

### 4.2 结算完成推送 (W3 末)
- 时间: 结算 run 完成后
- 内容: 期间毛利 + 异常数 + R1 链接
- 渠道: 老板企微

### 4.3 实时事件 (W4 可选)
- 入框事件触发企微群通知: **不推荐** (噪音大)
- 改: 早晨出框检查时 R6 早报一次汇总

---

## 5. 测试契约

### 5.1 端到端
- `e2e/freshcheck/run_basic_settle.sh` — 走完一次周结
- 准备 3 个测试 SKU + 1 个特价码, 跑入框/出框/盘点/结算
- 验证: R1 守恒 + R2 分摊正确 + R3 框损产生

### 5.2 验收场景 (需求 §7)
- `e2e/freshcheck/case01_weekly_3sku.sh` — 场景 1
- `e2e/freshcheck/case02_monthly_pool_change.sh` — 场景 2
- `e2e/freshcheck/case03_period_lock.sh` — 场景 3
- `e2e/freshcheck/case04_downgrade.sh` — 场景 4
- `e2e/freshcheck/case05_missing_record.sh` — 场景 5
- `e2e/freshcheck/case06_cross_window_refund.sh` — 场景 6
- `e2e/freshcheck/case07_sync_diff.sh` — 场景 7
- `e2e/freshcheck/case08_idempotency.sh` — 场景 8

---

**下一份**:`docs/freshcheck-rollout.md` — W1-W4 实施计划
