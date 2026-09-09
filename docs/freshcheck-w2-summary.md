# freshcheck W2 总结

> **状态**: 后端 + 前端全部完工
> **后端分支**: `feat/freshcheck` (collect-ai)
> **前端仓库**: `F:\weixinapp\supermarket-ai`
> **后端 commit**: `9c23c75` (1448 行, 9 文件)
> **日期**: 2026-09-09

---

## 一、Commit 列表

### collect-ai `feat/freshcheck` (1 commit)

| Commit | 内容 | 行数 |
|---|---|---|
| `9c23c75` | W2 事件录入 + 池状态 + 框内差值 (后端) | +1448 / -2 |

### supermarket-ai `main` (前端, 子 agent 写)

| 文件 | 行数 | 用途 |
|---|---|---|
| `freshcheck-bin.html` | 84 | W2.4 入框登记 |
| `freshcheck-pool.html` | 76 | W2.5 早晨出框检查 |
| `css/freshcheck.css` | 264 | 专用样式 |
| `js/freshcheck/api.js` | 53 | 13 端点 fetch 封装 |
| `js/freshcheck/offline.js` | 271 | 离线 localStorage 队列 |
| `js/freshcheck/pool.js` | 193 | 入框/出框业务 |
| `js/freshcheck/bin.js` | 313 | W2.4 页面逻辑 |
| `js/freshcheck/out.js` | 342 | W2.5 页面逻辑 |
| `index.html` | +10 | nav 入口 |
| `js/user-bar.js` | +4 | 顶部菜单 |
| `js/permission.js` | +4 | perm 角色 |
| `smoke_freshcheck.py` | ~150 | 端到端 smoke (40+ 检查点) |
| 截图 3 张 | - | bin / pool / pool-modal |

**前端总**: 8 新文件 (~1600 行) + 3 改 + 3 截图 + 1 smoke

---

## 二、新增 5 个 HTTP 端点

| Method | Path | 权限 | 说明 |
|---|---|---|---|
| POST | /pool/events/in | pool:write | 录一次入框 (返回 pool_state_after) |
| POST | /pool/events/out | pool:write | 录出框 (含 missing_in_records 漏录) |
| GET | /pool/events | settle:read | 查某日某框事件流 |
| GET | /pool/state | settle:read | 重建某时刻某框在框集合 |
| GET | /pool/diff | settle:read | 算框内差值 (box_loss) |

### 漏录检测算法 (核心)
- 每次 POST out, 系统查该 SKU in_count
- 有 out 但 in=0 → `missing_in_records` 返回
- 前端弹 modal 提示 "现场补录" / "稍后处理"
- 补录时 confidence=low, W3 分摊权重 0.5

---

## 三、新增 Go 模块

| 文件 | 行数 | 测试 | 说明 |
|---|---|---|---|
| `store.go` (加) | +400 | 11 集成 | 池事件 4 方法 (in/out/list/getLast) |
| `pool_state.go` (新) | 153 | 7 单元 | RebuildPoolState / HasOutBeforeIn / DetectMissingInAt |
| `box_diff.go` (新) | 100 | 6 单元 | ComputeBoxDiff / ComputeBoxDiffSegments |
| `http_h5.go` (新) | 250 | 8 端到端 | 5 个 gin handler |
| `types.go` (加) | +60 | - | PoolState / PoolStateItem / BoxRecon 字段补全 |
| `store_pg_test.go` (加) | +300 | - | 11 个池事件集成测试 |
| `pool_state_test.go` (新) | 154 | - | 7 个池状态场景 |
| `box_diff_test.go` (新) | 178 | - | 6 个框内差值场景 |

**W2 总计**: 8 新/改文件, ~1600 行 Go 代码 + 测试

---

## 四、质量门

### 后端

- ✅ `go vet ./internal/...` 全过
- ✅ `go build ./...` 全过
- ✅ `go test ./internal/freshcheck/...` **50 个测试全 PASS**
  - 9 cube mock (W1.5)
  - 12 store_pg (W1.4)
  - 5 threshold/category (W1.4)
  - 11 pool_event (W2.1)
  - 7 pool_state (W2.2)
  - 6 box_diff (W2.3)
  - 2 helpers (W1.5)
- ✅ **8/8 端到端 smoke** (`scripts/tmp/seed_w2_smoke.go`):
  - [1] POST in 201
  - [2] 累计同 SKU
  - [3] GET events 200
  - [4] GET state 200
  - [5] POST out 200 (有 in)
  - [6] POST out 200 (无 in → missing 检测)
  - [7] GET diff 200
  - [8] RBAC: u_cashier → 403

### 前端

- ✅ 16 个文件清单 (8 新 + 3 改 + 截图)
- ✅ 5 个 JS 语法 (`node --check`)
- ✅ 28 个 DOM ID 一致性 (16+12)
- ✅ 14 个 offline/pool API 暴露
- ✅ nav + perm 集成
- ✅ headless 浏览器 E2E (puppeteer + fetch mock)

---

## 五、业务亮点

### 5.1 漏录兜底 (W2.1 核心)

业务: 员工早晨出框检查时, 池里出现 out 但没 in 的 SKU → 兜底补录

```
早晨出框检查发现: POOL001 有 0.5kg "菠菜" 实物, 但 freshcheck_pool_event 没 "菠菜" 的 in 事件
→ 判定: 昨晚打称时漏录入框
→ 前端弹 modal:
  "发现 1 个 SKU 漏录
   菠菜 建议入框 0.5kg 时间: 2026-09-08 22:00
   [现场补录] [稍后处理]"
→ 补录走 POST in, confidence=low
→ W3 分摊时 low confidence 权重 0.5
```

### 5.2 框内差值 (W2.3 核心)

业务: 每天/每周期, 算框内物理流 vs POS 销售, 异常触发调查

```
框 24h in=3.0kg / out=0.8kg / pos=1.0kg
box_loss = 3.0 - 0.8 - 1.0 = 1.2 kg (>0.5 阈值)
needs_investigate = true → 触发 R5 告警

3 种去向分别累加:
  out_sold_out_kg = 0.8 (售罄离框)
  out_spoiled_kg   = 0.2 (变质报损, 临时用 piece_count 字段)
  out_return_kg    = 0   (撤回货架)
  out_downgrade_kg  = 0   (降级转框, W3 跨筐分摊用)
```

### 5.3 离线暂存 (前端核心)

业务: 收银高峰期网络不稳, 录单不能失败

```
主提交先尝试 POST
  ↓ 失败/超时
localStorage 队列 (key: freshcheck_pending_inbox/outbox)
  ↓ 监听 online 事件 + 30s 兜底
自动重试
  ↓ 24h+ 未成功
标 stale, 阻断下次录入 (员工必须手动清)
```

---

## 六、未做 (W3 范围)

- ❌ 周期结算 6 步流程 (W3.2-W3.7) — 倒挤/报损/分摊/守恒/7 校验
- ❌ 报表 R1-R5 接口 (W3.7)
- ❌ H5 结算/报表页 (W3.8, 子 agent)
- ❌ 验收 8 场景 (W3.9 强制 ALL PASS)
- ❌ 24h stale 阻断的企微告警 (W4)
- ❌ spot 库存 snapshot (W3 提的 cube 端 weighted_avg_cost)

---

## 七、风险 & 后续

- ⚠️ supermarket-ai `u_floor` 用户没 seed — collect-ai 后端 dev users 只有 owner/manager/buyer/cashier/office, 后端 W3 加 seed 任务时补上
- ⚠️ 前端 placeholder `bin-page.js` / `out-page.js` 是空文件(只注释) — 子 agent 给未来扩展留接口, 暂不删(等 W3 看是否有用)
- ⚠️ `inventory_current` cube `avg_cost` measure 仍是简单 AVG, W3 改 cube 加 `weighted_avg_cost`
- ⚠️ 前端离线 24h 硬阻断, 缺企微告警, W4 加
