// Package parser - 启发式解析 (Phase A, 2026-09-02)
//
// 取代旧 Parser.Parser + ParseImageBytes/ParseFile/parseAfterOcr/parseLines/buildUserPrompt
// 保留:
//   - heuristicParse: LLM 不可用 / 失败 / 手写供应商时的兜底
//   - 旧 toSkuRecords 删除 (handler 已用业务字段直接返 model.SkuRecord)
//
// 关键的"业务判断"(在 skill 里):
//   - 拆行规则 → skills/ocr-purchase/SKILL.md 步骤 1
//   - 规格 vs 数量 → skills/ocr-purchase/SKILL.md 步骤 3
//   - 数量识别 → skills/ocr-purchase/SKILL.md 步骤 2
//
// heuristicParse 只做最朴素的兜底:
//   - 6-14 位纯数字 → barcode
//   - 行内最大数字 → qty
//   - 其它 → name
package parser

import (
	"fmt"
	"log"
	"strings"

	"github.com/tinkler/collect-ai/internal/model"
)

// heuristicParse LLM 不可用 / 失败 / 手写供应商时的兜底
//   - 6-14 位纯数字 → barcode
//   - 短数字 → qty
//   - 其它文本 → name
//   跟旧 parser.go:151 等价 (Phase A 保留, 无变化)
func heuristicParse(lines []model.OcrLine) []model.ParsedOcrRow {
	out := make([]model.ParsedOcrRow, 0, len(lines))
	for _, l := range lines {
		var barcode string
		var nameParts []string
		for _, b := range l.Blocks {
			t := clean(b.Words)
			if barcode == "" && looksLikeBarcode(t) {
				barcode = t
			} else {
				nameParts = append(nameParts, t)
			}
		}
		if barcode == "" && len(nameParts) == 0 {
			continue
		}
		qty := ParseQty(l)
		row := model.ParsedOcrRow{
			Barcode: barcode,
			Name:    strings.Join(nameParts, " "),
		}
		if qty != nil {
			row.Qty = qty
			row.QtyRaw = fmt.Sprintf("%d", *qty)
		}
		out = append(out, row)
	}
	log.Printf("[heuristic] parsed %d lines", len(out))
	return out
}
