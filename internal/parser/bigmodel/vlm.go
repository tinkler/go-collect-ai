// Package bigmodel - VlmClient 多模态直读图 (Phase B+ 2026-09-03)
//
// 取代 OCR + 文本 LLM 链路:
//   旧: image → BigModel OCR (hand_write) → text → GLM-4-flash → JSON
//   新: image → GLM-4V (多模态) → JSON 直接出
//
// 设计:
//   - 同 endpoint (chat/completions), 只是 content 用 image_url 多模态
//   - 支持任意模型 (glm-4v / glm-4v-plus), 默认 glm-4v
//   - 直接返 string (JSON 内容), 解析由调用方用 ParseLlmJson
//   - 接口跟 LlmClient 保持风格一致 (model 每次传, 客户端无状态)
//
// 性能参考 (e2e 2026-09-03 验证):
//   - prompt tokens: 3399 (固定)
//   - completion: 500-800 / 图
//   - 耗时: 12-25s / 图
//   - 准确性: 13 位 EAN-13 barcode 100%, qty 100%
package bigmodel

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// VlmClient BigModel GLM-4V 多模态
type VlmClient struct {
	apiKey   string
	baseURL  string
	timeout  time.Duration
	language string // 默认 "CHN_ENG" (GLM-4V 自动处理)
}

func NewVlmClient(apiKey, baseURL string, timeoutSec int) *VlmClient {
	if baseURL == "" {
		baseURL = "https://open.bigmodel.cn/api/paas/v4"
	}
	return &VlmClient{
		apiKey:  apiKey,
		baseURL: baseURL,
		timeout: time.Duration(timeoutSec) * time.Second,
	}
}

const vlmDefaultModel = "glm-4v"

// vlmMessage 多模态 chat 消息
type vlmMessage struct {
	Role    string        `json:"role"`
	Content []vlmContent  `json:"content"`
}

type vlmContent struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *vlmImageURL    `json:"image_url,omitempty"`
}

type vlmImageURL struct {
	URL string `json:"url"`
}

// vlmRequest 多模态 chat 请求
type vlmRequest struct {
	Model       string         `json:"model"`
	Messages    []vlmMessage   `json:"messages"`
	Temperature float64        `json:"temperature,omitempty"`
	TopP        float64        `json:"top_p,omitempty"`
	MaxTokens   int            `json:"max_tokens,omitempty"`
}

// ChatWithImage 调 GLM-4V 多模态, 返回 LLM 文本响应 (期望 JSON 字符串)
//   - model: "glm-4v" / "glm-4v-plus" / 空 = 默认 glm-4v
//   - imageBytes: 图片原始 bytes (jpg/png)
//   - imageName: 文件名 (用于推断 mime type)
//   - sysPrompt: system prompt (业务指令)
//   - userPrompt: user prompt (图片内容以外的说明,可空)
func (c *VlmClient) ChatWithImage(sysPrompt, userPrompt, model string, imageBytes []byte, imageName string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("vlm client 未初始化")
	}
	if len(imageBytes) == 0 {
		return "", fmt.Errorf("imageBytes 不能为空")
	}
	if model == "" {
		model = vlmDefaultModel
	}

	// 推断 mime
	mime := "image/jpeg"
	ext := strings.ToLower(filepath.Ext(imageName))
	switch ext {
	case ".png":
		mime = "image/png"
	case ".webp":
		mime = "image/webp"
	case ".bmp":
		mime = "image/bmp"
	}

	// base64 data URL
	b64 := base64.StdEncoding.EncodeToString(imageBytes)
	dataURL := fmt.Sprintf("data:%s;base64,%s", mime, b64)

	// 构造 content 数组
	contents := []vlmContent{
		{Type: "image_url", ImageURL: &vlmImageURL{URL: dataURL}},
	}
	if userPrompt != "" {
		contents = append(contents, vlmContent{Type: "text", Text: userPrompt})
	}

	// messages: [system, user(image + text)]
	// 跟 OpenAI 多模态一致, system prompt 单独一条
	msgs := []vlmMessage{
		{Role: "user", Content: contents},
	}
	if sysPrompt != "" {
		// 插到 user 前面
		msgs = []vlmMessage{
			{Role: "system", Content: []vlmContent{{Type: "text", Text: sysPrompt}}},
			{Role: "user", Content: contents},
		}
	}

	req := vlmRequest{
		Model:       model,
		Messages:    msgs,
		Temperature: 0.1,
		TopP:        0.7,
		// BigModel GLM-4V max_tokens 限制 [1, 2048]
		// 我们解析单图供货单通常 500-800 tokens, 1024 足够
		MaxTokens:   1024,
	}
	body, _ := json.Marshal(req)

	httpReq, err := http.NewRequest("POST", c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("vlm http request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{Timeout: c.timeout}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("vlm http do: %w", err)
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("VLM HTTP %d: %s", resp.StatusCode, truncate(string(respBytes), 400))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return "", fmt.Errorf("VLM parse: %w; body=%s", err, truncate(string(respBytes), 200))
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("VLM 无 choices: %s", truncate(string(respBytes), 200))
	}
	return parsed.Choices[0].Message.Content, nil
}
