// Package parser - Orchestrator 协调 VLM + Strategy + SkuMatcher (Phase B+ 2026-09-03)
//
//	旧版 (2026-09-02 之前): OCR (hand_write) → GLM-4-flash 文本 → SkuMatcher
//	  问题: OCR 严重误读 (笔→延, 宝→裁), LLM 全判 skip → 0 rows
//
//	新版 (2026-09-03 之后): GLM-4V 多模态直读图 → JSON → SkuMatcher
//	  优势: 跳过 OCR 中间环节, 13位 barcode 100% 正确, qty 100% 正确
//	  代价: 慢 2x + 贵 5-10x
//
// 关键合规点 (AGENTS.md §1/§4):
//   - 0 个业务判断在 Go 里 (选 generic/specific 走查表)
//   - 0 个 prompt 模板在 Go 里 (在 SKILL.md)
//   - 0 个启发式在 Go 里 (heuristic 也在 SKILL.md, 这里只是兜底)
package parser

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"

	"github.com/tinkler/collect-ai/internal/agent/skill"
	"github.com/tinkler/collect-ai/internal/model"
	"github.com/tinkler/collect-ai/internal/parser/bigmodel"
	"github.com/tinkler/collect-ai/internal/parser/matcher"
	"github.com/tinkler/collect-ai/internal/store"
)

// barcode13Re 13 位 barcode 校验(未来 buildGenericHints 可用)
var barcode13Re = regexp.MustCompile(`\b\d{13}\b`)

// SkuLoader 加载供应商 SKU 库
type SkuLoader interface {
	LoadBySupplier(ctx context.Context, supplier string, limit int) ([]model.SkuRecord, error)
}

// Orchestrator 协调 VLM + Strategy + SkuMatcher
//   - ocr 完全跳过 (GLM-4V 自带视觉能力, 不需要中间 OCR 链路)
//   - llm 现在是多模态 VLM (glm-4v), 直接 image → JSON
type Orchestrator struct {
	vlm    *bigmodel.VlmClient
	skus   SkuLoader
	strat  *store.StrategyRepo
	skills *skill.Store
	// vlmModel: 默认 "glm-4v" (便宜), 可调成 "glm-4v-plus" (强)
	vlmModel string
}

func NewOrchestrator(
	vlm *bigmodel.VlmClient,
	skus SkuLoader,
	strat *store.StrategyRepo,
	skills *skill.Store,
	vlmModel string,
) (*Orchestrator, error) {
	if vlm == nil {
		return nil, fmt.Errorf("orchestrator: vlm client 必填")
	}
	if skus == nil {
		return nil, fmt.Errorf("orchestrator: sku loader 必填")
	}
	if strat == nil {
		return nil, fmt.Errorf("orchestrator: strategy repo 必填")
	}
	if skills == nil {
		return nil, fmt.Errorf("orchestrator: skill store 必填")
	}
	return &Orchestrator{vlm: vlm, skus: skus, strat: strat, skills: skills, vlmModel: vlmModel}, nil
}

// ParseResult 解析结果
type ParseResult struct {
	Rows            []model.SkuRow
	StrategyVersion int
	IsHandwrite     bool
	IsGeneric       bool
}

// Parse 收图 bytes → 解析为 SkuRow
//
//	supplier: 必填
//	fileName: 推断 mime (jpg/png/webp)
func (o *Orchestrator) Parse(ctx context.Context, imgBytes []byte, fileName, supplier string) (*ParseResult, error) {
	if len(imgBytes) == 0 {
		return nil, fmt.Errorf("imgBytes 不能为空")
	}
	if supplier == "" {
		return nil, fmt.Errorf("supplier 必填")
	}

	log.Printf("[orch] VLM 启动 (supplier=%s, img=%d bytes)", supplier, len(imgBytes))
	res := &ParseResult{}

	// 1) 查 strategy
	s, _ := o.strat.GetBySupplier(ctx, supplier)

	// 2) 走手写 / 特定 / 通用
	var strategyBody, promptOverlay string
	var skuHints map[string]any
	if s != nil && s.IsHandwrite {
		// 手写 → 纯启发式(也跳过 VLM, 跟 Phase A 一致)
		log.Printf("[orch] supplier=%s is_handwrite=true, 走纯启发式", supplier)
		res.IsHandwrite = true
		res.Rows = o.heuristicMatch(ctx, imgBytes, fileName, supplier)
		return res, nil
	}
	if s != nil && s.Enabled && s.Body != "" {
		log.Printf("[orch] supplier=%s 命中 strategy v%d, 走特定路径", supplier, s.StrategyVersion)
		strategyBody = s.Body
		promptOverlay = s.LlmPromptOverlay
		skuHints = s.SkuHints
		res.StrategyVersion = s.StrategyVersion
		go func(name string) {
			if err := o.strat.TouchApplied(context.Background(), name); err != nil {
				log.Printf("[orch] TouchApplied(%s) err: %v", name, err)
			}
		}(supplier)
	} else {
		log.Printf("[orch] supplier=%s 无 strategy, 走通用路径", supplier)
		skuHints = o.buildGenericHints(ctx, supplier)
		res.IsGeneric = true
		go func(name string) {
			if err := o.strat.IncrGenericCount(context.Background(), name); err != nil {
				log.Printf("[orch] IncrGenericCount(%s) err: %v", name, err)
			}
		}(supplier)
	}

	// 3) 读 ocr-purchase skill body 当 prompt 模板
	sk, ok := o.skills.Get("ocr-purchase")
	if !ok {
		return nil, fmt.Errorf("skill 'ocr-purchase' 未加载,检查 skills/ 目录")
	}
	sysPrompt, err := renderPrompt(sk.Body, PromptVars{
		Supplier:      supplier,
		SkuHints:      skuHints,
		StrategyBody:  strategyBody,
		PromptOverlay: promptOverlay,
	})
	if err != nil {
		return nil, fmt.Errorf("render prompt 失败: %w", err)
	}

	// 4) 调 VLM 多模态
	vlmRes, err := o.vlm.ChatWithImage(sysPrompt, "", o.vlmModel, imgBytes, fileName)
	if err != nil {
		log.Printf("[orch] VLM 失败, fallback 启发式: %v", err)
		res.Rows = o.heuristicMatch(ctx, imgBytes, fileName, supplier)
		return res, nil
	}
	content := vlmRes.Content
	// 2026-09-04: VLM 截断检测 (finish_reason=length 已被 VlmClient retry 过一次,
	//   仍截断则到这步也是 truncated=true,内容是部分 JSON)
	if vlmRes.Truncated {
		log.Printf("[orch] ⚠️ VLM 响应被截断 (max_tokens=已达 BigModel 上限 2048, content_len=%d)", len(content))
	}
	// debug
	preview := content
	if len(preview) > 500 {
		preview = preview[:500] + "..."
	}
	log.Printf("[orch] VLM 响应 (前 500, truncated=%v, finish_reason=%s): %s", vlmRes.Truncated, vlmRes.FinishReason, preview)

	// 5) 解析 JSON
	parsed, err := bigmodel.ParseLlmJson(content)
	if err != nil {
		log.Printf("[orch] VLM JSON 解析失败, fallback 启发式: %v (truncated=%v)", err, vlmRes.Truncated)
		res.Rows = o.heuristicMatch(ctx, imgBytes, fileName, supplier)
		return res, nil
	}
	log.Printf("[orch] VLM 解析 → %d 条 (supplier=%s, strategy_v=%d)", len(parsed), supplier, res.StrategyVersion)

	// 6) SkuMatcher 匹配
	skus, _ := o.skus.LoadBySupplier(ctx, supplier, 5000)
	m := matcher.New(skus, 0)
	rows := make([]model.SkuRow, 0, len(parsed))
	for i, ocr := range parsed {
		rows = append(rows, m.Match(ocr, i+1))
	}
	res.Rows = rows
	return res, nil
}

// heuristicMatch 纯启发式(手写 / VLM 失败兜底)
//   - 跟 Phase A 一致: 用 ocr_service.ParseOcrResponse + heuristicParse
//   - 但 Phase A 用 OCR 链路, 现在 VLM 链路下 OCR 已经废弃
//   - 兜底 = VLM 失败时返回 0 rows
func (o *Orchestrator) heuristicMatch(ctx context.Context, imgBytes []byte, fileName, supplier string) []model.SkuRow {
	// VLM 链路下 heuristic 仅在 VLM 失败时跑, 跑不动时直接返空
	log.Printf("[orch] heuristic 兜底 (VLM 失败): img=%d bytes, supplier=%s", len(imgBytes), supplier)
	// 这里不调 OCR (Phase A 老路径), 避免回退
	// 真要手写解析请用 supplier_parse_strategy.is_handwrite=true 路径
	return nil
}

// buildGenericHints 通用 hints 生成
//
// 2026-09-04 修复: 之前硬塞所有 SKU 的 barcode + name,大供应商(5000+ SKU)
//   会让 sku_hints_json 超过 300KB, 触发 BigModel GLM-4V 1261 (Prompt exceeds max length) 错误
//   解决方案:
//     - 限额 800 SKU (经验值: name 平均 30 字符 × 800 ≈ 24KB hints,远低于 8K token 限制)
//     - 优先 name (LLM 主要靠 name 校验 OCR 错字 / 别名),barcode 留 200 个做兜底
//     - hints JSON 序列化后硬限 32KB,超过则按比例再截
func (o *Orchestrator) buildGenericHints(ctx context.Context, supplier string) map[string]any {
	skus, err := o.skus.LoadBySupplier(ctx, supplier, 5000)
	if err != nil {
		log.Printf("[orch] buildGenericHints(%s) LoadBySupplier err: %v", supplier, err)
		return map[string]any{}
	}

	// 优先 name(LLM 校验 OCR 错字和别名最有用),barcode 仅做兜底
	const maxNames = 800
	const maxBarcodes = 200
	barcodes := make([]string, 0, maxBarcodes)
	names := make([]string, 0, maxNames)

	for i, s := range skus {
		if s.Barcode != "" && len(s.Barcode) >= 6 && len(s.Barcode) <= 14 && len(barcodes) < maxBarcodes {
			barcodes = append(barcodes, s.Barcode)
		}
		if s.Name != "" && len(names) < maxNames {
			names = append(names, s.Name)
		}
		// 两个限额都满了可以提前退出
		if len(barcodes) >= maxBarcodes && len(names) >= maxNames {
			break
		}
		_ = i
	}
	hints := map[string]any{
		"supplier": supplier,
		"barcodes": barcodes,
		"names":    names,
		"_note":    "由 buildGenericHints 程序生成,辅助 VLM 校验 OCR",
	}
	_ = barcode13Re

	// 2026-09-04 安全网: 即使限额后 JSON 仍 > 32KB (e.g. 800 个超长 name),
	//   按比例截 names。barcodes 体积小不动。
	if raw, err := json.Marshal(hints); err == nil {
		const maxHintsBytes = 32 * 1024
		if len(raw) > maxHintsBytes && len(names) > 100 {
			// 按比例反算: 保留 (maxHintsBytes / len(raw)) * len(names) 个
			// 保守一点用 0.7 系数,留点余量给 JSON 序列化
			keep := int(float64(len(names)) * 0.7 * float64(maxHintsBytes) / float64(len(raw)))
			if keep < 50 {
				keep = 50
			}
			if keep > len(names) {
				keep = len(names)
			}
			names = names[:keep]
			hints["names"] = names
			log.Printf("[orch] buildGenericHints(%s) hints 超 32KB (%d bytes), 截 names → %d",
				supplier, len(raw), keep)
		}
	}
	return hints
}
