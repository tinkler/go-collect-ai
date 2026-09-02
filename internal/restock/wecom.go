// Deprecated: 2026-09-02 restock 重构后抽到 internal/wecom/
//
//   旧的 restock.WeCom 类型 / NewWeCom / SendCard / SendAppChat / SendText /
//   OnMessage / OnAgentMessage / Start / Stop / Connected / DiscoveredChats
//   全部迁到 internal/wecom 包。
//
//   使用方式:
//     import "github.com/tinkler/collect-ai/internal/wecom"
//     client := wecom.New(wecom.Config{BotID, BotSecret, WSURL, BindFile})
//     client.OnMessage(...)
//     client.Start(ctx) / client.Stop()
//
//   restock 包不再依赖企微长连接, 业务模块 (restock / agent / trpc-agent-go)
//   各自注入 wecom.Client。
//
//   保留此空文件为 Go 包占位, 防止 import "internal/restock" 时缺失文件。
//   如要彻底清理, 请手动 rm internal/restock/wecom.go
package restock
