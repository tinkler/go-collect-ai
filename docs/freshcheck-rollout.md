# freshcheck W1-W4 实施计划

> 配套: `architecture.md` / `data-model.md` / `settlement.md` / `cube-plugins.md` / `h5-contract.md`
> 总周期: 4 周 (W1-W4), 每天 1-2 任务, 周末集成 + 验收
> 验收节点: W1 末 → 配 5 个特价码, W3 末 → 跑通 8 个验收场景, W4 末 → R6 日报自动推

---

## 0. 总体原则

1. **每周末集成一次**: 周末跑完所有 acceptance 场景, 当周任务才算 OK
2. **W1 必完成 PG 表 + 5 个配置 CRUD** (后面所有 W 都依赖)
3. **W3 末必须跑通 8 个验收场景** (需求 §7 强制)
4. **W4 末必须 R6 日报 + 告警闭环** (验收交付)
5. **任何任务都先写测试再写实现** (TDD, 沿用 restock 模式)
6. **每个文件改完跑 `go test ./internal/freshcheck/...` + `go vet ./internal/...`**

---

## W1 — 数据模型 + 基础 CRUD + 镜像 (5-6 天)

### W1.1 数据库 schema (Day 1, 1.5 人天)
- [ ] 写 `migrations/freshcheck_init.sql` (13 张表 CREATE TABLE IF NOT EXISTS, 沿用 pg.go Migrate 模式)
- [ ] 加到 `internal/store/pg.go:Migrate` 末尾
- [ ] 启动验证: `go run ./cmd/server`, psql 看表全建好
- [ ] seed `freshcheck_threshold` 12 行默认值
- [ ] seed `freshcheck_category_track` 5 行 (leaf/root/aquatic/meat/frozen)
- [ ] 写 `internal/freshcheck/types.go` (所有 struct + enum, 含 json tag)
- [ ] 跑 `go vet ./internal/...` 无报错

**DoD**: 13 张表 + 默认配置都建好, 启动后 psql 能查到

### W1.2 cube plugin 新增 (Day 2, 1.5 人天, 跨 cube-agent-server 仓库)
- [ ] 切到 `F:\go\src\github.com\tinkler\cube-agent-server` 仓库
- [ ] **新分支** `feat/freshcheck-cube` (从 main 切)
- [ ] 用 `cube-plugin-gen` skill 生成 `sales_with_refund` plugin.yaml
  - introspect 思迅 `t_rm_saleflow` 确认 `sale_way` 字段值
  - 确认 `origin_flow_id` 字段存在 (若无, 走 `voucher_no` 关联)
- [ ] 用 `cube-plugin-gen` skill 生成 `items_with_clsno` plugin.yaml
  - 跟 admin 确认生鲜子类 LIKE 规则 (5 个 clsname 模式)
- [ ] 加 `/v1/source-direct` HTTP 端点 (给 C7 用)
- [ ] 写 `plugins/sales_with_refund/plugin.yaml` + `plugins/items_with_clsno/plugin.yaml`
- [ ] 验证热加载: 改完 plugin.yaml, 重启 cube-agent-server, curl /v1/plugins 看到新

**DoD**: cube-agent-server 有 2 个新 plugin, 可 query

### W1.3 mappings.yaml + Executor (Day 2 后半, 0.5 人天)
- [ ] `configs/mappings.yaml` 加 `sources.hbpos.sales_with_refund` 段
- [ ] `configs/mappings.yaml` 加 `sources.hbpos.items_with_clsno` 段
- [ ] `internal/business/executor.go` 加 `SalesWithRefund()` / `FreshItems()` 方法
- [ ] 走 `e.query()` 私有方法, 跟 restock 模式一致
- [ ] 验证: `go test ./internal/business/...` 全过

**DoD**: 业务层有 2 个新方法, 端到端查 1 条数据返回

### W1.4 配置表 CRUD (Day 3, 1.5 人天)
- [ ] `internal/freshcheck/store.go` (PG 仓库) 加 5 类: sku_map / pool_code / loss_rate / threshold / category_track
- [ ] `internal/freshcheck/http_admin.go` 加 CRUD 接口
- [ ] `internal/api/router.go` 加 `/api/v1/freshcheck/config/*` 路由
- [ ] 配置变更前自动写 `freshcheck_config_snap` (helper function)
- [ ] 加 4 个新 perm: `freshcheck:pool:write` / `freshcheck:settle:read` / `freshcheck:settle:run` / `freshcheck:config:write` / `freshcheck:override`
- [ ] seed rbac 角色表加 perm 关联 (owner 拿 *, manager 拿全部, buyer 拿 settle+config, floor 拿 pool:write, office 拿 settle:read)
- [ ] 写单测: `internal/freshcheck/store_test.go` (CRUD 覆盖)

**DoD**: admin 能配 5 个特价码, 配损耗率, 配轨道

### W1.5 CubeQuerier (Day 3 后半, 0.5 人天)
- [ ] `internal/freshcheck/cube.go` 调 `business.Executor`
- [ ] 方法: `SalesInWindow()` / `PurchasesInWindow()` / `AvgCost()` / `StockSnapshot()` / `FreshItems()`
- [ ] 写单测: 用 mock cube, 验证参数和返回

**DoD**: 单元测试全过, 端到端能调通 (用真实 cube 验证 1 次)

### W1.6 同步校验 cron (Day 4, 0.5 人天)
- [ ] `internal/freshcheck/sync_check.go`: C7 校验
- [ ] `internal/freshcheck/service.go` 加 cron: 每小时 1 次
- [ ] 告警写 `freshcheck_alert` (rule_code='C7', severity='block' if fail)
- [ ] 自测: mock cube 返不同 sum, 验证告警触发

**DoD**: cron 跑起来, mock 数据能触发 C7

### W1.7 集成验证 (Day 4 后半, 0.5 人天)
- [ ] 写 e2e/freshcheck/w1_smoke.sh
- [ ] 走完: 启动 server → 配 5 个特价码 → 调 cube 拉 1 条数据 → 验证 PG 表写入
- [ ] 更新 AGENTS.md: 加 freshcheck 模块名到 §1 例外
- [ ] 提交: `feat/freshcheck` 分支 1 个 PR, 标题 "W1: 数据模型 + 基础 CRUD"

---

## W2 — 事件录入 H5 + 框内差值 + 池状态重建 (5-6 天)

### W2.1 池事件 CRUD (Day 5, 1 人天)
- [ ] `internal/freshcheck/store.go` 加 `freshcheck_pool_event` 表 CRUD
- [ ] `internal/freshcheck/http_h5.go` 加 `POST /pool/events/in` / `POST /pool/events/out` 接口
- [ ] 入参校验: item_no ∈ sku_map, pool_code ∈ pool_code
- [ ] 兜底: 出框检查发现无入框, 提示补录, 全部 `confidence=low`
- [ ] 写单测: 入框/出框/补录/重复提交冲突 (UNIQUE 约束)
- [ ] 端到端: 用 curl 录 1 条入框, psql 看写入

**DoD**: 4 类事件录入接口全通, 单测覆盖

### W2.2 池状态重建 (Day 5 后半, 1 人天)
- [ ] `internal/freshcheck/pool_state.go` 实现 `At(poolCode, t) []PoolItem`
- [ ] 算法: 扫描 `freshcheck_pool_event`, in - out 累计, 过滤 `event_time ≤ t`
- [ ] 跨筐降级: downgrade 时关当前 SKU @ 当前 pool, 开目标 pool 虚拟入框
- [ ] 加 `GET /pool/state` 接口
- [ ] 写单测: 3 个 SKU × 2 框 × 5 事件, 验证任意时刻集合

**DoD**: 单测覆盖正常/降级/补录/空状态, 端到端 1 次

### W2.3 框内差值算法 (Day 6, 1 人天)
- [ ] `internal/freshcheck/box_diff.go` 实现 `ComputeBoxDiff(periodID, poolCode, segStart, segEnd) (BoxRecon, error)`
- [ ] 算法: in_total - out_total - pos_total = box_loss_kg
- [ ] 出框去向 4 类分别聚合
- [ ] 写 `freshcheck_box_recon` 表
- [ ] 写单测: 5 个场景 (正常/全空/全 spoilage/降级/补录)

**DoD**: 单测覆盖, 端到端: 录事件后调 /pool/diff 看差异

### W2.4 H5 入框登记页面 (Day 6 后半, 0.5 人天, 子 agent)
- [ ] **开子 agent** 在 `F:\weixinapp\supermarket-ai` 工作
- [ ] 给子 agent 任务: 写 `freshcheck-bin.html` + `js/freshcheck/bin.js` + `css/freshcheck.css`
- [ ] UI: 大按钮 (1元/0.5元) + SKU 搜索 (autocomplete) + 称重 input + 提交
- [ ] 离线: localStorage 暂存, 提交失败重试
- [ ] perm 守卫: `require(['freshcheck:pool:write'])`
- [ ] 我 review 子 agent 产出, 合并到 supermarket-ai 仓库

**DoD**: H5 页面在浏览器跑通, 能录事件

### W2.5 H5 出框检查页面 (Day 7, 1 人天, 子 agent)
- [ ] **子 agent** 写 `freshcheck-pool.html` + `js/freshcheck/pool.js`
- [ ] UI: 早晨打开 → 拉当前池状态 → 逐 SKU 填剩余量 + 去向 (4 选 1)
- [ ] 兜底: 框内有实物但无入框记录, 弹"补录"modal
- [ ] 提交后看 R3 框内对账预览
- [ ] 我 review + 合并

**DoD**: 早晨店长跑通出框检查, 看到框内差值

### W2.6 集成验证 (Day 7 后半, 0.5 人天)
- [ ] 端到端: 1 个特价码 × 3 个 SKU × 2 天事件, 验证状态重建 + 框内差值
- [ ] 写 e2e/freshcheck/w2_pool_state.sh
- [ ] 提交: W2 1 个 PR

---

## W3 — 周期结算引擎 + 8 校验 + 报表 (7-8 天, **最重要**)

### W3.1 周期盘点录入 (Day 8, 0.5 人天)
- [ ] `internal/freshcheck/store.go` 加 `freshcheck_period_stock` 表 CRUD
- [ ] `internal/freshcheck/http_h5.go` 加 `POST /period/stock` + `GET /period/stock/template`
- [ ] Excel 解析: 用 GoExcel 或 SheetJS (前端)
- [ ] 强校验: item_no ∉ pool_code (C8)
- [ ] 写单测

**DoD**: H5 + Admin 都能录盘点, Excel 模板能下载

### W3.2 倒挤公式 + 报损剥离 (Day 8 后半, 1 人天)
- [ ] `internal/freshcheck/backflush.go`: 倒挤 + 报损
- [ ] 查损耗率 (effective_to IS NULL, effective_from 最大)
- [ ] 快/慢周转公式
- [ ] 写单测: 10 个 SKU 各种 turnover class + 各种 window_days

**DoD**: 单测覆盖, 端到端调一次算出来

### W3.3 多筐串联分摊 (Day 9-10, 2 人天)
- [ ] `internal/freshcheck/allocate.go`: Step 5 完整实现
- [ ] 框内差值法 (主) + 倒挤回退 (备)
- [ ] 物理约束 + 多次重分配
- [ ] 串联扣减 (高价先, 跨筐降级)
- [ ] 写单测: 8 个场景 (1 框 1 SKU / 1 框 N SKU / N 框 1 SKU 降级 / ...)

**DoD**: 8 个单测场景全过

### W3.4 6 步编排 (Day 10 后半, 1 人天)
- [ ] `internal/freshcheck/settle.go` `Service.RunSettlement()` 编排 Step 1-6
- [ ] `http_h5.go` 加 `POST /settle/run` 接口
- [ ] 幂等键: hash(branch + track + window + operator)
- [ ] 事务: 失败回滚全部派生
- [ ] 写单测: mock cube, 走完整 6 步

**DoD**: 单元测试 + 端到端: curl 触发一次结算, R1 数据写入

### W3.5 8 条校验 (Day 11, 1.5 人天)
- [ ] `internal/freshcheck/validate.go` C1-C8 全实现
- [ ] C7 已在 W1.6 实现, 验证挂入
- [ ] 每条校验写单测
- [ ] 告警写 `freshcheck_alert` 表
- [ ] 端到端: 故意触发 C1/C2/C5/C6 看告警

**DoD**: 8 条校验全过, 告警能写入并查

### W3.6 守恒校验 + 双轨互验 (Day 12, 1 人天)
- [ ] C3 / C4 强校验, 不通过即 transaction rollback
- [ ] 框内差值 vs 整窗倒挤偏差写入 `freshcheck_alloc.deviation_*`
- [ ] 偏差 > 10% 标记 `needs_review=true`
- [ ] 写单测: 故意打破守恒看回滚

**DoD**: 守恒校验生效, 偏差告警生效

### W3.7 报表 R1-R5 接口 (Day 12 后半, 1 人天)
- [ ] `http_h5.go` 加:
  - `GET /settle/periods` — 历史期列表
  - `GET /settle/periods/:id` — R1 单品毛利
  - `GET /settle/periods/:id/alloc` — R2 分摊
  - `GET /settle/periods/:id/box` — R3 框内对账
  - `GET /settle/periods/:id/alerts` — R5 异常清单
- [ ] R4 损耗趋势从 `freshcheck_loss_calibrate` 读
- [ ] 写单测

**DoD**: 5 类报表接口全过

### W3.8 H5 结算 + 报表页面 (Day 13, 1 人天, 子 agent)
- [ ] **子 agent** 写 `freshcheck-settle.html` + `freshcheck-report.html` + `js/freshcheck/settle.js` + `report.js`
- [ ] UI: 选轨道 → 录盘点 → 触发结算 → 进度 → 跳转 R1
- [ ] 报表: 表格 (R1) + 详情 (R2-R5) + CSV 导出
- [ ] 我 review + 合并

**DoD**: H5 跑通完整结算流, 老板能看到毛利

### W3.9 验收 8 场景 (Day 13 后半, 0.5 人天, **强制**)
- [ ] 写 `e2e/freshcheck/case01..08.sh` (8 个验收场景)
- [ ] 跑全 8 个, 必须 100% 过
- [ ] 修 bug (按需)
- [ ] 提交: W3 1 个 PR, 包含所有 e2e 脚本

**DoD**: 8 个验收场景 ALL PASS

---

## W4 — 损耗率校准 + R6 日报 + 异常闭环 (5-6 天)

### W4.1 损耗率校准 (Day 14, 1 人天)
- [ ] `internal/freshcheck/loss_rate.go` 实现校准
- [ ] 结算完成后自动跑, 写 `freshcheck_loss_calibrate`
- [ ] 偏差 > 20% 写 `freshcheck_alert` (rule='LOSS_CALIB', severity='info')
- [ ] `POST /loss-calibrate/:id/action` 处置
- [ ] 写单测

**DoD**: 校准流程闭环, admin 能处置

### W4.2 R6 日报 (Day 14 后半, 1 人天)
- [ ] `internal/freshcheck/r6_daily.go` 实现日报聚合
- [ ] 内容: 昨日漏登 / 框损 / 周期锁超线 / 当日特价销售
- [ ] cron: 每日 8:00 跑, 写入 `freshcheck_daily_report` (新表, W4 加)
- [ ] 推送: 企微 `wecom.SendAppChat` 店长
- [ ] `GET /daily-report?date=...` 接口
- [ ] 写单测

**DoD**: R6 每天自动推, H5 能查历史

### W4.3 跨窗退货 (Day 15, 1 人天)
- [ ] `internal/freshcheck/settle.go` Step 2.2 完善
- [ ] 加 `freshcheck_recalc_queue` 表 (W4)
- [ ] 跨期重算: 排入队列, 不阻塞当前结算
- [ ] `POST /settle/recalc/queue` admin 触发
- [ ] 写单测: 场景 6

**DoD**: 跨窗退货触发重算, 不影响当窗

### W4.4 周期锁超线 + 豁免 (Day 15 后半, 0.5 人天)
- [ ] C6 告警 (Step 1 已有)
- [ ] `POST /settle/periods/:id/override` admin 豁免
- [ ] 写 `freshcheck_alert` 记录豁免原因
- [ ] 写单测: 场景 3

**DoD**: 超线阻断 + 豁免流程

### W4.5 H5 配置管理 (Day 16, 0.5 人天, 子 agent)
- [ ] **子 agent** 写 admin 后台页面: `admin/freshcheck.html`
- [ ] UI: SKU 映射表 / 特价码 / 损耗率 / 阈值 / 轨道 / 校准处置
- [ ] 我 review + 合并

**DoD**: admin 能在 H5 配所有配置, 不需直连 PG

### W4.6 全量集成 + 8 场景回归 (Day 16 后半, 1 天, **强制**)
- [ ] 重跑 W3.9 8 个场景, 100% 过
- [ ] 跑 W1+W2+W3+W4 所有 e2e
- [ ] 性能压测: 1 次结算 ≤5min
- [ ] 修 bug
- [ ] 写部署文档: `docs/freshcheck-deploy.md`

**DoD**: 全量 ALL PASS

### W4.7 文档 + 试运行准备 (Day 17, 0.5 天)
- [ ] 更新 `docs/freshcheck-architecture.md` 加试运行期说明
- [ ] 写 `docs/freshcheck-runbook.md` (运维手册)
- [ ] 写培训材料: 给店长的"早晨 5 分钟"操作卡
- [ ] 准备首期建账数据 (从历史 t_bd_item_info 筛生鲜, 灌入 sku_map)
- [ ] 提交: W4 1 个 PR, 总 commit 合并 → main

**DoD**: 文档齐全, 可上线

---

## 风险与回退 (风险缓解)

| 风险 | 概率 | 缓解 |
|---|---|---|
| cube 聚合查询慢 | 中 | W1 用 view 优先; W2 benchmark, 不行就上 cache |
| 周期结算超 5min | 中 | W3 benchmark 一次; 超了用 TEMP TABLE 隔离 |
| 跨期重算死锁 | 低 | PG advisory lock + 重算队列 |
| H5 录入员不熟悉 | 中 | 兜底补录 + R6 早报; W4 培训 |
| LLM 不可用 | — | 纯计算, 不依赖 LLM, 风险 0 |
| 子 agent 产出不符 | 中 | 每个子 agent 任务都附 acceptance, 我亲自 review |

---

## 每日 standup (root + worker 子 agent)

- **早 9:30**: 看 mavis agent list, 启动当日子 agent
- **午 13:00**: 子 agent 进度跟进
- **晚 18:00**: 当日 commit, 跑当日 acceptance

---

## 提交节奏

- W1 末: PR #1 (W1, 6 commit)
- W2 末: PR #2 (W2, 8 commit)
- W3 末: PR #3 (W3, 12 commit, 含 8 个验收场景)
- W4 末: PR #4 (W4, 8 commit, 含文档)

合并到 main: 每个 PR 用户 review 后再 merge。

---

## 配套文档清单 (已完成 ✅, W1-W4 实施时按需细化)

- ✅ `docs/freshcheck-architecture.md` — 整体架构
- ✅ `docs/freshcheck-data-model.md` — 13 张表 schema
- ✅ `docs/freshcheck-settlement.md` — 6 步结算 + 8 校验
- ✅ `docs/freshcheck-cube-plugins.md` — cube 依赖
- ✅ `docs/freshcheck-h5-contract.md` — 前后端 API
- ✅ `docs/freshcheck-rollout.md` — W1-W4 计划 (本文件)
- ⏳ W4 末: `docs/freshcheck-deploy.md` + `docs/freshcheck-runbook.md`

---

**下一步**: 跟用户对齐, 决定:
1. **是否立刻开始 W1.1** (写 13 张表 schema + 改 pg.go)
2. 或者先**调整规划** (改 W1-W4 顺序, 砍掉某些功能, 改 perm 命名等)
3. 或者先**搭前端骨架** (在 supermarket-ai 同步开子 agent 写 H5 框架)

请告诉我下一步。
