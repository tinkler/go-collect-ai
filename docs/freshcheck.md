# freshcheck — 生鲜免日盘管理子系统

> 分支: `feat/freshcheck`
> 需求: [生鲜免日盘管理扩展子系统设计需求文档.md](生鲜免日盘管理扩展子系统设计需求文档.md) v1.0
> 后端: `F:\go\src\github.com\tinkler\collect-ai\internal\freshcheck\`
> 前端: `F:\weixinapp\supermarket-ai\freshcheck-*.html`
> 数据源: `F:\go\src\github.com\tinkler\cube-agent-server` (零同步, 按需聚合)

---

## 规划文档 (已写完 ✅)

| # | 文档 | 内容 | 行数 |
|---|---|---|---|
| 1 | [architecture.md](freshcheck-architecture.md) | 整体架构 / 技术选型 / 模块边界 / 阶段拆分 | ~280 |
| 2 | [data-model.md](freshcheck-data-model.md) | 13 张表 schema / 字段 / 索引 / 守恒 | ~470 |
| 3 | [settlement.md](freshcheck-settlement.md) | 6 步结算引擎 + 8 校验 + 损耗率校准 + 幂等 | ~430 |
| 4 | [cube-plugins.md](freshcheck-cube-plugins.md) | cube 端依赖清单 + 新增 2 plugin + C7 校验 | ~270 |
| 5 | [h5-contract.md](freshcheck-h5-contract.md) | 前后端 API 契约 + 5 个 H5 页面 + RBAC | ~360 |
| 6 | [rollout.md](freshcheck-rollout.md) | W1-W4 实施计划 (4 周, 17 天) | ~310 |

---

## 核心决策(已与用户对齐, 2026-09-08)

| 决策 | 选择 |
|---|---|
| 模块名 | **freshcheck** (内部目录 + 表前缀 + API 路径) |
| 前端位置 | `F:\weixinapp\supermarket-ai` (已存在, 独立 H5 项目) |
| 数据源策略 | **零同步**, cube 实时聚合查询 |
| LLM skill | **不建** (纯数学+配置表驱动) |
| 业务阈值 | **PG 配置表**, 改阈值免 build |
| 派生存储 | **纯落 PG** (全派生结果, 已结算只读) |
| 分支基线 | `main` 已合并 `feat/agentic-purchase`, 在其上切 `feat/freshcheck` |

---

## 13 张表总览 (W1 创建)

| # | 表 | 性质 |
|---|---|---|
| 1 | `freshcheck_sku_map` | 配置 (生鲜SKU映射) |
| 2 | `freshcheck_pool_code` | 配置 (特价码) |
| 3 | `freshcheck_loss_rate` | 配置 (损耗率) |
| 4 | `freshcheck_threshold` | 配置 (业务阈值) |
| 5 | `freshcheck_pool_event` | 人工 (入框/出框事件) |
| 6 | `freshcheck_period_stock` | 人工 (周期盘点) |
| 7 | `freshcheck_category_track` | 配置 (品类轨道) |
| 8 | `freshcheck_settlement` | 派生 (R1 周期单品毛利) |
| 9 | `freshcheck_alloc` | 派生 (R2 分摊明细) |
| 10 | `freshcheck_box_recon` | 派生 (R3 框内对账) |
| 11 | `freshcheck_alert` | 派生 (C1-C8 告警) |
| 12 | `freshcheck_loss_calibrate` | 派生+人工 (损耗率校准) |
| 13 | `freshcheck_config_snap` | 审计 (配置变更快照) |

---

## W1-W4 里程碑

| 阶段 | 内容 | 验收 |
|---|---|---|
| **W1** (5-6 天) | 数据模型 + 基础 CRUD + cube 调用 | admin 配 5 个特价码 |
| **W2** (5-6 天) | 事件录入 H5 + 框内差值 + 池状态重建 | 早晨出框检查, 框内差异可见 |
| **W3** (7-8 天) | 周期结算 6 步 + 8 校验 + R1-R5 报表 | **8 个验收场景 ALL PASS** |
| **W4** (5-6 天) | 损耗率校准 + R6 日报 + 跨窗退货 | R6 自动推 + 全量回归 |

---

## 快速命令

```bash
# 切到 freshcheck 分支
cd F:\go\src\github.com\tinkler\collect-ai
git checkout feat/freshcheck

# 启动 server (开发)
go run ./cmd/server

# 跑 freshcheck 测试
go test ./internal/freshcheck/...

# 端到端
bash e2e/freshcheck/w1_smoke.sh

# 切到 cube-agent-server 仓库 (W1.2 用)
cd F:\go\src\github.com\tinkler\cube-agent-server
git checkout -b feat/freshcheck-cube
```

---

## 当前状态 (2026-09-08)

- ✅ 分支已切: `feat/freshcheck`
- ✅ 6 份规划文档已写完 + 已 git add (待用户 review)
- ⏳ W1 待开工 (用户确认规划后即开始)
- ⏳ 前端 supermarket-ai 待开子 agent 协调

---

## 下一步 (请用户决定)

1. **直接开始 W1.1** (写 13 张表 schema + 改 pg.go + 跑通) — 推荐
2. **调整规划** (改 W 顺序 / 砍功能 / 改 perm / 改命名)
3. **先搭前端骨架** (在 supermarket-ai 同步开子 agent 写 H5 框架)
4. **暂缓** (先把其他工作做完再回来)
