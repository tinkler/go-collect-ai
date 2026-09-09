# freshcheck 试运行 / 部署指南

> 配套: [freshcheck.md](freshcheck.md) (总入口) / [freshcheck-pr-summary.md](freshcheck-pr-summary.md) (W1 PR)
> 目标: W2 启动前,先做"试运行期 (1 个月)"验证 W1 基础设施稳定
> 部署位置: collect-ai 单机 (跟现有 server 一起跑)

---

## 1. 环境要求

| 组件 | 最低 | 推荐 | 备注 |
|---|---|---|---|
| PostgreSQL | 12+ | 14+ | 跟 collect-ai 现有 PG 同一实例 |
| cube-agent-server | W1.6+ (含 /admin/source-direct 端点) | 最新 main | 必须用 `feat/freshcheck-cube` 分支 |
| collect-ai | W1 PR (e85e889 → 1faf5ca) | feat/freshcheck 分支 |  |
| Go | 1.21+ | 1.22+ | 跟现有 |
| 网络 | cube-agent-server 端口可达 (默认 8088) | 同机 | 防火墙开 8088 |

## 2. 数据库准备

### 2.1 创建数据库 (如未建)
```bash
psql -U postgres -c "CREATE DATABASE collectai;"
```

### 2.2 检查现有 schema 不冲突
```bash
psql -U postgres -d collectai -c "SELECT tablename FROM pg_tables WHERE tablename LIKE 'freshcheck_%';"
# 期望: 0 行 (首次部署)
```

### 2.3 启 collect-ai 自动迁移
```bash
# 启动时 Migrate() 自动建 13 张表 + seed 12+5 行
go run ./cmd/server
# 期望 log: "freshcheck: 13 张表就绪"
```

### 2.4 验证表 + seed
```bash
psql -U postgres -d collectai <<EOF
-- 13 张表
SELECT count(*) FROM pg_tables WHERE tablename LIKE 'freshcheck_%';
-- 期望: 13

-- 阈值 seed
SELECT count(*) FROM freshcheck_threshold;
-- 期望: 12

-- 品类轨道 seed
SELECT count(*) FROM freshcheck_category_track;
-- 期望: 5

-- 5 perm seed
SELECT count(*) FROM permissions WHERE id LIKE 'freshcheck:%';
-- 期望: 5

-- 9 role 关联
SELECT count(*) FROM role_permissions WHERE perm_id LIKE 'freshcheck:%';
-- 期望: 9
EOF
```

## 3. cube-agent-server 部署

### 3.1 切到 W1 分支
```bash
cd F:\go\src\github.com\tinkler\cube-agent-server
git checkout feat/freshcheck-cube
go build -o bin/cube-agent-server.exe ./cmd/agent
```

### 3.2 配置 datasource (config/datasources.yaml)
```yaml
datasources:
  - name: hbpos
    type: mssql
    driver: mssql
    dsn: "sqlserver://ai:ai6725.@127.0.0.1:1433?database=hbposv10&encrypt=disable&trustservercertificate=true"
    pool:
      max_open: 5
      max_idle: 2
      max_lifetime_sec: 600
```

### 3.3 启 cube-agent-server
```bash
./bin/cube-agent-server.exe
# 期望 log: "http server listening" + 2 个 freshcheck plugin 加载 (sales_with_refund / items_with_clsno)
```

### 3.4 验证 2 个 freshcheck plugin
```bash
curl http://127.0.0.1:8088/admin/plugins | jq '.plugins[] | select(.name | startswith("sales_with") or startswith("items_"))'
# 期望: 2 个 plugin
```

### 3.5 验证 /admin/source-direct
```bash
curl -X POST http://127.0.0.1:8088/admin/source-direct \
  -H "Content-Type: application/json" \
  -d '{"table":"t_rm_saleflow","op":"sum","column":"sale_money","since":"2026-09-08 00:00:00","until":"2026-09-08 23:59:59"}'
# 期望: {"datasource":"hbpos", "value":<some number>, "rows_counted":<N>, "duration_ms":<M>}
```

## 4. collect-ai 部署

### 4.1 切到 W1 分支
```bash
cd F:\go\src\github.com\tinkler\collect-ai
git checkout feat/freshcheck
go build -o bin/server.exe ./cmd/server
```

### 4.2 配置 .env
```bash
# 现有 + 新增
PG_HOST=127.0.0.1
PG_PORT=5432
PG_USER=postgres
PG_PASSWORD=postgres
PG_DATABASE=collectai

# 现有 (restock 也用, 沿用)
RESTOCK_BRANCH_NO=0001
```

### 4.3 启 collect-ai
```bash
./bin/server.exe
# 期望 log: "freshcheck: 13 张表就绪"
# 2026-09-09 W1.7: 不再有 FRESHCHECK_C7_CUBE_URL 和 C7 cron
```

### 4.4 端到端 smoke (curl)
```bash
# 1) 健康检查
curl http://127.0.0.1:8089/api/v1/freshcheck/health
# 期望: {"ok":true, "tables": [...13 项...], "count":13, "service":"freshcheck"}

# 2) dev-login 拿 token
$resp = Invoke-RestMethod -Method POST 'http://127.0.0.1:8089/api/v1/auth/dev-login?as_user=u_owner'
$token = $resp.access_token
# 或 (cookie 方式)
# curl -X POST -c cookies.txt 'http://127.0.0.1:8089/api/v1/auth/dev-login?as_user=u_owner'

# 3) 阈值列表
Invoke-RestMethod -Headers @{Authorization="Bearer $token"} 'http://127.0.0.1:8089/api/v1/freshcheck/config/thresholds' | Select-Object -ExpandProperty count
# 期望: 11

# 4) 品类轨道列表
Invoke-RestMethod -Headers @{Authorization="Bearer $token"} 'http://127.0.0.1:8089/api/v1/freshcheck/config/category-tracks' | Select-Object -ExpandProperty count
# 期望: 5

# 6) 校验 RBAC (u_floor 无 freshcheck:settle:read)
Invoke-WebRequest -Headers @{Authorization="Bearer (u_floor 的 token)"} 'http://127.0.0.1:8089/api/v1/freshcheck/config/thresholds'
# 期望: 403 Forbidden
```

完整脚本: `scripts/freshcheck_smoke.sh` (PowerShell / Bash 双版本)。

## 5. 监控 & 告警

### 5.1 启动检查
- 启动时 `Store.VerifyTablesExist()` 检查 13 张表
- 缺失 → log warning "freshcheck 缺失表: [...]", **不阻断**启动
- W1.1c seed 数据由 Migrate 一次性灌入 (ON CONFLICT DO NOTHING)

## 6. 备份 & 恢复

### 6.1 数据备份
```bash
# 13 张表全量
pg_dump -U postgres -d collectai -t 'freshcheck_*' > freshcheck_backup_$(date +%Y%m%d).sql
```

### 6.2 数据恢复
```bash
psql -U postgres -d collectai < freshcheck_backup_YYYYMMDD.sql
```

### 6.3 配置变更审计
所有配置表变更自动写 `freshcheck_config_snap`:
```sql
SELECT * FROM freshcheck_config_snap
WHERE table_name = 'freshcheck_threshold'
ORDER BY changed_at DESC LIMIT 10;
```

## 7. 升级路径 (W1 → W2)

### 7.1 升级前 checklist
- [ ] W1 PR review 通过
- [ ] collect-ai 跟 cube-agent-server 都在 feat/freshcheck-cube 分支
- [ ] PG + cube 在线, smoke 脚本全过
- [ ] 备份数据库 (`pg_dump freshcheck_*`)

### 7.2 升级步骤
1. 拉新代码: `git pull` (或 cherry-pick W2 commit)
2. 重新 build
3. 重启 collect-ai (Migrate 自动跑, 不破坏现有数据)
4. 验证 W1 端点还能用
5. 启用 W2 新端点 (入框/出框)

### 7.3 回滚 (万一 W2 引入 bug)
1. 切回 W1 commit: `git checkout feat/freshcheck-cube^`
2. 重新 build + 重启
3. W2 端点 404 (因为路由没注册) — 不影响 W1

## 8. 故障排查

| 现象 | 可能原因 | 解决 |
|---|---|---|
| 启动 log "freshcheck 缺失表" | Migrate 没跑 / PG 权限不够 | `psql -U postgres -d collectai` 手动 `\dt freshcheck_*` 看 |
| 11 行阈值 seed 缺失 | 老库没 ON CONFLICT | 手动跑: `INSERT INTO freshcheck_threshold ...` (见 [data-model.md §4](freshcheck-data-model.md#4-freshcheck_threshold--配置-业务阈值-k-v)) |
| 5 perm seed 缺失 | 同上 | 手动跑 role_permissions seed |
| 端点 403 | role_perm 没 seed 或 perm 不对 | `psql -c "SELECT * FROM role_permissions WHERE perm_id LIKE 'freshcheck:%';"` |
| `unknown entity "items_with_clsno"` | mappings.yaml 没读到 | 查 `cmd/server/main.go` 是否调 `NewRegistryFromYAML`, 或 mappings.yaml 路径错 |
| 集成测试全 SKIP | FRESHCHECK_TEST_PG_DSN 未设 | `export FRESHCHECK_TEST_PG_DSN=...` 后再跑 |

## 9. 试运行期 KPI (1 个月)

| KPI | 目标 | 监控方式 |
|---|---|---|
| 13 张表 0 异常 | 全 OK | `freshcheck_alert` 表查 |
| HTTP 端点 P99 响应 | < 200ms | 现有 Prometheus |
| 5 perm 角色绑定 | 9 行 | `SELECT COUNT(*) FROM role_permissions WHERE perm_id LIKE 'freshcheck:%'` |
| 11 阈值 seed 齐全 | 11 行 | `SELECT COUNT(*) FROM freshcheck_threshold` |
| 5 品类轨道 seed 齐全 | 5 行 | `SELECT COUNT(*) FROM freshcheck_category_track` |
| 集成测试通过率 | 100% (12 个) | `FRESHCHECK_TEST_PG_DSN=... go test` |

## 10. 文档清单

- [freshcheck.md](freshcheck.md) — 总入口
- [freshcheck-architecture.md](freshcheck-architecture.md) — 架构
- [freshcheck-data-model.md](freshcheck-data-model.md) — 13 张表
- [freshcheck-settlement.md](freshcheck-settlement.md) — 6 步结算 (W3 实施)
- [freshcheck-cube-plugins.md](freshcheck-cube-plugins.md) — cube 依赖
- [freshcheck-h5-contract.md](freshcheck-h5-contract.md) — H5 API (W2 实施)
- [freshcheck-rollout.md](freshcheck-rollout.md) — W1-W4 计划
- [freshcheck-pr-summary.md](freshcheck-pr-summary.md) — W1 PR
- [freshcheck-deploy.md](freshcheck-deploy.md) — 本文件
- 需求: `docs/生鲜免日盘管理扩展子系统设计需求文档.md`

## 11. 联系 & 反馈

- W1 已知限制:
  - [ ] `inventory_current` cube 的 `avg_cost` measure 是简单 AVG, 不等于加权平均. W3 改 cube 加 `weighted_avg_cost`
  - [ ] 启动期 schema 校验 (替代 C7 cron) 待 W3 加到 cube-agent-server 启动序列
