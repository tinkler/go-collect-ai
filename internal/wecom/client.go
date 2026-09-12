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
	startedAt    time.Time // 进程启 Start 时间
	lastAttempt  time.Time // 上一次 connect 尝试时间
	lastError    string    // 上一次 connect error (e.g. "read frame hdr: EOF")
	attemptCount int       // 累计重试次数
	pid          int       // 进程 ID, 排查多实例冲突

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
	BotID         string `json:"bot_id"`
	BotIDPrefix   string `json:"bot_id_prefix"` // 前 8 位 + "..."
	WSURL         string `json:"wsurl"`
	BindFile      string `json:"bind_file"`
	SecretFP      string `json:"secret_fp"` // sha1 前 8 hex, 脱敏
	PID           int    `json:"pid"`
	StartedAt     string `json:"started_at"` // RFC3339
	UptimeSec     int64  `json:"uptime_sec"`
	Connected     bool   `json:"connected"`
	Attempts      int    `json:"attempts"`        // 累计重试次数
	LastAttemptAt string `json:"last_attempt_at"` // RFC3339
	LastError     string `json:"last_error"`
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
//
//	chatID: 企微会话 ID
//	body:   完整消息 JSON (msgtype + 对应字段)
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
		// 2026-09-12: 打印对端 IP + 证书 issuer。
		//   排查出口 MITM / 上网行为管理: 正常应为公认可信 CA (如 DigiCert/GlobalSign),
		//   若看到企业自建 CA 名字 = 中间盒在解密 WebSocket, 碎帧/拦帧常发源于此。
		issuer := "(unknown)"
		state := tlsConn.ConnectionState()
		if len(state.PeerCertificates) > 0 {
			issuer = state.PeerCertificates[0].Issuer.String()
		}
		log.Printf("[wecom] tls ok: peer=%s issuer=%s", rawConn.RemoteAddr(), issuer)
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
	log.Printf("[wecom] subscribe sent, bot_id=%s (waiting up to 10s for server ack)", w.cfg.BotID)

	// 2026-09-12: 鉴权必须以服务端 ack 为准 (对齐官方 wecom-aibot SDK)。
	//   官方流程: subscribe → 收 {"errcode":0,"errmsg":"ok"} → 之后才启动心跳。
	//   旧实现 ack 读失败也"降级继续", 在一条未认证连接上空发 ping 45s 才被踢,
	//   既掩盖根因 (出口中间盒没把数据帧送到企微后端 → 永远不会有 ack),
	//   又白白触发订阅频率保护。现在: 拿不到 errcode=0 立即放弃本次连接, 走重连。
	ack, ackErr := w.readSubscribeAck(10 * time.Second)
	if ackErr != nil {
		w.closeConn()
		return fmt.Errorf("subscribe ack: %w", ackErr)
	}
	if ack == "" {
		w.closeConn()
		return fmt.Errorf("subscribe ack timeout: server silent (data frame likely dropped by egress middlebox)")
	}

	var probe map[string]any
	if err := json.Unmarshal([]byte(ack), &probe); err != nil {
		w.closeConn()
		return fmt.Errorf("subscribe ack non-json: %s", truncateForLog(ack, 200))
	}
	ecRaw, hasErrcode := probe["errcode"]
	if !hasErrcode {
		w.closeConn()
		return fmt.Errorf("subscribe ack missing errcode: %s", truncateForLog(ack, 200))
	}
	ec, _ := ecRaw.(float64)
	if ec != 0 {
		w.closeConn()
		return fmt.Errorf("subscribe rejected: errcode=%v errmsg=%v", ecRaw, probe["errmsg"])
	}
	log.Printf("[wecom] authenticated: errcode=0 errmsg=%v (heartbeat 30s)", probe["errmsg"])

	if w.onConnect != nil {
		w.onConnect()
	}

	// 心跳: 官方建议/SDK 默认 30s, 认证成功后才启动。
	//   实测官方网关对 30s 心跳稳定 (2026-09-12 探针验证),
	//   ping 写失败 = 连接已断, 关掉让 readLoop/connectLoop 走重连。
	pingTicker := time.NewTicker(30 * time.Second)
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

	// 2026-09-12: readLoop 退出后必须完整关闭 TCP。
	//   服务端断开只发 FIN, 客户端若不 close(), socket 永远停在 CLOSE_WAIT:
	//   1) 每轮失败泄漏一个僵尸连接 (netstat 全是 CLOSE-WAIT)
	//   2) 服务端视角旧连接一直"半活着", 干扰单 bot 单连接判定,
	//      可能诱发新连接被踢 → ack 成功后秒 EOF 的死循环
	err = w.readLoop()
	w.closeConn()
	return err
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

// readSubscribeAck subscribe 后在 deadline 内读服务端鉴权响应 (text/binary 帧)。
//   - 命中: 返回 payload 字符串
//   - 超时 (deadline 内没数据): 返回 "", err (超时错误)
//   - EOF (服务端 close TCP): 返回 "", err
//
// 中间夹杂的 ping/pong 控制帧最多跳过 3 个。
func (w *Client) readSubscribeAck(deadline time.Duration) (string, error) {
	conn := w.conn
	if conn == nil {
		return "", fmt.Errorf("conn nil")
	}
	type deadlineSetter interface{ SetDeadline(time.Time) error }
	tc, canDeadline := conn.(deadlineSetter)
	if canDeadline {
		_ = tc.SetDeadline(time.Now().Add(deadline))
		defer tc.SetDeadline(time.Time{})
	}

	for skipped := 0; skipped < 3; skipped++ {
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

		if opcode == 0x8 {
			return "", fmt.Errorf("server sent close frame after subscribe: payload=%s", string(payload))
		}
		if !fin {
			// aibot 协议用单帧, 不处理 fragmented
			return "", fmt.Errorf("unexpected fragmented frame (opcode=%d)", opcode)
		}
		if opcode != 0x1 && opcode != 0x2 {
			// ping/pong 等控制帧 — 回 pong 后继续等数据帧
			if opcode == 0x9 {
				_ = w.pong(payload)
			}
			continue
		}
		return string(payload), nil
	}
	return "", fmt.Errorf("too many control frames while waiting subscribe ack")
}

// writeFrame 写一帧
//
// 2026-09-12: 必须单缓冲单次 Write。
//
//	旧实现把帧拆成 3 次 Write (2B 头 / 4B mask / payload), 走 TLS 时可能
//	拆成 3 个 record。企微官方网关能正常收 (RFC 6455 是字节流), 但客户出口
//	的 DPI / 上网行为管理按 record 解析 WebSocket 时会丢掉这种"碎帧", 表现为
//	握手 101 成功、subscribe 石沉大海、~45s 后服务端鉴权超时断连。
//	gorilla/websocket、官方 Python SDK 均为整帧一次写。
func (w *Client) writeFrame(opcode byte, payload []byte) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	conn := w.conn
	if conn == nil {
		return fmt.Errorf("not connected")
	}

	plen := len(payload)
	var hdrLen int
	switch {
	case plen < 126:
		hdrLen = 2
	case plen < 65536:
		hdrLen = 4
	default:
		hdrLen = 10
	}

	frame := make([]byte, hdrLen+4+plen)
	frame[0] = 0x80 | opcode
	switch {
	case plen < 126:
		frame[1] = 0x80 | byte(plen)
	case plen < 65536:
		frame[1] = 0x80 | 126
		binary.BigEndian.PutUint16(frame[2:], uint16(plen))
	default:
		frame[1] = 0x80 | 127
		binary.BigEndian.PutUint64(frame[2:], uint64(plen))
	}

	var maskKey [4]byte
	if _, err := rand.Read(maskKey[:]); err != nil {
		return err
	}
	copy(frame[hdrLen:hdrLen+4], maskKey[:])

	masked := frame[hdrLen+4:]
	for i := range payload {
		masked[i] = payload[i] ^ maskKey[i%4]
	}

	_, err := conn.Write(frame)
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
		// 2026-09-12 实测: 被新连接替换时, 服务端只推 disconnected_event 一帧,
		// 之后 TCP 静默不 FIN (readLoop 不处理会永久卡死, 永远不会重连)。
		// 此刻必须主动关连接 → readLoop 报错退出 → connectLoop 立即重连。
		// 注意: 出现这条日志 = 有另一个持相同 bot_id 的实例在抢连接
		// (旧进程没停 / 另一台机器 / 容器 / 别人的 go run), 必须清理重复实例,
		// 否则双方会 60s 一轮互相踢。
		log.Printf("[wecom] disconnected_event: 本连接已被同 bot_id 的另一个实例踢下线, 立即重连")
		w.closeConn()
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
