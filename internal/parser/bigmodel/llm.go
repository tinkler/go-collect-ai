package bigmodel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
//
//	model: "glm-4-flash" / "glm-4-plus" / "" (回退 glm-4-flash)
func (c *LlmClient) ChatCompletion(sysPrompt, userPrompt, model string) (string, error) {
	payload := map[string]any{
		"model": resolveLlmModel(model),
		"messages": []map[string]string{
			{"role": "system", "content": sysPrompt},
			{"role": "user", "content": userPrompt},
		},
		"temperature":     0.1,
		"top_p":           0.7,
		"max_tokens":      8192,
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

// ErrTruncated 响应被截断 (max_tokens 不够),ParseLlmJson 无法挽救。
// caller 应该: 切更小图重试 / 换 glm-4v-plus / 调低单图密度
var ErrTruncated = fmt.Errorf("LLM 响应被截断 (max_tokens 触顶)")

// ParseLlmJson 解析 LLM 返回, 跳过 type=skip, 客户端二次过滤
//
// Phase A (2026-09-02): 旧的 DefaultSystemPrompt / DefaultInventoryPrompt / DefaultPurchasePrompt
// 全部删掉,迁到 skills/ocr-purchase/SKILL.md 由 ParserOrchestrator 渲染调用。
//
// 2026-09-04: 增加截断检测。被截断时(fence 闭合缺 + 没有 ] 收尾),
//   - 尝试截断到最后一个 `}` 之前,挽救部分 rows
//   - 挽救失败则返回 ErrTruncated(让 caller 知道是 token 上限,不是格式问题)
func ParseLlmJson(msg string) ([]model.ParsedOcrRow, error) {
	msg = strings.TrimSpace(msg)
	fence := regexp.MustCompile("```(?:json)?\\s*(.*?)\\s*```")
	if m := fence.FindStringSubmatch(msg); m != nil {
		msg = m[1]
	}
	msg = strings.TrimSpace(msg)

	// 2026-09-04 截断检测: 没有闭合 ] 通常是 max_tokens 触顶
	//   正常 JSON 一定以 } 或 ] 结尾,否则就是被截断
	looksTruncated := !strings.HasSuffix(msg, "}") && !strings.HasSuffix(msg, "]")

	var token any
	if err := json.Unmarshal([]byte(msg), &token); err != nil {
		// 兜底: 截取 [] 段
		start := strings.Index(msg, "[")
		end := strings.LastIndex(msg, "]")
		if start < 0 || end <= start {
			// 截断场景:尝试截到最后一个 } 之前,挽救完整行
			if looksTruncated {
				if recovered, ok := recoverTruncatedRows(msg); ok {
					log.Printf("[ParseLlmJson] 截断响应挽救成功, 恢复 %d 行", len(recovered))
					return recovered, nil
				}
			}
			if looksTruncated {
				return nil, fmt.Errorf("%w: %s", ErrTruncated, truncate(msg, 200))
			}
			return nil, fmt.Errorf("LLM 返回非 JSON: %s", truncate(msg, 200))
		}
		token = nil
		if err2 := json.Unmarshal([]byte(msg[start:end+1]), &token); err2 != nil {
			if looksTruncated {
				return nil, fmt.Errorf("%w: %s; inner_err=%v", ErrTruncated, truncate(msg, 200), err2)
			}
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

	return parseRowsArray(arr), nil
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

// recoverTruncatedRows 截断响应挽救:用 brace 深度匹配找出所有完整 row
//   - 典型场景: VLM 在某个 row 中途被 max_tokens 截断,前面的 row 完整
//   - 策略: 从 array 起点 `[` 开始扫描,逐字符追踪 `{`/`}` 嵌套深度
//     深度回到 0 时,得到一个完整 row,单独 parse 后累积
//   - 限制: 只能挽救"已闭合"的 row;被截断的最后一 row 必然丢
func recoverTruncatedRows(msg string) ([]model.ParsedOcrRow, bool) {
	// 找 array 起点(优先 ["rows": [ 之后,否则就第一个 [)
	arrayStart := -1
	if i := strings.Index(msg, `"rows"`); i >= 0 {
		j := strings.Index(msg[i:], "[")
		if j >= 0 {
			arrayStart = i + j
		}
	}
	if arrayStart < 0 {
		arrayStart = strings.Index(msg, "[")
	}
	if arrayStart < 0 {
		return nil, false
	}

	// brace 深度匹配,收集所有完整 row
	depth := 0
	rowStart := -1
	var rows []map[string]any
	for i := arrayStart; i < len(msg); i++ {
		c := msg[i]
		if c == '{' {
			if depth == 0 {
				rowStart = i
			}
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 && rowStart >= 0 {
				rowJSON := msg[rowStart : i+1]
				var row map[string]any
				if err := json.Unmarshal([]byte(rowJSON), &row); err == nil {
					rows = append(rows, row)
				}
				rowStart = -1
			}
		} else if depth < 0 {
			// array 闭合 ] 已经过了,后面都是 wrapper 闭合
			break
		}
	}
	if len(rows) == 0 {
		return nil, false
	}
	// 复用正常解析流程(过滤 type=skip / header / subtitle 等)
	return parseRowsArray(rows), true
}

// parseRowsArray 把 arr 转成 ParsedOcrRow(供 recoverTruncatedRows 复用)
//
// 2026-09-04 强化:
//   - barcode 不强制 13 位, 支持 6-14 位 (8/10/12/13/14 都常见)
//   - qty 允许小数 (0.5/1.5/2.5 等), 不再过滤 0 (0 是合法采购数 0)
//   - 客户端兜底: 去 barcode 里的非数字字符 (空格/横线/引号/字母)
func parseRowsArray(arr []map[string]any) []model.ParsedOcrRow {
	out := make([]model.ParsedOcrRow, 0, len(arr))
	for _, o := range arr {
		typ, _ := o["type"].(string)
		if strings.ToLower(typ) == "skip" {
			continue
		}
		barcode, _ := o["barcode"].(string)
		name, _ := o["name"].(string)
		// 2026-09-04 修复: GLM-4V 偶发把 barcode 输出成脏格式
		//   - "1234 5678 9012" (含空格) → "123456789012"
		//   - "1234-5678-9012" (含横线) → "123456789012"
		//   - "0 1234567890123" (前缀 0 + 空格) → "0123456789012"
		//   - 任意位数 (8/10/12/13/14) 都保留, 不强制 13 位
		//   - 长度 < 6 或 > 14 → 返空 (无效, 跳过)
		barcode = normalizeBarcode(barcode)
		var qtyRaw string
		switch v := o["qty"].(type) {
		case float64:
			// 2026-09-04: 保留小数, 12.5 写成 "12.5" 而不是 int(12.5) = 12
			qtyRaw = formatFloatQty(v)
		case string:
			qtyRaw = v
		}
		// 2026-09-04 修复: qty 可以是 0 / 小数 / 负数 (parseQty 已经能 parse)
		//   之前过滤 0 是错的 (0 是合法采购数 0, 商家偶尔会写)
		//   之前只支持整数, 现在 parseQty 走 string 分支也支持小数
		var qty *int
		if qtyRaw != "" {
			if v, ok := parseQty(qtyRaw); ok {
				qty = &v
			}
		}
		// 客户端硬过滤(同 ParseLlmJson)
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
	return out
}

// formatFloatQty 把 float64 格式化为最短合理字符串
//   - 12.0 → "12"
//   - 12.5 → "12.5"
//   - 0.5 → "0.5"
func formatFloatQty(v float64) string {
	if v == float64(int(v)) {
		return strconv.Itoa(int(v))
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// normalizeBarcode 客户端兜底:把脏 barcode 清洗成 6-14 位纯数字
//
//	规则:
//	  1) trim
//	  2) 去所有非数字字符 (空格/横线/字母/小数点/引号/中文逗号 等)
//	  3) 长度 6-14 → 返清洗后字符串
//	  4) 其他长度 → 返空 (无效, matcher 走 L2/L3)
//
//	不强制 13 位, 商超供货单常见 8/10/12/13/14 位
func normalizeBarcode(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	cleaned := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			cleaned = append(cleaned, c)
		}
	}
	if len(cleaned) < 6 || len(cleaned) > 14 {
		return ""
	}
	return string(cleaned)
}
