# freshcheck 数据模型 (13 张表)

> 全部表名加 `freshcheck_` 前缀; 配置类用 JSONB + 审计快照; 派生表全部不可手工改(只走服务层写)。
> 索引: 时序类全用 `(branch_no, item_no, op_time DESC)`; 配置类用业务主键 UNIQUE; 派生用 `(period_id, ...)`。
> 状态机: `status` 字段显式枚举, 不删行。

---

## 0. 通用约定

- **时间戳**: 全部 `TIMESTAMPTZ`(UTC 存, 本地时渲染)。`op_time` = 业务事件时间(可改) vs `recorded_at` = 录库时间(只读)。
- **置信度**: `confidence` enum: `high` (实时录入) / `low` (事后补录/漏录兜底)。`weight_factor` 计算时由 confidence 派生 (high=1.0, low=0.5)。
- **门店**: `branch_no TEXT`(用 collect-ai 现有 `cfg.BranchNo` 单门店; 多门店后续扩展)。
- **币种**: `amount NUMERIC(12,2)`(成本/收入); `quantity NUMERIC(12,4)`(重量支持 4 位小数, 件数支持整数)。
- **审计**: 配置表任何 UPDATE/DELETE 前必须先写 `freshcheck_config_snap` 一行; 派生表永远不 UPDATE(只 INSERT)。

---

## 1. `freshcheck_sku_map` — 生鲜 SKU 映射表 (配置)

> 思迅 `t_bd_item_info.item_no` → 生鲜子类 + 关联特价码 + 默认成本。

```sql
CREATE TABLE IF NOT EXISTS freshcheck_sku_map (
    id                BIGSERIAL PRIMARY KEY,
    branch_no         TEXT NOT NULL DEFAULT '0001',
    item_no           TEXT NOT NULL,                              -- 思迅货号 (RTRIM 存)
    item_name         TEXT NOT NULL DEFAULT '',
    fresh_category    TEXT NOT NULL,                              -- 'leaf' | 'root' | 'aquatic' | 'meat' | 'frozen'
    turnover_class    TEXT NOT NULL CHECK (turnover_class IN ('fast','slow')),
    shelf_life_days   INT  NOT NULL DEFAULT 7,                    -- 商品生命期 (快周转封顶用)
    default_pool_code TEXT,                                       -- 关联特价码 (NULL=正常销售不入特价池)
    is_active         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (branch_no, item_no)
);
CREATE INDEX IF NOT EXISTS idx_fcsm_category ON freshcheck_sku_map(branch_no, fresh_category) WHERE is_active;
CREATE INDEX IF NOT EXISTS idx_fcsm_pool ON freshcheck_sku_map(branch_no, default_pool_code) WHERE default_pool_code IS NOT NULL;
```

---

## 2. `freshcheck_pool_code` — 特价码定义表 (配置)

> "1元/斤" / "0.5元/斤" / "1元/2件" 等。归因优先级 = `price DESC` (高价先入)。

```sql
CREATE TABLE IF NOT EXISTS freshcheck_pool_code (
    id              BIGSERIAL PRIMARY KEY,
    branch_no       TEXT NOT NULL DEFAULT '0001',
    pool_code       TEXT NOT NULL,                                -- 思迅货号 (特价码), 销售流水挂这下面
    pool_name       TEXT NOT NULL,                                -- '1元/斤' / '0.5元/斤' / '1元2件'
    pricing_mode    TEXT NOT NULL CHECK (pricing_mode IN ('weight','piece')),
    unit_price      NUMERIC(10,2) NOT NULL,                       -- 单价 (元/斤 或 元/件)
    piece_weight    NUMERIC(10,4),                                -- 计件模式: 每件折重(kg), 供"修正特价消耗量"反推件数
    priority_rank   INT NOT NULL DEFAULT 0,                       -- 多筐并存归因优先级 (0=最高, 越大越低)
    is_active       BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (branch_no, pool_code)
);
CREATE INDEX IF NOT EXISTS idx_fcpc_priority ON freshcheck_pool_code(branch_no, priority_rank) WHERE is_active;
```

---

## 3. `freshcheck_loss_rate` — 损耗率规则表 (配置)

> 公式 (3.2 Step 4): 快周转 = 采购量 × 日损耗率 × min(生命期, 窗口天数); 慢周转 = 平均在库量 × 日干耗率 × 窗口天数。

```sql
CREATE TABLE IF NOT EXISTS freshcheck_loss_rate (
    id                  BIGSERIAL PRIMARY KEY,
    fresh_category      TEXT NOT NULL,                            -- 'leaf' | 'root' | ...
    turnover_class      TEXT NOT NULL CHECK (turnover_class IN ('fast','slow')),
    loss_type           TEXT NOT NULL CHECK (loss_type IN ('natural','spoilage','process')),
    daily_rate          NUMERIC(8,6) NOT NULL,                    -- e.g. 0.050000 = 5%/天
    effective_from      DATE NOT NULL DEFAULT CURRENT_DATE,
    effective_to        DATE,                                     -- NULL=当前生效
    is_calibrated       BOOLEAN NOT NULL DEFAULT FALSE,           -- 校准过的人工值
    last_calibrate_at   TIMESTAMPTZ,
    last_calibrate_by   TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (fresh_category, turnover_class, loss_type, effective_from)
);
CREATE INDEX IF NOT EXISTS idx_fclr_active ON freshcheck_loss_rate(fresh_category, turnover_class, loss_type) WHERE effective_to IS NULL;
```

---

## 4. `freshcheck_threshold` — 业务阈值表 (配置)

> C1-C8 阈值 + 损耗率校准偏差阈值, 改这里不用 build。

```sql
CREATE TABLE IF NOT EXISTS freshcheck_threshold (
    key             TEXT PRIMARY KEY,                              -- 'c1_pool_saturation_pct' | 'c2_leakage_multiplier_weekly' | ...
    value           NUMERIC(10,4) NOT NULL,
    unit            TEXT NOT NULL DEFAULT '',                     -- 'pct' | 'multiplier' | 'days' | 'deviation_pct'
    description     TEXT NOT NULL DEFAULT '',
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by      TEXT
);
```

**默认 seed (W1 启动时灌入)**:
| key | value | 说明 |
|---|---|---|
| `c1_pool_saturation_pct` | 15.00 | 池内饱和度容差 |
| `c2_leakage_multiplier_weekly` | 2.00 | 周结池外泄漏倍数 |
| `c2_leakage_multiplier_long` | 1.80 | 月结/长窗口泄漏倍数 |
| `c6_period_lock_leaf_days` | 7 | 叶菜周期锁 |
| `c6_period_lock_root_days` | 30 | 根茎周期锁 |
| `c6_period_lock_aquatic_days` | 14 | 水产周期锁 |
| `c6_period_lock_meat_days` | 14 | 肉类周期锁 |
| `c6_period_lock_frozen_days` | 30 | 冻品周期锁 |
| `c7_sync_diff_pct` | 0.01 | 同步差异告警 |
| `c8_period_stock_coverage_pct` | 100.00 | 盘点覆盖率 (100% 必全) |
| `loss_calibrate_deviation_pct` | 20.00 | 校准偏差告警 |
| `low_confidence_weight` | 0.50 | 低置信度分摊权重 |

---

## 5. `freshcheck_pool_event` — 特价池事件表 (人工录入, 事实表)

> 入框/出框事件流, 全系统归因的枢轴。

```sql
CREATE TABLE IF NOT EXISTS freshcheck_pool_event (
    id              BIGSERIAL PRIMARY KEY,
    branch_no       TEXT NOT NULL DEFAULT '0001',
    pool_code       TEXT NOT NULL,                                -- 特价码 (fk -> freshcheck_pool_code.pool_code)
    item_no         TEXT NOT NULL,                                -- 原 SKU 货号
    event_kind      TEXT NOT NULL CHECK (event_kind IN ('in','out')),
    event_time      TIMESTAMPTZ NOT NULL,                         -- 业务事件时间 (入框=打称时刻 / 出框=早晨检查时刻)
    recorded_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),           -- 录库时间 (只读)
    weight_kg       NUMERIC(12,4) NOT NULL DEFAULT 0,             -- 重量 (计件=件数×piece_weight)
    piece_count     INT NOT NULL DEFAULT 0,                       -- 件数 (计件模式填)
    out_destination TEXT CHECK (out_destination IN ('sold_out','spoiled','return_to_shelf','downgrade')),
    downgrade_to    TEXT,                                         -- 出框去向=降级转框 时, 目标特价码
    operator        TEXT NOT NULL,                                -- 操作员
    confidence      TEXT NOT NULL DEFAULT 'high' CHECK (confidence IN ('high','low')),
    source          TEXT NOT NULL DEFAULT 'h5' CHECK (source IN ('h5','admin','import')),
    note            TEXT NOT NULL DEFAULT '',
    -- 防漏录: 同一 (branch,pool,item,in/out) 时间冲突
    UNIQUE (branch_no, pool_code, item_no, event_kind, event_time)
);
CREATE INDEX IF NOT EXISTS idx_fcpe_pool_time ON freshcheck_pool_event(branch_no, pool_code, event_time DESC);
CREATE INDEX IF NOT EXISTS idx_fcpe_item_time ON freshcheck_pool_event(branch_no, item_no, event_time DESC);
CREATE INDEX IF NOT EXISTS idx_fcpe_low_conf ON freshcheck_pool_event(confidence) WHERE confidence = 'low';
```

> 兜底: 出框检查发现无未闭口入框, 现场补录, 全部 `confidence=low`。

---

## 6. `freshcheck_period_stock` — 周期盘点表 (人工录入, 事实表)

> 每个结算周期, 每个生鲜 SKU 期末实物盘点一行。

```sql
CREATE TABLE IF NOT EXISTS freshcheck_period_stock (
    id              BIGSERIAL PRIMARY KEY,
    branch_no       TEXT NOT NULL DEFAULT '0001',
    period_id       BIGINT NOT NULL,                              -- 关联 freshcheck_settlement.period_id
    item_no         TEXT NOT NULL,                                -- 原 SKU (必须非特价码, 系统校验)
    item_name       TEXT NOT NULL DEFAULT '',
    qty             NUMERIC(12,4) NOT NULL,                       -- 期末实盘数量
    unit            TEXT NOT NULL DEFAULT '',
    stock_time      TIMESTAMPTZ NOT NULL,                         -- 盘点时点 (窗口终点)
    operator        TEXT NOT NULL,
    confidence      TEXT NOT NULL DEFAULT 'high' CHECK (confidence IN ('high','low')),
    source          TEXT NOT NULL DEFAULT 'h5' CHECK (source IN ('h5','admin','import')),
    note            TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (period_id, item_no)
);
CREATE INDEX IF NOT EXISTS idx_fcps_period ON freshcheck_period_stock(period_id);
```

> 导入时强校验: 货号 ∈ `freshcheck_sku_map.item_no` 且 ∉ `freshcheck_pool_code.pool_code`。

---

## 7. `freshcheck_category_track` — 品类结算轨道配置 (配置)

> 哪个子类走哪个周期锁天数, 多节奏结算的关键。

```sql
CREATE TABLE IF NOT EXISTS freshcheck_category_track (
    id                  BIGSERIAL PRIMARY KEY,
    fresh_category      TEXT NOT NULL,                            -- 'leaf' | 'root' | ...
    branch_no           TEXT NOT NULL DEFAULT '0001',
    track_code          TEXT NOT NULL,                            -- 'leaf-weekly' | 'root-monthly' | 'custom-15d'
    period_lock_days    INT  NOT NULL,                            -- 7/14/30/任意
    stock_freq          TEXT NOT NULL CHECK (stock_freq IN ('daily','weekly','monthly','none')),
    require_stock       BOOLEAN NOT NULL DEFAULT TRUE,            -- 期末是否必盘
    last_settle_at      TIMESTAMPTZ,                              -- 上次结算时点 (= 窗口起点)
    next_settle_deadline TIMESTAMPTZ,                             -- 下次结算 deadline (last_settle + period_lock)
    is_active           BOOLEAN NOT NULL DEFAULT TRUE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (branch_no, fresh_category)
);
```

---

## 8. `freshcheck_settlement` — 周期结算结果表 (派生)

> R1 周期单品毛利表。每 SKU 每窗口 1 行。

```sql
CREATE TABLE IF NOT EXISTS freshcheck_settlement (
    id                    BIGSERIAL PRIMARY KEY,
    branch_no             TEXT NOT NULL DEFAULT '0001',
    period_id             BIGINT NOT NULL,                         -- 一次结算所有 SKU 同 period_id
    track_code            TEXT NOT NULL,                           -- 'leaf-weekly' ...
    fresh_category        TEXT NOT NULL,
    item_no               TEXT NOT NULL,                           -- 原 SKU
    item_name             TEXT NOT NULL DEFAULT '',
    -- 流量
    begin_qty             NUMERIC(12,4) NOT NULL DEFAULT 0,        -- 期初 (上期期末)
    purchase_qty          NUMERIC(12,4) NOT NULL DEFAULT 0,        -- 窗口内采购
    normal_sale_qty       NUMERIC(12,4) NOT NULL DEFAULT 0,        -- 原 SKU 正常销售 (含临时低价)
    normal_sale_amt       NUMERIC(12,2) NOT NULL DEFAULT 0,        -- 正常销售收入
    pool_alloc_qty        NUMERIC(12,4) NOT NULL DEFAULT 0,        -- 特价分摊销量
    pool_alloc_amt        NUMERIC(12,2) NOT NULL DEFAULT 0,        -- 特价分摊收入
    end_qty               NUMERIC(12,4) NOT NULL DEFAULT 0,        -- 期末盘点
    backflush_qty         NUMERIC(12,4) NOT NULL DEFAULT 0,        -- 倒挤清仓量
    loss_qty              NUMERIC(12,4) NOT NULL DEFAULT 0,        -- 报损量 (Step 4)
    box_loss_qty          NUMERIC(12,4) NOT NULL DEFAULT 0,        -- 框损量 (双轨差异, Step 3 算)
    -- 钱
    avg_cost              NUMERIC(12,4) NOT NULL,                  -- 加权平均成本 (结算时点快照)
    sale_cost             NUMERIC(12,2) NOT NULL DEFAULT 0,        -- 销售成本 (正常+特价+报损) × avg_cost
    total_revenue         NUMERIC(12,2) NOT NULL DEFAULT 0,        -- 总收入
    gross_profit          NUMERIC(12,2) NOT NULL DEFAULT 0,        -- 毛利额
    gross_profit_rate     NUMERIC(8,4),                            -- 毛利率 (0.2500 = 25%)
    -- 标记
    confidence            TEXT NOT NULL DEFAULT 'high',
    is_overridden         BOOLEAN NOT NULL DEFAULT FALSE,          -- 管理员手工改过
    override_reason       TEXT,
    -- 窗口
    window_start          TIMESTAMPTZ NOT NULL,
    window_end            TIMESTAMPTZ NOT NULL,
    window_days           INT  NOT NULL,
    -- 状态
    status                TEXT NOT NULL DEFAULT 'finalized' CHECK (status IN ('finalized','recalculating','overridden','voided')),
    settled_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    settled_by            TEXT NOT NULL,
    -- 守恒标记 (C3/C4 校验)
    conservation_ok       BOOLEAN NOT NULL DEFAULT TRUE,
    conservation_msg      TEXT,
    -- 幂等
    idempotency_key       TEXT NOT NULL,                           -- branch+track+window+settled_by hash
    UNIQUE (idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_fcs_period ON freshcheck_settlement(period_id, item_no);
CREATE INDEX IF NOT EXISTS idx_fcs_track ON freshcheck_settlement(track_code, window_end DESC);
CREATE INDEX IF NOT EXISTS idx_fcs_item ON freshcheck_settlement(branch_no, item_no, window_end DESC);
```

> **守恒公式** (硬约束): `normal_sale_qty + pool_alloc_qty + loss_qty + box_loss_qty = begin_qty + purchase_qty - end_qty` (= backflush_qty + normal_sale_qty)
> 违反即 `conservation_ok=false`, status 进入 `recalculating`, 提示人工重算。

---

## 9. `freshcheck_alloc` — 归因分摊明细 (派生)

> R2 特价归因明细。每 SKU 每特价框每时段 1 行。

```sql
CREATE TABLE IF NOT EXISTS freshcheck_alloc (
    id                  BIGSERIAL PRIMARY KEY,
    period_id           BIGINT NOT NULL,                          -- 关联 freshcheck_settlement.period_id
    pool_code           TEXT NOT NULL,
    segment_start       TIMESTAMPTZ NOT NULL,                     -- 池活动时段起点
    segment_end         TIMESTAMPTZ NOT NULL,                     -- 池活动时段终点
    item_no             TEXT NOT NULL,                            -- 原 SKU
    -- 输入
    in_weight_kg        NUMERIC(12,4) NOT NULL DEFAULT 0,        -- 时段入框重量
    out_weight_kg       NUMERIC(12,4) NOT NULL DEFAULT 0,        -- 时段出框剩余
    spoiled_weight_kg   NUMERIC(12,4) NOT NULL DEFAULT 0,        -- 时段出框报损
    weight_diff_kg      NUMERIC(12,4) NOT NULL DEFAULT 0,        -- in - out - spoiled (框内差值权重)
    pool_pos_qty        NUMERIC(12,4) NOT NULL DEFAULT 0,        -- 该时段特价码 POS 销量
    pool_pos_amt        NUMERIC(12,2) NOT NULL DEFAULT 0,        -- 该时段特价码 POS 销售额
    -- 权重
    share_weight        NUMERIC(8,6) NOT NULL DEFAULT 0,          -- 该 SKU 在池内权重 (weight_diff / sum)
    confidence_factor   NUMERIC(4,2) NOT NULL DEFAULT 1.00,      -- 0.50 / 1.00
    -- 产出
    alloc_qty           NUMERIC(12,4) NOT NULL DEFAULT 0,        -- 分得特价销量
    alloc_amt           NUMERIC(12,2) NOT NULL DEFAULT 0,        -- 分得特价收入
    -- 双轨偏差
    backflush_alloc_qty NUMERIC(12,4) NOT NULL DEFAULT 0,        -- 倒挤轨算出的分摊量
    deviation_qty       NUMERIC(12,4) NOT NULL DEFAULT 0,        -- 差异 (alloc - backflush_alloc)
    deviation_rate      NUMERIC(8,4),                             -- 差异率
    needs_review        BOOLEAN NOT NULL DEFAULT FALSE,          -- 偏差 > 阈值
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (period_id, pool_code, segment_start, item_no)
);
CREATE INDEX IF NOT EXISTS idx_fca_pool ON freshcheck_alloc(period_id, pool_code);
```

---

## 10. `freshcheck_box_recon` — 框内对账 (派生)

> R3 框内对账表。每框每时段 1 行。

```sql
CREATE TABLE IF NOT EXISTS freshcheck_box_recon (
    id                  BIGSERIAL PRIMARY KEY,
    period_id           BIGINT NOT NULL,
    branch_no           TEXT NOT NULL DEFAULT '0001',
    pool_code           TEXT NOT NULL,
    segment_start       TIMESTAMPTZ NOT NULL,
    segment_end         TIMESTAMPTZ NOT NULL,
    in_total_kg         NUMERIC(12,4) NOT NULL DEFAULT 0,        -- 入框合计
    out_total_kg        NUMERIC(12,4) NOT NULL DEFAULT 0,        -- 出框合计 (含去向)
    out_sold_out_kg     NUMERIC(12,4) NOT NULL DEFAULT 0,
    out_spoiled_kg      NUMERIC(12,4) NOT NULL DEFAULT 0,
    out_return_kg       NUMERIC(12,4) NOT NULL DEFAULT 0,
    out_downgrade_kg    NUMERIC(12,4) NOT NULL DEFAULT 0,
    pos_total_kg        NUMERIC(12,4) NOT NULL DEFAULT 0,        -- POS 实售
    box_loss_kg         NUMERIC(12,4) NOT NULL DEFAULT 0,        -- 不明差异 = in - out - pos
    box_loss_amt        NUMERIC(12,2) NOT NULL DEFAULT 0,        -- 按该框 unit_price 折算
    -- 责任归属
    responsible_user    TEXT,                                     -- 该时段出框检查员工
    responsible_at      TIMESTAMPTZ,
    needs_investigate   BOOLEAN NOT NULL DEFAULT FALSE,
    investigate_note    TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (period_id, pool_code, segment_start)
);
```

---

## 11. `freshcheck_alert` — 校验告警日志 (派生)

> C1-C8 触发明细 + 处置状态。

```sql
CREATE TABLE IF NOT EXISTS freshcheck_alert (
    id              BIGSERIAL PRIMARY KEY,
    branch_no       TEXT NOT NULL DEFAULT '0001',
    period_id       BIGINT,                                       -- NULL=非结算相关告警
    rule_code       TEXT NOT NULL,                                -- 'C1' | 'C2' | ... | 'C8' | 'LOSS_CALIB'
    severity        TEXT NOT NULL CHECK (severity IN ('info','warn','block')),
    entity_type     TEXT NOT NULL,                                -- 'pool' | 'item' | 'category' | 'sync'
    entity_id       TEXT NOT NULL,                                -- pool_code | item_no | track_code | ...
    message         TEXT NOT NULL,
    payload         JSONB NOT NULL DEFAULT '{}'::jsonb,           -- 触发时的上下文 (差异量/阈值/快照)
    status          TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','overridden','fixed','ignored')),
    override_by     TEXT,
    override_reason TEXT,
    override_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_fca_status ON freshcheck_alert(branch_no, status, created_at DESC) WHERE status = 'open';
CREATE INDEX IF NOT EXISTS idx_fca_period ON freshcheck_alert(period_id) WHERE period_id IS NOT NULL;
```

---

## 12. `freshcheck_loss_calibrate` — 损耗率校准历史 (派生+人工)

```sql
CREATE TABLE IF NOT EXISTS freshcheck_loss_calibrate (
    id                  BIGSERIAL PRIMARY KEY,
    branch_no           TEXT NOT NULL DEFAULT '0001',
    fresh_category      TEXT NOT NULL,
    turnover_class      TEXT NOT NULL,
    period_window_start DATE NOT NULL,
    period_window_end   DATE NOT NULL,
    -- 实测
    measured_loss_qty   NUMERIC(12,4) NOT NULL,
    measured_throughput NUMERIC(12,4) NOT NULL,                   -- 总流转量 (采购+期初)
    measured_rate       NUMERIC(8,6) NOT NULL,                    -- measured_loss / measured_throughput
    -- 预设
    preset_rate         NUMERIC(8,6) NOT NULL,
    -- 偏差
    deviation_pct       NUMERIC(8,4) NOT NULL,                    -- (measured - preset) / preset * 100
    -- 处置
    action              TEXT NOT NULL DEFAULT 'pending' CHECK (action IN ('pending','update_preset','investigate','discard')),
    action_by           TEXT,
    action_at           TIMESTAMPTZ,
    action_note         TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (branch_no, fresh_category, turnover_class, period_window_end)
);
```

---

## 13. `freshcheck_config_snap` — 配置变更快照 (审计)

> 配置表任何 UPDATE/DELETE 前自动 INSERT 一行, 含完整旧值 JSONB。

```sql
CREATE TABLE IF NOT EXISTS freshcheck_config_snap (
    id              BIGSERIAL PRIMARY KEY,
    table_name      TEXT NOT NULL,                                -- 'freshcheck_sku_map' | ...
    row_pk          TEXT NOT NULL,                                -- 业务主键 (item_no / pool_code / ...)
    action          TEXT NOT NULL CHECK (action IN ('update','delete','create')),
    old_value       JSONB,                                        -- NULL for create
    new_value       JSONB,                                        -- NULL for delete
    changed_by      TEXT NOT NULL,
    changed_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    rollback_to     TIMESTAMPTZ,                                  -- 已回滚则填回滚时间
    rollback_by     TEXT
);
CREATE INDEX IF NOT EXISTS idx_fcs_snap ON freshcheck_config_snap(table_name, row_pk, changed_at DESC);
```

> 回滚: 拿 `id` 找出 `old_value` / `new_value`, 反向 UPDATE/INSERT 即可, 同样要写新一行 snap。

---

## 14. 关键不变量(在 service 层强校验)

1. **SPEC 货号隔离**: `freshcheck_period_stock.item_no` ∉ `freshcheck_pool_code.pool_code`
2. **特价池事件唯一**: 同一 (branch, pool, item, kind, event_time) 不可重复
3. **状态机**: `freshcheck_settlement.status` 只允许 `finalized → recalculating → finalized`, `voided` 终态
4. **窗口单调**: 同一 track_code 的 `window_start` 严格大于上次 `window_end`
5. **守恒 C3**: `Σ pool_alloc_qty per pool_code` = `Σ POS 销售量(特价码货号)`
6. **守恒 C4**: `(normal_sale + pool_alloc + loss + box_loss) = (begin + purchase - end)`
7. **配置变更审计**: `freshcheck_sku_map` / `freshcheck_pool_code` / `freshcheck_loss_rate` / `freshcheck_threshold` UPDATE/DELETE 前必写 snap

---

## 15. cube 端依赖(零同步,按需聚合)

| 用途 | cube plugin | 必需字段 | 状态 |
|---|---|---|---|
| 销售流水(正/退) | `sales` 增强 / 新建 `sales_with_refund` | flow_id, item_no, sale_qnty, sale_money, oper_date, sale_way (退=?) | **W1 新建** |
| 采购入库 | `purchases` | item_no, real_qty, cost_price, oper_date, voucher_no | 已有 |
| 库存快照 | `inventory_current` | item_no, stock_qty, avg_cost | 已有 |
| 商品主表 | `t_bd_item_info` | item_no, item_name, item_clsno, item_clsname, item_brand | 已有 |
| 特价码定义 | `freshcheck_pool_code` cube | pool_code, pool_name, unit_price | **W1 新建** |

> 注意: cube 端**不**写入 freshcheck 派生表(零同步原则), 但 cube 端 `freshcheck_pool_code` cube 跟 collect-ai 端 `freshcheck_pool_code` PG 表是同一份业务主数据的两份存储(后端 service 写入 PG 后异步同步到 cube; 或者人工在 cube 端维护,后端定时拉)。

---

**下一份**:`docs/freshcheck-settlement.md` — 6 步结算引擎详细设计
