# freshcheck 依赖的 cube plugin 清单

> 数据源: `cube-agent-server` (`F:\go\src\github.com\tinkler\cube-agent-server`)
> 原则: 零同步, 按需聚合查询; 不在 collect-ai 端做 ETL 缓存
> 现有 plugin: 14 个 (见 `F:\go\src\github.com\tinkler\cube-agent-server\plugins/`)
> 新增 plugin: **3 个** (`sales_with_refund` / `items_with_clsno` / `freshcheck_pool_cube`)
> 修改 plugin: **1 个** (`sales` 增强, 加 sale_way 区分正/退)

---

## 0. 现有可用 plugin (不需改)

| Plugin | 路径 | 用途 | 状态 |
|---|---|---|---|
| `sales` | `plugins/sales/plugin.yaml` | 销售流水(已含 oper_date, sale_qnty, sale_money, in_price, item_no) | 已有 |
| `purchases` | `plugins/purchases/plugin.yaml` | 采购入库(含 voucher_no, real_qty, cost_price, oper_date) | 已有 |
| `t_bd_item_info` | `plugins/t_bd_item_info/plugin.yaml` | 商品主表(含 clsno, clsname, brand, stock_qty Scalar Subquery) | 已有 |
| `inventory_current` | `plugins/inventory_current/plugin.yaml` | 库存快照(branch, item, stock_qty, avg_cost) | 已有 |
| `items` | `plugins/items/plugin.yaml` | 商品精简版 | 已有 |

> `t_bd_item_info` 已含 `stock_qty` (Scalar Subquery + 库存聚合), `avg_cost` 走 `inventory_current` 即可。

---

## 1. 新增 `sales_with_refund` — 含退货标记的销售流

**W1 必做**, 因为现有 `sales` plugin 不区分正/退, 退货回冲算法 (Step 2) 没法跑。

### 1.1 源表
- 思迅: `t_rm_saleflow` (主表)
- 退货标记: `sale_way` 字段, 退货行 `sale_way='B'`, 正常 `sale_way='A'`(待确认 SQL Server 2008 R2 的实际值, 实施时 introspect)

### 1.2 关键字段
```yaml
- name: sales_with_refund
  sql: |
    SELECT
      s.flow_id,
      RTRIM(s.branch_no)     AS branch_no,
      s.oper_date,
      RTRIM(s.item_no)       AS item_no,
      RTRIM(ISNULL(i.item_name, ''))  AS item_name,
      RTRIM(ISNULL(i.item_clsno, '')) AS item_clsno,
      RTRIM(ISNULL(c.item_clsname, '')) AS item_clsname,
      RTRIM(ISNULL(i.main_supcust, '')) AS main_supcust,
      s.sale_qnty,                                              -- 退货为负
      s.sale_price,
      s.sale_money,
      s.in_price,
      s.sale_way,                                               -- 'A' 正 / 'B' 退
      ISNULL(s.voucher_no, '')  AS voucher_no,                  -- 原单号
      ISNULL(s.origin_flow_id, 0) AS origin_flow_id,            -- 退货原 flow_id
      -- 派生: 退货关联原单 oper_date (供 Step 2 回冲)
      ISNULL(o.oper_date, '1900-01-01') AS origin_oper_date,
      -- 派生: 该行是否退货
      CASE WHEN s.sale_way = 'B' THEN 1 ELSE 0 END AS is_refund
    FROM dbo.t_rm_saleflow s
    LEFT JOIN dbo.t_rm_saleflow o
      ON s.origin_flow_id = o.flow_id
    LEFT JOIN dbo.t_bd_item_info i ON RTRIM(s.item_no) = RTRIM(i.item_no)
    LEFT JOIN dbo.t_bd_item_cls  c ON RTRIM(i.item_clsno) = RTRIM(c.item_clsno)
    WHERE s.oper_date >= DATEADD(YEAR, -1, GETDATE())
  primary_key: flow_id
  measures:
    - { name: count,    type: count }
    - { name: sale_qnty, type: sum, sql: sale_qnty }     -- 退货为负, sum 自动抵消
    - { name: sale_money, type: sum, sql: sale_money }
    - { name: refund_qnty, type: sum, sql: CASE WHEN is_refund=1 THEN ABS(sale_qnty) ELSE 0 END }
    - { name: refund_amt,  type: sum, sql: CASE WHEN is_refund=1 THEN ABS(sale_money) ELSE 0 END }
  dimensions:
    - { name: flow_id,  type: number, primary_key: true }
    - { name: oper_date, type: time }
    - { name: branch_no, type: string }
    - { name: item_no,   type: string }
    - { name: is_refund, type: number }                    -- 0/1, AI 可过滤
    - { name: sale_way,  type: string }                    -- 'A' / 'B'
    - { name: voucher_no, type: string }
    - { name: origin_flow_id, type: number }
    - { name: origin_oper_date, type: time }
```

### 1.3 使用方式
```go
// Step 2 退货回冲
cube.Query("sales_with_refund", filters: {
    "oper_date": [windowStart, windowEnd],
    "branch_no": branch,
}, measures: ["sale_qnty", "sale_money", "refund_qnty", "refund_amt"])
```

---

## 2. 新增 `items_with_clsno` — 给生鲜子类打标

> 现有 `t_bd_item_info` 已含 `item_clsno` / `item_clsname`, 但**没有子类列**。
> 生鲜子类 (leaf/root/aquatic/meat/frozen) 需要从 `item_clsname` 字符串里识别 — 用 cube 端**派生列** (SQL CASE WHEN) 静态映射, 不调 LLM。

### 2.1 源表
- 思迅: `t_bd_item_info` LEFT JOIN `t_bd_item_cls`
- 子类映射: SQL 端 CASE WHEN 硬编码(同义商品名 → 子类), 后续可改

### 2.2 关键 SQL
```yaml
- name: items_with_clsno
  sql: |
    SELECT
      RTRIM(i.item_no)         AS item_no,
      RTRIM(i.item_name)       AS item_name,
      RTRIM(i.item_clsno)      AS item_clsno,
      RTRIM(c.item_clsname)    AS item_clsname,
      RTRIM(i.item_brand)      AS item_brand,
      -- 生鲜子类派生 (用 clsname LIKE 匹配, 后续可改成 LLM 但当前静态)
      CASE
        WHEN c.item_clsname LIKE '%叶菜%' OR c.item_clsname LIKE '%青菜%' OR c.item_clsname LIKE '%蔬菜%' THEN 'leaf'
        WHEN c.item_clsname LIKE '%根茎%' OR c.item_clsname LIKE '%土豆%' OR c.item_clsname LIKE '%萝卜%' THEN 'root'
        WHEN c.item_clsname LIKE '%水产%' OR c.item_clsname LIKE '%鱼%' OR c.item_clsname LIKE '%虾%' THEN 'aquatic'
        WHEN c.item_clsname LIKE '%肉%' OR c.item_clsname LIKE '%禽%' THEN 'meat'
        WHEN c.item_clsname LIKE '%冻%' THEN 'frozen'
        ELSE NULL  -- 非生鲜, freshcheck 不处理
      END AS fresh_category,
      -- 快/慢周转 (临时规则: 根茎/冻品 = 慢, 其他 = 快)
      CASE
        WHEN c.item_clsname LIKE '%根茎%' OR c.item_clsname LIKE '%冻%' THEN 'slow'
        ELSE 'fast'
      END AS turnover_class,
      i.main_supcust,
      RTRIM(i.item_unit) AS unit
    FROM dbo.t_bd_item_info i
    LEFT JOIN dbo.t_bd_item_cls c ON RTRIM(i.item_clsno) = RTRIM(c.item_clsno)
    WHERE i.item_no IS NOT NULL
  primary_key: item_no
  measures:
    - { name: count, type: count }
  dimensions:
    - { name: item_no, type: string, primary_key: true }
    - { name: item_name, type: string }
    - { name: item_clsno, type: string }
    - { name: item_clsname, type: string }
    - { name: fresh_category, type: string }    -- NULL=非生鲜
    - { name: turnover_class, type: string }
    - { name: unit, type: string }
```

### 2.3 使用方式
```go
// W1 建账: 把 fresh_category 非 NULL 的 item 灌入 freshcheck_sku_map
cube.Query("items_with_clsno", filters: {"fresh_category": "not_null"}, measures: ["count"])
// 一次性 200+ 行, 给 admin 审核, 写入 freshcheck_sku_map
```

---

## 3. 新增 `freshcheck_pool_cube` — 特价码视图 (只读)

> 特价码定义在 collect-ai 端 `freshcheck_pool_code` 表; 但销售流水的特价码货号需要回查定义, 把定义同步到 cube 端作只读视图, 减少跨库 join。

### 3.1 数据流
- 业务主数据: `collect-ai.internal.freshcheck_pool_code` (PG 表)
- cube 视图: 由 `cube-agent-server` 通过**外部数据源**或**手动**初始化
- 当前实现: **cube 端硬编码** (SQL 端 JOIN 思迅 `t_bd_item_info` 看 `item_no` 是不是特价码范围)

> 折中: 第一版**不**单独建 cube, 直接在 collect-ai 端 JOIN 内存 dict 即可, 跟 `inventory_current` 一样模式。

### 3.2 不需新增 (W1 决定)
**W1 决定不新建此 cube plugin**。理由:
- 特价码定义表行数 ≤ 50 (单店), 加载到 Go 内存 dict 完全够
- 避免 cube-agent-server 多源数据耦合
- 后续真有性能问题再加 (W3 末 benchmark 决定)

---

## 4. 修改 `sales` plugin — 增强可选项 (W3 可选)

> `sales` 已经够用, 但缺 `voucher_no` 字段(原单号)。
> 优先用 `sales_with_refund` (W1 必做), `sales` 不动。

---

## 5. cube 调用契约 (collect-ai 端)

### 5.1 CubeQuerier 接口
```go
// internal/freshcheck/cube.go
type CubeQuerier struct {
    Gateway *business.Gateway
}

func (q *CubeQuerier) SalesInWindow(ctx, branch string, start, end time.Time) ([]SalesRow, error) {
    raw, err := q.Gateway.Query(ctx, "sales_with_refund", []string{
        "flow_id", "item_no", "item_name", "oper_date", "sale_qnty", "sale_money",
        "is_refund", "origin_flow_id", "origin_oper_date",
    }, map[string]any{
        "branch_no": branch,
        "oper_date": []time.Time{start, end},
    }, 10000)
    // 走 business.Executor 而非 Gateway 直调 (12.1 规则)
}

func (q *CubeQuerier) PurchasesInWindow(ctx, branch string, start, end time.Time) ([]PurchaseRow, error)
func (q *CubeQuerier) AvgCost(ctx, itemNo string) (float64, error)         // 用 inventory_current.avg_cost
func (q *CubeQuerier) StockSnapshot(ctx, branch string) (map[itemNo]Stock, error)
func (q *CubeQuerier) FreshItems(ctx) ([]FreshItem, error)                  // 用 items_with_clsno
```

### 5.2 Gateway 接入
复用 collect-ai 现有 `business.Executor`:
```go
// internal/business/executor.go 加
func (e *Executor) SalesWithRefund(ctx, bizFields []string, filters map[string]any, limit int) ([]Row, error)
func (e *Executor) FreshItems(ctx) ([]Row, error)
```

### 5.3 mappings.yaml 增量
```yaml
# configs/mappings.yaml 加 sources.hbpos 段
sources:
  hbpos:
    sales_with_refund:
      cube: sales_with_refund
      fields:
        flow_id: flow_id
        item_no: item_no
        item_name: item_name
        sale_qnty: sale_qnty
        sale_money: sale_money
        oper_date: oper_date
        is_refund: is_refund
        origin_flow_id: origin_flow_id
        origin_oper_date: origin_oper_date
    items_with_clsno:
      cube: items_with_clsno
      fields:
        item_no: item_no
        item_name: item_name
        item_clsno: item_clsno
        item_clsname: item_clsname
        fresh_category: fresh_category
        turnover_class: turnover_class
        unit: unit
```

---

## 6. C7 同步校验 (一致性)

> 需求 2.1 "每轮同步后执行一致性校验"
> **W1 实现**: cron 每小时触发一次
> 校验逻辑: cube 聚合 24h 内的 `sale_money` / `purchase_qty` / `stock_qty`, 跟思迅源库 3 张表对账

### 6.1 cube 端拉数据
```go
// 24h 销售
cubeSales := cube.Aggregate("sales_with_refund", "sale_money", dateRange: [now-24h, now])
// 24h 采购
cubePurchases := cube.Aggregate("purchases", "real_qty", dateRange: [now-24h, now])
```

### 6.2 思迅源库直查
> **违反 AGENTS.md 12.1 规则** ❌ — collect-ai 严禁直连思迅。
> **改**: 把"思迅源库"理解为 **思迅 SQL Server**, 让 `cube-agent-server` 暴露一个**直查思迅的 HTTP 端点** (W1 阶段新增):

```go
// cube-agent-server 加 (W1 跟 sales_with_refund 一起做)
GET /v1/source-direct?cube=t_rm_saleflow&op=sum&col=sale_money&since=24h
// 走 cube-agent-server 现有思迅连接, 返 sum/avg/count
```

> 这样 collect-ai 还是只走 cube, 不直连思迅。

### 6.3 collect-ai 端 cron
```go
// internal/freshcheck/sync_check.go
func (s *Service) SyncCheckTick(ctx) {
    for retry := 0; retry < 3; retry++ {
        src := cube.SourceDirect("t_rm_saleflow", "sum(sale_money)", since24h)
        dst := cube.Aggregate("sales_with_refund", "sale_money", since24h)
        diff := abs(src - dst) / src
        if diff < 0.0001 { return }
        time.Sleep(2*time.Minute)
        cube.PluginReload("sales_with_refund")
    }
    s.Alert("C7", "block", "sync", diff)
}
```

---

## 7. cube 端新增 plugin 任务清单

### 7.1 W1 必须
- [ ] **`sales_with_refund`**: 写 plugin.yaml + introspect 思迅 `t_rm_saleflow` 确认 `sale_way` / `origin_flow_id` 字段存在
- [ ] **`items_with_clsno`**: 写 plugin.yaml + 跟 admin 确认生鲜子类映射规则 (clsname LIKE 模式)
- [ ] **cube-agent-server `SourceDirect` 端点**: 给 C7 用
- [ ] **`mappings.yaml`**: 增量加 `sales_with_refund` / `items_with_clsno` 段
- [ ] **`business.Executor`**: 加 `SalesWithRefund()` / `FreshItems()` 方法
- [ ] **`Gateway.Ping()`**: 验证 cube 通了 (启动时 sanity check)

### 7.2 W2 必须
- [ ] 验证 cube `sales_with_refund` 退货回冲逻辑端到端
- [ ] 验证 `items_with_clsno` 子类命中率 ≥ 80% (不命中的走 admin 手工标)

### 7.3 W3 可选
- [ ] cube 端加 `freshcheck_pool_cube` (如果 benchmarks 显示内存 dict 不够)
- [ ] cube 端加预聚合 `daily_sales_per_item` 视图 (Step 2 优化)

### 7.4 W4 必做
- [ ] 跨期退货的"原单 oper_date" cube 端验证
- [ ] 损耗率校准的 throughput 聚合 cube 端优化

---

**下一份**:`docs/freshcheck-h5-contract.md` — 前后端 API 契约
