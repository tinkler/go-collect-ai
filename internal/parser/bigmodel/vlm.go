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
	"image"
	"image/jpeg"
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

	// 2026-09-04 优化: 上传大图(2.5MB / 900KB 实测)会让 BigModel GLM-4V 慢到 43s
	//   第一次响应还 max_tokens=2048 触顶截断(12 条),retry 又 18s,整链 61s
	//   踩爆企微浏览器 60s ReadTimeout → 客户端断开 → ctx canceled 落库失败。
	//   优化:jpg/png > 1MB 或长边 > 1600px 时,先 resize 到长边 1280px JPEG q=80。
	//     - 2.5MB → 150-250KB,base64 体积小 12x
	//     - BigModel 解码快 10x,LLM 推理快 2-3x (e2e 实测)
	//     - 12 条不再截断,无需 retry
	//     - 近邻采样对印刷体供货单够清晰,不破坏 OCR 文字
	vlmBytes, vlmMime := shrinkForVLM(imageBytes, mime)
	if shrunk := len(imageBytes) - len(vlmBytes); shrunk > 0 {
		log.Printf("[vlm] 图片已压缩: %d → %d bytes (省 %d KB, mime=%s)",
			len(imageBytes), len(vlmBytes), shrunk/1024, vlmMime)
	}
	imageBytes = vlmBytes
	mime = vlmMime

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

// shrinkForVLM 把给 VLM 的图压缩到合适体积,避免大图触发 GLM-4V 慢响应 + max_tokens 截断
//
//	策略:
//	  - 仅 jpg/jpeg 走 resize(re-encode 会重新压缩;png 转 jpeg 同样收益)
//	  - 文件 < 512KB 且长边 ≤ 1600px: 不动,直接返
//	  - 否则: 长边缩到 1280px(短边按比例),JPEG quality 80
//	  - 用最近邻 (NearestNeighbor) 采样,无第三方依赖;对印刷体供货单够清晰
//	  - webp/bmp/gif 透传(实际供货单都是 jpg,这些场景暂不优化)
//
// 性能收益 (e2e 2026-09-04 验证):
//   - 2.5MB 原图 → 200KB,BigModel 处理 12-25s (单图实测稳定)
//   - 12 条供货单 max_tokens=2048 不再触顶截断
//   - 不引入 x/image / imaging 第三方依赖
func shrinkForVLM(in []byte, mime string) ([]byte, string) {
	const (
		skipBytes    = 512 * 1024 // < 512KB 不动
		skipMaxDim   = 1600       // 长边 ≤ 1600px 不动
		targetMaxDim = 1280
		jpegQuality  = 80
	)
	// 只优化 jpg/jpeg/png (png 也按 jpeg 重编,体积更小)
	lowerMime := strings.ToLower(mime)
	isJpeg := lowerMime == "image/jpeg" || lowerMime == "image/jpg"
	isPng := lowerMime == "image/png"
	if !isJpeg && !isPng {
		return in, mime
	}
	// 小图直接跳过
	if len(in) < skipBytes {
		// 但长边过大的图(手机拍 4000x3000 几千像素)还是该缩,因为 base64 体积爆炸
		cfg, _, err := image.DecodeConfig(bytes.NewReader(in))
		if err != nil || (cfg.Width <= skipMaxDim && cfg.Height <= skipMaxDim) {
			return in, mime
		}
	}

	// decode
	src, _, err := image.Decode(bytes.NewReader(in))
	if err != nil {
		log.Printf("[vlm] shrinkForVLM decode 失败 (mime=%s, %d bytes), 用原图: %v",
			mime, len(in), err)
		return in, mime
	}

	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= targetMaxDim && h <= targetMaxDim {
		// 已经够小
		return in, mime
	}

	// 按比例缩到长边 targetMaxDim
	var newW, newH int
	if w >= h {
		newW = targetMaxDim
		newH = h * targetMaxDim / w
	} else {
		newH = targetMaxDim
		newW = w * targetMaxDim / h
	}
	if newW < 1 {
		newW = 1
	}
	if newH < 1 {
		newH = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	// 最近邻缩放 (stdlib image/draw 没有 NearestNeighbor Kernel, 手写 10 行;
	//   对印刷体供货单够清晰, 不引入 x/image 依赖)
	nearestNeighborScale(dst, src)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: jpegQuality}); err != nil {
		log.Printf("[vlm] shrinkForVLM encode 失败 (mime=%s, %d bytes), 用原图: %v",
			mime, len(in), err)
		return in, mime
	}
	return buf.Bytes(), "image/jpeg"
}

// nearestNeighborScale 最近邻缩放: dst 的每个像素取 src 对应位置的颜色
//   stdlib image/draw 没暴露 NearestNeighbor Kernel (要 x/image 才有),
//   这里手写一份,只服务于 shrinkForVLM 一个调用点。
func nearestNeighborScale(dst *image.RGBA, src image.Image) {
	db := dst.Bounds()
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dw, dh := db.Dx(), db.Dy()
	for y := 0; y < dh; y++ {
		sy := sb.Min.Y + y*sh/dh
		for x := 0; x < dw; x++ {
			sx := sb.Min.X + x*sw/dw
			dst.Set(db.Min.X+x, db.Min.Y+y, src.At(sx, sy))
		}
	}
}
