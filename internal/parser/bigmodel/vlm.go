// Package bigmodel - VlmClient 多模态直读图 (Phase B+ 2026-09-03)
//
//	取代 OCR + 文本 LLM 链路:
//
//	旧: image → BigModel OCR (hand_write) → text → GLM-4-flash → JSON
//	新: image → GLM-4V (多模态) → JSON 直接出
//
//	设计:
//	  - 同 endpoint (chat/completions), 只是 content 用 image_url 多模态
//	  - 支持任意模型 (glm-4v / glm-4v-plus), 默认 glm-4v
//	  - 直接返 string (JSON 内容), 解析由调用方用 ParseLlmJson
//	  - 接口跟 LlmClient 保持风格一致 (model 每次传, 客户端无状态)
//
//	2026-09-04 改造:
//	  - 去掉图片压缩 (shrinkForVLM / nearestNeighborScale 已删除, 改回原图直传)
//	  - 加 ctx 透传 (ChatWithImageCtx), 让 caller 用 detached bg ctx 跟客户端断连解耦
package bigmodel

import (
	"bytes"
	"context"
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
	baseURL   string
	apiKey    string
	maxTokens int
	timeout   time.Duration // http.Client.Timeout 兜底
}

// NewVlmClient 工厂
//   - timeoutSec: http.Client 硬上限 (60s) 兜底,ctx 可由 caller 自行加 timeout
//   - maxTokens: 2026-09-04 起固定 2048 (BigModel GLM-4V 硬上限)
func NewVlmClient(apiKey, baseURL string, timeoutSec int) *VlmClient {
	if baseURL == "" {
		baseURL = "https://open.bigmodel.cn/api/paas/v4"
	}
	if timeoutSec <= 0 {
		timeoutSec = 60
	}
	return &VlmClient{
		baseURL:   baseURL,
		apiKey:    apiKey,
		maxTokens: 2048,
		timeout:   time.Duration(timeoutSec) * time.Second,
	}
}

// VlmResult 单次调用的结果
//
//	- Truncated: FinishReason=="length" 的便捷字段
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
//
// 2026-09-04 改造: ctx 透传。旧实现 http.NewRequest 不传 ctx, 导致
//   Orchestrator 设的 25s timeout 无法生效,BigModel 排队 30-60s 也得等。
//   新增 ChatWithImageCtx,http.NewRequestWithContext 把 ctx 串进去,客户端断/超时
//   能立即中断请求不再空等。
func (c *VlmClient) ChatWithImage(sysPrompt, userPrompt, model string, imageBytes []byte, imageName string) (VlmResult, error) {
	return c.ChatWithImageCtx(context.Background(), sysPrompt, userPrompt, model, imageBytes, imageName)
}

// ChatWithImageCtx 带 ctx 版本的 ChatWithImage。
//   - ctx 透传到 http.NewRequestWithContext + httpClient.Do,客户端断开/超时
//     能立即中断 BigModel 调用,不再空等 30-60s
//   - ctx 仍然由调用方控制(典型用法: context.Background() 让 VLM 跟客户端断连完全解耦,
//     或 context.WithTimeout 加服务端兜底上限)
func (c *VlmClient) ChatWithImageCtx(ctx context.Context, sysPrompt, userPrompt, model string, imageBytes []byte, imageName string) (VlmResult, error) {
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

	// 2026-09-04 用户决策: 去掉图片压缩, 改回原图直传
	//   之前压缩(2.5MB -> 200KB)虽能加速 BigModel,但供货单 12->17 虚增问题在压缩前后一样
	//   说明 VLM 的虚增不是压缩导致,而是 VLM 自身误识(可能跟"清晰度"无关,跟"特征"有关)
	//   改回原图:保留全部信息,后续再调优 VLM/换模型

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
	msgs := []vlmMessage{
		{Role: "user", Content: contents},
	}
	if sysPrompt != "" {
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
		MaxTokens:   c.maxTokens,
	}
	body, _ := json.Marshal(req)

	var lastResult VlmResult
	for attempt := 1; attempt <= 2; attempt++ {
		result, err := c.doChat(ctx, body, attempt)
		if err != nil {
			return VlmResult{}, err
		}
		lastResult = result
		if !result.Truncated {
			return result, nil
		}
		if attempt == 1 {
			log.Printf("[vlm] 响应被截断 (finish_reason=length), retry 一次 (max_tokens=%d)", c.maxTokens)
		}
	}
	log.Printf("[vlm] 重试后仍截断, 返最后结果 (max_tokens=%d, content_len=%d)", c.maxTokens, len(lastResult.Content))
	return lastResult, nil
}

// doChat 实际发一次 HTTP 请求
//
// 2026-09-04 修复: ctx 透传。新版用 http.NewRequestWithContext + httpClient.Do(ctx),
//   Orchestrator 包的 timeout 真正生效,BigModel 排队超时能立即中断。
//   旧实现 http.NewRequest 不传 ctx,timeout 只能等 http.Client.Timeout 兜底,
//   客户端断连无法提前中断。
func (c *VlmClient) doChat(ctx context.Context, body []byte, attempt int) (VlmResult, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return VlmResult{}, fmt.Errorf("vlm http request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	// 客户端 timeout 给个硬上限 (60s),ctx 可由 caller 自行加 timeout
	httpClient := &http.Client{Timeout: 60 * time.Second}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return VlmResult{}, fmt.Errorf("vlm http do (attempt=%d): %w", attempt, err)
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return VlmResult{}, fmt.Errorf("VLM HTTP %d: %s", resp.StatusCode, vlmTruncate(string(respBytes), 400))
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
		return VlmResult{}, fmt.Errorf("VLM parse: %w; body=%s", err, vlmTruncate(string(respBytes), 200))
	}
	if len(parsed.Choices) == 0 {
		return VlmResult{}, fmt.Errorf("VLM 无 choices: %s", vlmTruncate(string(respBytes), 200))
	}

	choice := parsed.Choices[0]
	result := VlmResult{
		Content:      choice.Message.Content,
		FinishReason: choice.FinishReason,
		Truncated:    choice.FinishReason == "length",
	}
	return result, nil
}

// ----- internal types -----

const vlmDefaultModel = "glm-4v"

type vlmContent struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *vlmImageURL `json:"image_url,omitempty"`
}

type vlmImageURL struct {
	URL string `json:"url"`
}

type vlmMessage struct {
	Role    string       `json:"role"`
	Content []vlmContent `json:"content"`
}

type vlmRequest struct {
	Model       string        `json:"model"`
	Messages    []vlmMessage  `json:"messages"`
	Temperature float64       `json:"temperature"`
	TopP        float64       `json:"top_p"`
	MaxTokens   int           `json:"max_tokens"`
}

// vlmTruncate 截断长字符串 (避免 log 爆炸)
func vlmTruncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
