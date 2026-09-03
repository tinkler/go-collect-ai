// Package parser - prompt 模板渲染 (Phase A, 2026-09-02)
//
// 取代旧 llm.go:DefaultPurchasePrompt (硬编码 ~200 行 prompt)
// 新做法:
//   1. skills/ocr-purchase/SKILL.md 正文 = 模板 (含 4 个变量占位符)
//   2. Go 端 renderPrompt 替换 4 个变量, 不引入模板引擎 (避免依赖)
//
// 变量:
//   {supplier}          → 供应商名
//   {sku_hints_json}    → JSON 字符串(barcodes/names/units/...)
//   {strategy_body}     → 供应商特定策略正文(可空)
//   {prompt_overlay}    → 供应商特定追加提示(可空)
package parser

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tinkler/collect-ai/internal/model"
)

// PromptVars 4 个变量
type PromptVars struct {
	Supplier      string
	SkuHints      map[string]any
	StrategyBody  string
	PromptOverlay string
}

// renderPrompt 简单 string.Replace 替换 4 个变量
//   - 用 strings.ReplaceAll 而非 template.TextTemplate: 避免模板注入 + 简单可控
//   - sku_hints_json 自动 marshal 成 JSON
//   - 空 strategy_body / prompt_overlay 也安全(替换为空字符串)
func renderPrompt(body string, v PromptVars) (string, error) {
	hintsJSON, err := json.Marshal(v.SkuHints)
	if err != nil {
		return "", fmt.Errorf("sku_hints 序列化失败: %w", err)
	}

	out := body
	out = strings.ReplaceAll(out, "{supplier}", v.Supplier)
	out = strings.ReplaceAll(out, "{sku_hints_json}", string(hintsJSON))
	out = strings.ReplaceAll(out, "{strategy_body}", v.StrategyBody)
	out = strings.ReplaceAll(out, "{prompt_overlay}", v.PromptOverlay)
	return out, nil
}

// buildUserPrompt 拼 user prompt (OCR 文字行 → LLM 输入)
//   - 复用旧 parser.go:138 的格式
func buildUserPrompt(lines []model.OcrLine) string {
	var sb strings.Builder
	for i, l := range lines {
		words := make([]string, 0, len(l.Blocks))
		for _, b := range l.Blocks {
			words = append(words, b.Words)
		}
		fmt.Fprintf(&sb, "[行%d] top=%d  text=\"%s\"\n", i+1, l.Top, strings.Join(words, " "))
	}
	return fmt.Sprintf("OCR 识别的文本行如下 (%d 行):\n%s\n\n请按规则解析为 JSON 数组:", len(lines), sb.String())
}
