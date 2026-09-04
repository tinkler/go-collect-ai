// Package parser - Orchestrator 双引擎联合解析 (2026-09-04 重构, 对齐 tin-nova)
//
//	旧版 (2026-09-04 之前): GLM-4V 多模态直读图 → JSON → SkuMatcher L1~L5 匹配回填
//	  问题: 1) buildGenericHints 塞 800 name + 200 barcode 进 prompt, 高消耗 token
//	       2) SkuMatcher L3~L5 模糊/启发式 "修正匹配" (OCR 误读强行挽回), 误报高
//	       3) VLM 单引擎, 复杂表格误读无兜底
//
//	新版 (2026-09-04 之后, 对齐 F:\go\src\github.com\tinkler\tin-nova):
//	  引擎1: 智谱 prime-sync 文件解析 (印刷体/表格 OCR) → 纯文本
//	  引擎2: DeepSeek 视觉模型 (deepseek-v4-flash-vision-exp) 收图 + OCR 文本参考
//	         → 结构化 JSON {supplier_name, items:[{barcode,name,qty,price}]}
//	  不做:  SKU hints 注入 (数据库增强识别) / SkuMatcher L2~L5 修正/模糊匹配
//	  仍做:  L1 barcode 全等 (trim 后精确匹配) 对应回填
//	    → barcode 能对应上供应商商品库, 就填 matched_* + stock_qty + unit (IsNew=false, 已匹配)
//	    → barcode 对应不上才是新品 (IsNew=true, matched_* 置空, status=新品)
//
// 关键合规点 (AGENTS.md §1/§4):
//   - 0 个业务判断在 Go 里 (L1 是纯字典查找, 属于 §1.1 纯算法例外, 非业务判定)
//   - 0 个 prompt 模板在 Go 里 (在 skills/ocr-purchase/SKILL.md)
package parser

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/tinkler/collect-ai/internal/agent/skill"
	"github.com/tinkler/collect-ai/internal/model"
	"github.com/tinkler/collect-ai/internal/parser/bigmodel"
	"github.com/tinkler/collect-ai/internal/parser/glmocr"

	tmodel "trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/model/openai"
)

// ProductSearcher 查询供应商商品库 (解耦 business.Executor, 便于单测 mock)
//
//	返回值字段 (业务名): barcode / product_name / supplier_id / supplier_name / stock_qty / unit
type ProductSearcher interface {
	SearchProducts(supplierKeyword string, limit int) ([]map[string]any, error)
}

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
	// products: L1/L2 直接对应回填用的供应商商品库查询 (不做 L3~L5 修正匹配)
	products ProductSearcher
}

func NewOrchestrator(
	ocr *glmocr.Client,
	dsAPIKey, dsBaseURL, dsModel string,
	skills *skill.Store,
	products ProductSearcher,
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
	if products == nil {
		return nil, fmt.Errorf("orchestrator: ProductSearcher 必填 (L1/L2 对应回填用)")
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
		ocr:      ocr,
		vision:   openai.New(dsModel, opts...),
		skills:   skills,
		products: products,
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

// Parse 收图 bytes → 双引擎解析为 SkuRow (L1 barcode 全等对应回填, 否则新品)
//
//	不做: L2~L5 (name 匹配/模糊/后缀/相似度, 用户 2026-09-04 决策明确删除)
//	仍做: L1 barcode 全等 (trim 后精确) → 对应填 matched_* + stock_qty + unit (已匹配)
//	      barcode 无法对应 → IsNew=true, status=新品, matched_* 置空
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
	t1 := time.Now()
	ocrRes, err := o.ocr.FileParserSync(ctx, &glmocr.FileParserSyncRequest{
		FileData: imgBytes,
		FileName: fileName,
	})
	if err != nil {
		log.Printf("[orch] ⚠️ 引擎1 FileParserSync 失败 (耗时 %s), 引擎2 纯视觉跑: %v", time.Since(t1).Round(time.Millisecond), err)
	} else {
		ocrText = strings.TrimSpace(ocrRes.Content)
		log.Printf("[orch] 引擎1 prime-sync OK (耗时 %s): content_len=%d status=%s",
			time.Since(t1).Round(time.Millisecond), len(ocrText), ocrRes.Status)
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
	t2 := time.Now()
	content, err := o.visionChat(ctx, prompt, imgBytes, fileName)
	if err != nil {
		return nil, fmt.Errorf("引擎2 DeepSeek 视觉失败 (耗时 %s): %w", time.Since(t2).Round(time.Millisecond), err)
	}
	log.Printf("[orch] 引擎2 DeepSeek 视觉 OK (耗时 %s, 响应 %d chars)",
		time.Since(t2).Round(time.Millisecond), len(content))
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

	// 5) 查供应商商品库 → L1 (barcode trim 后全等) 对应回填
	//    不做 L2~L5 (name 匹配/修正匹配, 用户 2026-09-04 决策); 查询失败不阻断, 降级全当新品
	byBarcode := map[string]map[string]any{}
	if skuList, sErr := o.products.SearchProducts(supplier, 5000); sErr != nil {
		log.Printf("[orch] ⚠️ 供应商商品库查询失败 (L1 跳过, 全当新品): %v", sErr)
	} else {
		log.Printf("[orch] 供应商商品库 → %d 条 (L1 barcode 对应, supplier=%s)", len(skuList), supplier)
		for _, sk := range skuList {
			if bc, _ := sk["barcode"].(string); strings.TrimSpace(bc) != "" {
				byBarcode[strings.TrimSpace(bc)] = sk
			}
		}
	}

	res := &ParseResult{Rows: make([]model.SkuRow, 0, len(parsed))}
	newCnt, l1Cnt := 0, 0
	for i, p := range parsed {
		row := model.SkuRow{
			Seq:        i + 1, // handler 会按多图累加覆盖
			RawBarcode: p.Barcode,
			RawName:    p.Name,
			RawQty:     p.QtyRaw,
			Qty:        p.Qty,
			UnitPrice:  p.Price,
			Status:     "新品",
			IsNew:      true,
		}
		// L1: barcode 全等 (trim 后精确)
		if rawBc := strings.TrimSpace(p.Barcode); rawBc != "" {
			if sk, ok := byBarcode[rawBc]; ok {
				fillMatched(&row, sk)
				l1Cnt++
			}
		}
		if row.IsNew {
			newCnt++
		}
		res.Rows = append(res.Rows, row)
	}
	log.Printf("[orch] 对应回填完成 (total=%d, L1=%d, 新品=%d, supplier=%s)",
		len(res.Rows), l1Cnt, newCnt, supplier)
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

// MatchSupplierRows 用 L1 对已有 rows 重新匹配 (供 handler.Rematch 复用)
//
//	保留: RowID / Seq / ImageIndex / Raw* / Qty / UnitPrice / IsDeleted
//	重填: matched_* / StockQty / Unit / Status / IsNew
//	语义: 不做 L2~L5 (name/模糊/后缀/相似度), 仅 barcode 全等直接对应
func (o *Orchestrator) MatchSupplierRows(ctx context.Context, supplier string, rows []model.SkuRow) []model.SkuRow {
	byBarcode := map[string]map[string]any{}
	if skuList, sErr := o.products.SearchProducts(supplier, 5000); sErr != nil {
		log.Printf("[orch] ⚠️ Rematch 供应商商品库查询失败 (降级全当新品): %v", sErr)
	} else {
		log.Printf("[orch] Rematch 商品库 → %d 条 (L1 barcode, supplier=%s)", len(skuList), supplier)
		for _, sk := range skuList {
			if bc, _ := sk["barcode"].(string); strings.TrimSpace(bc) != "" {
				byBarcode[strings.TrimSpace(bc)] = sk
			}
		}
	}
	out := make([]model.SkuRow, 0, len(rows))
	for _, r := range rows {
		row := model.SkuRow{
			RowID:      r.RowID,
			Seq:        r.Seq,
			ImageIndex: r.ImageIndex,
			RawBarcode: r.RawBarcode,
			RawName:    r.RawName,
			RawQty:     r.RawQty,
			Qty:        r.Qty,
			UnitPrice:  r.UnitPrice,
			IsDeleted:  r.IsDeleted,
			Status:     "新品",
			IsNew:      true,
		}
		if rawBc := strings.TrimSpace(r.RawBarcode); rawBc != "" {
			if sk, ok := byBarcode[rawBc]; ok {
				fillMatched(&row, sk)
			}
		}
		out = append(out, row)
	}
	return out
}

// fillMatched 把 SearchProducts 返回的 sku map 字段填进 SkuRow 的 matched_* / 库存 / 单位
//
//	副作用: row.IsNew=false, row.Status="已匹配", 以及各 matched 字段/StockQty/Unit 回填
func fillMatched(row *model.SkuRow, sk map[string]any) {
	if bc, _ := sk["barcode"].(string); bc != "" {
		row.MatchedBarcode = bc
	}
	if pn, _ := sk["product_name"].(string); pn != "" {
		row.MatchedName = pn
	}
	if sn, _ := sk["supplier_name"].(string); sn != "" {
		row.MatchedSupp = sn
	}
	if sid, _ := sk["supplier_id"].(string); sid != "" {
		row.MatchedSrc = sid
	}
	if sq, ok := sk["stock_qty"].(float64); ok {
		sq2 := sq
		row.StockQty = &sq2
	}
	if u, _ := sk["unit"].(string); u != "" {
		row.Unit = u
	}
	row.Status = "已匹配"
	row.IsNew = false
}
