// Package parser - prompt 模板渲染 (Phase A, 2026-09-02; 2026-09-04 双引擎改造)
//
// 取代旧 llm.go:DefaultPurchasePrompt (硬编码 ~200 行 prompt)
// 新做法:
//   1. skills/ocr-purchase/SKILL.md 正文 = 模板 (含 2 个变量占位符)
//   2. Go 端 renderPrompt 替换变量, 不引入模板引擎 (避免依赖)
//
// 变量 (2026-09-04 双引擎: 去掉 sku_hints_json / strategy_body / prompt_overlay):
//   {supplier}  → 供应商名
//   {ocr_text}  → 智谱 prime-sync 文件解析出的纯文本 (引擎1, 供引擎2参考)
package parser

import (
	"fmt"
	"strings"
)

// PromptVars 模板变量
type PromptVars struct {
	Supplier string
	OcrText  string
}

// renderPrompt 简单 string.Replace 替换变量
//   - 用 strings.ReplaceAll 而非 template.TextTemplate: 避免模板注入 + 简单可控
func renderPrompt(body string, v PromptVars) (string, error) {
	if v.Supplier == "" {
		return "", fmt.Errorf("supplier 不能为空")
	}
	out := body
	out = strings.ReplaceAll(out, "{supplier}", v.Supplier)
	out = strings.ReplaceAll(out, "{ocr_text}", v.OcrText)
	return out, nil
}
