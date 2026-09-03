package bigmodel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tinkler/collect-ai/internal/model"
)

const chatEndpoint = "https://open.bigmodel.cn/api/paas/v4/chat/completions"

// LlmClient BigModel GLM-4 chat completions
//   - model 是每次调用传的, 不存在 client 上, 便于 per-template 切换
//   - 合法值: "glm-4-flash" / "glm-4-plus" 等
//   - 空字符串时 client 自动回退到 "glm-4-flash"
type LlmClient struct {
	apiKey  string
	baseURL string
	timeout time.Duration
}

func NewLlmClient(apiKey, baseURL string, timeoutSec int) *LlmClient {
	if baseURL == "" {
		baseURL = "https://open.bigmodel.cn/api/paas/v4"
	}
	return &LlmClient{
		apiKey:  apiKey,
		baseURL: baseURL,
		timeout: time.Duration(timeoutSec) * time.Second,
	}
}

// resolveLlmModel 空值回退到 glm-4-flash
func resolveLlmModel(model string) string {
	if model == "" {
		return "glm-4-flash"
	}
	return model
}

// ChatCompletion 调 LLM, 返回 choices[0].message.content
//   model: "glm-4-flash" / "glm-4-plus" / "" (回退 glm-4-flash)
func (c *LlmClient) ChatCompletion(sysPrompt, userPrompt, model string) (string, error) {
	payload := map[string]any{
		"model": resolveLlmModel(model),
		"messages": []map[string]string{
			{"role": "system", "content": sysPrompt},
			{"role": "user", "content": userPrompt},
		},
		"temperature":    0.1,
		"top_p":          0.7,
		"max_tokens":     8192,
		"response_format": map[string]string{"type": "json_object"},
	}
	bs, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", c.baseURL+"/chat/completions", bytes.NewReader(bs))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{Timeout: c.timeout}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	bodyBs, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("LLM HTTP %d: %s", resp.StatusCode, truncate(string(bodyBs), 400))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err = json.Unmarshal(bodyBs, &parsed); err != nil {
		return "", err
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("LLM 无 choices: %s", truncate(string(bodyBs), 200))
	}
	return parsed.Choices[0].Message.Content, nil
}

// ParseLlmJson 解析 LLM 返回, 跳过 type=skip, 客户端二次过滤
//
// Phase A (2026-09-02): 旧的 DefaultSystemPrompt / DefaultInventoryPrompt / DefaultPurchasePrompt
// 全部删掉,迁到 skills/ocr-purchase/SKILL.md 由 ParserOrchestrator 渲染调用。
func ParseLlmJson(msg string) ([]model.ParsedOcrRow, error) {
	msg = strings.TrimSpace(msg)
	fence := regexp.MustCompile("```(?:json)?\\s*(.*?)\\s*```")
	if m := fence.FindStringSubmatch(msg); m != nil {
		msg = m[1]
	}

	var token any
	if err := json.Unmarshal([]byte(msg), &token); err != nil {
		// 兜底: 截取 [] 段
		start := strings.Index(msg, "[")
		end := strings.LastIndex(msg, "]")
		if start < 0 || end <= start {
			return nil, fmt.Errorf("LLM 返回非 JSON: %s", truncate(msg, 200))
		}
		token = nil
		if err2 := json.Unmarshal([]byte(msg[start:end+1]), &token); err2 != nil {
			return nil, fmt.Errorf("LLM JSON parse 失败: %w; body=%s", err2, truncate(msg, 200))
		}
	}

	var arr []map[string]any
	switch t := token.(type) {
	case map[string]any:
		// {rows:[...]} 包装
		for _, k := range []string{"rows", "items", "data", "result"} {
			if v, ok := t[k]; ok {
				if ja, ok := v.([]any); ok {
					for _, item := range ja {
						if m, ok := item.(map[string]any); ok {
							arr = append(arr, m)
						}
					}
				}
				break
			}
		}
	case []any:
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				arr = append(arr, m)
			}
		}
	}
	if arr == nil {
		return nil, fmt.Errorf("LLM JSON 没有数组字段: %s", truncate(msg, 200))
	}

	out := make([]model.ParsedOcrRow, 0, len(arr))
	for _, o := range arr {
		typ, _ := o["type"].(string)
		if strings.ToLower(typ) == "skip" {
			continue
		}
		barcode, _ := o["barcode"].(string)
		name, _ := o["name"].(string)
		// qty 可能是 number (LLM) 或 string (兼容)
		var qtyRaw string
		switch v := o["qty"].(type) {
		case float64:
			qtyRaw = strconv.Itoa(int(v))
		case string:
			qtyRaw = v
		}
		if name == "" {
			name = ""
		}
		var qty *int
		if qtyRaw != "" {
			if v, ok := parseQty(qtyRaw); ok {
				qty = &v
			}
		}
		// 客户端二次过滤
		if looksLikeHeader(name) {
			continue
		}
		if looksLikeIsolatedUnit(name) {
			continue
		}
		if looksLikeSubtitle(name) {
			continue
		}
		if looksLikeSignature(name) {
			continue
		}
		if containsMultipleBarcodes(name, barcode) {
			continue
		}
		if name == "" && barcode == "" && qty == nil {
			continue
		}
		out = append(out, model.ParsedOcrRow{Barcode: barcode, Name: name, QtyRaw: qtyRaw, Qty: qty})
	}
	return out, nil
}

// parseQty 解析 "12" / "12.0" / "12件" / "3.5" -> int
func parseQty(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	// 提取首个数字 (整数或小数)
	re := regexp.MustCompile(`\d+(\.\d+)?`)
	m := re.FindString(s)
	if m == "" {
		return 0, false
	}
	var d float64
	_, err := fmt.Sscanf(m, "%f", &d)
	if err != nil {
		return 0, false
	}
	return int(d + 0.5), true
}

// ===== 客户端硬规则 (不依赖 LLM) =====

var headerKeywords = []string{"行号", "条码", "商品名称", "规格", "单位", "盘点数", "抽盘数", "进价",
	"数量", "抽盘", "单价", "金额", "采购数量", "类别", "名称"}

var subtitleKeywords = []string{"堆头", "堆", "区", "类", "饮料区", "粮油类", "酒水类", "日化区", "冷藏区",
	"冷冻区", "调味区", "零食品", "纸品区", "洗化区", "饮料柜", "酒水柜", "蒙牛", "伊利", "加多宝"}

var signatureKeywords = []string{"初盘人", "复盘人", "抽盘人", "盘点人", "签名", "日期", "经办人", "审核人"}

var isolatedUnits = []string{"件", "排", "箱", "盒", "袋", "桶", "包"}

func looksLikeHeader(name string) bool {
	if name == "" {
		return false
	}
	hits := 0
	for _, k := range headerKeywords {
		if strings.Contains(name, k) {
			hits++
		}
	}
	return hits >= 3
}

func looksLikeSubtitle(name string) bool {
	if name == "" {
		return false
	}
	t := strings.TrimSpace(name)
	if t == "" || len([]rune(t)) > 10 {
		return false
	}
	hasDigit6 := regexp.MustCompile(`\d{6,}`).MatchString(t)
	if hasDigit6 {
		return false
	}
	for _, k := range subtitleKeywords {
		if t == k || strings.Contains(t, k) {
			return true
		}
	}
	return false
}

func looksLikeSignature(name string) bool {
	if name == "" {
		return false
	}
	for _, k := range signatureKeywords {
		if strings.Contains(name, k) {
			return true
		}
	}
	return false
}

func looksLikeIsolatedUnit(name string) bool {
	if name == "" {
		return false
	}
	t := strings.TrimSpace(name)
	if t == "" || len([]rune(t)) > 3 {
		return false
	}
	for _, u := range isolatedUnits {
		if t == u {
			return true
		}
	}
	return false
}

func containsMultipleBarcodes(name, barcode string) bool {
	text := barcode + " " + name
	matches := regexp.MustCompile(`\b\d{13}\b`).FindAllString(text, -1)
	return len(matches) >= 2
}
