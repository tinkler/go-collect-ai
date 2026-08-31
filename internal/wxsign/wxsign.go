// Package wxsign 企业微信 JS-SDK 签名服务 (2026-08-31)
//
//   提供 2 个公开端点 (H5 自建应用必须):
//     GET /api/v1/wx/sign?url=...          → 企业身份签名 (corpid, timestamp, nonceStr, signature)
//     GET /api/v1/wx/agent-sign?url=...    → 应用身份签名 (corpid, agentid, timestamp, nonceStr, signature)
//
//   前端 (新版 @wecom/jssdk) 用法:
//     const { corpid, timestamp, nonceStr, signature } = await fetch('/api/v1/wx/sign?url='+url).then(r=>r.json())
//     ww.register({
//       corpId: corpid,
//       agentId: <agentid>,
//       jsApiList: ['scanQRCode'],
//       getConfigSignature: async (url) => fetch('/api/v1/wx/sign?url='+encodeURIComponent(url)).then(r=>r.json()),
//       getAgentConfigSignature: async (url) => fetch('/api/v1/wx/agent-sign?url='+encodeURIComponent(url)).then(r=>r.json()),
//     })
//     ww.scanQRCode({ needResult:1, scanType:['qrCode','barCode'], success(r){...} })
//
//   dev 模式: cfg.WeComCorpID/AgentID/CorpSecret 为空时, 端点返回 503 + reason="dev_mode",
//   前端看到就 fallback 到手动输入 + 拍照 (已有逻辑)
//
//   注意:
//   - access_token / jsapi_ticket 全局缓存 2h (官方有效期), 提前 5min 续
//   - sha1 签名算法见官方文档
//   - 跨进程实例要共享 token: 当前用单进程内存, 后续多副本需切 Redis
package wxsign

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Service 签名服务
type Service struct {
	corpID     string
	agentID    string
	corpSecret string

	httpClient *http.Client

	mu         sync.Mutex
	accessTok  string
	tokExpire  time.Time
	agentTicket string
	tkExpire   time.Time
}

// New 构造, dev 模式 corpID 空也没事, 调用时会返 503
func New(corpID, agentID, corpSecret string) *Service {
	return &Service{
		corpID:     corpID,
		agentID:    agentID,
		corpSecret: corpSecret,
		httpClient: &http.Client{Timeout: 8 * time.Second},
	}
}

// IsConfigured 是否配了企微凭证 (前端用来判断走 wx-sdk 还是手动 fallback)
func (s *Service) IsConfigured() bool {
	return s.corpID != "" && s.agentID != "" && s.corpSecret != ""
}

// SignConfig 企业身份签名 (corpid)
func (s *Service) SignConfig(ctx context.Context, rawURL string) (corpid, timestamp, nonceStr, signature string, err error) {
	if !s.IsConfigured() {
		return "", "", "", "", fmt.Errorf("dev_mode: WeComCorpID/AgentID/CorpSecret 未配置")
	}
	tk, err := s.getAgentJsapiTicket(ctx)
	if err != nil {
		return "", "", "", "", fmt.Errorf("get agent jsapi_ticket: %w", err)
	}
	cleanURL := stripURLHash(rawURL)
	timestamp, nonceStr = genNonce()
	signature = sha1Sign(map[string]string{
		"jsapi_ticket": tk,
		"noncestr":     nonceStr,
		"timestamp":    timestamp,
		"url":          cleanURL,
	})
	return s.corpID, timestamp, nonceStr, signature, nil
}

// SignAgent 应用身份签名 (corpid + agentid) — agentConfig 用
func (s *Service) SignAgent(ctx context.Context, rawURL string) (corpid, agentid, timestamp, nonceStr, signature string, err error) {
	if !s.IsConfigured() {
		return "", "", "", "", "", fmt.Errorf("dev_mode: WeComCorpID/AgentID/CorpSecret 未配置")
	}
	tk, err := s.getAgentJsapiTicket(ctx)
	if err != nil {
		return "", "", "", "", "", fmt.Errorf("get agent jsapi_ticket: %w", err)
	}
	cleanURL := stripURLHash(rawURL)
	timestamp, nonceStr = genNonce()
	signature = sha1Sign(map[string]string{
		"jsapi_ticket": tk,
		"noncestr":     nonceStr,
		"timestamp":    timestamp,
		"url":          cleanURL,
	})
	return s.corpID, s.agentID, timestamp, nonceStr, signature, nil
}

// ============== 内部 ==============

func (s *Service) getAccessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.accessTok != "" && time.Now().Add(5*time.Minute).Before(s.tokExpire) {
		return s.accessTok, nil
	}
	apiURL := fmt.Sprintf("https://qyapi.weixin.qq.com/cgi-bin/gettoken?corpid=%s&corpsecret=%s",
		url.QueryEscape(s.corpID), url.QueryEscape(s.corpSecret))
	req, _ := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg   string `json:"errmsg"`
		Token    string `json:"access_token"`
		Expires  int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("decode: %w (body=%s)", err, truncate(string(body), 200))
	}
	if r.ErrCode != 0 || r.Token == "" {
		return "", fmt.Errorf("wecom errcode=%d errmsg=%s (检查 WECOM_CORP_ID/SECRET 是否有误)", r.ErrCode, r.ErrMsg)
	}
	s.accessTok = r.Token
	s.tokExpire = time.Now().Add(time.Duration(r.Expires) * time.Second)
	log.Printf("[wxsign] access_token refreshed, expires_in=%ds", r.Expires)
	return s.accessTok, nil
}

func (s *Service) getAgentJsapiTicket(ctx context.Context) (string, error) {
	s.mu.Lock()
	if s.agentTicket != "" && time.Now().Add(5*time.Minute).Before(s.tkExpire) {
		s.mu.Unlock()
		return s.agentTicket, nil
	}
	s.mu.Unlock()

	tok, err := s.getAccessToken(ctx)
	if err != nil {
		return "", err
	}

	apiURL := fmt.Sprintf("https://qyapi.weixin.qq.com/cgi-bin/ticket/get?access_token=%s&type=agent_config", tok)
	req, _ := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg   string `json:"errmsg"`
		Ticket   string `json:"ticket"`
		Expires  int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	if r.ErrCode != 0 || r.Ticket == "" {
		return "", fmt.Errorf("wecom ticket errcode=%d errmsg=%s (检查应用 agentid 对应 secret 是否对, 以及可信IP是否加白)", r.ErrCode, r.ErrMsg)
	}

	s.mu.Lock()
	s.agentTicket = r.Ticket
	s.tkExpire = time.Now().Add(time.Duration(r.Expires) * time.Second)
	s.mu.Unlock()
	log.Printf("[wxsign] agent jsapi_ticket refreshed, expires_in=%ds", r.Expires)
	return s.agentTicket, nil
}

// stripURLHash 去掉 # 及其后面部分 (官方签名算法要求)
func stripURLHash(raw string) string {
	if i := strings.Index(raw, "#"); i >= 0 {
		return raw[:i]
	}
	return raw
}

// genNonce 生成时间戳 + 16 字节随机 hex
func genNonce() (ts, nonce string) {
	ts = strconv.FormatInt(time.Now().Unix(), 10)
	b := make([]byte, 16)
	rand.Read(b)
	nonce = hex.EncodeToString(b)
	return
}

// sha1Sign 按官方算法: 参数按 ASCII 升序拼接成 key=value&key=value... 形式
func sha1Sign(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	// 简单 sort (ASCII 顺序, 小写参数名都满足)
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	str := strings.Join(parts, "&")
	h := sha1.Sum([]byte(str))
	return hex.EncodeToString(h[:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
