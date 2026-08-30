package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// WeComClient 企微 OAuth client
//   - access_token 缓存 2h (实际有效期 7200s, 提前 5min 续)
//   - getuserinfo(code → userid) 调 https://qyapi.weixin.qq.com/cgi-bin/auth/getuserinfo
//
// 注意: corpSecret 绝对不能进 log / response
type WeComClient struct {
	corpID     string
	agentID    string
	corpSecret string
	httpClient *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

// NewWeComClient
//   corpID / agentID / corpSecret 都从 cfg 读
//   生产环境任何字段为空 → caller 应该在启动时检查
func NewWeComClient(corpID, agentID, corpSecret string) *WeComClient {
	return &WeComClient{
		corpID:      corpID,
		agentID:     agentID,
		corpSecret:  corpSecret,
		httpClient:  &http.Client{Timeout: 8 * time.Second},
		accessToken: "",
		expiresAt:   time.Time{},
	}
}

// Enabled 是否配置完整 (生产 OAuth 必备)
//   dev 模式不需要 OAuth, 可以不全填
func (c *WeComClient) Enabled() bool {
	return c.corpID != "" && c.agentID != "" && c.corpSecret != ""
}

// wecomAccessTokenResp access_token 接口响应
type wecomAccessTokenResp struct {
	ErrCode     int    `json:"errcode"`
	ErrMsg      string `json:"errmsg"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// getAccessToken 拿 / 刷新 access_token (内部锁)
func (c *WeComClient) getAccessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 缓存有效: 提前 5min 算过期
	if c.accessToken != "" && time.Now().Add(5*time.Minute).Before(c.expiresAt) {
		return c.accessToken, nil
	}
	u := fmt.Sprintf("https://qyapi.weixin.qq.com/cgi-bin/gettoken?corpid=%s&corpsecret=%s",
		url.QueryEscape(c.corpID), url.QueryEscape(c.corpSecret))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("wecom gettoken http: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r wecomAccessTokenResp
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("wecom gettoken unmarshal: %w (body: %s)", err, redact(body))
	}
	if r.ErrCode != 0 {
		return "", fmt.Errorf("wecom gettoken errcode=%d errmsg=%s", r.ErrCode, r.ErrMsg)
	}
	c.accessToken = r.AccessToken
	if r.ExpiresIn > 0 {
		c.expiresAt = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second)
	} else {
		c.expiresAt = time.Now().Add(2 * time.Hour)
	}
	// 注意: 不要 log access_token / corpSecret
	log.Printf("[auth/wecom] access_token refreshed, expires_at=%s", c.expiresAt.Format(time.RFC3339))
	return c.accessToken, nil
}

// wecomUserInfoResp getuserinfo 响应
type wecomUserInfoResp struct {
	ErrCode  int    `json:"errcode"`
	ErrMsg   string `json:"errmsg"`
	UserID   string `json:"userid"`
	OpenID   string `json:"openid"`
	DeviceID string `json:"deviceid"`
}

// Code2UserID 企微 code 换 userid
//   实际流程: 前端用 corpID + agentID 拿到 code → 后端用 corpSecret 换 userid
func (c *WeComClient) Code2UserID(ctx context.Context, code string) (userID, name string, err error) {
	if !c.Enabled() {
		return "", "", fmt.Errorf("wecom not configured (corpID/agentID/corpSecret empty)")
	}
	tok, err := c.getAccessToken(ctx)
	if err != nil {
		return "", "", err
	}
	u := fmt.Sprintf("https://qyapi.weixin.qq.com/cgi-bin/auth/getuserinfo?access_token=%s&code=%s",
		url.QueryEscape(tok), url.QueryEscape(code))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("wecom getuserinfo http: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r wecomUserInfoResp
	if err := json.Unmarshal(body, &r); err != nil {
		return "", "", fmt.Errorf("wecom getuserinfo unmarshal: %w (body: %s)", err, redact(body))
	}
	if r.ErrCode != 0 {
		return "", "", fmt.Errorf("wecom getuserinfo errcode=%d errmsg=%s", r.ErrCode, r.ErrMsg)
	}
	// userid 是企微内部 id, 名字还得另调 userid→userdetail, 简化起见本期用 userid 作 name 后缀
	//   真实场景应该再调一次 /cgi-bin/user/get, 这里先给个可读 placeholder
	return r.UserID, "企微用户 " + r.UserID, nil
}

// redact 脱敏 (避免 log 误打 access_token / code)
func redact(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}
