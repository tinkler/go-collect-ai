# freshcheck W3 总结

> **状态**: 后端 W3.1-W3.7 全部完工 (6 步编排 + 7 校验 + 双轨互验 + R1-R5 报表)
> **后端分支**: `feat/freshcheck` (collect-ai)
> **后端 commit**: 6 commits (W3.1 → W3.7)
> **日期**: 2026-09-09
> **剩余**: W3.8 H5 前端 (子 agent) + W3.9 验收 8 场景

---

## 一、Commit 列表

### collect-ai `feat/freshcheck` (6 commits)

| Commit | 阶段 | 净行 |
|---|---|---|
| `f8b6274` | W3.1 周期盘点 CRUD + C8 覆盖率 | +864 |
| `26b8373` | W3.2 倒挤 + 报损剥离 (Step 3+4) | +705 |
| `0b567a1` | W3.3 多筐串联分摊 (Step 5 核心) | +604 |
| `e0a04ad` | W3.4 6 步编排 + Service.RunSettlement | +644 |
| `c176ca7` | W3.5 7 校验 (C1-C6+C8) + alert 写入 + R5 | +467 |
| `d775578` | W3.6 双轨互验 + R3 + W3.7 R1-R4 报表 | +468 |

**总: 6 commits / +3752 行**

---

## 二、模块清单

### 算法 (纯函数, 无 PG/cube)

| 文件 | 内容 | 单测 |
|---|---|---|
| `backflush.go` | Step 3 倒挤 (begin+pur-sale-end) + Step 4 损耗剥离 (快/慢) | 11 PASS |
| `allocate.go` | Step 5 多筐串联 + 主备算法 + 物理约束 (5 iter 重分配) | 8 PASS |
| `reconcile.go` | W3.6 双轨互验 (alloc vs 倒挤偏差 > 5%) + R3 框内对账 | 0 (待 W3.9) |

### 编排 + 校验 (依赖 Store/Cube)

| 文件 | 内容 |
|---|---|
| `settle.go` | Service struct + RunSettlement 6 步编排 (Step 1-6) |
| `validate.go` | W3.5 7 校验: C1 池饱和度 / C2 池外泄漏 / C3 总量守恒 / C4 成本守恒 / C5 退货负数 / C6 周期锁 / C8 盘点缺项 |

### 端点 (24 freshcheck 路由)

| 阶段 | 端点 | perm |
|---|---|---|
| W1.4 | GET /freshcheck/health | public |
| W1.4 | CRUD /freshcheck/config/{sku-map,pool-codes,loss-rates,thresholds,category-tracks} | config:write / settle:read |
| W2.1b | POST /freshcheck/pool/events/{in,out} + GET /freshcheck/pool/{events,state,diff} | pool:write / settle:read |
| W3.1 | CRUD /freshcheck/period/stock + GET /freshcheck/period/stock/coverage | pool:write / settle:read |
| W3.4 | POST /freshcheck/settle/run | settle:run |
| W3.5 | GET /freshcheck/settle/periods/:id/alerts (R5) | settle:read |
| W3.6 | (无新端点, 内部 RunReconciliation) | - |
| W3.7 | GET /freshcheck/settle/periods/:id (R1) | settle:read |
| W3.7 | GET /freshcheck/settle/periods/:id/alloc (R2) | settle:read |
| W3.7 | GET /freshcheck/settle/periods/:id/box (R3) | settle:read |
| W3.7 | GET /freshcheck/settle/loss-trend (R4) | settle:read |

---

## 三、PG 派生表写入

| 表 | 阶段 | 写表方法 |
|---|---|---|
| `freshcheck_settlement` | W3.4 | InsertSettlement (含 idempotency_key UNIQUE) |
| `freshcheck_alloc` | W3.4 + W3.6 | InsertAlloc + UpdateAllocDeviation |
| `freshcheck_box_recon` | W3.6 | InsertBoxRecon (含 UNIQUE 冲突 → ErrDuplicateKey) |
| `freshcheck_alert` | W3.5 | InsertAlert (JSONB payload) |

---

## 四、算法核心 (W3.2 + W3.3)

### Step 3+4 倒挤 + 报损 (backflush.go)

```
backflush  = begin + purchase - normal_sale - end
loss (fast) = purchase × daily_rate × min(shelf_life, days)
loss (slow) = ((begin + end) / 2) × daily_rate × days
pool_adj   = max(backflush - loss, 0)
```

### Step 5 多筐串联 (allocate.go)

```
1. 按 unit_price DESC 排序 pools
2. 主算法: weight = (in - out - spoiled) × confidence
3. 退化: sum_weights <= 0 → 切到备算法 weight = remaining × confidence
4. 物理约束: 5 iter 重分配 (excess 重分给未满 SKU)
5. 串联扣减: remaining[item] -= alloc[item]
```

---

## 五、测试覆盖

### 单测 (W3.2 + W3.3 + W3.4 helper)

| 包 | 测试 | 结果 |
|---|---|---|
| backflush_test.go | 11 场景 (basic/begin0/end0/refund/neg/missing_end/missing_loss/fast/slow/days/summary) | 11/11 PASS |
| allocate_test.go | 8 场景 (1P1S/1PNS/NP1S cascade/物理约束/退化/Summary/跨框/校验) | 8/8 PASS |
| settle_test.go | 5 helper (window/key/sort/bridge/summary) | 5/5 PASS |

### e2e smoke (e2e/freshcheck_smoke.ps1)

- 35/35 PASS (W1 5 + W2 20 + W3.1 10)
- 0 回归

### 待 W3.9 验收

- 8 场景 ALL PASS (强制, docs/freshcheck-rollout.md §7)
- 7 校验 e2e 触发 (C1-C6 + C8)
- 端到端 RunSettlement 跑真实 cube 数据

---

## 六、W3.8 + W3.9 待办

### W3.8 H5 前端 (子 agent)

任务: 在 `F:\weixinapp\supermarket-ai` 写
- `freshcheck-settle.html` 结算触发页
- `freshcheck-report.html` 报表页 (R1-R5)
- `js/freshcheck/settle.js` + `report.js`
- 复用 W2.4/W2.5 的 offline + permission 模式

### W3.9 验收 8 场景

- 场景 1: 周期未到 (C6 block)
- 场景 2: 期末盘点缺项 (C8 warn)
- 场景 3: 池内饱和度超限 (C1 warn)
- 场景 4: 退货超销售 (C5 block)
- 场景 5: 守恒失败 (C3/C4 block)
- 场景 6: 双轨偏差大 (needs_review)
- 场景 7: 跨筐降级 (TODO, W3.6.1)
- 场景 8: 损耗率校准自动触发 (LOSS_CALIB)

---

## 七、暂未实现 (W3.6.1 / W3.9 后)

- 跨筐降级 (out_destination=downgrade): 框关闭 + 目标框虚拟入框
- 损耗率自动校准 (CalibrateLossRate cron)
- 事务: W3.4 逐条 Insert, 失败不阻断 (W3.5/W3.6 alert 写入)
- C5 跨日退货检测 (暂用总 normal_sale < 0 简化)
- C6 动态查 freshcheck_category_track 周期锁天数 (暂硬编码 track_code -> days)
