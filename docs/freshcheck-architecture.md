# 生鲜免日盘管理子系统 (freshcheck) — 架构总览

> 配套需求文档:`docs/生鲜免日盘管理扩展子系统设计需求文档.md`(v1.0)
> 分支:`feat/freshcheck`(从 main 切出,已合并 feat/agentic-purchase)
> 前端:`F:\weixinapp\supermarket-ai`(独立 H5 项目, git main, 已具备 RBAC+企微+采购基础)
> 数据源:`cube-agent-server`(不直连思迅, 走 cube-plugin-gen skill)

---

## 一、需求回顾(8 大部分)

| # | 主题 | 关键内容 |
|---|---|---|
| 1 | 项目概述 | 3 大问题: 共享特价码归因断裂 / 免日盘成本核算 / 多节奏结算 |
| 2 | 数据需求 | 思迅只读镜像 + 人工录入(特价池事件/盘点) + 基础配置 |
| 3 | 数据加工 | 池状态重建 + 6 步周期结算 + 双轨互验 + 8 条校验 |
| 4 | 产出结果 | R1-R5 周期报表 + R6 日报 + R7 采购建议 |
| 5 | 非功能 | 安全(只读镜像, 旁挂库隔离) / 容错(离线录入, 幂等结算) |
| 6 | 业务规则(10 条硬约束) | 特价码识别 / 倒挤公式 / 退货回冲 / 多筐降级串联 / 周期锁等 |
| 7 | 验收场景(8 个最小集) | 周结/月结/不定时结/降级/漏录/跨窗退货/同步失败/幂等 |
| 8 | 术语 | 特价码/倒挤清仓/框内差值法/框损/品类结算轨道/周期锁 |

## 二、关键决策(2026-09-08 用户拍板)

| 决策 | 选择 | 理由 |
|---|---|---|
| 模块名 | **freshcheck** | 跟 restock/purchasealert 命名风格统一 |
| 表前缀 | `freshcheck_*` | 11 张新表 |
| API 前缀 | `/api/v1/freshcheck/*` | 跟 restock 一致 |
| 数据源 | **cube-agent-server 聚合查询**(零同步) | 用户特殊说明 #2, 不做数据落地镜像 |
| LLM skill | **不建**(纯数学+配置表驱动) | 业务判断全是"阈值+分类+公式",无 LLM 推理 |
| 业务阈值 | **PG 表**(`freshcheck_threshold`/`freshcheck_loss_rate`) | 不硬编码 Go,改阈值免 build |
| 周期结算存储 | **纯落 PG**(全派生结果) | 已结算只读约束简单,可追溯可重算 |
| 前端位置 | **`F:\weixinapp\supermarket-ai`**(已存在) | 独立 H5, 子 agent 在此开发 |

## 三、模块边界(强制约束)

### 3.1 不允许做的事

1. **不直连思迅 HB POS 数据库**(走 cube)
2. **不写思迅主库**(旁挂库 = collect-ai 现有 PG 实例, 同一 schema)
3. **不同步销售/采购/库存数据到本地表**(全部 cube 聚合查询, 用完即弃)
4. **不改造 POS 收银端/条码秤**(特价识别全靠特价码货号本身)
5. **不在 Go 写业务判断函数**(走 PG 配置表 + 数学公式)
6. **不写"判断/分类/建议"类 skill**(无 LLM 推理需求)
7. **不覆盖已结算窗口结果**(只能走带审计的"重算"流程)

### 3.2 允许做的事

1. 人工录入界面(入框/出框/盘点/基础配置)
2. cube 聚合查询(sales/purchases/t_bd_item_info + 新加 sales_with_refund/items_with_clsno 等)
3. 周期结算引擎(纯计算, 写 freshcheck_settlement / freshcheck_alloc / freshcheck_box_recon)
4. 校验与告警日志
5. 周期报表 R1-R5 + 日报 R6
6. 损耗率校准(派生)
7. H5 移动端录入 + 报表展示

## 四、技术架构

### 4.1 数据流(零同步 + cube 实时聚合)

```
[思迅 HB POS v10 / SQL Server 2008 R2]
        ↓ (只读, cube-agent-server 内置连接)
[cube-agent-server / plugins/]
  ├── sales                  (已有, 缺退款标记)
  ├── sales_with_refund      (新增, 含退货原单号)
  ├── purchases              (已有)
  ├── t_bd_item_info         (已有, 含 stock_qty)
  ├── inventory_current      (已有)
  ├── items_with_clsno       (新增, 联表给生鲜子类)
  └── freshcheck_pool        (新增, 特价码定义)
        ↓ (HTTP /v1/load)
[collect-ai / internal/freshcheck/]
  ├── cube.go            CubeQuerier 调 Gateway
  ├── settle.go          周期结算引擎(6 步流程)
  ├── box.go             框内差值算法 + 状态重建
  ├── validate.go        8 条校验
  ├── loss.go            损耗率校准
  └── store.go           PG 仓库(人工录入 + 派生结果)
        ↓ (PG / HTTP)
[F:\weixinapp\supermarket-ai/h5-freshcheck/]
  ├── freshcheck-pool.html    早晨出框检查
  ├── freshcheck-bin.html     打称入框登记
  ├── freshcheck-stock.html   周期盘点录入
  ├── freshcheck-settle.html  触发结算 + 看报表
  └── freshcheck-report.html  R1-R5 报表查看
```

### 4.2 关键技术选型

| 维度 | 选型 | 理由 |
|---|---|---|
| HTTP 框架 | gin(沿用) | 跟 collect-ai 一致 |
| PG 驱动 | pgx/v5 + pgxpool(沿用) | 已有模式 |
| 配置 | viper(沿用) + PG 业务配置表 | 业务阈值走 PG |
| Cube 客户端 | business.Gateway(沿用 12.1 规则) | 严禁 import parser/agent |
| LLM | 无(纯计算) | 需求里全是数学+分类 |
| RBAC | 沿用 role + user_roles(6.2 配 freshcheck perm) | 新增 4 个 perm |
| 报表导出 | csv(轻量, Excel 直接打开) | 后续可换 xlsx skill |
| 离线录入 | 前端 IndexedDB 暂存 + 恢复后补传 | H5 端实现 |
| 多并发结算 | 分布式锁(同一品类轨道, 同时只能一个结算) | PG advisory lock |

### 4.3 模块目录(collect-ai)

```
internal/freshcheck/
├── types.go            # 所有 struct + enum (In/Out/Box/Allocation/...)
├── cube.go             # CubeQuerier 调 Gateway (零同步, 按需聚合)
├── store.go            # PG 仓库 (人工表 + 派生表)
├── pool_state.go       # 池状态重建 (事件流 → 任意时刻在框集合)
├── box_diff.go         # 框内差值算法 (入框-出框-报损)
├── backflush.go        # 倒挤公式 + 报损剥离
├── allocate.go         # 多筐串联分摊 (按价格优先级, 跨筐降级)
├── settle.go           # 6 步结算引擎 (Step1-6 编排)
├── validate.go         # 8 条校验 (C1-C8) + 守恒检查
├── loss_rate.go        # 损耗率校准 (派生 + 待办)
├── r6_daily.go         # R6 日报 (每早推)
├── http_h5.go          # H5 接口 (录/查/触发结算)
├── http_admin.go       # Admin 接口 (配置/重算/豁免)
└── service.go          # 编排 (Start/Stop + 调度)

cmd/server/main.go      # 注入 freshcheck.Service
internal/api/router.go  # 加 /api/v1/freshcheck/* 路由
internal/store/pg.go    # 加 freshcheck_* 表 CREATE TABLE IF NOT EXISTS
```

### 4.4 仓库表(11 张,全部 `freshcheck_` 前缀)

| # | 表 | 性质 | 说明 |
|---|---|---|---|
| 1 | `freshcheck_sku_map` | 配置 | 生鲜SKU映射(货号→生鲜子类) |
| 2 | `freshcheck_pool_code` | 配置 | 特价码定义(货号→计价/优先级) |
| 3 | `freshcheck_loss_rate` | 配置 | 损耗率规则(子类×类型×快/慢周转) |
| 4 | `freshcheck_threshold` | 配置 | 业务阈值(C1/C2/C6/校准偏差) |
| 5 | `freshcheck_pool_event` | 人工 | 特价池事件(入框/出框,带重量+置信度) |
| 6 | `freshcheck_period_stock` | 人工 | 周期盘点(期末实物数) |
| 7 | `freshcheck_category_track` | 配置 | 品类结算轨道(子类→周期锁) |
| 8 | `freshcheck_settlement` | 派生 | 周期结算结果(每SKU每窗口) |
| 9 | `freshcheck_alloc` | 派生 | 归因分摊明细(每SKU每特价框每时段) |
| 10 | `freshcheck_box_recon` | 派生 | 框内对账(入框合计/出框/框损) |
| 11 | `freshcheck_alert` | 派生 | 校验告警(C1-C8 触发记录) |
| 12 | `freshcheck_loss_calibrate` | 派生+人工 | 损耗率校准历史(实测/偏差/确认) |
| 13 | `freshcheck_config_snap` | 审计 | 配置变更前快照(支持回滚) |

> 需求文档列了 8 个实体, 我加了 2 个(轨道配置 + 配置快照), 11→13 张。

## 五、阶段拆分(W1-W4)

### W1: 数据模型 + 基础 CRUD + 配置
- 13 张表 schema
- pg.go Migrate 加表
- 商品映射/特价码/损耗率/阈值的 CRUD(管理端)
- 基础 cube 调用(sku 列表 / 库存 / 销售窗口)
- 验收: admin 能配 5 个特价码, 能配损耗率表

### W2: 事件录入 H5 + 框内差值 + 池状态重建
- 入框/出框事件录入 H5
- 漏录兜底检测
- 池状态重建(任意时刻在框集合)
- 框内差值算法
- 验收: 早晨店长出框检查,系统能算出"框内不明差异"

### W3: 周期结算引擎 + 双轨互验 + 报表 R1-R5
- 6 步结算 Step 1-6
- 8 条校验 C1-C8
- 双轨互验(框内 vs 倒挤)
- R1 周期单品毛利表 / R2 特价归因明细 / R3 框内对账 / R4 损耗趋势 / R5 异常清单
- 验收: 3 SKU 共享 1 元框周结 → R1 守恒 / R2 分摊正确 / 框损产生

### W4: 损耗率校准 + R6 日报 + 异常闭环
- R6 早报(每日自动)
- 跨窗退货回冲 + C5 触发重算
- 周期锁超线 C6 告警 + 管理员豁免
- 损耗率校准流程(偏差 20% → 待办)
- 8 个验收场景全跑通

## 六、风险与对策

| 风险 | 概率 | 对策 |
|---|---|---|
| cube 聚合查询慢(单店 500 SKU × 1 年流水) | 中 | cube-plugin-gen skill 出静态 SQL, 走 view 优先; 加日期/货号索引 |
| 周期结算性能瓶颈(双轨全量) | 中 | 用 TEMP TABLE 隔离中间计算; 单 SKU 独立事务; 6 步可中断重入 |
| 人工录入事件时间错乱(补录) | 高 | 入框=打称时刻强一致, 改后框=早晨检查时刻, 全部带 `confidence` |
| 退货回冲跨期重算雪崩 | 中 | 重算只对"受影响的 SKU 子集", 不全量重跑 |
| 周期锁超线阻断结算 | 中 | 显式豁免 + 审计; 超锁窗口标记 yellow |
| 配置变更丢失 | 低 | 变更前自动 `freshcheck_config_snap` 快照 |
| H5 录入员不熟悉流程 | 中 | 漏录兜底 + 低置信度标记; R6 日报每天暴露漏登 |

## 七、首次跑通(MVP 验收标准)

按需求文档第 7 节验收场景, W3 结束需过:
- ✅ 场景 1: 周结 3SKU 共享 1元框(R1 守恒 / R2 分摊正确 / 框损产生)
- ✅ 场景 2: 月结池内容月中变更两次(事件时间切段归因)
- ✅ 场景 3: 不定时结 17天超 7天锁(C6 告警 + 豁免流程)
- ✅ 场景 4: 降级 1元→0.5元(两框串联, 分摊不重不漏)
- ✅ 场景 5: 漏录菠菜(兜底补录 + 低置信度)
- ✅ 场景 6: 跨窗退货(回冲原窗口, C5 通过)
- ✅ 场景 7: 同步失败 0.1% 差异(C7 告警 + 重试, 结算阻断)
- ✅ 场景 8: 幂等(同窗口跑两次结果一致)

## 八、未做(明确排除)

- ❌ 不做采购下单/销售收银/库存记账替代(思迅已有)
- ❌ 不追求"每斤特价实物精确归属 SKU"(业务约束下不可达)
- ❌ 不做实时毛利看板(周期粒度)
- ❌ 不处理非生鲜品类
- ❌ 不做 H5 之外的其他终端(企微 H5 + 管理 web 即可)
- ❌ R7 采购建议本期不做(只 R1-R6)

---

**配套文档**:
- `docs/freshcheck-data-model.md` — 13 张表 SQL 草案
- `docs/freshcheck-settlement.md` — 6 步结算引擎详细设计
- `docs/freshcheck-cube-plugins.md` — cube-agent-server 依赖清单
- `docs/freshcheck-h5-contract.md` — 前后端 API 契约
- `docs/freshcheck-rollout.md` — W1-W4 实施计划
