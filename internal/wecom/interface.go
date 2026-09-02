package wecom

import "context"

// =====================================================================
// 公开接口 (供 restock / agent / trpc-agent-go 注入)
// =====================================================================

// Sender 发消息接口
//   SendAppChat: aibot_send_msg 帧, body 需含 msgtype + 对应字段
//   SendCard:    SendAppChat 的语义化封装
//   SendText:    纯文本消息的简化调用
type Sender interface {
	SendAppChat(ctx context.Context, chatID string, body []byte) error
	SendCard(ctx context.Context, chatID string, body []byte) error
	SendText(ctx context.Context, chatID, text string) error
}

// Receiver 收消息接口
//   OnMessage:      收到文本消息 (业务回调)
//   OnAgentMessage: 收到文本消息 (智能 Agent 桥接, 跟 OnMessage 并存, 按 chat_id 白名单过滤)
//   OnConnect:      长连接建立成功
type Receiver interface {
	OnMessage(fn func(chatID, userID, text string))
	OnAgentMessage(fn func(chatID, userID, text string))
	OnConnect(fn func())
}

// Lifecycle 生命周期接口
type Lifecycle interface {
	Start(ctx context.Context) error
	Stop()
	Connected() bool
}

// ChatLister 已发现 chat 列表接口 (供 admin API 用)
type ChatLister interface {
	DiscoveredChats() []ChatBinding
}
