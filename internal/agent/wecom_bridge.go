package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/event"
)

// =====================================================================
// W2 — 企微对话 Agent 桥接
//
// 把 wcm.OnAgentMessage(chatID, userID, text) 钩子连到 agent.Runner:
//   1) chat_id 白名单过滤(只接管显式指定的群)
//   2) 同 chat 串行(防重入)
//   3) 30/min/chat 频控(企微协议上限)
//   4) 流式累积 LLM 回复,截断 ≤ 200 字
//   5) LLM 不可用时降级返友好提示
//   6) 错误兜底返"我没听懂,换个说法试试"
//
// 设计目标:用户店端验证时,即使 LLM 暂时不可用,系统也不崩,且 30/min
// 频控不撞。
// =====================================================================

// Sender 抽象发消息接口(由 wecom client 实现),便于单测 mock
type Sender interface {
	SendText(ctx context.Context, chatID, text string) error
}

// AgentRunner LLM Agent 抽象(*Runner 已实现)
//
//	让 Bridge 可被单测 mock,避免引入真实 LLM 依赖
type AgentRunner interface {
	Enabled() bool
	Run(ctx context.Context, userID, sessionID, message string) (<-chan *RunnerEvent, error)
}

// BridgeConfig 桥接配置
type BridgeConfig struct {
	// ChatIDs 显式启用 Agent 的 chat_id 白名单(逗号或空格分隔)
	//   空=不接管任何群(Bridge 是个 no-op)
	ChatIDs []string

	// MaxReplyChars 单条回复最大字符数(默认 200, 企微频控)
	MaxReplyChars int

	// PerMinuteRate 每分钟最大消息数(默认 25, 留 5 条余量给企微其他场景)
	PerMinuteRate int

	// RunTimeout 单次 LLM 调用超时(默认 60s)
	RunTimeout time.Duration
}

// DefaultBridgeConfig 合理默认
func DefaultBridgeConfig() BridgeConfig {
	return BridgeConfig{
		ChatIDs:       nil,
		MaxReplyChars: 200,
		PerMinuteRate: 25,
		RunTimeout:    60 * time.Second,
	}
}

// Bridge wcm <-> agent runner 桥
//
// 设计:per-chat 串行 worker + buffered channel
//   - 每个 chat 一个 worker goroutine 顺序消费 channel
//   - 消息入队不丢(同 chat 串行,跨 chat 并行)
//   - 频控 (PerMinuteRate) 在 worker 内部 allowSend 判定
//   - 频控时降级返"消息太多"提示,不放回 queue
type Bridge struct {
	cfg    BridgeConfig
	runner AgentRunner
	sender Sender

	mu      sync.Mutex
	chatSet map[string]struct{}     // 白名单 set
	workers map[string]chan msgItem // per-chat queue (lazy init)
	rate    map[string][]time.Time  // chat -> 最近 1 分钟 send 时间
}

// msgItem 排队消息
type msgItem struct {
	userID string
	text   string
}

// NewBridge 构造桥
func NewBridge(cfg BridgeConfig, runner AgentRunner, sender Sender) *Bridge {
	set := make(map[string]struct{}, len(cfg.ChatIDs))
	for _, c := range cfg.ChatIDs {
		c = strings.TrimSpace(c)
		if c != "" {
			set[c] = struct{}{}
		}
	}
	if cfg.MaxReplyChars <= 0 {
		cfg.MaxReplyChars = 200
	}
	if cfg.PerMinuteRate <= 0 {
		cfg.PerMinuteRate = 25
	}
	if cfg.RunTimeout <= 0 {
		cfg.RunTimeout = 60 * time.Second
	}
	return &Bridge{
		cfg:     cfg,
		runner:  runner,
		sender:  sender,
		chatSet: set,
		workers: make(map[string]chan msgItem),
		rate:    make(map[string][]time.Time),
	}
}

// Handle wcm.OnAgentMessage 钩子入口
//
//	立即返回;消息入 per-chat queue,worker 串行处理
func (b *Bridge) Handle(chatID, userID, text string) {
	chatID = strings.TrimSpace(chatID)
	text = strings.TrimSpace(text)
	if chatID == "" || text == "" {
		return
	}
	if !b.shouldHandle(chatID) {
		// 不在白名单: 之前是静默丢弃, 排查 "发了调试没回复" 时完全无迹可循
		// 现在打一行 skip 日志 (msg 量低, main.go OnMessage 已打原文, 这里只标原因)
		log.Printf("[bridge] skip chat=%s user=%s (not in COLLECTAI_AGENT_CHAT_IDS whitelist, len=%d)",
			chatID, userID, len(b.chatSet))
		return
	}
	queue := b.getOrCreateWorker(chatID)
	// 非阻塞入队(满了 drop + log,避免阻塞 wcm 主循环)
	select {
	case queue <- msgItem{userID: userID, text: text}:
	default:
		log.Printf("[bridge] chat=%s queue full, drop user=%s text=%q", chatID, userID, text)
	}
}

// getOrCreateWorker 懒启动 per-chat worker (idempotent, sync.Mutex 保护)
func (b *Bridge) getOrCreateWorker(chatID string) chan msgItem {
	b.mu.Lock()
	defer b.mu.Unlock()
	if q, ok := b.workers[chatID]; ok {
		return q
	}
	q := make(chan msgItem, 64)
	b.workers[chatID] = q
	go b.runWorker(chatID, q)
	return q
}

// runWorker 持续消费 queue,直到 channel close
func (b *Bridge) runWorker(chatID string, queue chan msgItem) {
	for item := range queue {
		b.processOne(context.Background(), chatID, item.userID, item.text)
	}
}

func (b *Bridge) shouldHandle(chatID string) bool {
	if len(b.chatSet) == 0 {
		return false
	}
	_, ok := b.chatSet[chatID]
	return ok
}

// allowSend 频控: 超 PerMinuteRate/chat/分钟 → false
//
//	顺便 record 一次发送(扣配额)
func (b *Bridge) allowSend(chatID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-time.Minute)
	old := b.rate[chatID]
	keep := old[:0]
	for _, t := range old {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	if len(keep) >= b.cfg.PerMinuteRate {
		b.rate[chatID] = keep
		return false
	}
	keep = append(keep, now)
	b.rate[chatID] = keep
	return true
}

func (b *Bridge) processOne(parent context.Context, chatID, userID, text string) {
	ctx, cancel := context.WithTimeout(parent, b.cfg.RunTimeout)
	defer cancel()

	// 调试快速通道: @机器人 + 文字 == "调试" → 返 chat_id 等诊断信息
	// 走频控(占 1 条配额防滥用),不走 LLM,不截断
	if isDebugCommand(text) {
		if !b.allowSend(chatID) {
			b.sendSafe(ctx, chatID, "本群消息太多,我先停一下,稍等几秒再试")
			return
		}
		b.sendDebugInfo(ctx, chatID, userID)
		return
	}

	// 频控
	if !b.allowSend(chatID) {
		b.sendSafe(ctx, chatID, "本群消息太多,我先停一下,稍等几秒再试")
		return
	}

	// LLM 不可用
	if b.runner == nil || !b.runner.Enabled() {
		log.Printf("[bridge] LLM 未配置 chat=%s user=%s text=%q", chatID, userID, text)
		b.sendSafe(ctx, chatID, "智能助理暂未配置(需 COLLECTAI_LLM_API_KEY),无法回复。工具已就绪,可在 H5 端调用。")
		return
	}

	// Runner.Run 流式
	events, err := b.runner.Run(ctx, userID, chatID, text)
	if err != nil {
		log.Printf("[bridge] runner.Run err: %v", err)
		b.sendSafe(ctx, chatID, "我没听懂,换个说法试试")
		return
	}

	// 累积 chunks
	var buf strings.Builder
	chunks := 0
	for ev := range events {
		if ev == nil || ev.Raw == nil {
			continue
		}
		chunk := extractTextDelta(ev.Raw)
		if chunk != "" {
			buf.WriteString(chunk)
			chunks++
		}
	}
	msg := strings.TrimSpace(buf.String())
	if msg == "" {
		log.Printf("[bridge] empty reply chat=%s chunks=%d", chatID, chunks)
		b.sendSafe(ctx, chatID, "我没听懂,换个说法试试")
		return
	}

	// 截断
	if len(msg) > b.cfg.MaxReplyChars {
		msg = msg[:b.cfg.MaxReplyChars] + "..."
	}
	b.sendSafe(ctx, chatID, msg)
}

// sendSafe send 但不阻塞 main flow 出错(只 log)
func (b *Bridge) sendSafe(ctx context.Context, chatID, text string) {
	if b.sender == nil {
		log.Printf("[bridge] sender nil, drop: chat=%s text=%q", chatID, text)
		return
	}
	if err := b.sender.SendText(ctx, chatID, text); err != nil {
		log.Printf("[bridge] SendText err chat=%s: %v", chatID, err)
	}
}

// isDebugCommand 判断是否是 "调试" 调试命令
//
// 严格匹配: 跳过所有 @xxx 形式的 at 标记后,只剩一个 token == "调试"
//
// 命中:
//   - "调试"
//   - "@机器人 调试"
//   - "@机器人  调试"  (中间多余空格)
//
// 不命中:
//   - "@机器人 调试 info"      (还有别的内容)
//   - "@机器人 调 试"            (两个字分开,严格匹配)
//   - "调试一下"                 (前缀匹配了,严格 "调试" 才算)
//
// 为什么不用 strings.Contains: 防误触发 — "帮我调试一下" 这种文本不该命中
func isDebugCommand(text string) bool {
	parts := strings.Fields(text)
	nonAt := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// 跳过 @xxx 形式的 at 标记 (整 token 以 @ 开头)
		if strings.HasPrefix(p, "@") {
			continue
		}
		nonAt = append(nonAt, p)
	}
	return len(nonAt) == 1 && nonAt[0] == "调试"
}

// sendDebugInfo 发送调试诊断信息(含 chat_id)
//
// 不调 LLM, 不截断, 占 1 条频控配额
// 信息故意简短(< 200 字),便于用户直接复制贴出来对照
func (b *Bridge) sendDebugInfo(ctx context.Context, chatID, userID string) {
	llmStatus := "disabled"
	if b.runner != nil && b.runner.Enabled() {
		llmStatus = "enabled"
	}
	// 用 \n 真实换行,企微 text 消息会原样显示
	msg := fmt.Sprintf(
		"🤖 调试 OK\nchat_id: %s\nuser_id: %s\nllm: %s\n白名单: %s\nts: %s",
		chatID,
		userID,
		llmStatus,
		func() string {
			if _, ok := b.chatSet[chatID]; ok {
				return "yes"
			}
			return fmt.Sprintf("no (%d 个群)", len(b.chatSet))
		}(),
		time.Now().Format("2006-01-02 15:04:05"),
	)
	log.Printf("[bridge] DEBUG chat=%s user=%s llm=%s", chatID, userID, llmStatus)
	b.sendSafe(ctx, chatID, msg)
}

// extractTextDelta 从 trpc-agent-go event 抽一个文本 chunk
//
//	streaming: 走 Choice.Delta.Content
//	非 streaming: 走 Choice.Message.Content
func extractTextDelta(ev *event.Event) string {
	if ev.Response == nil {
		return ""
	}
	for _, ch := range ev.Response.Choices {
		if ch.Delta.Content != "" {
			return ch.Delta.Content
		}
		if ch.Message.Content != "" {
			return ch.Message.Content
		}
	}
	return ""
}

// =====================================================================
// 默认 Sender 包装 — 构造 aibot_send_msg body 调 wcm.SendAppChat
// =====================================================================

// WecomSender 包装 restock WeCom 客户端(只需 SendAppChat)
type WecomSender interface {
	SendAppChat(ctx context.Context, chatID string, body []byte) error
}

// NewWecomSender 默认 sender(用 aibot_send_msg text 类型)
func NewWecomSender(w WecomSender) Sender {
	return &wecomSenderAdapter{w: w}
}

type wecomSenderAdapter struct {
	w WecomSender
}

func (a *wecomSenderAdapter) SendText(ctx context.Context, chatID, text string) error {
	// aibot_send_msg body 格式 (企微智能机器人 长连接协议)
	//   {
	//     "cmd": "aibot_send_msg",
	//     "headers": {"req_id": "..."},
	//     "body": {
	//       "chat_id": "...",
	//       "msgtype": "text",
	//       "text": {"content": "..."}
	//     }
	//   }
	body := map[string]any{
		"msgtype": "text",
		"text":    map[string]string{"content": text},
	}
	bs, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal aibot_send_msg body: %w", err)
	}
	// chatID 已在外层指定,这里只包 body(由 wcm 拼 req_id + cmd)
	return a.w.SendAppChat(ctx, chatID, bs)
}
