# check_perms

诊断工具（2026-09-02 restock 重构时临时用）。

- 删 `main.go` 改 README 是为了避免跟其他 `cmd/*/main.go` 冲突（Go 同包多 main 编译报错）
- 历史 main.go 包含：
  - 查 PG `permissions` / `roles` / `role_permissions` 全部数据
  - 调 cube-agent-server 试查 items cube schema
  - 模拟 `restock.userRoleOf` 的 SQL 跑（确认 column not exist bug）

如果以后要重新启用：
1. 恢复 `main.go`（从 git log `2026-09-02` 之前的版本里）
2. 跑 `go run ./cmd/check_perms`
3. 用完再改回 README 占位
