package restock

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
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

	"crypto/tls"

	"gopkg.in/yaml.v3"
)

// =====================================================================
// 企业微信智能机器人 长连接客户端(WebSocket 模式)
//
// API 文档: https://developer.work.weixin.qq.com/document/path/101833
// 协议: 客户端主动建立 WS → 发 aibot_subscribe 认证 → 收 aibot_event_callback
//       → 30s 一次 ping → 发消息走 aibot_send_msg 帧
//
// 凭证: BotID + Secret (就这 2 个,不需要 CorpID/AgentID/Token/AESKey)
//
// 关键约束:
//   - 一个机器人同时只能有 1 个有效长连接(新连接会踢旧连接)
//   - 群 chat_id 不能预先创建,用户在群里发消息后企微推过来
//   - 主动发消息: 用户必须先在会话里发过消息(24 小时内)
//   - 频率限制: 30 条/分钟/会话, 1000 条/小时/会话
// =====================================================================

// WeCom 客户端
type WeCom struct {
	cfg *RestockConfig

	// bindings: chat_id -> "floor" / "office" / ""
	bindings   map[string]string
	discovered map[string]time.Time // 首次发现时间
	mu         sync.RWMutex

	// 长连接状态
	conn      io.ReadWriteCloser
	writeMu   sync.Mutex
	connected bool

	// 事件回调(由 service.go 注册)
	OnButtonClick func(chatID, userID, taskID, eventKey string) // 按钮点击
	OnMessage     func(chatID, userID, text string)             // 文本消息(用于自动发现)
	OnConnect     func()                                        // 连接成功

	// 内部
	stopCh   chan struct{}
	stopOnce sync.Once
	reqID    uint64

	// for TLS ws://
	tlsConfig *tls.Config
}

// ChatBinding 持久化结构(YAML)
type ChatBinding struct {
	ChatID     string    `yaml:"chat_id" json:"chat_id"`
	Role       string    `yaml:"role" json:"role"` // "floor" / "office" / ""
	FirstSeen  time.Time `yaml:"first_seen" json:"first_seen"`
	LastActive time.Time `yaml:"last_active,omitempty" json:"last_active,omitempty"`
	Note       string    `yaml:"note,omitempty" json:"note,omitempty"`
}

func NewWeCom(cfg *RestockConfig) *WeCom {
	w := &WeCom{
		cfg:        cfg,
		bindings:   make(map[string]string),
		discovered: make(map[string]time.Time),
		stopCh:     make(chan struct{}),
	}
	w.loadBindings()
	return w
}

// Start 启长连接(后台 goroutine)
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

// connectLoop 重连循环(指数退避)
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

// connect 单次连接生命周期: 拨号 → WS 握手 → 订阅 → 收发循环 → 断开返回
func (w *WeCom) connect(ctx context.Context) error {
	// 解析 wss://host:port/path → 拿 host, port, path
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
	// Host 头不含端口(如果是 443/80)
	hostHeader := u.Host

	log.Printf("[wecom] dialing %s ...", host)
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	rawConn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return fmt.Errorf("tcp dial: %w", err)
	}

	// wss:// 需要 TLS 握手
	var wsConn io.ReadWriteCloser = rawConn
	if u.Scheme == "wss" {
		log.Printf("[wecom] tls handshake to %s ...", hostHeader)
		tlsConn := tls.Client(rawConn, &tls.Config{
			ServerName: hostHeader,
			MinVersion: tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			rawConn.Close()
			return fmt.Errorf("tls handshake: %w", err)
		}
		wsConn = tlsConn
		log.Printf("[wecom] tls handshake ok")
	}

	// WS 客户端握手
	wsKey, err := w.wsHandshake(wsConn, hostHeader, path)
	if err != nil {
		wsConn.Close()
		return fmt.Errorf("ws handshake: %w", err)
	}
	log.Printf("[wecom] ws handshake ok, key=%s", wsKey)

	w.mu.Lock()
	w.conn = wsConn
	w.connected = true
	w.mu.Unlock()

	// 订阅
	if err := w.subscribe(); err != nil {
		w.closeConn()
		return fmt.Errorf("subscribe: %w", err)
	}
	log.Printf("[wecom] subscribed, bot_id=%s", w.cfg.WeComBotID)

	if w.OnConnect != nil {
		w.OnConnect()
	}

	// 启心跳 + 收消息
	pingTicker := time.NewTicker(25 * time.Second) // 略小于 30s
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

	// 收消息主循环
	return w.readLoop()
}

// wsHandshake WebSocket 客户端握手(简化版,只支持 wss://host:port/)
//   返回 Sec-WebSocket-Accept 校验后的 key
func (w *WeCom) wsHandshake(conn io.ReadWriteCloser, host, path string) (string, error) {
	// 生成 Sec-WebSocket-Key
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

	// tls.Conn 支持 SetDeadline, 普通 net.Conn 也支持
	// io.ReadWriteCloser 没有 SetDeadline,我们尽量用 timeout 控制
	if tc, ok := conn.(interface{ SetDeadline(time.Time) error }); ok {
		_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
	}
	if _, err := conn.Write([]byte(req)); err != nil {
		return "", err
	}

	// 读响应(直到 \r\n\r\n)
	buf := make([]byte, 4096)
	n, err := readUntil(conn, buf, "\r\n\r\n")
	if err != nil {
		return "", err
	}
	resp := string(buf[:n])
	if !strings.Contains(resp, " 101 ") && !strings.Contains(resp, " 101\r") {
		return "", fmt.Errorf("ws upgrade failed: %s", strings.SplitN(resp, "\r\n", 2)[0])
	}

	// 校验 Sec-WebSocket-Accept
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
		// 读 frame header (最少 2 字节,最长 14 字节含 8 字节 masking key)
		if _, err := io.ReadFull(w.conn, header[:2]); err != nil {
			return fmt.Errorf("read frame hdr: %w", err)
		}
		fin := (header[0] & 0x80) != 0
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

		_ = fin
		switch opcode {
		case 0x1: // text
			w.handleMessage(payload)
		case 0x8: // close
			log.Printf("[wecom] received close frame")
			return fmt.Errorf("server closed")
		case 0x9: // ping
			_ = w.pong(payload)
		case 0xA: // pong
			// noop
		default:
			// binary / 其它忽略
		}
	}
}

// writeFrame 写一帧(客户端必须 mask)
func (w *WeCom) writeFrame(opcode byte, payload []byte) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	conn := w.conn
	if conn == nil {
		return fmt.Errorf("not connected")
	}

	var header [14]byte
	header[0] = 0x80 | opcode // FIN=1

	plen := len(payload)
	if plen < 126 {
		header[1] = 0x80 | byte(plen)
		_, err := conn.Write(header[:2])
		if err != nil {
			return err
		}
	} else if plen < 65536 {
		header[1] = 0x80 | 126
		binary.BigEndian.PutUint16(header[2:], uint16(plen))
		_, err := conn.Write(header[:4])
		if err != nil {
			return err
		}
	} else {
		header[1] = 0x80 | 127
		binary.BigEndian.PutUint64(header[2:], uint64(plen))
		_, err := conn.Write(header[:10])
		if err != nil {
			return err
		}
	}

	// 客户端必须 mask
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

// sendJSON 通用发 JSON 帧
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
// 业务消息收发
// =====================================================================

// ChatFrame 收消息 envelope
type ChatFrame struct {
	Cmd     string         `json:"cmd"`
	Headers map[string]any `json:"headers"`
	Body    struct {
		MsgID     string `json:"msgid"`
		CreateAt  int64  `json:"create_time"`
		AIBotID   string `json:"aibotid"`
		ChatID    string `json:"chatid"`
		ChatType  string `json:"chattype"` // single / group
		From      struct {
			CorpID string `json:"corpid"`
			UserID string `json:"userid"`
		} `json:"from"`
		ResponseURL string `json:"response_url"`
		MsgType     string `json:"msgtype"` // text / event
		Text        *struct {
			Content string `json:"content"`
		} `json:"text,omitempty"`
		Event *struct {
			EventType          string `json:"eventtype"`
			TemplateCardEvent  *struct {
				CardType string `json:"card_type"`
				EventKey string `json:"event_key"`
				TaskID   string `json:"task_id"`
			} `json:"template_card_event,omitempty"`
		} `json:"event,omitempty"`
	} `json:"body"`
}

func (w *WeCom) handleMessage(payload []byte) {
	var f ChatFrame
	if err := json.Unmarshal(payload, &f); err != nil {
		log.Printf("[wecom] parse frame: %v payload=%s", err, Truncate(string(payload), 300))
		return
	}

	switch f.Cmd {
	case "aibot_event_callback":
		w.handleEvent(&f)
	case "aibot_msg_callback":
		w.handleTextMsg(&f)
	default:
		// 订阅响应、ping 响应等: 打印
		if f.Cmd == "" {
			// 可能是订阅响应包,忽略
			return
		}
		log.Printf("[wecom] frame cmd=%s: %s", f.Cmd, Truncate(string(payload), 200))
	}
}

func (w *WeCom) handleEvent(f *ChatFrame) {
	// 首次发现 chat_id(只要有 chatid 就记)
	if f.Body.ChatID != "" {
		w.recordChat(f.Body.ChatID)
	}
	if f.Body.Event == nil {
		return
	}
	switch f.Body.Event.EventType {
	case "template_card_event":
		if f.Body.Event.TemplateCardEvent == nil {
			return
		}
		ev := f.Body.Event.TemplateCardEvent
		// 解析 event_key (格式: "DONE|task_id" / "SHORT|task_id")
		kind, taskID := parseButtonKey2(ev.EventKey)
		log.Printf("[wecom] button click: chat=%s user=%s key=%s -> kind=%s task=%s",
			f.Body.ChatID, f.Body.From.UserID, ev.EventKey, kind, taskID)
		if w.OnButtonClick != nil && taskID != "" {
			w.OnButtonClick(f.Body.ChatID, f.Body.From.UserID, taskID, ev.EventKey)
		}
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
	if f.Body.Text != nil && w.OnMessage != nil {
		w.OnMessage(f.Body.ChatID, f.Body.From.UserID, f.Body.Text.Content)
	}
}

func parseButtonKey2(key string) (kind, taskID string) {
	if i := strings.Index(key, "|"); i >= 0 {
		return key[:i], key[i+1:]
	}
	return "", key
}

// recordChat 记录 chat_id(首次发现)
func (w *WeCom) recordChat(chatID string) {
	w.mu.Lock()
	if _, exists := w.discovered[chatID]; !exists {
		w.discovered[chatID] = time.Now()
		w.mu.Unlock()
		_ = w.saveBindings()
		log.Printf("[wecom] NEW chat discovered: %s (use POST /api/v1/restock/wecom/chats/bind to bind role)", chatID)
		return
	}
	w.mu.Unlock()
}

// =====================================================================
// 发消息
// =====================================================================

// SendAppChat 发消息到指定 chat_id(role 找 chat_id)
//   role: "floor" / "office" / 任何已绑定的 chat_id
func (w *WeCom) SendAppChat(ctx context.Context, chatID string, body []byte) error {
	chatID = w.resolveChatID(chatID)
	if chatID == "" {
		return fmt.Errorf("no chat_id for role, bind first via /api/v1/restock/wecom/chats/bind")
	}

	// 解析 body 拿 msgtype 和对应字段
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

// resolveChatID role → chat_id
func (w *WeCom) resolveChatID(roleOrChatID string) string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if strings.HasPrefix(roleOrChatID, "wr") || strings.HasPrefix(roleOrChatID, "wc") {
		// 直接是 chat_id
		return roleOrChatID
	}
	for cid, role := range w.bindings {
		if role == roleOrChatID {
			return cid
		}
	}
	return ""
}

// =====================================================================
// Bindings 持久化
// =====================================================================

func (w *WeCom) bindingsFile() string {
	if w.cfg.WeComBindFile != "" {
		return w.cfg.WeComBindFile
	}
	return "./wecom_bindings.yaml"
}

func (w *WeCom) loadBindings() {
	// 优先读 .yaml(新格式), 如果不存在再读 .json(老格式, 一次性迁移)
	yamlPath := w.bindingsFile()
	jsonPath := yamlPath
	if strings.HasSuffix(yamlPath, ".yaml") {
		jsonPath = strings.TrimSuffix(yamlPath, ".yaml") + ".json"
	} else if strings.HasSuffix(yamlPath, ".yml") {
		jsonPath = strings.TrimSuffix(yamlPath, ".yml") + ".json"
	}

	var bs []byte
	var err error
	if bs, err = os.ReadFile(yamlPath); err != nil {
		// yaml 不存在, 试老 json
		if bs, err = os.ReadFile(jsonPath); err != nil {
			return
		}
		log.Printf("[wecom] migrating bindings: %s -> %s", jsonPath, yamlPath)
	}

	var list []ChatBinding
	// 试 yaml
	if err := yaml.Unmarshal(bs, &list); err != nil {
		// 试 json(老文件)
		if jerr := json.Unmarshal(bs, &list); jerr != nil {
			log.Printf("[wecom] load bindings: yaml=%v json=%v", err, jerr)
			return
		}
	}
	w.mu.Lock()
	for _, b := range list {
		w.bindings[b.ChatID] = b.Role
		if b.FirstSeen.IsZero() {
			w.discovered[b.ChatID] = time.Now()
		} else {
			w.discovered[b.ChatID] = b.FirstSeen
		}
	}
	w.mu.Unlock()

	// 迁移成功后, 删老 json
	if yamlPath != jsonPath {
		if _, err := os.Stat(jsonPath); err == nil {
			_ = os.Remove(jsonPath)
			log.Printf("[wecom] removed old %s", jsonPath)
		}
		// 立即用新格式重写一次(标准化)
		_ = w.saveBindings()
	}
}

func (w *WeCom) saveBindings() error {
	w.mu.RLock()
	list := make([]ChatBinding, 0, len(w.discovered))
	for cid, seen := range w.discovered {
		list = append(list, ChatBinding{
			ChatID:    cid,
			Role:      w.bindings[cid],
			FirstSeen: seen,
		})
	}
	w.mu.Unlock()

	// 头部加注释(让用户知道这是自动生成的)
	header := []byte("# wecom_bindings.yaml - 自动生成,手编后会被覆盖\n# role: floor | office | (空)\n# 手动改后请通过 POST /api/v1/restock/wecom/chats/bind 写回\n")
	bs, err := yaml.Marshal(list)
	if err != nil {
		return err
	}
	return os.WriteFile(w.bindingsFile(), append(header, bs...), 0o644)
}

// BindChat 绑定 chat_id 到 role
func (w *WeCom) BindChat(chatID, role string) error {
	if role != "floor" && role != "office" && role != "" {
		return fmt.Errorf("role must be floor/office/empty")
	}
	w.mu.Lock()
	w.bindings[chatID] = role
	if _, ok := w.discovered[chatID]; !ok {
		w.discovered[chatID] = time.Now()
	}
	w.mu.Unlock()
	return w.saveBindings()
}

// ListChats 列出所有已知 chat
func (w *WeCom) ListChats() []ChatBinding {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make([]ChatBinding, 0, len(w.discovered))
	for cid, seen := range w.discovered {
		out = append(out, ChatBinding{
			ChatID:    cid,
			Role:      w.bindings[cid],
			FirstSeen: seen,
		})
	}
	return out
}

// IsConnected 长连接状态
func (w *WeCom) IsConnected() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.connected
}

// =====================================================================
// HTTP helpers (保留供旧代码使用,实际不再调用)
// =====================================================================

// SendAppChatLegacy 兼容旧签名
func (w *WeCom) SendAppChatLegacy(chatID string) bool { return false }

// Truncate 字符串截断(供同包使用)
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
