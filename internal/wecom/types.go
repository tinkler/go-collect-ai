// Package wecom 企业微信智能机器人长连接客户端 (通用)
//
// 2026-09-02: 从 internal/restock 抽出来的通用包
//   - 不绑定 floor/office 角色, 按 chat_id 发送
//   - 长连接 + 收消息 (OnMessage / OnAgentMessage) + 按 chat_id 发送 (SendCard / SendAppChat / SendText)
//   - Bindings 持久化 (YAML) 用于记录自动发现的 chat_id
//
// 协议: 客户端主动建立 WS → 发 aibot_subscribe 认证 → 收 aibot_event_callback
//       → 30s 一次 ping → 发消息走 aibot_send_msg 帧
//
// API 文档: https://developer.work.weixin.qq.com/document/path/101833
package wecom

import "time"

// =====================================================================
// 通用类型
// =====================================================================

// ChatBinding 持久化结构 (YAML)
type ChatBinding struct {
	ChatID     string    `yaml:"chat_id" json:"chat_id"`
	Note       string    `yaml:"note,omitempty" json:"note,omitempty"`
	FirstSeen  time.Time `yaml:"first_seen" json:"first_seen"`
	LastActive time.Time `yaml:"last_active,omitempty" json:"last_active,omitempty"`
}

// ChatFrame 收消息 envelope
type ChatFrame struct {
	Cmd     string         `json:"cmd"`
	Headers map[string]any `json:"headers"`
	Body    struct {
		MsgID    string `json:"msgid"`
		CreateAt int64  `json:"create_time"`
		AIBotID  string `json:"aibotid"`
		ChatID   string `json:"chatid"`
		ChatType string `json:"chattype"`
		From     struct {
			CorpID string `json:"corpid"`
			UserID string `json:"userid"`
		} `json:"from"`
		ResponseURL string `json:"response_url"`
		MsgType     string `json:"msgtype"`
		Text        *struct {
			Content string `json:"content"`
		} `json:"text,omitempty"`
		Event *struct {
			EventType string `json:"eventtype"`
		} `json:"event,omitempty"`
	} `json:"body"`
}

// Config 客户端配置 (从 cfg / env 读)
type Config struct {
	BotID     string
	BotSecret string
	WSURL     string // 默认 wss://openws.work.weixin.qq.com
	BindFile  string // 默认 ./wecom_bindings.yaml
}
