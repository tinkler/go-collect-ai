// Package restock 精简后的企微长连接客户端 (2026-09-02)
//
// 保留: 长连接 + 收消息 + 按 chat_id 发送 + bindings 持久化
// 删掉: floor/office 角色绑定, SendUpdateCard (不推群), 旧 OnButtonClick
//
// 角色绑定被删是因为新版 restock 不推群, 未来通用化后由调用方自己维护 chat_id 列表
package restock

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// =====================================================================
// 企业微信智能机器人 长连接客户端(WebSocket 模式)
//
// API 文档: https://developer.work.weixin.qq.com/document/path/101833
// 协议: 客户端主动建立 WS → 发 aibot_subscribe 认证 → 收 aibot_event_callback
//       → 30s 一次 ping → 发消息走 aibot_send_msg 帧
//
// 凭证: BotID + Secret
//
// 关键约束:
//   - 一个机器人同时只能有 1 个有效长连接
//   - 群 chat_id 用户在群里发消息后企微推过来
//   - 主动发消息: 用户必须先在会话里发过消息(24 小时内)
//   - 频率限制: 30 条/分钟/会话, 1000 条/小时/会话
// =====================================================================

// WeCom 客户端
type WeCom struct {
	cfg *RestockConfig

	// chat_id 列表 (自动发现)
	discovered map[string]time.Time
	mu         sync.RWMutex

	// 长连接状态
	conn      io.ReadWriteCloser
	writeMu   sync.Mutex
	connected bool

	// 事件回调 (由 service / main 注册)
	OnMessage      func(chatID, userID, text string)
	OnAgentMessage func(chatID, userID, text string)
	OnConnect      func()

	// 内部
	stopCh   chan struct{}
	stopOnce sync.Once
	reqID    uint64
}

// ChatBinding 持久化结构 (YAML)
type ChatBinding struct {
	ChatID     string    `yaml:"chat_id" json:"chat_id"`
	Note       string    `yaml:"note,omitempty" json:"note,omitempty"`
	FirstSeen  time.Time `yaml:"first_seen" json:"first_seen"`
	LastActive time.Time `yaml:"last_active,omitempty" json:"last_active,omitempty"`
}

func NewWeCom(cfg *RestockConfig) *WeCom {
	w := &WeCom{
		cfg:        cfg,
		discovered: make(map[string]time.Time),
		stopCh:     make(chan struct{}),
	}
	w.loadBindings()
	return w
}

// Start 启长连接 (后台 goroutine)
func (w *WeCom) Start(ctx context.Context) error {
	if w.cfg.WeComBotID == "" || w.cfg.WeComBotSecret == "" {
		return fmt.Errorf("WECOM_BOT_ID / WECOM_BOT_SECRET 未配置")
	}
	go w.connectLoop(ctx)
	return nil
}

func (w *WeCom) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopCh)
		w.closeConn()
	})
}

// connectLoop 重连循环 (指数退避)
func (w *WeCom) connectLoop(ctx context.Context) {
	backoff := 2 * time.Second
	maxBackoff := 60 * time.Second
	for {
		select {
		case <-w.stopCh:
			return
		case <-ctx.Done():
			return
		default:
		}
		if err := w.connect(ctx); err != nil {
			log.Printf("[wecom] connect failed: %v (retry in %s)", err, backoff)
			select {
			case <-w.stopCh:
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		backoff = 2 * time.Second
	}
}

// connect 单次连接生命周期
func (w *WeCom) connect(ctx context.Context) error {
	u, err := url.Parse(w.cfg.WeComWSURL)
	if err != nil {
		return fmt.Errorf("parse ws url: %w", err)
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "wss" {
			host = u.Host + ":443"
		} else {
			host = u.Host + ":80"
		}
	}
	path := u.Path
	if path == "" {
		path = "/"
	}
	hostHeader := u.Host

	log.Printf("[wecom] dialing %s ...", host)
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	rawConn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return fmt.Errorf("tcp dial: %w", err)
	}

	var wsConn io.ReadWriteCloser = rawConn
	if u.Scheme == "wss" {
		tlsConn := tls.Client(rawConn, &tls.Config{
			ServerName: hostHeader,
			MinVersion: tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			rawConn.Close()
			return fmt.Errorf("tls handshake: %w", err)
		}
		wsConn = tlsConn
	}

	wsKey, err := w.wsHandshake(wsConn, hostHeader, path)
	if err != nil {
		wsConn.Close()
		return fmt.Errorf("ws handshake: %w", err)
	}
	_ = wsKey

	w.mu.Lock()
	w.conn = wsConn
	w.connected = true
	w.mu.Unlock()

	if err := w.subscribe(); err != nil {
		w.closeConn()
		return fmt.Errorf("subscribe: %w", err)
	}
	log.Printf("[wecom] subscribed, bot_id=%s", w.cfg.WeComBotID)

	if w.OnConnect != nil {
		w.OnConnect()
	}

	pingTicker := time.NewTicker(25 * time.Second)
	defer pingTicker.Stop()

	go func() {
		for {
			select {
			case <-w.stopCh:
				return
			case <-pingTicker.C:
				if err := w.ping(); err != nil {
					log.Printf("[wecom] ping failed: %v", err)
					w.closeConn()
					return
				}
			}
		}
	}()

	return w.readLoop()
}

// wsHandshake WebSocket 客户端握手
func (w *WeCom) wsHandshake(conn io.ReadWriteCloser, host, path string) (string, error) {
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return "", err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)

	req := fmt.Sprintf("GET %s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\n"+
		"Sec-WebSocket-Version: 13\r\n"+
		"User-Agent: collect-ai-wecom/1.0\r\n"+
		"\r\n", path, host, key)

	if tc, ok := conn.(interface{ SetDeadline(time.Time) error }); ok {
		_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
	}
	if _, err := conn.Write([]byte(req)); err != nil {
		return "", err
	}

	buf := make([]byte, 4096)
	n, err := readUntil(conn, buf, "\r\n\r\n")
	if err != nil {
		return "", err
	}
	resp := string(buf[:n])
	if !strings.Contains(resp, " 101 ") && !strings.Contains(resp, " 101\r") {
		return "", fmt.Errorf("ws upgrade failed: %s", strings.SplitN(resp, "\r\n", 2)[0])
	}

	expected := computeAcceptKey(key)
	if !strings.Contains(resp, expected) {
		return "", fmt.Errorf("ws accept key mismatch: want %s in %s", expected, Truncate(resp, 200))
	}
	if tc, ok := conn.(interface{ SetDeadline(time.Time) error }); ok {
		_ = tc.SetDeadline(time.Time{})
	}
	return key, nil
}

func computeAcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func readUntil(r io.Reader, buf []byte, delim string) (int, error) {
	total := 0
	delimBs := []byte(delim)
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		if n > 0 {
			total += n
			if total >= len(delimBs) && string(buf[total-len(delimBs):total]) == delim {
				return total, nil
			}
		}
		if err != nil {
			if err == io.EOF {
				return total, nil
			}
			return total, err
		}
	}
	return total, fmt.Errorf("buffer full before delim")
}

// readLoop 读消息主循环
func (w *WeCom) readLoop() error {
	header := make([]byte, 14)
	for {
		if _, err := io.ReadFull(w.conn, header[:2]); err != nil {
			return fmt.Errorf("read frame hdr: %w", err)
		}
		opcode := header[0] & 0x0F
		masked := (header[1] & 0x80) != 0
		payloadLen := int(header[1] & 0x7F)

		if payloadLen == 126 {
			var ext [2]byte
			if _, err := io.ReadFull(w.conn, ext[:]); err != nil {
				return err
			}
			payloadLen = int(binary.BigEndian.Uint16(ext[:]))
		} else if payloadLen == 127 {
			var ext [8]byte
			if _, err := io.ReadFull(w.conn, ext[:]); err != nil {
				return err
			}
			payloadLen = int(binary.BigEndian.Uint64(ext[:]))
		}

		var maskKey [4]byte
		if masked {
			if _, err := io.ReadFull(w.conn, maskKey[:]); err != nil {
				return err
			}
		}

		payload := make([]byte, payloadLen)
		if payloadLen > 0 {
			if _, err := io.ReadFull(w.conn, payload); err != nil {
				return err
			}
			if masked {
				for i := range payload {
					payload[i] ^= maskKey[i%4]
				}
			}
		}

		switch opcode {
		case 0x1: // text
			w.handleMessage(payload)
		case 0x8: // close
			return fmt.Errorf("server closed")
		case 0x9: // ping
			_ = w.pong(payload)
		case 0xA: // pong
			// noop
		}
	}
}

// writeFrame 写一帧
func (w *WeCom) writeFrame(opcode byte, payload []byte) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	conn := w.conn
	if conn == nil {
		return fmt.Errorf("not connected")
	}

	var header [14]byte
	header[0] = 0x80 | opcode

	plen := len(payload)
	if plen < 126 {
		header[1] = 0x80 | byte(plen)
		if _, err := conn.Write(header[:2]); err != nil {
			return err
		}
	} else if plen < 65536 {
		header[1] = 0x80 | 126
		binary.BigEndian.PutUint16(header[2:], uint16(plen))
		if _, err := conn.Write(header[:4]); err != nil {
			return err
		}
	} else {
		header[1] = 0x80 | 127
		binary.BigEndian.PutUint64(header[2:], uint64(plen))
		if _, err := conn.Write(header[:10]); err != nil {
			return err
		}
	}

	maskKey := [4]byte{}
	if _, err := rand.Read(maskKey[:]); err != nil {
		return err
	}
	if _, err := conn.Write(maskKey[:]); err != nil {
		return err
	}

	masked := make([]byte, plen)
	for i := range payload {
		masked[i] = payload[i] ^ maskKey[i%4]
	}
	_, err := conn.Write(masked)
	return err
}

func (w *WeCom) nextReqID() string {
	w.reqID++
	return "restock-" + strconv.FormatUint(w.reqID, 10) + "-" + strconv.FormatInt(time.Now().UnixNano()%1_000_000_000, 10)
}

func (w *WeCom) sendJSON(v any) error {
	bs, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.writeFrame(0x1, bs)
}

func (w *WeCom) subscribe() error {
	return w.sendJSON(map[string]any{
		"cmd":     "aibot_subscribe",
		"headers": map[string]any{"req_id": w.nextReqID()},
		"body": map[string]any{
			"bot_id": w.cfg.WeComBotID,
			"secret": w.cfg.WeComBotSecret,
		},
	})
}

func (w *WeCom) ping() error {
	return w.sendJSON(map[string]any{
		"cmd":     "ping",
		"headers": map[string]any{"req_id": w.nextReqID()},
	})
}

func (w *WeCom) pong(payload []byte) error {
	return w.writeFrame(0xA, payload)
}

func (w *WeCom) closeConn() {
	w.mu.Lock()
	if w.conn != nil {
		w.conn.Close()
		w.conn = nil
	}
	w.connected = false
	w.mu.Unlock()
}

// =====================================================================
// 收消息
// =====================================================================

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

func (w *WeCom) handleMessage(payload []byte) {
	var f ChatFrame
	if err := json.Unmarshal(payload, &f); err != nil {
		log.Printf("[wecom] parse frame: %v", err)
		return
	}

	switch f.Cmd {
	case "aibot_event_callback":
		w.handleEvent(&f)
	case "aibot_msg_callback":
		w.handleTextMsg(&f)
	default:
		if f.Cmd == "" {
			return
		}
		log.Printf("[wecom] frame cmd=%s: %s", f.Cmd, Truncate(string(payload), 200))
	}
}

func (w *WeCom) handleEvent(f *ChatFrame) {
	if f.Body.ChatID != "" {
		w.recordChat(f.Body.ChatID)
	}
	if f.Body.Event == nil {
		return
	}
	switch f.Body.Event.EventType {
	case "enter_chat":
		log.Printf("[wecom] user entered chat: user=%s", f.Body.From.UserID)
	case "disconnected_event":
		log.Printf("[wecom] disconnected event (new connection took over)")
	}
}

func (w *WeCom) handleTextMsg(f *ChatFrame) {
	if f.Body.ChatID != "" {
		w.recordChat(f.Body.ChatID)
	}
	if f.Body.Text != nil {
		if w.OnMessage != nil {
			w.OnMessage(f.Body.ChatID, f.Body.From.UserID, f.Body.Text.Content)
		}
		if w.OnAgentMessage != nil {
			w.OnAgentMessage(f.Body.ChatID, f.Body.From.UserID, f.Body.Text.Content)
		}
	}
}

// recordChat 记录 chat_id
func (w *WeCom) recordChat(chatID string) {
	w.mu.Lock()
	if _, exists := w.discovered[chatID]; !exists {
		w.discovered[chatID] = time.Now()
		w.mu.Unlock()
		_ = w.saveBindings()
		log.Printf("[wecom] NEW chat discovered: %s", chatID)
		return
	}
	w.mu.Unlock()
}

// =====================================================================
// 发消息
// =====================================================================

// SendCard 发卡片消息到指定 chat_id
//   chatID: 企微会话 ID
//   body: 完整消息 JSON (msgtype + 对应字段, e.g. {"msgtype":"text","text":{"content":"hi"}})
func (w *WeCom) SendCard(ctx context.Context, chatID string, body []byte) error {
	if chatID == "" {
		return fmt.Errorf("chat_id required")
	}

	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		return fmt.Errorf("body not JSON: %w", err)
	}
	msgType, _ := msg["msgtype"].(string)
	if msgType == "" {
		return fmt.Errorf("body.msgtype empty")
	}
	payload, ok := msg[msgType].(map[string]any)
	if !ok {
		return fmt.Errorf("body.%s not object", msgType)
	}

	frame := map[string]any{
		"cmd":     "aibot_send_msg",
		"headers": map[string]any{"req_id": w.nextReqID()},
		"body": map[string]any{
			"chatid":    chatID,
			"chat_type": 2, // 群聊
			"msgtype":   msgType,
			msgType:     payload,
		},
	}
	return w.sendJSON(frame)
}

// SendAppChat 兼容 agent.WecomSender interface
//   2026-09-02: 跟 SendCard 签名一致, 等价方法
func (w *WeCom) SendAppChat(ctx context.Context, chatID string, body []byte) error {
	return w.SendCard(ctx, chatID, body)
}

// SendText 简化的发文本
func (w *WeCom) SendText(ctx context.Context, chatID, text string) error {
	body := map[string]any{
		"msgtype": "text",
		"text":    map[string]any{"content": text},
	}
	bs, _ := json.Marshal(body)
	return w.SendCard(ctx, chatID, bs)
}

// =====================================================================
// Bindings 持久化
// =====================================================================

func (w *WeCom) bindingsFile() string {
	p := w.cfg.WeComBindFile
	if p == "" {
		return "./wecom_bindings.yaml"
	}
	return p
}

func (w *WeCom) loadBindings() {
	p := w.bindingsFile()
	bs, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var list []ChatBinding
	if err := yaml.Unmarshal(bs, &list); err != nil {
		log.Printf("[wecom] load bindings: %v", err)
		return
	}
	w.mu.Lock()
	for _, b := range list {
		if b.FirstSeen.IsZero() {
			w.discovered[b.ChatID] = time.Now()
		} else {
			w.discovered[b.ChatID] = b.FirstSeen
		}
	}
	w.mu.Unlock()
}

func (w *WeCom) saveBindings() error {
	w.mu.RLock()
	list := make([]ChatBinding, 0, len(w.discovered))
	for cid, seen := range w.discovered {
		list = append(list, ChatBinding{
			ChatID:    cid,
			FirstSeen: seen,
		})
	}
	w.mu.RUnlock()

	header := []byte("# wecom_bindings.yaml - 自动生成,手编后会被覆盖\n")
	bs, err := yaml.Marshal(list)
	if err != nil {
		return err
	}
	return os.WriteFile(w.bindingsFile(), append(header, bs...), 0o644)
}

// =====================================================================
// 公共访问
// =====================================================================

// Connected 是否已连接
func (w *WeCom) Connected() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.connected
}

// DiscoveredChats 列出已发现的 chat_id (用于 admin API)
func (w *WeCom) DiscoveredChats() []ChatBinding {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make([]ChatBinding, 0, len(w.discovered))
	for cid, seen := range w.discovered {
		out = append(out, ChatBinding{ChatID: cid, FirstSeen: seen})
	}
	return out
}

// Truncate 截断字符串 (供日志用)
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
