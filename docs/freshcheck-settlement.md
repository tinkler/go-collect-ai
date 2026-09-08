# freshcheck 周期结算引擎 — 6 步流程 + 8 条校验

> 配套:`docs/freshcheck-architecture.md` §四、`docs/freshcheck-data-model.md` §8-13。
> 入口: `internal/freshcheck/settle.go` 的 `Service.RunSettlement(trackCode, periodEnd)`。

---

## 一、6 步流程概览

```
┌─ Step 1: 窗口确定 ───────────────────────────────────┐
│  起点=上次结算终点, 终点=本次盘点时点                  │
│  校验: 窗口 ≤ track.period_lock_days (C6)             │
└─────────────────────────────────────────────────────┘
                        ↓
┌─ Step 2: 流水切窗 + 退货回冲 ─────────────────────────┐
│  正常/特价/采购按 oper_date 落窗                       │
│  退货按 flow_id 反查原单号 → 归入原窗口冲减              │
│  跨期: 若原窗口已结算, 生成"原窗口调整" 重算            │
└─────────────────────────────────────────────────────┘
                        ↓
┌─ Step 3: 倒挤计算 (每 SKU) ──────────────────────────┐
│  backflush = begin + purchase − normal_sale − end      │
│  正常销售含: 原 SKU 货号上的全部销售 (含临时促销)        │
└─────────────────────────────────────────────────────┘
                        ↓
┌─ Step 4: 报损剥离 (每 SKU, 按品类参数化) ────────────┐
│  快: loss = purchase × daily_rate × min(life, days)    │
│  慢: loss = avg_in_stock × daily_rate × days           │
│  修正特价消耗 = max(backflush − loss, 0)                │
└─────────────────────────────────────────────────────┘
                        ↓
┌─ Step 5: 池内分摊 (多筐串联, 按价格优先级) ─────────┐
│  按 pool.priority_rank DESC 串联处理 (高价先)         │
│  每框每时段: 框内差值法 / 倒挤回退 权重重算              │
│  物理约束: 分摊量 ≤ 倒挤剩余, 超出截断重分配             │
│  串联扣减: 高价框分摊后扣减 SKU 倒挤剩余                 │
└─────────────────────────────────────────────────────┘
                        ↓
┌─ Step 6: 毛利计算 (每 SKU) ──────────────────────────┐
│  sale_cost = (normal + alloc + loss) × avg_cost        │
│  total_revenue = normal_amt + alloc_amt                 │
│  gross_profit = total_revenue − sale_cost               │
│  gross_profit_rate = gross_profit / total_revenue       │
│  守恒校验 C3/C4, 失败则 transaction rollback           │
└─────────────────────────────────────────────────────┘
```

## 二、Step 1: 窗口确定

### 2.1 入口参数
```go
type SettleRequest struct {
    BranchNo    string
    TrackCode   string                       // 'leaf-weekly' ...
    PeriodEnd   time.Time                    // 本次盘点时点
    PeriodStock []PeriodStockInput           // 期末实盘
    Operator    string
    Override    bool                         // 超期豁免标志
    OverrideReason string                    // 豁免原因
}
```

### 2.2 流程
1. **分布式锁** (PG advisory lock): `pg_advisory_xact_lock(hashtext(track_code))` 防止同品类并发结算。
2. 查 `freshcheck_category_track` 取 `last_settle_at` (= 起点) 和 `period_lock_days`。
3. 首次结算: `last_settle_at` 为 NULL → 取 SKU 映射中 `created_at` 最早者作起点(初始化时)。
4. **C6 校验**: `period_end - last_settle_at > period_lock_days` → 阻断(除非 `Override=true` 且写 `freshcheck_alert`)。
5. 写 `freshcheck_settlement` 的 `period_id` (序列) 和 `window_*` 字段。

### 2.3 伪代码
```go
func (s *Service) RunSettlement(ctx, req) (*SettleResult, error) {
    tx, _ := s.pool.Begin(ctx); defer tx.Rollback()
    var lockKey int32 = hash32(req.TrackCode)
    tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockKey)

    track := s.Store.GetTrack(ctx, req.BranchNo, req.TrackCode)
    windowStart := track.LastSettleAt
    if windowStart == nil {
        windowStart = s.Store.EarliestSkuMapAt(ctx, req.BranchNo, req.TrackCode)
    }
    windowEnd := req.PeriodEnd
    days := int(windowEnd.Sub(*windowStart).Hours()/24) + 1

    // C6 周期锁
    if days > track.PeriodLockDays {
        s.Store.WriteAlert(ctx, &Alert{
            RuleCode: "C6",
            Severity: "block",
            EntityType: "category",
            EntityId: req.TrackCode,
            Message: fmt.Sprintf("窗口 %d 天 > 周期锁 %d 天", days, track.PeriodLockDays),
        })
        if !req.Override {
            return nil, ErrPeriodLockExceeded
        }
    }

    periodID := s.Store.AllocPeriodID(ctx)
    // ... 继续 Step 2-6
}
```

## 三、Step 2: 流水切窗 + 退货回冲

### 3.1 输入数据来源 (全部走 cube 聚合)

| 数据 | cube 端查询 | 落窗字段 |
|---|---|---|
| 正常销售 | `sales WHERE item_no IN (sku_map.item_no) AND oper_date IN [window]` | `normal_sale_qty/amt` |
| 特价销售 | `sales_with_refund WHERE item_no IN (pool_code.pool_code) AND oper_date IN [window]` | `pool_pos_qty/amt`(进 alloc) |
| 采购入库 | `purchases WHERE item_no IN (sku_map.item_no) AND oper_date IN [window]` | `purchase_qty` |
| 期末盘点 | `freshcheck_period_stock WHERE period_id = $period` | `end_qty` |
| 退货原单号 | `sales_with_refund` 退货行的 `refund_origin_flow_id` | 重算原窗口 |

### 3.2 退货回冲算法

```go
// 1. 本窗口销售流水中, 找出所有退货行 (sale_way='refund')
refundsInWindow := cube.Query(`
    SELECT flow_id, item_no, sale_qnty, sale_money, refund_origin_flow_id
    FROM sales_with_refund
    WHERE oper_date >= $1 AND oper_date <= $2
      AND sale_way = 'refund'
`)

// 2. 每个退货: 反查原单号, 找原交易日
for r := range refundsInWindow {
    origin := cube.Query(`
        SELECT oper_date, flow_id
        FROM sales_with_refund
        WHERE flow_id = $r.refund_origin_flow_id
    `)
    if origin == nil {
        s.Alert("ORIGIN_NOT_FOUND", r)  // 原单已归档, 异常
        continue
    }

    if origin.OperDate < windowStart {
        // 3a. 原窗口已结算, 找其 period_id
        originPeriod := s.Store.FindSettlementByTime(ctx, r.ItemNo, origin.OperDate)
        if originPeriod != nil && originPeriod.Status == "finalized" {
            // 写"原窗口调整" 记录, 触发"重算"流 (单独流程, 不在本次跑)
            s.Store.CreateAdjustmentRequest(ctx, originPeriod.PeriodID, r)
            // 当前窗口的 SKU 销售 = 原 + 退货冲减(若窗口内有)
            s.Alert("CROSS_PERIOD_REFUND", r)
        }
    } else {
        // 3b. 原窗口未结算, 直接把负数加到当前聚合 (不去 cube 再查)
        normalSale[origin.ItemNo] -= r.SaleQnty
        normalAmt[origin.ItemNo]  -= r.SaleMoney
    }
}
```

### 3.3 落窗输出
```go
type WindowFlow struct {
    ItemNo       string
    NormalSaleQty float64      // 含退货冲减
    NormalSaleAmt float64
    PoolPosQty   map[pool]float64  // 特价码 → POS 销量 (给 Step 5)
    PoolPosAmt   map[pool]float64
    PurchaseQty  float64
}
```

## 四、Step 3: 倒挤公式

```go
// 对每个 SKU
backflush := beginQty + purchaseQty - normalSaleQty - endQty
// 注: endQty 来自 freshcheck_period_stock, 若缺(C8 阻断) 则报错
```

**C8 校验**: 落窗前检查 `freshcheck_period_stock` 中 `period_id = $period` 的行数 == sku_map 该品类行数, 否则阻断。

**C5 校验**: 任何 SKU `normalSaleQty < 0` (除退货处理) 触发告警(说明还有未回冲的退货)。

## 五、Step 4: 报损剥离

### 5.1 损耗率匹配

```go
lossRate := s.Store.GetLossRate(ctx, sku.FreshCategory, sku.TurnoverClass, "natural")
// 若 effective_to IS NULL 且 effective_from <= windowStart, 视为当前生效
// 多个匹配取 effective_from 最大者
```

### 5.2 快/慢周转公式

```go
if sku.TurnoverClass == "fast" {
    effectiveDays := min(sku.ShelfLifeDays, days)
    expectedLoss := purchaseQty * lossRate.DailyRate * float64(effectiveDays)
} else { // slow
    avgInStock := (beginQty + endQty) / 2  // 简化估算
    expectedLoss := avgInStock * lossRate.DailyRate * float64(days)
}

// 修正特价消耗 (不能为负)
poolAdjustable := max(backflush - expectedLoss, 0)
```

### 5.3 业务规则
- **快周转** (叶菜/鲜肉/水产): 损耗跟"采购量 × 在架天数"挂钩, 封顶生命期
- **慢周转** (根茎/冻品): 损耗跟"平均在库量 × 在库天数"挂钩(干耗为主)
- **变质/加工** 损耗率独立, 不参与默认分摊(用户后续录入)

## 六、Step 5: 池内分摊 (核心)

### 6.1 多筐串联 (按价格优先级)

```go
// 1. 拉所有"窗口内有过活动"的特价码
activePools := s.Store.GetActivePoolsInWindow(ctx, windowStart, windowEnd, branchNo)

// 2. 按 priority_rank ASC (0=最高=单价最高) 排序
sort.Slice(activePools, func(i, j int) bool {
    return activePools[i].UnitPrice > activePools[j].UnitPrice  // 价高先
})

// 3. 每个 SKU 的 "剩余倒挤" 初始化
remainingBackflush := map[item]float64{}
for _, sku := range skuList {
    remainingBackflush[sku.ItemNo] = poolAdjustable[sku.ItemNo]
}

// 4. 串联处理
for _, pool := range activePools {
    poolSegs := s.PoolState.RebuildSegments(ctx, pool.PoolCode, windowStart, windowEnd)
    // poolSegs: 每段 {Start, End, Items[].{ItemNo, InWeight, OutWeight, OutDest, Spoiled}}
    
    for _, seg := range poolSegs {
        // 5.2 框内差值法 (主)
        // 5.3 倒挤回退 (备)
        // 5.4 分摊 + 守恒
    }
}
```

### 6.2 框内差值法 (主算法)

```go
// 该时段该框: 各 SKU 的 (入框 - 出框 - 报损) 重量
weights := []float64{}
for _, it := range seg.Items {
    if remainingBackflush[it.ItemNo] <= 0 { continue }  // 已分完, 跳过
    diff := it.InWeight - it.OutWeight - it.SpoiledWeight
    if diff <= 0 { continue }                            // 净出, 跳过
    weights[it.ItemNo] = diff * it.ConfidenceFactor
}

sumWeights := sum(weights)
if sumWeights == 0 {
    // 5.3 退化到倒挤回退
    useBackflushFallback := true
}
```

### 6.3 倒挤回退 (备算法)

```go
// 用该 SKU 的"修正特价消耗"当权重 (per pool segment)
weights[itemNo] = remainingBackflush[itemNo] * confidenceFactor
```

### 6.4 分摊 + 物理约束

```go
// 1. 算初步分摊
for itemNo, w := range weights {
    if sumWeights > 0 {
        initialAlloc[pool][seg][itemNo] = seg.PoolPosQty * w / sumWeights
    }
}

// 2. 物理约束: 单 SKU 分摊 ≤ 该 SKU 剩余倒挤
for iter := 0; iter < 5; iter++ {  // 多次重分配
    excess := 0.0
    for itemNo, q := range initialAlloc[pool][seg] {
        if q > remainingBackflush[itemNo] {
            excess += q - remainingBackflush[itemNo]
            initialAlloc[itemNo] = remainingBackflush[itemNo]
            remainingBackflush[itemNo] = 0
        }
    }
    if excess == 0 { break }
    // 把 excess 按权重比例重分配给未满的 SKU
    others := sum(weights, excluding full)
    for itemNo, w := range others {
        initialAlloc[itemNo] += excess * w / others
    }
}

// 3. 串联扣减: 高价框分摊后扣减 SKU 剩余
for itemNo, q := range initialAlloc[pool][seg] {
    remainingBackflush[itemNo] -= q
}

// 4. C1 校验: 池内饱和度
totalAlloc := sum(initialAlloc[pool][seg])
saturation := abs(totalAlloc - seg.PoolPosQty) / seg.PoolPosQty
if saturation > threshold.C1PoolSaturationPct / 100 {
    s.Alert("C1", "warn", pool, saturation)
}
```

### 6.5 跨筐降级 (出框去向=downgrade)

```go
// 出框事件 destination = 'downgrade' 时:
//   - 该 SKU 在当前特价框"活动闭口"
//   - 同时生成目标特价框的"新入框"事件逻辑关联 (不出现在 pool_event 表, 只在分摊上下文里)
//
// 在 rebuildSegments 时:
//   - 遇到 downgrade_out: 关当前 SKU @ 当前 pool @ currentTime
//   - 同时开目标 pool 的 "虚拟入框" = downgrade 重量, confidence=high
```

### 6.6 池状态重建 (任意时刻在框集合)

```go
// pool_state.go
func (ps *PoolState) At(ctx, poolCode string, t time.Time) []PoolItem

// 算法: 扫描 freshcheck_pool_event, 计算每 SKU 的 "in" - "out" 累计
// 在 t 时刻 "在框" = 累计 > 0
// 累计 weight = 入框weight 累计 - 出框weight 累计 (按 event_time ≤ t 过滤)
```

## 七、Step 6: 毛利计算 + 守恒

```go
for _, sku := range skuList {
    avgCost := cube.GetAvgCost(ctx, sku.ItemNo)  // 结算时点加权平均
    saleCost := (normalSaleQty + poolAllocQty[sku] + lossQty) * avgCost
    totalRev := normalSaleAmt + poolAllocAmt[sku]
    grossProfit := totalRev - saleCost
    rate := grossProfit / totalRev
    
    // C3 守恒: Σ pool_alloc_qty == Σ POS 销量
    // C4 守恒: (normal + alloc + loss + box_loss) == (begin + purchase - end)
    ok1 := abs(sumAllocQty - sumPosQty) < 0.01
    ok2 := abs((normal+alloc+loss+boxLoss) - (begin+purchase-end)) < 0.01
    conservationOK := ok1 && ok2
    
    s.Store.InsertSettlement(ctx, &Settlement{
        ...
        ConservationOK: conservationOK,
        ConservationMsg: if !ok1 { "C3" } + if !ok2 { "C4" },
    })
}
```

## 八、8 条校验 (C1-C8)

| ID | 规则 | 阈值 | 处置 | 实现位置 |
|---|---|---|---|---|
| **C1** 池内饱和度 | `|Σalloc - POS| / POS` | ≤ 15% | 容差内归一化; 超限告警(不阻断) | Step 5.6 |
| **C2** 池外泄漏 | `backflush > loss × k` | 周 2× / 月 1.8× | 告警"疑似漏勾选" (warn) | Step 3 之后 |
| **C3** 总量守恒 | `Σalloc == ΣPOS` | 恒等 | 违反即结算失败(阻断) | Step 6 |
| **C4** 成本守恒 | `(n+a+l+b) == (bgn+pur-end)` | 恒等 | 违反即结算失败(阻断) | Step 6 |
| **C5** 退货负数 | `normalSaleQty < 0` 跨多日 | 不允许 | 触发退货回冲重算(阻断) | Step 2 |
| **C6** 周期锁 | `days > period_lock` | 7/14/30 | 超线阻断(需 admin 豁免) | Step 1 |
| **C7** 同步一致 | `cube vs source` | ≤ 0.01% | 自动重试 3 次, 失败阻断 | 同步 cron (W1) |
| **C8** 盘点缺项 | `period_stock 覆盖率` | 100% | 缺项阻断(需豁免) | Step 2 之前 |

### 8.1 C7 实现位置
C7 跟结算流程**不直接绑定**, 它是**镜像同步**层的硬约束。W1 阶段实现 cube 同步 cron:
```go
func (s *Sync) DiffCheck(ctx) error {
    srcSum := cube.Source().Query("SELECT SUM(sale_money) FROM t_rm_saleflow WHERE oper_date > ?", since24h)
    cubeSum := cube.Aggregate("sales", "total_revenue", dateRange: [since24h, now])
    diff := abs(srcSum - cubeSum) / srcSum
    if diff > 0.0001 {
        // 重试 3 次
        for i := 0; i < 3; i++ {
            cube.PluginReload()  // 强制 cube 重载
            time.Sleep(2*time.Minute)
            srcSum = reQuery()
            cubeSum = reAggregate()
            if diff < 0.0001 { break }
        }
        if diff > 0.0001 {
            s.Alert("C7", "block", "sync", diff)
            return ErrSyncMismatch
        }
    }
}
```

## 九、双轨互验 (框内差值 vs 整窗倒挤)

| 轨道 | 来源 | 用途 |
|---|---|---|
| 框内差值 | `Σ(in - out - spoiled)` per pool per segment | 池内分摊权重 + 框内不明差异检测 |
| 整窗倒挤 | `begin + purchase − normal_sale − end` | 总量约束 + 报损剥离基数 |

### 9.1 框损 (box_loss) 计算

```go
for _, pool := range activePools {
    for _, seg := range poolSegs {
        inTotal  := sum(seg.Items.InWeight)
        outTotal := sum(seg.Items.OutWeight)        // 含 4 个去向
        posTotal := seg.PoolPosQty                  // cube 拉
        
        boxLossKg := inTotal - outTotal - posTotal
        boxLossAmt := boxLossKg * pool.UnitPrice    // 按本框价折算
        
        s.Store.InsertBoxRecon(ctx, &BoxRecon{
            PeriodID: periodID,
            PoolCode: pool.PoolCode,
            SegmentStart: seg.Start,
            SegmentEnd: seg.End,
            InTotal: inTotal,
            OutTotal: outTotal,
            ...
            BoxLossKg: boxLossKg,
            BoxLossAmt: boxLossAmt,
            ResponsibleUser: seg.OutOperator,      // 早晨出框员工
            NeedsInvestigate: abs(boxLossKg) > 0.5,  // 阈值可配
        })
    }
}
```

### 9.2 偏差告警

```go
// 对每个 SKU: 比较 alloc_qty (差值轨) vs backflush_alloc_qty (倒挤轨)
for _, a := range allocs {
    if a.AllocQty == 0 && a.BackflushAllocQty == 0 { continue }
    devRate := abs(a.AllocQty - a.BackflushAllocQty) / max(a.AllocQty, a.BackflushAllocQty)
    a.DeviationQty = a.AllocQty - a.BackflushAllocQty
    a.DeviationRate = devRate
    a.NeedsReview = devRate > 0.10  // 10% 阈值, 可配
}
```

## 十、损耗率校准 (W4)

```go
func (s *Service) CalibrateLossRate(ctx, periodID) {
    // 1. 聚合该轨道窗口内的"实测损耗率"
    measureds := s.Store.AggregateMeasured(ctx, periodID)
    //    measured = sum(loss_qty) / sum(begin + purchase - end - normal_sale - pool_alloc)
    
    for _, m := range measureds {
        preset := s.Store.GetActiveLossRate(ctx, m.Category, m.TurnoverClass)
        deviation := abs(m.Measured - preset.DailyRate) / preset.DailyRate
        
        s.Store.InsertLossCalibrate(ctx, &LossCalibrate{
            FreshCategory: m.Category,
            TurnoverClass: m.TurnoverClass,
            PeriodWindowStart: m.WindowStart,
            PeriodWindowEnd: m.WindowEnd,
            MeasuredRate: m.Measured,
            PresetRate: preset.DailyRate,
            DeviationPct: deviation * 100,
            Action: if deviation > 0.20 { "pending" } else { "discard" },
        })
        
        if deviation > 0.20 {
            s.Alert("LOSS_CALIB", "info", m.Category, deviation)
            // 写待办, 管理员确认: 更新预设 or 排查流程
        }
    }
}
```

## 十一、幂等性 (验收场景 8)

```go
type SettleRequest struct {
    IdempotencyKey string  // = hash(branch + track + window_start + window_end + settled_by)
}

// 1. 结算开始: SELECT ... WHERE idempotency_key = $1
//    - 找到 finalized: 直接返回旧结果 (不重算)
//    - 找到 recalculating: 报错"另一进程正在结算"
//    - 找不到: 继续
// 2. 事务: DELETE FROM freshcheck_settlement WHERE period_id = $1 (旧派生)
//          DELETE FROM freshcheck_alloc WHERE period_id = $1
//          DELETE FROM freshcheck_box_recon WHERE period_id = $1
//    (不动 freshcheck_alert, 历史告警保留)
// 3. 重跑 6 步
// 4. 写新结果, COMMIT
```

## 十二、性能估算 (单店 ≤500 SKU)

| 阶段 | IO | 耗时预估 | 备注 |
|---|---|---|---|
| Step 1 | 查 1 行 + 锁 | <10ms | |
| Step 2 | cube 聚合 4 次 (sales/退货/purchases/items) | 2-5s | 走 cube cache, 1 年数据 |
| Step 3 | 内存计算 | <100ms | |
| Step 4 | 查损耗率表 | <50ms | |
| Step 5 | 池状态重建 (200 SKU × 5 框 × 7 天) | 1-3s | 内存计算, 主要 sort |
| Step 6 | 写 4 张派生表 (500 行) | 1-2s | 批量 INSERT |
| **总** | | **5-12s** | 满足需求 ≤5min (单店) |

> 性能瓶颈主要在 cube 聚合查询。W1 阶段优化 cube plugin, 走 view 优先 + 索引。

## 十三、并发与重算

1. **同品类并发**: PG advisory lock 防双开
2. **跨品类并发**: 不互锁, 独立
3. **重算流程** (W3 末):
   - 写 `status='recalculating'`
   - 清空该 period_id 的 3 张派生表
   - 重跑 6 步
   - 写 `status='finalized'` 或 `is_overridden=true`
4. **跨期重算** (W4):
   - 跨窗退货触发, 找到原 period_id, 标 `recalculating`
   - 排入"重算队列" (新表 `freshcheck_recalc_queue` W4 加), 不阻塞当前结算

---

**下一份**:`docs/freshcheck-cube-plugins.md` — cube-agent-server 依赖与新增 plugin 清单
