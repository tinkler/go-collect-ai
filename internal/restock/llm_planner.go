package restock

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/tinkler/collect-ai/internal/parser/bigmodel"
)

// LlmPlanner 批量 LLM 算"建议补货量"
//
// 复用 collect-ai 现有 bigmodel.LlmClient
// 设计要点:
//   1. 批量调用:把当日触发补货的 SKU 一次喂给 LLM,横向对比 + 省 token
//   2. 缓存:默认 6h,避免每次 cron 都重算
//   3. 降级:LLM 调用失败时用规则值,不阻塞业务
type LlmPlanner struct {
	llm        *bigmodel.LlmClient
	model      string
	cacheHrs   int
	enabled    bool

	mu       sync.RWMutex
	cache    map[string]cachedQty // item_no -> {qty, exp}
	lastPlan time.Time
}

type cachedQty struct {
	qty  int
	exp  time.Time
	why  string
}

func NewLlmPlanner(llm *bigmodel.LlmClient, model string, enabled bool, cacheHrs int) *LlmPlanner {
	if cacheHrs <= 0 {
		cacheHrs = 6
	}
	return &LlmPlanner{
		llm:      llm,
		model:    model,
		cacheHrs: cacheHrs,
		enabled:  enabled,
		cache:    make(map[string]cachedQty),
	}
}

// Plan 算单个 SKU 的建议补货量(优先查缓存,缓存过期或不存在才调 LLM)
func (p *LlmPlanner) Plan(ctx context.Context, sku *SkuSnapshot, ruleQty int, supplierFillRate float64) int {
	if !p.enabled {
		return p.adjustBySupplier(ruleQty, supplierFillRate)
	}

	p.mu.RLock()
	c, ok := p.cache[sku.ItemNo]
	p.mu.RUnlock()
	if ok && time.Now().Before(c.exp) {
		return c.qty
	}

	// 缓存 miss → 等下次批量调用;这次先用规则值
	return p.adjustBySupplier(ruleQty, supplierFillRate)
}

// PlanBatch 批量算 (cron 每 6h 跑一次)
//   输入:当日所有触发补货的 SKU 列表
//   输出:写缓存(供后续 cron tick 用)
func (p *LlmPlanner) PlanBatch(ctx context.Context, skus []*SkuSnapshot, defaultRuleQty map[string]int) error {
	if !p.enabled {
		return nil
	}
	if len(skus) == 0 {
		return nil
	}
	if p.llm == nil {
		return nil
	}

	prompt := buildPlanPrompt(skus, defaultRuleQty)
	sysPrompt := "你是商超补货量优化助手。根据商品近 30 日销量、当前库存、促销计划、供应商历史供应能力,输出每个商品的最优建议补货量。只输出 JSON,不要解释。"

	reply, err := p.llm.ChatCompletion(sysPrompt, prompt, p.model)
	if err != nil {
		log.Printf("[restock] LLM plan batch failed: %v (fallback to rule)", err)
		return err
	}

	parsed, err := parsePlanReply(reply)
	if err != nil {
		log.Printf("[restock] LLM plan parse failed: %v", err)
		return err
	}

	now := time.Now().Add(time.Duration(p.cacheHrs) * time.Hour)
	p.mu.Lock()
	for itemNo, q := range parsed {
		if q < 1 {
			q = 1
		}
		p.cache[itemNo] = cachedQty{qty: q, exp: now, why: "llm"}
	}
	p.lastPlan = time.Now()
	p.mu.Unlock()
	log.Printf("[restock] LLM plan: %d items planned", len(parsed))
	return nil
}

func (p *LlmPlanner) adjustBySupplier(ruleQty int, fillRate float64) int {
	if fillRate <= 0 || fillRate >= 1.0 {
		return ruleQty
	}
	// fill_rate 低 → 申请量放大
	mult := 1.0 / fillRate
	if mult > 2.0 {
		mult = 2.0
	}
	return ruleQty * int(mult*10) / 10
}

func buildPlanPrompt(skus []*SkuSnapshot, defaultQty map[string]int) string {
	var b strings.Builder
	b.WriteString("# 商超补货量优化任务\n\n")
	b.WriteString("## 背景\n")
	b.WriteString("- 商超有 200+ SKU,每 6 小时跑一次补货量优化\n")
	b.WriteString("- ROP(Reorder Point)= max(昨日销量×1.5, 7日均×1.5, 5)已用于决定是否触发\n")
	b.WriteString("- 你的任务:为下面列出的商品输出最终建议补货量\n\n")
	b.WriteString("## 规则值(兜底)\n")
	b.WriteString("- 目标量 = max(7日均×7, 7日均×1.5促销, 7日均) - 当前库存\n")
	b.WriteString("- supplier fill_rate < 0.5 时,自动 ×1.5\n\n")
	b.WriteString("## 调整建议(可超出规则值)\n")
	b.WriteString("- 促销期前 3 天:加 30%\n")
	b.WriteString("- 历史 fill_rate 低(<0.5):再 ×1.3(预防供应不足)\n")
	b.WriteString("- 7 日均 vs 30 日均差异大(>50%):用 7 日均更准\n")
	b.WriteString("- 商品是低值易耗品(<5 元/件):用规则值即可,别加太多\n\n")
	b.WriteString("## 输入(每行一个商品)\n")
	b.WriteString("```\n")
	for _, sku := range skus {
		fmt.Fprintf(&b, "item_no=%s | name=%s | stock=%d | yest=%d | 7d_avg=%d | 30d_avg=%d | promo=%v | rule_qty=%d\n",
			sku.ItemNo, sku.ItemName, sku.Stock, sku.YesterdaySales, sku.SevenDayAvg,
			sku.ThirtyDayAvg, sku.HasPromo7d, defaultQty[sku.ItemNo])
	}
	b.WriteString("```\n\n")
	b.WriteString("## 输出格式(JSON 数组)\n")
	b.WriteString("```json\n")
	b.WriteString("{\"plans\": [{\"item_no\": \"xxx\", \"qty\": 24, \"reason\": \"促销期备货\"}]}\n")
	b.WriteString("```")
	return b.String()
}

func parsePlanReply(reply string) (map[string]int, error) {
	reply = strings.TrimSpace(reply)
	// 尝试提取 JSON
	start := strings.Index(reply, "{")
	end := strings.LastIndex(reply, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object found")
	}
	js := reply[start : end+1]
	var wrap struct {
		Plans []struct {
			ItemNo string `json:"item_no"`
			Qty    int    `json:"qty"`
		} `json:"plans"`
	}
	if err := json.Unmarshal([]byte(js), &wrap); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	out := make(map[string]int, len(wrap.Plans))
	for _, p := range wrap.Plans {
		if p.ItemNo != "" && p.Qty > 0 {
			out[p.ItemNo] = p.Qty
		}
	}
	return out, nil
}
