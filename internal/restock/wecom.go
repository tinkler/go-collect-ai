package restock

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// WeCom 企业微信智能机器人 client
//
// 复用:无(企微 client 是新增)
// 文档: https://developer.work.weixin.qq.com/document/path/90236
//
// 主要能力:
//   - get_access_token: 7200s 过期,内存缓存
//   - appchat/msg 群应用消息发送(传 chat_id)
//   - URL 验证(GET,首次配置回调 URL 时)
//   - 回调加解密(AES-256-CBC,PKCS#7,EncodingAESKey)
//   - 回调签名校验(SHA1)
type WeCom struct {
	cfg *RestockConfig

	mu          sync.Mutex
	accessToken string
	tokenExp    time.Time
	httpClient  *http.Client
}

func NewWeCom(cfg *RestockConfig) *WeCom {
	return &WeCom{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// GetAccessToken 取(并缓存)access_token
func (w *WeCom) GetAccessToken(ctx context.Context) (string, error) {
	if w.cfg.WeComCorpID == "" || w.cfg.WeComAgentSecret == "" {
		return "", fmt.Errorf("WECOM_CORP_ID / WECOM_AGENT_SECRET 未配置")
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	// 提前 5 分钟刷新
	if w.accessToken != "" && time.Now().Add(5*time.Minute).Before(w.tokenExp) {
		return w.accessToken, nil
	}

	url := fmt.Sprintf("https://qyapi.weixin.qq.com/cgi-bin/gettoken?corpid=%s&corpsecret=%s",
		w.cfg.WeComCorpID, w.cfg.WeComAgentSecret)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := w.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var r struct {
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("parse token: %w (body=%s)", err, truncate(string(body), 200))
	}
	if r.ErrCode != 0 {
		return "", fmt.Errorf("gettoken errcode=%d errmsg=%s", r.ErrCode, r.ErrMsg)
	}
	w.accessToken = r.AccessToken
	w.tokenExp = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second)
	return w.accessToken, nil
}

// CreateAppChat 建群
//   POST https://qyapi.weixin.qq.com/cgi-bin/appchat/create?access_token=...
//   body: {"name":"群名","owner":"userid","userlist":["userid1","userid2"],"chatid":"可选,自定义 ID"}
//   返回: {"errcode":0,"errmsg":"ok","chatid":"wrXXX"}
//
// 注意: 智能机器人应用建群需要 owner 是应用的可见范围成员,userlist 同理
func (w *WeCom) CreateAppChat(ctx context.Context, name, owner string, userlist []string, customChatID string) (string, error) {
	token, err := w.GetAccessToken(ctx)
	if err != nil {
		return "", err
	}
	if owner == "" {
		return "", fmt.Errorf("owner is required (use 应用可见范围内的 userid)")
	}

	payload := map[string]any{
		"name":    name,
		"owner":   owner,
		"userlist": userlist,
	}
	if customChatID != "" {
		payload["chatid"] = customChatID
	}
	bs, _ := json.Marshal(payload)

	url := "https://qyapi.weixin.qq.com/cgi-bin/appchat/create?access_token=" + token
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBs, _ := io.ReadAll(resp.Body)

	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		ChatID  string `json:"chatid"`
	}
	if err := json.Unmarshal(respBs, &r); err != nil {
		return "", fmt.Errorf("parse create: %w (body=%s)", err, truncate(string(respBs), 200))
	}
	if r.ErrCode != 0 {
		return "", fmt.Errorf("appchat.create errcode=%d errmsg=%s", r.ErrCode, r.ErrMsg)
	}
	return r.ChatID, nil
}

// SendAppChat 发群应用消息到指定 chat_id
//   chatID: 群 chat_id
//   body:   完整的 msgtype=template_card JSON
func (w *WeCom) SendAppChat(ctx context.Context, chatID string, body []byte) error {
	if chatID == "" {
		return fmt.Errorf("chatID is empty")
	}
	token, err := w.GetAccessToken(ctx)
	if err != nil {
		return err
	}

	// 在 body 顶层注入 chatid(企微要求)
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		return fmt.Errorf("body not JSON: %w", err)
	}
	msg["chatid"] = chatID
	bs, _ := json.Marshal(msg)

	url := "https://qyapi.weixin.qq.com/cgi-bin/appchat/send?access_token=" + token
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBs, _ := io.ReadAll(resp.Body)

	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(respBs, &r); err != nil {
		return fmt.Errorf("parse send: %w", err)
	}
	if r.ErrCode != 0 {
		return fmt.Errorf("appchat.send errcode=%d errmsg=%s", r.ErrCode, r.ErrMsg)
	}
	return nil
}

// VerifyURL GET /wecom/callback 首次配置 URL 时验证
//   URL 上 query: ?msg_signature=...&timestamp=...&nonce=...&echostr=...
//   流程: SHA1 校验 → AES 解密 echostr → 返回明文
func (w *WeCom) VerifyURL(signature, timestamp, nonce, echostr string) (string, error) {
	if err := w.checkSignature(signature, timestamp, nonce, echostr); err != nil {
		return "", err
	}
	plain, err := w.decrypt(echostr)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// DecryptEvent POST /wecom/callback 业务消息解密
//   流程: SHA1 校验 → AES 解密 msg → 解析 XML
func (w *WeCom) DecryptEvent(signature, timestamp, nonce, encrypt string) ([]byte, error) {
	if err := w.checkSignature(signature, timestamp, nonce, encrypt); err != nil {
		return nil, err
	}
	return w.decrypt(encrypt)
}

// checkSignature SHA1(token, timestamp, nonce, encrypt) 字典序排序后拼接
func (w *WeCom) checkSignature(signature, timestamp, nonce, encrypt string) error {
	if w.cfg.WeComCallbackToken == "" {
		return fmt.Errorf("WECOM_CALLBACK_TOKEN 未配置")
	}
	parts := []string{w.cfg.WeComCallbackToken, timestamp, nonce, encrypt}
	sort.Strings(parts)
	h := sha1.New()
	h.Write([]byte(strings.Join(parts, "")))
	got := hex.EncodeToString(h.Sum(nil))
	if got != signature {
		return fmt.Errorf("signature mismatch: got=%s want=%s", got, signature)
	}
	return nil
}

// decrypt AES-256-CBC 解密 EncodingAESKey
//   EncodingAESKey 是 43 字符 base64 → 32 字节 key
//   IV = AESKey 前 16 字节
//   明文 = 16 字节随机 + 真实消息 + 4 字节 msg_len(网络字节序) + receiveid
func (w *WeCom) decrypt(encrypt string) ([]byte, error) {
	if w.cfg.WeComCallbackAES == "" {
		return nil, fmt.Errorf("WECOM_CALLBACK_AES_KEY 未配置")
	}
	// 企微的 EncodingAESKey 是 43 字符 base64(无 padding),加 "=" 补齐
	aesKeyB64 := w.cfg.WeComCallbackAES
	if len(aesKeyB64) == 43 {
		aesKeyB64 += "="
	}
	aesKey, err := base64.StdEncoding.DecodeString(aesKeyB64)
	if err != nil {
		return nil, fmt.Errorf("base64 aes key: %w", err)
	}

	encBs, err := base64.StdEncoding.DecodeString(encrypt)
	if err != nil {
		return nil, fmt.Errorf("base64 encrypt: %w", err)
	}

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	if len(encBs)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext not block-aligned")
	}
	iv := aesKey[:16]
	mode := cipher.NewCBCDecrypter(block, iv)
	plain := make([]byte, len(encBs))
	mode.CryptBlocks(plain, encBs)

	// 去 PKCS#7 padding
	padLen := int(plain[len(plain)-1])
	if padLen < 1 || padLen > 32 {
		return nil, fmt.Errorf("bad padding: %d", padLen)
	}
	plain = plain[:len(plain)-padLen]

	// 前 16 字节随机 → 跳过
	// 接下来 N 字节是消息
	// 4 字节(网络字节序)msg_len
	// 剩下是 receiveid
	if len(plain) < 20 {
		return nil, fmt.Errorf("plaintext too short: %d", len(plain))
	}
	msgLen := binary.BigEndian.Uint32(plain[16:20])
	if int(msgLen) > len(plain)-20 {
		return nil, fmt.Errorf("msg_len=%d exceeds plain", msgLen)
	}
	return plain[20 : 20+msgLen], nil
}

// Encrypt 加密(回执响应企微时使用,业务上不太需要,留接口)
func (w *WeCom) Encrypt(plain []byte) (string, error) {
	aesKeyB64 := w.cfg.WeComCallbackAES
	if len(aesKeyB64) == 43 {
		aesKeyB64 += "="
	}
	aesKey, err := base64.StdEncoding.DecodeString(aesKeyB64)
	if err != nil {
		return "", err
	}
	// 16 字节随机 + plain + 4 字节 len(网络序) + receiveid
	buf := make([]byte, 16+len(plain)+4+len(w.cfg.WeComCorpID))
	rand.Read(buf[:16])
	copy(buf[16:], plain)
	binary.BigEndian.PutUint32(buf[16+len(plain):], uint32(len(plain)))
	copy(buf[16+len(plain)+4:], w.cfg.WeComCorpID)

	// PKCS#7 padding
	padLen := aes.BlockSize - len(buf)%aes.BlockSize
	pad := bytes.Repeat([]byte{byte(padLen)}, padLen)
	buf = append(buf, pad...)

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", err
	}
	iv := aesKey[:16]
	mode := cipher.NewCBCEncrypter(block, iv)
	enc := make([]byte, len(buf))
	mode.CryptBlocks(enc, buf)
	return base64.StdEncoding.EncodeToString(enc), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Truncate 导出(供同包其它文件使用)
func Truncate(s string, n int) string { return truncate(s, n) }
