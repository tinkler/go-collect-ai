# freshcheck W1 PR 总结

> **目标**: 提交 W1 阶段所有 commit 为 1 个 PR
> **分支**: `feat/freshcheck` (collect-ai) + `feat/freshcheck-cube` (cube-agent-server)
> **基础**: `main` (已合并 feat/agentic-purchase)
> **W1 完工日期**: 2026-09-09
> **配套文档**: [freshcheck.md](freshcheck.md) (总入口)

---

## 一、PR 概览

| 项 | 详情 |
|---|---|
| 标题建议 | `feat(freshcheck): W1 数据模型 + cube + 配置 CRUD` |
| 模块名 | `freshcheck` (生鲜免日盘管理) |
| 影响范围 | collect-ai 13 张新表 + 13 个 HTTP 端点 / cube-agent-server 2 个 cube plugin |
| AGENTS.md 合规 | §12.1 严格遵守 (collect-ai 不直连思迅, 走 cube 抽象) |
| 配置变更 | 0 个 (无 env 变量新增) |
| 依赖新增 | 0 个 go.mod 新依赖 |

## 二、Commit 列表 (7 个, 跨 2 仓库)

### collect-ai `feat/freshcheck` (5 commit)

| Commit | 内容 | 行数 |
|---|---|---|
| `9d1adcf` | 6 份规划文档 (architecture/data-model/settlement/cube-plugins/h5-contract/rollout) | 2422 |
| `e85e889` | W1.1: 13 张表 + types.go + store 骨架 | 978 |
| `79e66b5` | W1.3: mappings.yaml + Executor 加 sales_with_refund / items_with_clsno | 207 |
| `c6a567d` | W1.4: 5 表 CRUD + HTTP + 路由 + RBAC perm (5 perm) | 1343 |
| `e8e49ac` | W1.5: CubeQuerier 5 方法 + 9 mock 单测 | 738 |

### cube-agent-server `feat/freshcheck-cube` (1 commit)

| Commit | 内容 | 行数 |
|---|---|---|
| `b9a06fa` | W1.2: sales_with_refund + items_with_clsno plugin (2 个 YAML) | 303 |

**总工作量**: 4296 行 (代码 + SQL + YAML + 文档 + 测试)

## 三、新增 13 张 PG 表 (W1.1)

### 配置表 (5)
- `freshcheck_sku_map` — 生鲜 SKU 映射 (货号 → 生鲜子类 + 关联特价码)
- `freshcheck_pool_code` — 特价码定义 (1元/斤 / 0.5元/斤)
- `freshcheck_loss_rate` — 损耗率规则 (子类 × 周转类 × 类型, 版本化)
- `freshcheck_threshold` — 业务阈值 K-V (12 行 seed)
- `freshcheck_category_track` — 品类结算轨道 (5 行 seed)

### 人工录入表 (2)
- `freshcheck_pool_event` — 入框/出框事件 (带 confidence + 去重 UNIQUE)
- `freshcheck_period_stock` — 周期盘点 (期末实盘)

### 派生表 (6)
- `freshcheck_settlement` — R1 周期单品毛利 (幂等键 UNIQUE)
- `freshcheck_alloc` — R2 特价归因明细
- `freshcheck_box_recon` — R3 框内对账
- `freshcheck_alert` — C1-C8 告警日志
- `freshcheck_loss_calibrate` — 损耗率校准历史
- `freshcheck_config_snap` — 配置变更快照 (审计)

### 13 张表存在性验证
启动时自动调 `Store.VerifyTablesExist()` 检查所有 13 张表都建好。

## 四、新增 cube plugin (W1.2)

### `sales_with_refund` (基于 siss_saleflow view + LEFT JOIN t_rm_saleflow)
- **核心**: 含 `is_refund` (0/1) + `voucher_no` 原单号 + `origin_flow_id` 退单原 flow_id
- **measure**: count / total_qnty / total_revenue / total_cost / total_gross_profit / refund_qnty / refund_amt / normal_revenue
- **dimension**: row_id / oper_date (time) / branch_no / item_no / item_name / item_clsno / item_clsname / item_brand / main_supcust / is_refund / sell_way / voucher_no / origin_flow_id

### `items_with_clsno` (t_bd_item_info + t_bd_item_cls LEFT JOIN)
- **核心**: 派生 `fresh_category` (leaf/root/aquatic/meat/frozen, NULL=非生鲜) + `turnover_class` (fast/slow)
- **measure**: count / stock_qty / fresh_count / leaf_count / root_count / aquatic_count / meat_count / frozen_count / slow_count
- **dimension**: item_no (PK) / item_name / item_clsno / item_clsname / fresh_category / turnover_class

## 五、新增 HTTP 端点 (W1.4 + W1.6)

### collect-ai `/api/v1/freshcheck/*` (14 个)

| Method | Path | 权限 | 说明 |
|---|---|---|---|
| GET | /freshcheck/health | 公开 | 启动 sanity check (13 张表齐不齐) |
| GET | /freshcheck/config/sku-map | settle:read | 列生鲜 SKU 映射 (按 fresh_category 过滤) |
| GET | /freshcheck/config/sku-map/:item_no | settle:read | 单条查 |
| PUT | /freshcheck/config/sku-map/:item_no | config:write | upsert |
| GET | /freshcheck/config/pool-codes | settle:read | 列特价码 (按 unit_price DESC) |
| GET | /freshcheck/config/pool-codes/:pool | settle:read | 单条查 |
| PUT | /freshcheck/config/pool-codes/:pool | config:write | upsert |
| GET | /freshcheck/config/loss-rates | settle:read | 列损耗率 (按子类过滤) |
| PUT | /freshcheck/config/loss-rates | config:write | upsert (新版本自动 close 旧版) |
| GET | /freshcheck/config/thresholds | settle:read | 列 12 行阈值 K-V |
| PUT | /freshcheck/config/thresholds/:key | config:write | 改单个阈值 (写 snap 审计) |
| GET | /freshcheck/config/category-tracks | settle:read | 列品类轨道 |
| PUT | /freshcheck/config/category-tracks/:fc | config:write | upsert |

## 六、新增 RBAC 权限 (5 个)

```
freshcheck:pool:write      入框/出框事件录入 (H5 录单用)
freshcheck:settle:read     查看周期结算结果 (R1-R5 报表)
freshcheck:settle:run      触发周期结算 + 重算
freshcheck:config:write    改生鲜SKU/特价码/损耗率/阈值/轨道
freshcheck:override        周期锁超线豁免
```

**角色分配** (与 docs/freshcheck-architecture.md §3 表格一致):
- `manager` — 全部 5 个
- `buyer` — settle:read + settle:run + config:write
- `floor` — pool:write (录单)
- `office` — settle:read (看报表)
- `owner` — 通配 `*` 自动覆盖

## 七、新增单测 (W1.5)

### mock 单测 (9 个全 PASS, 0 依赖 PG)
- `cube_test.go`: 9 个 (SalesWithRefundInWindow / PurchasesInWindow / AvgCost / StockSnapshot / FreshItemsByCategory / helpers)

### 集成测试 (12 个, 默认 SKIP, 等 PG 启动)
- `store_pg_test.go`: 5 表 CRUD + snap 审计 (5 张表 × 2-3 场景)
- 跑法: `FRESHCHECK_TEST_PG_DSN=... go test ./internal/freshcheck/...`

## 八、配置变更

### env 变量 (0 个新增)
W1 无新增 env 变量 (W1.6 的 FRESHCHECK_C7_CUBE_URL 在设计反思后已撤掉)

### 配置文件 (无变更)
- `configs/mappings.yaml` 新增 2 段 (sales_with_refund + items_with_clsno), 跟现有结构一致
- `cmd/server/main.go` 注入 `freshcheck.Store` (仅此)

### 数据库
- 13 张新表 (`freshcheck_*` 前缀) 在 `Migrate()` 自动 CREATE
- **11 行** `freshcheck_threshold` seed (W1.7 移除 c7_sync_diff_pct)
- 5 行 `freshcheck_category_track` seed
- 5 行 `permissions` seed
- 9 行 `role_permissions` 关联 seed

## 九、试运行验证 (待用户启 PG)

### 9.1 启动 PG + cube-agent-server
```bash
# 1) 启 PostgreSQL
#    (用户本地; 数据库名 collectai, 用户 postgres, 密码 postgres)
# 2) 启 cube-agent-server
cd F:\go\src\github.com\tinkler\cube-agent-server
git checkout feat/freshcheck-cube
go run ./cmd/agent
# 期望: "http server listening" + 2 个 freshcheck plugin 自动加载

# 3) 启 collect-ai (W1 PR)
cd F:\go\src\github.com\tinkler\collect-ai
git checkout feat/freshcheck
$env:RESTOCK_BRANCH_NO = "0001"
go run ./cmd/server
# 期望 log: "freshcheck: 13 张表就绪"
```

### 9.2 验证 13 张表自动建好
```bash
# PowerShell
$env:PGPASSWORD = "postgres"
psql -U postgres -d collectai -c "SELECT tablename FROM pg_tables WHERE tablename LIKE 'freshcheck_%' ORDER BY tablename;"
# 期望: 13 行
```

### 9.3 验证 seed 数据
```bash
psql -U postgres -d collectai -c "SELECT * FROM freshcheck_threshold ORDER BY key;"
# 期望: 11 行 (c1_pool_saturation_pct=15.00, c6_period_lock_leaf_days=7, ...)

psql -U postgres -d collectai -c "SELECT * FROM freshcheck_category_track ORDER BY fresh_category;"
# 期望: 5 行 (leaf-weekly / root-monthly / aquatic-biweekly / meat-biweekly / frozen-monthly)
```

### 9.4 跑集成测试
```bash
$env:FRESHCHECK_TEST_PG_DSN = "postgres://postgres:postgres@127.0.0.1:5432/collectai?sslmode=disable"
cd F:\go\src\github.com\tinkler\collect-ai
go test ./internal/freshcheck/... -v
# 期望: 21 个测试 (9 PASS + 12 PASS, 0 SKIP)
```

### 10.5 端到端 smoke
```bash
# 用 dev-login 拿 token
$token = (curl -X POST 'http://127.0.0.1:8089/api/v1/auth/dev-login?as_user=u_owner' -c cookies.txt).access_token

# 健康检查
curl http://127.0.0.1:8089/api/v1/freshcheck/health
# 期望: {"ok":true, "tables": [...], "count":13}

# 列 11 行阈值
curl -H "Authorization: Bearer $token" http://127.0.0.1:8089/api/v1/freshcheck/config/thresholds
# 期望: 11 个 K-V
```

完整 smoke 脚本见 `e2e/freshcheck_smoke.ps1` (15 个 PowerShell 测试, 含健康/鉴权/CRUD/集成测试)。

## 十、未做 (W2-W4)

- W2 事件录入 H5 (入框/出框/池状态重建/框内差值) — W2 待启
- W3 周期结算引擎 6 步 + 7 校验 (C7 移除) + 报表 R1-R5 — W3 待启
- W4 损耗率校准 + R6 日报 + 跨窗退货 + 异常闭环 — W4 待启

## 十一、设计反思 (W1.7 2026-09-09)

**W1 早期 W1.6 包含 C7 同步校验 + cube-agent-server `/admin/source-direct` 端点 + collect-ai `SyncChecker` cron**,
被 review 后整体撤掉。理由:

1. **零同步架构下 C7 语义不成立**: 需求 §2.1 "镜像库与源库汇总差异" 前提是"有镜像库", 但项目选
   "零同步" 架构 (按需 cube 聚合查询, 不落本地副本), 没有"两份独立数据"可供对账。
2. **cube-agent-server SourceDirect 破坏职责**: cube-agent-server 职责是"管理 cube 抽象", SourceDirect
   是"绕出 cube 暴露 SQL 注入面", 加白名单(3 表 + 5 ops + 15 columns)治标不治本, 后续会持续膨胀。
3. **思迅异常无须 collect-ai 监控**: 思迅库挂掉 → POS 收银停 → 业务停摆, 在 collect-ai
   端监控属于多余。

**撤掉清单**:
- cube-agent-server commit `86a4eb2` (revert `2ce79dc`)
- collect-ai: `source_client.go` / `source_client_test.go` / `sync_check.go` / `HTTPHealthSyncCheck` 全删
- pg.go seed: `c7_sync_diff_pct` 移除
- types.go: `ThrC7SyncDiffPct` 常量移除
- API: `/admin/sync-check` 端点移除
- env: `FRESHCHECK_C7_CUBE_URL` 不再使用
- RBAC: `freshcheck:override` perm 保留 (周期锁豁免仍用), 不再绑 C7

**净效果**:
- collect-ai 净减 ~700 行 (SourceClient + sync_check + 6 单测)
- cube-agent-server 净减 ~300 行 (SourceDirect + 装配)
- 13 张表保留, 11 行阈值保留, 5 类配置 CRUD 保留
- 9 个 cube mock 单测保留, 12 个集成测试保留

**未来需要时** (W3 性能优化时):
- 启动期 schema 校验: cube-agent-server 启动时 introspect 思迅视图, 对比 cube YAML, 不一致 5xx 拒绝启动
- 这才是合理的"健康校验"位置 — 启动期一次性, 不需要 cron

## 十二、Review 检查清单

- [ ] 13 张表 schema 跟 [freshcheck-data-model.md](freshcheck-data-model.md) 一致
- [ ] mappings.yaml 2 段结构正确 (业务名 → 物理名)
- [ ] 5 perm + 9 role 关联 seed 正确
- [ ] 13 HTTP 端点签名跟 [freshcheck-h5-contract.md](freshcheck-h5-contract.md) §1.5 一致
- [ ] 5 类配置 CRUD 入参校验严格 (枚举值 / 数值范围)
- [ ] 9 mock 单测全 PASS
- [ ] cube plugin YAML 8 条最佳实践 (RTRIM / type:time / pre-aggregate / explicit column)
- [ ] AGENTS.md §12 规则 (业务名/物理名分离, 严禁 import parser/agent)
- [ ] 确认 C7 撤干净 (pg.go / types.go / main.go / router.go / http_admin.go 都无残留)
