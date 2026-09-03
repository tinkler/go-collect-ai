# Restock Task 字段说明

> **作用**:LLM 读 `restock_task` 表时,对照本表理解每个字段。
> 数据来源:`internal/restock/store.go` + `types.go` 的 DDL。

## 完整字段

| 字段 | 类型 | 含义 | 计算公式 |
|---|---|---|---|
| `id` | bigint | 主键 | - |
| `branch` | text | 门店号(0001 默认) | - |
| `item_no` | text | SKU | - |
| `item_name` | text | 商品名(从 cube 拉) | - |
| `inv_snapshot` | numeric | 抓取时库存数 | 拉 `t_im_branch_stock.stock_qty` |
| `daily_avg` | numeric | 加权日均 | `0.4 × 昨日 + 0.4 × 7日均 + 0.2 × 30日均` |
| `rop` | numeric | Reorder Point | `max(daily_avg × 1.5, 5)` |
| `priority` | text | 优先级 | `P0 / P1 / P2 / P3`(见 priority_semantics.md) |
| `status` | text | 状态 | `open / acknowledged / settled / cancelled` |
| `suggest_qty` | numeric | 建议补货量 | LLM 算(本 skill 可接管) |
| `supplier_id` | int | 供应商 ID | - |
| `supplier_name` | text | 供应商名 | - |
| `fill_rate` | numeric | 供应商交付率 | `supplier_reliability.fill_rate` |
| `acked_by` | text | 接手人 | - |
| `acked_at` | timestamp | 接手时间 | - |
| `settled_at` | timestamp | 完成时间 | - |
| `cancelled_reason` | text | 取消原因 | - |
| `last_update_at` | timestamp | 最近变更 | - |

## 字段间关系

```
trigger:     inv_snapshot <= rop
priority:    days_until_stockout = inv_snapshot / daily_avg
suggest_qty: daily_avg × lead_days + safety_stock
             上调: × (1 / fill_rate)   // 缺货风险缓冲
             向上取: ceil_unit (默认 12)
```

## 关键状态机

```
open --(员工点 ack)--> acknowledged --(完成补货)--> settled
  |                          |
  |                          +--(手动 cancel)--> cancelled
  +--(超时升级)--> 升级 priority 但状态保持 open
  +--(自动 cancel)--> cancelled (临期/黑名单/滞销)
```

## 编辑权限

DDL 改 schema 需要 migration(脚本:`migrations/xxx_add_restock_*.sql`)。改完需要重启 collect-ai。

## 更新历史

- 2026-09-02 v1.0 初版
