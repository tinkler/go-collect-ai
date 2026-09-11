package wecom

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

// Client 通用企微长连接客户端
type Client struct {
	cfg Config

	// chat_id 列表 (自动发现)
	discovered map[string]time.Time
	mu         sync.RWMutex

	// 长连接状态
	conn      io.ReadWriteCloser
	writeMu   sync.Mutex
	connected bool

	// 2026-09-11: 诊断字段 (admin status 接口用)
	//   - 用户排查 "已发现 = 0" 时一眼看出是 bot_id 错 / 进程冲突 / secret 错
	startedAt    time.Time     // 进程启 Start 时间
	lastAttempt  time.Time     // 上一次 connect 尝试时间
	lastError    string        // 上一次 connect error (e.g. "read frame hdr: EOF")
	attemptCount int           // 累计重试次数
	pid          int           // 进程 ID, 排查多实例冲突

	// 事件回调 (由 service / main 注册)
	onMessage      func(chatID, userID, text string)
	onAgentMessage func(chatID, userID, text string)
	onConnect      func()

	// 内部
	stopCh   chan struct{}
	stopOnce sync.Once
	reqID    uint64
}

// 编译期断言: *Client 实现全部公开接口
var (
	_ Sender     = (*Client)(nil)
	_ Receiver   = (*Client)(nil)
	_ Lifecycle  = (*Client)(nil)
	_ ChatLister = (*Client)(nil)
)

// New 构造
func New(cfg Config) *Client {
	if cfg.WSURL == "" {
		cfg.WSURL = "wss://openws.work.weixin.qq.com"
	}
	if cfg.BindFile == "" {
		cfg.BindFile = "./wecom_bindings.yaml"
	}
	w := &Client{
		cfg:        cfg,
		discovered: make(map[string]time.Time),
		stopCh:     make(chan struct{}),
		startedAt:  time.Now(),
		pid:        os.Getpid(),
	}
	w.loadBindings()
	return w
}

// Start 启长连接 (后台 goroutine)
func (w *Client) Start(ctx context.Context) error {
	if w.cfg.BotID == "" || w.cfg.BotSecret == "" {
		return fmt.Errorf("WECOM_BOT_ID / WECOM_BOT_SECRET 未配置")
	}
	go w.connectLoop(ctx)
	return nil
}

// Stop 停止
func (w *Client) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopCh)
		w.closeConn()
	})
}

// Connected 是否已连接
func (w *Client) Connected() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.connected
}

// BotID 暴露配置的 bot_id (供 admin status 接口用)
func (w *Client) BotID() string {
	return w.cfg.BotID
}

// Diagnose 2026-09-11: 返回诊断信息 (admin status 接口用)
//
//	排查"已发现 = 0"时一眼能看出:
//	  - bot_id 前 8 位 (确认是这个 bot, 不是别的)
//	  - wsurl  (确认连的是对的环境, 不是沙箱/老端点)
//	  - secret fingerprint (sha1 前 8 hex, 确认 secret 内容对, 不回显原文)
//	  - pid + started_at + uptime (确认进程没多份, 启动时长合理)
//	  - attempts + last_attempt + last_error (确认有持续重试, 错误内容直观)
type Diagnose struct {
	BotID           string `json:"bot_id"`
	BotIDPrefix     string `json:"bot_id_prefix"`     // 前 8 位 + "..."
	WSURL           string `json:"wsurl"`
	BindFile        string `json:"bind_file"`
	SecretFP        string `json:"secret_fp"`         // sha1 前 8 hex, 脱敏
	PID             int    `json:"pid"`
	StartedAt       string `json:"started_at"`        // RFC3339
	UptimeSec       int64  `json:"uptime_sec"`
	Connected       bool   `json:"connected"`
	Attempts        int    `json:"attempts"`          // 累计重试次数
	LastAttemptAt   string `json:"last_attempt_at"`   // RFC3339
	LastError       string `json:"last_error"`
}

func (w *Client) Diagnose() Diagnose {
	w.mu.RLock()
	defer w.mu.RUnlock()
	now := time.Now()
	d := Diagnose{
		BotID:     w.cfg.BotID,
		WSURL:     w.cfg.WSURL,
		BindFile:  w.cfg.BindFile,
		Connected: w.connected,
		Attempts:  w.attemptCount,
		LastError: w.lastError,
		PID:       w.pid,
		StartedAt: w.startedAt.Format(time.RFC3339),
	}
	if len(w.cfg.BotID) > 8 {
		d.BotIDPrefix = w.cfg.BotID[:8] + "..."
	} else {
		d.BotIDPrefix = w.cfg.BotID
	}
	if !w.lastAttempt.IsZero() {
		d.LastAttemptAt = w.lastAttempt.Format(time.RFC3339)
	}
	if !w.startedAt.IsZero() {
		d.UptimeSec = int64(now.Sub(w.startedAt).Seconds())
	}
	if w.cfg.BotSecret != "" {
		sum := sha1.Sum([]byte(w.cfg.BotSecret))
		d.SecretFP = fmt.Sprintf("%x", sum[:4]) // 8 hex 字符
	}
	return d
}

// DiscoveredChats 列出已发现的 chat_id
func (w *Client) DiscoveredChats() []ChatBinding {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make([]ChatBinding, 0, len(w.discovered))
	for cid, seen := range w.discovered {
		out = append(out, ChatBinding{ChatID: cid, FirstSeen: seen})
	}
	return out
}

// =====================================================================
// 公开方法 (实现 Receiver)
// =====================================================================

// OnMessage 注册文本消息回调
func (w *Client) OnMessage(fn func(chatID, userID, text string)) { w.onMessage = fn }

// OnAgentMessage 注册 Agent 桥接回调
func (w *Client) OnAgentMessage(fn func(chatID, userID, text string)) { w.onAgentMessage = fn }

// OnConnect 注册连接成功回调
func (w *Client) OnConnect(fn func()) { w.onConnect = fn }

// =====================================================================
// 发消息 (实现 Sender)
// =====================================================================

// SendCard 发卡片消息到指定 chat_id
//   chatID: 企微会话 ID
//   body:   完整消息 JSON (msgtype + 对应字段)
func (w *Client) SendCard(ctx context.Context, chatID string, body []byte) error {
	return w.sendAibotMsg(chatID, body)
}

// SendAppChat 兼容 agent.WecomSender interface (跟 SendCard 等价)
func (w *Client) SendAppChat(ctx context.Context, chatID string, body []byte) error {
	return w.sendAibotMsg(chatID, body)
}

// SendText 简化的发文本
func (w *Client) SendText(ctx context.Context, chatID, text string) error {
	body := map[string]any{
		"msgtype": "text",
		"text":    map[string]any{"content": text},
	}
	bs, _ := json.Marshal(body)
	return w.sendAibotMsg(chatID, bs)
}

func (w *Client) sendAibotMsg(chatID string, body []byte) error {
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

// =====================================================================
// 长连接实现 (内部)
// =====================================================================

// connectLoop 重连循环 (指数退避)
func (w *Client) connectLoop(ctx context.Context) {
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
		// 2026-09-11: 记录 attempt 时间 (admin status 用)
		w.mu.Lock()
		w.lastAttempt = time.Now()
		w.attemptCount++
		w.mu.Unlock()

		if err := w.connect(ctx); err != nil {
			w.mu.Lock()
			w.lastError = err.Error()
			w.mu.Unlock()
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
		// 连上后清掉 error
		w.mu.Lock()
		w.lastError = ""
		w.mu.Unlock()
		backoff = 2 * time.Second
	}
}

// connect 单次连接生命周期
func (w *Client) connect(ctx context.Context) error {
	u, err := url.Parse(w.cfg.WSURL)
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
	log.Printf("[wecom] subscribed, bot_id=%s (waiting up to 3s for server ack)", w.cfg.BotID)

	// 2026-09-12: 主动读 subscribe 响应
	//   背景: 之前 subscribe 完直接进 readLoop, 如果服务端收完 subscribe 立刻关连接
	//   第一个 read 就 EOF, 看不到服务端给的真正原因
	//   现在: 3s 内读一帧, parse JSON 打印 errcode/errmsg, 然后再进 readLoop
	//   - 服务端给 error: 立刻 close 并 return, 不再进 readLoop
	//   - 服务端给 ok: 继续 (跟之前一样进 readLoop)
	//   - 3s 没响应 (服务端静默): 记 timeout, 仍然进 readLoop
	if ack, ackErr := w.readSubscribeAck(3 * time.Second); ackErr != nil {
		w.closeConn()
		return fmt.Errorf("subscribe ack read: %w", ackErr)
	} else if ack != "" {
		// 看一眼 errcode
		var probe map[string]any
		if json.Unmarshal([]byte(ack), &probe) == nil {
			if ec, ok := probe["errcode"]; ok {
				log.Printf("[wecom] subscribe response: errcode=%v errmsg=%v", ec, probe["errmsg"])
			} else if cmd, ok := probe["cmd"]; ok {
				log.Printf("[wecom] subscribe response: cmd=%v (ack payload=%d bytes)", cmd, len(ack))
			} else {
				log.Printf("[wecom] subscribe response: %s", truncateForLog(ack, 300))
			}
		} else {
			log.Printf("[wecom] subscribe response (non-json): %s", truncateForLog(ack, 300))
		}
	} else {
		log.Printf("[wecom] subscribe ack timeout (3s) — server silent, falling through to readLoop")
	}

	if w.onConnect != nil {
		w.onConnect()
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
func (w *Client) wsHandshake(conn io.ReadWriteCloser, host, path string) (string, error) {
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
func (w *Client) readLoop() error {
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

		// 2026-09-10: 防恶意/异常大帧导致 makeslice: len out of range
		// server-to-client 按 WS 协议不应 mask, payload 1MiB 上限够用
		const maxFrame = 1 << 20
		if payloadLen < 0 || payloadLen > maxFrame {
			return fmt.Errorf("ws frame too large: %d", payloadLen)
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

// readSubscribeAck 2026-09-12: subscribe 后主动读一帧 (带 deadline)
//   - 命中 (有响应): 返回 payload 字符串
//   - 超时 (3s 内没数据): 返回 "", nil
//   - EOF (服务端 close TCP): 返回 "", err  (这样 connect() 立即 return, 不再进 readLoop)
//   - 其它读错误: 返回 "", err
//
// 用途: 当服务端 subscribe 后立刻 close, 之前 readLoop 看到的是裸 EOF,
//   现在先 read 一帧能看到 server 真正给的 errcode/errmsg
func (w *Client) readSubscribeAck(deadline time.Duration) (string, error) {
	conn := w.conn
	if conn == nil {
		return "", fmt.Errorf("conn nil")
	}
	type deadlineSetter interface{ SetDeadline(time.Time) error }
	if tc, ok := conn.(deadlineSetter); ok {
		_ = tc.SetDeadline(time.Now().Add(deadline))
		// 读完记得清掉
		defer tc.SetDeadline(time.Time{})
	}

	// 1) 读 2 字节 frame header
	var hdr [2]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		if err == io.EOF {
			return "", fmt.Errorf("server closed connection (EOF on first read after subscribe)")
		}
		return "", err
	}
	fin := hdr[0]&0x80 != 0
	opcode := hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	plen := int(hdr[1] & 0x7F)

	// 2) 读 ext length
	if plen == 126 {
		var ext [2]byte
		if _, err := io.ReadFull(conn, ext[:]); err != nil {
			return "", err
		}
		plen = int(binary.BigEndian.Uint16(ext[:]))
	} else if plen == 127 {
		var ext [8]byte
		if _, err := io.ReadFull(conn, ext[:]); err != nil {
			return "", err
		}
		plen = int(binary.BigEndian.Uint64(ext[:]))
	}

	// 3) 防大帧
	if plen < 0 || plen > 1<<20 {
		return "", fmt.Errorf("subscribe ack frame too large: %d", plen)
	}

	// 4) mask key (server 不该 mask, 但稳妥处理)
	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(conn, maskKey[:]); err != nil {
			return "", err
		}
	}

	// 5) payload
	payload := make([]byte, plen)
	if plen > 0 {
		if _, err := io.ReadFull(conn, payload); err != nil {
			return "", err
		}
		if masked {
			for i := range payload {
				payload[i] ^= maskKey[i%4]
			}
		}
	}

	// 仅关心 text/binary 帧
	if opcode == 0x8 {
		return "", fmt.Errorf("server sent close frame after subscribe: payload=%s", string(payload))
	}
	if !fin {
		// 简单起见不处理 fragmented — aibot 协议用单帧
		return "", fmt.Errorf("unexpected fragmented frame (opcode=%d)", opcode)
	}
	if opcode != 0x1 && opcode != 0x2 {
		// ping/pong 等控制帧 — 忽略, 递归再读 (保险起见 1 次)
		return w.readSubscribeAck(deadline)
	}
	return string(payload), nil
}

// writeFrame 写一帧
func (w *Client) writeFrame(opcode byte, payload []byte) error {
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

func (w *Client) nextReqID() string {
	w.reqID++
	return "wecom-" + strconv.FormatUint(w.reqID, 10) + "-" + strconv.FormatInt(time.Now().UnixNano()%1_000_000_000, 10)
}

func (w *Client) sendJSON(v any) error {
	bs, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.writeFrame(0x1, bs)
}

func (w *Client) subscribe() error {
	return w.sendJSON(map[string]any{
		"cmd":     "aibot_subscribe",
		"headers": map[string]any{"req_id": w.nextReqID()},
		"body": map[string]any{
			"bot_id": w.cfg.BotID,
			"secret": w.cfg.BotSecret,
		},
	})
}

func (w *Client) ping() error {
	return w.sendJSON(map[string]any{
		"cmd":     "ping",
		"headers": map[string]any{"req_id": w.nextReqID()},
	})
}

func (w *Client) pong(payload []byte) error {
	return w.writeFrame(0xA, payload)
}

// truncateForLog 截断字符串 (日志用)
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func (w *Client) closeConn() {
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

func (w *Client) handleMessage(payload []byte) {
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

func (w *Client) handleEvent(f *ChatFrame) {
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

func (w *Client) handleTextMsg(f *ChatFrame) {
	if f.Body.ChatID != "" {
		w.recordChat(f.Body.ChatID)
	}
	if f.Body.Text != nil {
		if w.onMessage != nil {
			w.onMessage(f.Body.ChatID, f.Body.From.UserID, f.Body.Text.Content)
		}
		if w.onAgentMessage != nil {
			w.onAgentMessage(f.Body.ChatID, f.Body.From.UserID, f.Body.Text.Content)
		}
	}
}

// recordChat 记录 chat_id
func (w *Client) recordChat(chatID string) {
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
// Bindings 持久化
// =====================================================================

func (w *Client) bindingsFile() string {
	return w.cfg.BindFile
}

func (w *Client) loadBindings() {
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

func (w *Client) saveBindings() error {
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
// 工具
// =====================================================================

// Truncate 截断字符串 (供日志用)
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
