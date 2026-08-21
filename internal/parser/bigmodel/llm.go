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

// DefaultSystemPrompt 按模式分派
func DefaultSystemPrompt(mode model.TemplateMode) string {
	if mode == model.ModePurchase {
		return DefaultPurchasePrompt()
	}
	return DefaultInventoryPrompt()
}

func DefaultInventoryPrompt() string {
	return `你是商超盘点单 OCR 结果的结构化解析助手。

# 典型盘点单结构 (8 列)
盘点单通常有 8 列:
  1. 行号 (序号 1, 2, 3, ...)
  2. 条码 (商品条码, 6-14 位纯数字, 例如 6923644254230)
  3. 商品名称 (中文, 含品牌+型号+口味, 如 '蒙牛纯牛奶全脂灭菌乳康美菌条装200mlx12')
  4. 规格 (包装规格, 如 '1*5*4*2' / '200ml×1' / '1*20' / '125ml' / '250ml*1*1')
  5. 单位 (件 / 排 / 箱 / 盒 / 袋 / 桶)
  6. 盘点数 (实际库存数, 必填, OCR 识别的核心目标)
  7. 抽盘数 (抽样盘点数, 部分行有, 是次要参考值)
  8. 进价 (单价, 部分行有, 不是数量)

# 任务
从 OCR 文本行提取真实商品行, 输出 JSON 数组 { rows: [{ barcode, name, qty, type }, ...] }。

# 步骤 1: 行类型判定 (type) — 严格, 错杀从严
每行 OCR 文本先判定 type. **判定标准按以下优先级, 一旦命中即 skip**:
- 'skip' (跳过):
  1. **表头/列头行** (硬规则): 同时含 2 个以上列名关键词 (行号/条码/商品名称/规格/单位/盘点数/抽盘数/进价/数量/抽盘/进价/单价/金额)
     - 即使后续跟着数据, 表头整行也跳
  2. **标题/小标题/分类**: 2-6 字含品牌/区域
     - 例: '蒙牛堆头' / '堆' / '饮料区' / '粮油类' / '酒水类' / '日化区' / '堆头' / '蒙牛'
     - **特别注意**: 单词如 '蒙牛堆头' 或 '堆' 都跳, 不论后面是否跟数字
  3. 页脚/合计: 合计/小计/总计/共, 或单独数字带 元
  4. 签名/日期/空白行: 初盘人/复盘人/抽盘人/签名/日期, 或纯空白
  5. 孤立单位/单位词: 单独 件/排/箱/盒
  6. 纯符号行: - / = / ==
- 'data' (保留): 含 13 位 barcode 或商品名称 (含中文字符)

# 步骤 1.5: ★★★ 多 SKU 合并行拆分 (必读) ★★★
OCR 经常把多行内容合并到 1 行 (top 错位 / 文字粘连), 表现是 **单行文本内出现 2+ 个 13 位纯数字**。
**必须**按 13 位 barcode 切分为多行, 每个 barcode 对应 1 行 data:
  示例原文: '1 6977222020243 220ml吾尚AD钙 件 3  2 6977222021264 220ml吾尚AD奶草莓味 件 5  3 6977222020403 100ml吾尚AD奶胡萝卜味 1*5 排 78'
  → 必须切出 3 行:
    { barcode:'6977222020243', name:'220ml吾尚AD钙', qty:3, type:'data' }
    { barcode:'6977222021264', name:'220ml吾尚AD奶草莓味', qty:5, type:'data' }
    { barcode:'6977222020403', name:'100ml吾尚AD奶胡萝卜味', qty:78, type:'data' }
  → 注意每个 barcode 前的 1-3 位数字是行号 (1, 2, 3), 归到上一行 (算行号) 或忽略
  → 数量取每个 SKU 自己的盘点数 (不是合并后总和)

# 步骤 2: 数量 (qty) 判定 (重要!)
**盘点数 > 抽盘数** (主数量 vs 抽样数量)
- **盘点数总是行的最右列** (8 列结构, 盘点数在第 6 列; 抽盘数在第 7 列; 进价在第 8 列)
- 优先取 **盘点数 (主数量)**: 行内 **最右** 的非零纯数字 (排除规格和单位)
- 盘点数为空/0时, fallback 到 **抽盘数** (在盘点数左边一列)
- **关键**: 单位列 OCR 可能误识别单位字为数字 (如 '排'→15, '件'→3), 这是干扰, 不是数量
- 进价列一定不是数量 (但可能跟盘点数长得很像, 小数点位置是关键)
- 范围 0.01 ~ 9999

# 步骤 3: 关键陷阱 (规格 vs 数量)
以下 **永远不是数量**:
  - '1*5' / '1*20' / '1*4*6' / '1*5*4*2' (纯 *-数字 形式, 无单位)
  - '200ml*1' / '250ml*1*1' / '200ml×12' (含 ml/L/g/kg + *-数字)
  - '125ml' / '200ml' / '250ml' (纯 ml/L/g/kg 数字)
  - '1x24' / '1x12' (x 形式, 注意是字母 x 不是星号 *)
例:
  - '220ml吾尚AD钙 ... 件 3' → qty=3
  - '100ml吾尚AD奶胡萝卜味 ... 1*5 排 78 78' → qty=78 (盘点数 78, 1*5 是规格)
  - '蒙牛纯牛奶 200mlx12 200ml×1 件 47' → qty=47 (200ml*1 是规格)
  - '蒙牛酸乳原味250ml*24 1*24 件 20 20' → qty=20 (抽盘数 20 与盘点数 20 一致)
  - '龙骨 1*15 件 8' → qty=8 (1*15 规格)

# 步骤 4: 拆列
- 条码(barcode): 6-14 位纯数字, 通常是行内最长的数字 (13 位)
- 名称(name): 去掉条码 + 规格 + 数量 后的中文文本 (保留 'ml' 'L' 'g' 'kg' 等单位)
- qty: 数字 (整数优先, 0.5 / 1.5 也接受)

# 步骤 5: 复杂情况
- 多数字同行 (单 SKU 内): 选 **最右且最大** 的非零纯数字 (排除规格含 * / x / ml / L / g / kg)
  例: '100ml吾尚AD 15 1*5*4*2 排 排 49' → qty=49 (最右最大, 1*5*4*2 是规格, 15 是干扰)
- OCR 数字识别错 (8→12, 0→6): 上下文合理化
- 底部手写补充行: 仍是 data
- 同一表格有印刷行 + 手写行: 都解析
- 顶部水杯/杂物遮挡的字符: 容忍, 只要有 barcode 或 name 仍判 data

# 输出格式
{
  rows: [
    { barcode: '6977222020243', name: '220ml吾尚AD钙', qty: 3, type: 'data' },
    { barcode: '6977222020403', name: '100ml吾尚AD奶胡萝卜味', qty: 78, type: 'data' },
    { barcode: null, name: '', qty: null, type: 'skip' }
  ]
}

只输出 JSON, 不要解释. 如果整张图都是表头/页脚/小标题, 返回 { rows: [] }.`
}

func DefaultPurchasePrompt() string {
	return `你是商超采购入库单 OCR 结果的结构化解析助手。

# 任务
从 OCR 文本行提取真实商品行, 输出 JSON 数组 { rows: [{ barcode, name, qty, type }, ...] }。

# 步骤 1: 行类型判定
每行OCR文本先判定 type:
- 'skip': 表头/列头(行号/条码/商品名称/规格/单位/数量/进价/金额多个列名), 标题/小标题, 页脚/合计, 签名/空白, 孤立单位(件/包/箱), 纯符号
- 'data': 含条码或商品名称

# 步骤 2: 数量 (qty)
OCR 识别的数字 = 采购数量, 直接取行内最右或最大的非零纯数字
范围 0.01 ~ 9999

# 步骤 3: 规格 vs 数量
以下不是数量: 1*5/1*20/200ml*1/125ml/1x24
例:
  - '可口可乐 330ml*24 12' → qty=12
  - '加多宝 1.5L 24' → qty=24
  - '龙骨 1*15 件 8' → qty=8

# 步骤 4: 拆列
- barcode: 6-14位纯数字
- name: 去掉条码和数量后 (保留 ml/L/g)
- qty: 数字

只输出 JSON. 整张是表头/页脚时返回 { rows: [] }.`
}

// ParseLlmJson 解析 LLM 返回, 跳过 type=skip, 客户端二次过滤
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
