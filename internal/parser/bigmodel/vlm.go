// Package bigmodel - VlmClient 多模态直读图 (Phase B+ 2026-09-03)
//
// 取代 OCR + 文本 LLM 链路:
//
//	旧: image → BigModel OCR (hand_write) → text → GLM-4-flash → JSON
//	新: image → GLM-4V (多模态) → JSON 直接出
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
	"log"
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
	// maxTokens 单次响应 token 上限。
	// BigModel GLM-4V 限制 [1, 2048],2026-09-04 线上事故发现 1024 不足以覆盖
	// 多行(>15 行)供货单,响应会被截断 → finish_reason="length" → JSON 解析失败。
	// 默认提到 2048 = BigModel 硬上限,无法再 retry 提升;只能让 orchestrator
	// 检测到 length 后用启发式兜底,或换 glm-4v-plus(更贵但更准)。
	maxTokens int
}

func NewVlmClient(apiKey, baseURL string, timeoutSec int) *VlmClient {
	if baseURL == "" {
		baseURL = "https://open.bigmodel.cn/api/paas/v4"
	}
	return &VlmClient{
		apiKey:    apiKey,
		baseURL:   baseURL,
		timeout:   time.Duration(timeoutSec) * time.Second,
		maxTokens: 2048, // GLM-4V 硬上限
	}
}

const vlmDefaultModel = "glm-4v"

// SetMaxTokens 覆盖默认 max_tokens(只用于测试/特殊场景,生产建议保持 2048)
func (c *VlmClient) SetMaxTokens(n int) {
	if n > 0 && n <= 2048 {
		c.maxTokens = n
	}
}

// vlmMessage 多模态 chat 消息
type vlmMessage struct {
	Role    string       `json:"role"`
	Content []vlmContent `json:"content"`
}

type vlmContent struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *vlmImageURL `json:"image_url,omitempty"`
}

type vlmImageURL struct {
	URL string `json:"url"`
}

// vlmRequest 多模态 chat 请求
type vlmRequest struct {
	Model       string       `json:"model"`
	Messages    []vlmMessage `json:"messages"`
	Temperature float64      `json:"temperature,omitempty"`
	TopP        float64      `json:"top_p,omitempty"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
}

// VlmResult VLM 响应结果
//   - Content: LLM 文本响应(期望 JSON 字符串)
//   - FinishReason: "stop"=正常结束, "length"=max_tokens 截断(响应不全),
//     "content_filter"=被安全过滤, "tool_calls"=调用工具(不应该出现)
//   - Truncated: FinishReason=="length" 的便捷字段
type VlmResult struct {
	Content      string
	FinishReason string
	Truncated    bool
}

// ChatWithImage 调 GLM-4V 多模态, 返回 VlmResult
//   - model: "glm-4v" / "glm-4v-plus" / 空 = 默认 glm-4v
//   - imageBytes: 图片原始 bytes (jpg/png)
//   - imageName: 文件名 (用于推断 mime type)
//   - sysPrompt: system prompt (业务指令)
//   - userPrompt: user prompt (图片内容以外的说明,可空)
//
// 2026-09-04 修复:截断时(finish_reason=length)自动 retry 一次,缓解偶发截断
//   - 由于 GLM-4V max_tokens 上限 2048,retry 仍可能截断
//   - 重试后仍截断则返回最后一次结果(让 caller 决定如何降级)
func (c *VlmClient) ChatWithImage(sysPrompt, userPrompt, model string, imageBytes []byte, imageName string) (VlmResult, error) {
	if c == nil {
		return VlmResult{}, fmt.Errorf("vlm client 未初始化")
	}
	if len(imageBytes) == 0 {
		return VlmResult{}, fmt.Errorf("imageBytes 不能为空")
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
		// 2026-09-04: 1024→2048(BigModel GLM-4V 硬上限)
		// 单图供货单 >15 行时会触顶 1024,导致响应被截断 → JSON 解析失败
		MaxTokens: c.maxTokens,
	}
	body, _ := json.Marshal(req)

	// 2026-09-04: 截断时自动 retry 一次
	// 偶发场景:同样的 prompt+image 重跑一次,可能不再截断(LLM 采样波动)
	var lastResult VlmResult
	for attempt := 1; attempt <= 2; attempt++ {
		result, err := c.doChat(body, attempt)
		if err != nil {
			return VlmResult{}, err
		}
		lastResult = result
		if !result.Truncated {
			return result, nil
		}
		if attempt == 1 {
			// 第一次截断 → 重试一次(同 max_tokens,只是 LLM 重新采样)
			log.Printf("[vlm] 响应被截断 (finish_reason=length), retry 一次 (max_tokens=%d)", c.maxTokens)
		}
	}
	// 两次都截断,返回最后一次结果(由 caller 决定如何降级)
	log.Printf("[vlm] 重试后仍截断, 返最后结果 (max_tokens=%d, content_len=%d)", c.maxTokens, len(lastResult.Content))
	return lastResult, nil
}

// doChat 实际发一次 HTTP 请求
func (c *VlmClient) doChat(body []byte, attempt int) (VlmResult, error) {
	httpReq, err := http.NewRequest("POST", c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return VlmResult{}, fmt.Errorf("vlm http request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{Timeout: c.timeout}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return VlmResult{}, fmt.Errorf("vlm http do (attempt=%d): %w", attempt, err)
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return VlmResult{}, fmt.Errorf("VLM HTTP %d: %s", resp.StatusCode, truncate(string(respBytes), 400))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return VlmResult{}, fmt.Errorf("VLM parse: %w; body=%s", err, truncate(string(respBytes), 200))
	}
	if len(parsed.Choices) == 0 {
		return VlmResult{}, fmt.Errorf("VLM 无 choices: %s", truncate(string(respBytes), 200))
	}

	choice := parsed.Choices[0]
	result := VlmResult{
		Content:      choice.Message.Content,
		FinishReason: choice.FinishReason,
		Truncated:    choice.FinishReason == "length",
	}
	return result, nil
}
