package parser

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/tinkler/collect-ai/internal/model"
	"github.com/tinkler/collect-ai/internal/parser/agent"
	"github.com/tinkler/collect-ai/internal/parser/bigmodel"
	"github.com/tinkler/collect-ai/internal/parser/matcher"
)

// Parser OCR + LLM + 匹配 完整流程
type Parser struct {
	ocr    *bigmodel.OcrClient
	llm    *bigmodel.LlmClient
	agt    *agent.Client
	useLlm bool
	fuzzy  int
}

func New(ocr *bigmodel.OcrClient, llm *bigmodel.LlmClient, agt *agent.Client, useLlm bool, fuzzy int) *Parser {
	return &Parser{ocr: ocr, llm: llm, agt: agt, useLlm: useLlm, fuzzy: fuzzy}
}

// ParseImageBytes 收图 bytes → 返回已匹配的 SkuRow 列表
//   supplier:     必填, 用于加载 SKU 库
//   mode:         "inventory" (默认) / "purchase"
//   customPrompt: 可选, 模板自带 LLM 提示词 (空 = 用 default prompt)
//   ocrModel:     BigModel OCR tool_type (空 = client 兜底 "hand_write")
//   llmModel:     BigModel LLM model (空 = client 兜底 "glm-4-flash")
//   → 两个 model 字段都允许 per-template 覆盖, 解析时由 handler 按 template 决议后传入
func (p *Parser) ParseImageBytes(ctx context.Context, imgBytes []byte, fileName, supplier, mode, customPrompt, ocrModel, llmModel string) ([]model.SkuRow, []model.OcrLine, []byte, error) {
	if p == nil {
		return nil, nil, nil, fmt.Errorf("parser 未初始化")
	}
	if supplier == "" {
		return nil, nil, nil, fmt.Errorf("supplier 必填")
	}
	// 1) OCR
	raw, err := p.ocr.RecognizeBytes(fileName, imgBytes, ocrModel)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("OCR 失败: %w", err)
	}
	return p.parseAfterOcr(ctx, raw, imgBytes, supplier, mode, customPrompt, llmModel)
}

// ParseFile 同上, 但从文件读
func (p *Parser) ParseFile(ctx context.Context, path, supplier, mode, customPrompt, ocrModel, llmModel string) ([]model.SkuRow, []model.OcrLine, []byte, error) {
	imgBytes, err := readFileBytes(path)
	if err != nil {
		return nil, nil, nil, err
	}
	raw, err := p.ocr.RecognizeFile(path, ocrModel)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("OCR 失败: %w", err)
	}
	return p.parseAfterOcr(ctx, raw, imgBytes, supplier, mode, customPrompt, llmModel)
}

func (p *Parser) parseAfterOcr(ctx context.Context, rawBlocks []model.OcrWordBlock, imgBytes []byte, supplier, mode, customPrompt, llmModel string) ([]model.SkuRow, []model.OcrLine, []byte, error) {
	// 2) 按 top 分行 + 拆合并行
	lines := ParseOcrResponse(rawBlocks)
	log.Printf("[parser] OCR → %d 行", len(lines))

	// 3) 解析行 (LLM 优先, 失败回退启发式)
	parsed, err := p.parseLines(ctx, lines, mode, customPrompt, llmModel)
	if err != nil {
		return nil, lines, imgBytes, fmt.Errorf("解析行失败: %w", err)
	}
	log.Printf("[parser] 解析 → %d 条 (raw %d 行)", len(parsed), len(lines))

	// 4) 加载供应商 SKU
	skus, err := p.agt.LoadSupplierSkus(supplier, 5000)
	if err != nil {
		return nil, lines, imgBytes, fmt.Errorf("加载 SKU 失败: %w", err)
	}
	log.Printf("[parser] 供应商 [%s] → %d 条 SKU", supplier, len(skus))

	// 5) 级联匹配
	m := matcher.New(skus, p.fuzzy)
	rows := make([]model.SkuRow, 0, len(parsed))
	for i, ocr := range parsed {
		rows = append(rows, m.Match(ocr, i+1))
	}

	// 6) 盘点模式: 重算 StockDiff / StockMismatch
	if mode == string(model.ModeInventory) {
		for i := range rows {
			if rows[i].StockQty != nil && rows[i].Qty != nil {
				diff := float64(*rows[i].Qty) - *rows[i].StockQty
				rows[i].StockDiff = &diff
				rows[i].StockMismatch = diff != 0
				if rows[i].StockMismatch && rows[i].Status == "OK" {
					rows[i].Status = "盘存差异"
				}
			}
		}
	}
	return rows, lines, imgBytes, nil
}

func (p *Parser) parseLines(ctx context.Context, lines []model.OcrLine, mode, customPrompt, llmModel string) ([]model.ParsedOcrRow, error) {
	if !p.useLlm {
		return heuristicParse(lines), nil
	}
	modeEnum := model.ModeInventory
	if mode == string(model.ModePurchase) {
		modeEnum = model.ModePurchase
	}
	sysPrompt := customPrompt
	if sysPrompt == "" {
		sysPrompt = bigmodel.DefaultSystemPrompt(modeEnum)
	}
	userPrompt := buildUserPrompt(lines)
	content, err := p.llm.ChatCompletion(sysPrompt, userPrompt, llmModel)
	if err != nil {
		log.Printf("[parser] LLM 失败, fallback 启发式: %v", err)
		return heuristicParse(lines), nil
	}
	// 调试: dump LLM 原始输出前 500 字符
	preview := content
	if len(preview) > 500 {
		preview = preview[:500] + "..."
	}
	log.Printf("[parser] LLM 输出: %s", preview)
	rows, err := bigmodel.ParseLlmJson(content)
	if err != nil {
		log.Printf("[parser] LLM JSON 解析失败, fallback 启发式: %v", err)
		return heuristicParse(lines), nil
	}
	return rows, nil
}

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

// heuristicParse LLM 不可用时的兜底: 6-14 位纯数字 → barcode, 短数字 → qty
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
	return out
}

func readFileBytes(path string) ([]byte, error) {
	return osReadFile(path)
}
