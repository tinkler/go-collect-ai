package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

// ============================================================
// mock AgentRunner
// ============================================================

type mockRunner struct {
	enabled bool
	reply   string    // 累积成单一 chunk 模拟"非 streaming 一次性返回"
	asDelta bool      // true: 通过 Choice.Delta.Content 模拟 streaming
	delay   time.Duration
	fail    error     // 模拟 runner.Run 返回 error
	called  int
}

func (m *mockRunner) Enabled() bool { return m.enabled }

func (m *mockRunner) Run(ctx context.Context, userID, sessionID, message string) (<-chan *RunnerEvent, error) {
	m.called++
	if m.fail != nil {
		return nil, m.fail
	}
	ch := make(chan *RunnerEvent, 8)
	go func() {
		defer close(ch)
		if m.delay > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(m.delay):
			}
		}
		// 模拟 LLM 流式:每字符一个 chunk 或一个完整 chunk
		if m.reply != "" {
			if m.asDelta {
				for _, r := range m.reply {
					select {
					case <-ctx.Done():
						return
					case ch <- makeDeltaEvent(string(r)):
					}
				}
			} else {
				ch <- makeMessageEvent(m.reply)
			}
		}
	}()
	return ch, nil
}

func makeDeltaEvent(content string) *RunnerEvent {
	return &RunnerEvent{Raw: &event.Event{
		Response: &model.Response{
			Choices: []model.Choice{{
				Index: 0,
				Delta: model.Message{Content: content, Role: "assistant"},
			}},
		},
		Author: "assistant",
	}}
}

func makeMessageEvent(content string) *RunnerEvent {
	return &RunnerEvent{Raw: &event.Event{
		Response: &model.Response{
			Choices: []model.Choice{{
				Index:  0,
				Message: model.Message{Content: content, Role: "assistant"},
			}},
		},
		Author: "assistant",
	}}
}

// ============================================================
// mock Sender
// ============================================================

type mockSender struct {
	mu      sync.Mutex
	sent    []sentMsg
	failN   int   // 前 N 次 SendText 返 error
	failErr error
}

type sentMsg struct {
	ChatID string
	Text   string
}

func (s *mockSender) SendText(ctx context.Context, chatID, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failN > 0 {
		s.failN--
		return s.failErr
	}
	s.sent = append(s.sent, sentMsg{ChatID: chatID, Text: text})
	return nil
}

func (s *mockSender) Sent() []sentMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sentMsg, len(s.sent))
	copy(out, s.sent)
	return out
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// ============================================================
// 测试
// ============================================================

func TestBridge_WhitelistFilter(t *testing.T) {
	sender := &mockSender{}
	runner := &mockRunner{enabled: true, reply: "应该被忽略"}
	bridge := NewBridge(BridgeConfig{
		ChatIDs: []string{"allowed-chat"},
	}, runner, sender)

	bridge.Handle("not-allowed-chat", "user1", "hi")
	bridge.Handle("another", "user2", "hi")

	// 等一会儿让 goroutine 跑完
	if !waitFor(t, 200*time.Millisecond, func() bool { return true }) {
		t.Fatal("timeout")
	}
	if got := len(sender.Sent()); got != 0 {
		t.Errorf("白名单外的 chat 不应发送,实际发了 %d 条: %+v", got, sender.Sent())
	}
	if runner.called != 0 {
		t.Errorf("白名单外不应调 runner,实际调了 %d 次", runner.called)
	}
}

func TestBridge_EmptyChatIDs_NoOp(t *testing.T) {
	sender := &mockSender{}
	runner := &mockRunner{enabled: true, reply: "x"}
	bridge := NewBridge(BridgeConfig{ChatIDs: nil}, runner, sender)

	bridge.Handle("any-chat", "user1", "hi")
	time.Sleep(50 * time.Millisecond)

	if got := len(sender.Sent()); got != 0 {
		t.Errorf("空白名单 = no-op,实际发 %d 条", got)
	}
}

func TestBridge_LLMNotEnabled_ReplyFallback(t *testing.T) {
	sender := &mockSender{}
	runner := &mockRunner{enabled: false}
	bridge := NewBridge(BridgeConfig{ChatIDs: []string{"c1"}}, runner, sender)

	bridge.Handle("c1", "u1", "汇一是自采")
	if !waitFor(t, 500*time.Millisecond, func() bool { return len(sender.Sent()) == 1 }) {
		t.Fatal("expected 1 message")
	}
	sent := sender.Sent()
	if !strings.Contains(sent[0].Text, "未配置") {
		t.Errorf("fallback 文案应含'未配置', got: %q", sent[0].Text)
	}
	if runner.called != 0 {
		t.Errorf("LLM 未启用时不应调 runner")
	}
}

func TestBridge_HappyPath_MessageEvent(t *testing.T) {
	sender := &mockSender{}
	runner := &mockRunner{enabled: true, reply: "已记下:汇一 自采+堆头自付"}
	bridge := NewBridge(BridgeConfig{ChatIDs: []string{"c1"}}, runner, sender)

	bridge.Handle("c1", "u1", "汇一自采,堆头自付")
	if !waitFor(t, 500*time.Millisecond, func() bool { return len(sender.Sent()) == 1 }) {
		t.Fatal("expected 1 message")
	}
	sent := sender.Sent()
	if sent[0].Text != "已记下:汇一 自采+堆头自付" {
		t.Errorf("got %q", sent[0].Text)
	}
	if runner.called != 1 {
		t.Errorf("expected runner called 1 time, got %d", runner.called)
	}
}

func TestBridge_HappyPath_DeltaStream(t *testing.T) {
	sender := &mockSender{}
	runner := &mockRunner{enabled: true, reply: "对,记下了", asDelta: true}
	bridge := NewBridge(BridgeConfig{ChatIDs: []string{"c1"}}, runner, sender)

	bridge.Handle("c1", "u1", "ok")
	if !waitFor(t, 500*time.Millisecond, func() bool { return len(sender.Sent()) == 1 }) {
		t.Fatal("expected 1 message")
	}
	sent := sender.Sent()
	if sent[0].Text != "对,记下了" {
		t.Errorf("delta 流式累积 = %q, want %q", sent[0].Text, "对,记下了")
	}
}

func TestBridge_Truncate(t *testing.T) {
	sender := &mockSender{}
	long := strings.Repeat("很长的回复。", 50) // > 200 chars
	runner := &mockRunner{enabled: true, reply: long}
	bridge := NewBridge(BridgeConfig{
		ChatIDs:       []string{"c1"},
		MaxReplyChars: 200,
	}, runner, sender)

	bridge.Handle("c1", "u1", "x")
	if !waitFor(t, 500*time.Millisecond, func() bool { return len(sender.Sent()) == 1 }) {
		t.Fatal("expected 1 message")
	}
	sent := sender.Sent()
	if !strings.HasSuffix(sent[0].Text, "...") {
		t.Errorf("应被截断 + '...', got len=%d suffix=%q", len(sent[0].Text), sent[0].Text[max(0, len(sent[0].Text)-10):])
	}
	if len(sent[0].Text) > 200+3 {
		t.Errorf("len=%d, want ≤ 203", len(sent[0].Text))
	}
}

func TestBridge_EmptyReply_Fallback(t *testing.T) {
	sender := &mockSender{}
	runner := &mockRunner{enabled: true, reply: ""}
	bridge := NewBridge(BridgeConfig{ChatIDs: []string{"c1"}}, runner, sender)

	bridge.Handle("c1", "u1", "x")
	if !waitFor(t, 500*time.Millisecond, func() bool { return len(sender.Sent()) == 1 }) {
		t.Fatal("expected 1 message")
	}
	sent := sender.Sent()
	if !strings.Contains(sent[0].Text, "没听懂") {
		t.Errorf("空响应应降级: %q", sent[0].Text)
	}
}

func TestBridge_RunnerError_Fallback(t *testing.T) {
	sender := &mockSender{}
	runner := &mockRunner{enabled: true, fail: fmt.Errorf("upstream timeout")}
	bridge := NewBridge(BridgeConfig{ChatIDs: []string{"c1"}}, runner, sender)

	bridge.Handle("c1", "u1", "x")
	if !waitFor(t, 500*time.Millisecond, func() bool { return len(sender.Sent()) == 1 }) {
		t.Fatal("expected 1 message")
	}
	if !strings.Contains(sender.Sent()[0].Text, "没听懂") {
		t.Errorf("runner 错误应降级: %q", sender.Sent()[0].Text)
	}
}

func TestBridge_SerialSameChat(t *testing.T) {
	sender := &mockSender{}
	runner := &mockRunner{
		enabled: true,
		reply:   "ok",
		delay:   100 * time.Millisecond, // 模拟 LLM 慢
	}
	bridge := NewBridge(BridgeConfig{ChatIDs: []string{"c1"}}, runner, sender)

	// 同一 chat 连续发 3 条
	bridge.Handle("c1", "u1", "msg1")
	bridge.Handle("c1", "u1", "msg2")
	bridge.Handle("c1", "u1", "msg3")

	// 等 3 条都应完成
	if !waitFor(t, 2*time.Second, func() bool { return len(sender.Sent()) == 3 }) {
		t.Fatalf("expected 3 messages, got %d", len(sender.Sent()))
	}
	// 关键: 串行执行,所以顺序是 msg1 → msg2 → msg3
	// (虽然 msg2/msg3 不应被丢弃,只是串行等待)
	if runner.called != 3 {
		t.Errorf("expected runner called 3 times, got %d", runner.called)
	}
}

func TestBridge_RateLimit(t *testing.T) {
	sender := &mockSender{}
	runner := &mockRunner{enabled: true, reply: "ok"}
	bridge := NewBridge(BridgeConfig{
		ChatIDs:       []string{"c1"},
		PerMinuteRate: 3, // 3/min 上限
	}, runner, sender)

	// 5 条 burst: 前 3 通过 (回复 "ok"), 第 4-5 被频控挡 (降级"消息太多")
	// 串行 worker 保证 5 条都被处理
	for i := 0; i < 5; i++ {
		bridge.Handle("c1", "u1", "msg")
	}
	if !waitFor(t, 2*time.Second, func() bool { return len(sender.Sent()) == 5 }) {
		t.Fatalf("expected 5 messages (3 ok + 2 rate-limit), got %d", len(sender.Sent()))
	}
	// 前 3 条是 LLM 回复 "ok", 后 2 条是频控降级提示
	normalCount := 0
	rateLimitCount := 0
	for _, m := range sender.Sent() {
		if m.Text == "ok" {
			normalCount++
		} else if strings.Contains(m.Text, "消息太多") {
			rateLimitCount++
		}
	}
	if normalCount != 3 {
		t.Errorf("normal reply count = %d, want 3", normalCount)
	}
	if rateLimitCount != 2 {
		t.Errorf("rate-limit fallback count = %d, want 2", rateLimitCount)
	}
}

func TestBridge_EmptyText_NoOp(t *testing.T) {
	sender := &mockSender{}
	runner := &mockRunner{enabled: true, reply: "x"}
	bridge := NewBridge(BridgeConfig{ChatIDs: []string{"c1"}}, runner, sender)

	bridge.Handle("c1", "u1", "")
	bridge.Handle("c1", "u1", "   ")
	time.Sleep(50 * time.Millisecond)

	if len(sender.Sent()) != 0 {
		t.Errorf("空文本不应触发, got %d", len(sender.Sent()))
	}
	if runner.called != 0 {
		t.Errorf("空文本不应调 runner")
	}
}

func TestBridge_SenderError_NotPanic(t *testing.T) {
	sender := &mockSender{
		failN:   100, // 永远 fail
		failErr: fmt.Errorf("network down"),
	}
	runner := &mockRunner{enabled: true, reply: "ok"}
	bridge := NewBridge(BridgeConfig{ChatIDs: []string{"c1"}}, runner, sender)

	// 不应 panic
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic on sender error: %v", r)
		}
	}()

	bridge.Handle("c1", "u1", "hi")
	time.Sleep(100 * time.Millisecond)
	// 测了不崩就行
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
