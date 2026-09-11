// Package wecomchat 企微群绑定配置 + 用途分发 (2026-09-11)
//
// 背景:
//   - 之前 chat_id 全部走 env (PROMOTION_ALERT_CHAT_ID / OWNER_CHAT_ID / COLLECTAI_AGENT_CHAT_IDS)
//   - 改一次要重启服务,且无 UI,商超老板用不动
//   - 现在改 PG wecom_chat_binding 表,UI 在 admin/system.html 配,热更新
//
// 设计目标:
//   - 单一收口: 收消息 + 推消息都查同一张表
//   - 启动期 env 作 seed: 有 env 没 PG → 启动时自动建 row;有 PG → PG 优先
//   - 热更新: 改完 PG binding → 调 /admin/wecom/reload → 内存 cache 立即生效
//
// 6 种 purpose (跟 promotion_fee / 智能采购 / 财务结算子系统对应):
//   - agent        智能对话群 (LLM 通用工具调用,包括 remember_supplier_policy / record_promotion_fee 等)
//   - fee          费用录入群 (同 agent,但系统 prompt 偏向费用; MVP 跟 agent 走同一条路径)
//   - promo_alert  堆头费到期预警群 (W3.3 cron 推送目标,出站单向)
//   - owner        店主私享群 (W4.3 supplier_pay cron 推送目标,出站单向)
//   - office       办公室通用群 (预留,目前跟 log 一样仅记录)
//   - floor        卖场通用群 (预留,目前跟 log 一样仅记录)
//   - log          仅记录不响应 (默认,新发现但未配置的群)
//   - other        其它自定义
package wecomchat

import "time"

// ChatBinding 单条群绑定 (wecom_chat_binding 表 1:1)
type ChatBinding struct {
	ChatID    string    `json:"chat_id"`
	Purpose   string    `json:"purpose"`               // agent | fee | promo_alert | owner | office | floor | log | other
	Label     string    `json:"label"`                  // 人类可读名 (e.g. "便利店-梁老板")
	Enabled   bool      `json:"enabled"`                // false = 停用 (不会入任何 handler)
	Note      string    `json:"note"`
	FirstSeen *time.Time `json:"first_seen,omitempty"`  // wecom client 首次发现该 chat 的时间
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AllowedPurposes 允许的 purpose 白名单
//
// 改这里要同步改 SQL CHECK 约束 (internal/store/pg.go)
var AllowedPurposes = map[string]bool{
	"agent":       true,
	"fee":         true,
	"promo_alert": true,
	"owner":       true,
	"office":      true,
	"floor":       true,
	"log":         true,
	"other":       true,
}

// PurposeDescription 给前端下拉框用
var PurposeDescription = []PurposeInfo{
	{Value: "log", Label: "仅记录", Desc: "收到消息仅打日志,不响应不出站(默认,新发现未配)"},
	{Value: "agent", Label: "智能对话", Desc: "LLM 通用对话,自动用 8 个业务工具(政策/费用/节假日)"},
	{Value: "fee", Label: "费用录入", Desc: "专收堆头/陈列/DM 等费用,走 LLM 走 record_promotion_fee 工具"},
	{Value: "promo_alert", Label: "堆头费预警", Desc: "W3.3 cron 推堆头/陈列到期费用的目标群(出站单向)"},
	{Value: "owner", Label: "店主私享", Desc: "W4.3 供应商结算 cron 推送给店长的群(出站单向)"},
	{Value: "office", Label: "办公室", Desc: "办公室通用群,目前仅记录(预留)"},
	{Value: "floor", Label: "卖场", Desc: "卖场通用群,目前仅记录(预留)"},
	{Value: "other", Label: "其它", Desc: "其它自定义用途,仅记录"},
}

// PurposeInfo 用途说明
type PurposeInfo struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Desc  string `json:"desc"`
}

// DiscoveredChat 自动发现但未配置的 chat (从 wecom client 内存 + YAML 合并)
type DiscoveredChat struct {
	ChatID    string    `json:"chat_id"`
	FirstSeen time.Time `json:"first_seen"`
	// 已配置则带 purpose,未配置则空
	Purpose string `json:"purpose,omitempty"`
	Label   string `json:"label,omitempty"`
	Enabled bool   `json:"enabled"`
	Bound   bool   `json:"bound"` // true = 已在 PG 配过
}

// StatusResponse /admin/wecom/status 响应
type StatusResponse struct {
	Connected         bool   `json:"connected"`           // wecom WS 长连接是否建立
	BotID             string `json:"bot_id,omitempty"`    // 已配置的 bot id
	DiscoveredCount   int    `json:"discovered_count"`    // 自动发现 chat 数
	BoundCount        int    `json:"bound_count"`         // PG 配过数
	BoundByPurpose    map[string]int `json:"bound_by_purpose"` // 各 purpose 计数
	LastReloadAt      time.Time `json:"last_reload_at"`
}
