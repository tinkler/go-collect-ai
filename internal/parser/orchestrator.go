// Package parser - Orchestrator 双引擎联合解析 (2026-09-04 重构, 对齐 tin-nova)
//
//	旧版 (2026-09-04 之前): GLM-4V 多模态直读图 → JSON → SkuMatcher L1/L2/L3 匹配回填
//	  问题: 1) buildGenericHints 塞 800 name + 200 barcode 进 prompt, 高消耗 token
//	       2) SkuMatcher L1/L2/L3 需加载 5000 SKU 全库匹配
//	       3) VLM 单引擎, 复杂表格误读无兜底
//
//	新版 (2026-09-04 之后, 对齐 F:\go\src\github.com\tinkler\tin-nova):
//	  引擎1: 智谱 prime-sync 文件解析 (印刷体/表格 OCR) → 纯文本
//	  引擎2: DeepSeek 视觉模型 (deepseek-v4-flash-vision-exp) 收图 + OCR 文本参考
//	         → 结构化 JSON {supplier_name, items:[{barcode,name,qty,price}]}
//	  不做: SKU hints 注入 (数据库增强识别) / SkuMatcher L1~L3 匹配
//	  语义: 所有行直接当新 SKU, matched_* 属性一律置空 (IsNew=true, status=新品)
//
// 关键合规点 (AGENTS.md §1/§4):
//   - 0 个业务判断在 Go 里
//   - 0 个 prompt 模板在 Go 里 (在 skills/ocr-purchase/SKILL.md)
package parser

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/tinkler/collect-ai/internal/agent/skill"
	"github.com/tinkler/collect-ai/internal/model"
	"github.com/tinkler/collect-ai/internal/parser/bigmodel"
	"github.com/tinkler/collect-ai/internal/parser/glmocr"

	tmodel "trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/model/openai"
)

// ocrTextMaxLen 智谱 OCR 文本注入 prompt 的长度上限 (对齐 tin-nova 6000 截断)
const ocrTextMaxLen = 6000

// Orchestrator 双引擎协调: 智谱 prime-sync OCR + DeepSeek 视觉
type Orchestrator struct {
	// ocr 引擎1: 智谱 prime-sync 文件解析 (印刷体/表格 → 纯文本)
	ocr *glmocr.Client
	// vision 引擎2: DeepSeek 视觉模型 (图 + OCR 文本 → 结构化 JSON)
	vision tmodel.Model
	// skills: 读 skills/ocr-purchase/SKILL.md 当 prompt 模板 (AGENTS.md §1)
	skills *skill.Store
}

func NewOrchestrator(
	ocr *glmocr.Client,
	dsAPIKey, dsBaseURL, dsModel string,
	skills *skill.Store,
) (*Orchestrator, error) {
	if ocr == nil {
		return nil, fmt.Errorf("orchestrator: glmocr client 必填")
	}
	if strings.TrimSpace(dsAPIKey) == "" {
		return nil, fmt.Errorf("orchestrator: DEEPSEEK_API_KEY 必填 (双引擎引擎2)")
	}
	if skills == nil {
		return nil, fmt.Errorf("orchestrator: skill store 必填")
	}
	opts := []openai.Option{
		openai.WithAPIKey(dsAPIKey),
		openai.WithVariant(openai.VariantDeepSeek),
		openai.WithTextOnlyMessageContent(false), // 允许 image content part
	}
	if strings.TrimSpace(dsBaseURL) != "" {
		opts = append(opts, openai.WithBaseURL(dsBaseURL))
	}
	if dsModel == "" {
		dsModel = "deepseek-v4-flash-vision-exp" // 对齐 tin-nova 默认
	}
	return &Orchestrator{
		ocr:    ocr,
		vision: openai.New(dsModel, opts...),
		skills: skills,
	}, nil
}

// ParseResult 解析结果
//
//	2026-09-04 双引擎重构: strategy / handwrite / generic 路径全部移除,
//	StrategyVersion 恒为 0 (字段保留兼容 handler, 不再查 strategy 表)
type ParseResult struct {
	Rows            []model.SkuRow
	StrategyVersion int
}

// Parse 收图 bytes → 双引擎解析为 SkuRow (全部按新 SKU 返回)
//
//	supplier: 必填
//	fileName: 推断 mime / file_type
func (o *Orchestrator) Parse(ctx context.Context, imgBytes []byte, fileName, supplier string) (*ParseResult, error) {
	if len(imgBytes) == 0 {
		return nil, fmt.Errorf("imgBytes 不能为空")
	}
	if supplier == "" {
		return nil, fmt.Errorf("supplier 必填")
	}

	log.Printf("[orch] 双引擎启动 (supplier=%s, img=%d bytes)", supplier, len(imgBytes))

	// 1) 引擎1: 智谱 prime-sync 文件解析 (印刷体/表格 → 纯文本)
	//    失败不阻断: 引擎2 纯视觉照样能跑, 只是少了参考文本
	ocrText := ""
	ocrRes, err := o.ocr.FileParserSync(ctx, &glmocr.FileParserSyncRequest{
		FileData: imgBytes,
		FileName: fileName,
	})
	if err != nil {
		log.Printf("[orch] ⚠️ 引擎1 FileParserSync 失败, 引擎2 纯视觉跑: %v", err)
	} else {
		ocrText = strings.TrimSpace(ocrRes.Content)
		log.Printf("[orch] 引擎1 prime-sync OK: content_len=%d status=%s", len(ocrText), ocrRes.Status)
	}
	if len(ocrText) > ocrTextMaxLen {
		ocrText = ocrText[:ocrTextMaxLen] + "...(truncated)"
	}
	if ocrText == "" {
		ocrText = "(引擎1无输出,以你的视觉识别为准)"
	}

	// 2) 读 ocr-purchase skill body 当 prompt 模板 ({supplier} / {ocr_text} 注入)
	sk, ok := o.skills.Get("ocr-purchase")
	if !ok {
		return nil, fmt.Errorf("skill 'ocr-purchase' 未加载,检查 skills/ 目录")
	}
	prompt, err := renderPrompt(sk.Body, PromptVars{Supplier: supplier, OcrText: ocrText})
	if err != nil {
		return nil, fmt.Errorf("render prompt 失败: %w", err)
	}

	// 3) 引擎2: DeepSeek 视觉模型 (图 + prompt → 结构化 JSON)
	content, err := o.visionChat(ctx, prompt, imgBytes, fileName)
	if err != nil {
		return nil, fmt.Errorf("引擎2 DeepSeek 视觉失败: %w", err)
	}
	preview := content
	if len(preview) > 500 {
		preview = preview[:500] + "..."
	}
	log.Printf("[orch] 引擎2 响应 (前 500): %s", preview)

	// 4) 解析 JSON (复用 ParseLlmJson: 截断挽救 + header/subtitle 过滤)
	parsed, err := bigmodel.ParseLlmJson(content)
	if err != nil {
		return nil, fmt.Errorf("引擎2 JSON 解析失败: %w", err)
	}
	log.Printf("[orch] 双引擎解析 → %d 条 (supplier=%s)", len(parsed), supplier)

	// 5) 转 SkuRow: 不做任何 SKU 匹配, 全部按新 SKU
	//    matched_* 属性一律置空 (用户 2026-09-04 决策)
	res := &ParseResult{Rows: make([]model.SkuRow, 0, len(parsed))}
	for i, p := range parsed {
		res.Rows = append(res.Rows, model.SkuRow{
			Seq:        i + 1, // handler 会按多图累加覆盖
			RawBarcode: p.Barcode,
			RawName:    p.Name,
			RawQty:     p.QtyRaw,
			Qty:        p.Qty,
			UnitPrice:  p.Price,
			Status:     "新品",
			IsNew:      true,
		})
	}
	return res, nil
}

// visionChat 调 DeepSeek 视觉模型, 聚合流式/非流式响应为完整文本
func (o *Orchestrator) visionChat(ctx context.Context, prompt string, imgBytes []byte, fileName string) (string, error) {
	msg := tmodel.NewUserMessage(prompt)
	msg.AddImageData(imgBytes, "auto", "jpeg")

	events, err := o.vision.GenerateContent(ctx, &tmodel.Request{
		Messages: []tmodel.Message{msg},
	})
	if err != nil {
		return "", fmt.Errorf("GenerateContent: %w", err)
	}

	var sb strings.Builder
	for ev := range events {
		if ev == nil {
			continue
		}
		if ev.Error != nil {
			return "", fmt.Errorf("vision response error: %s", ev.Error.Message)
		}
		for _, ch := range ev.Choices {
			if ch.Delta.Content != "" {
				sb.WriteString(ch.Delta.Content)
			} else if ch.Message.Content != "" {
				sb.WriteString(ch.Message.Content)
			}
		}
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", fmt.Errorf("vision 模型返回空内容")
	}
	return out, nil
}
