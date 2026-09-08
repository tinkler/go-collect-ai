// Package purchasealert 采购订单智能提醒规则引擎 (W3.2)
//
// 4 类规则:
//   - BlockEntry:   供应商被 block_entry=true 限入场 → severity=block
//   - NoReturn:     供应商 allow_return=false → severity=warn
//   - Offseason:    SKU 类别与当前季节不匹配 → severity=info (本期简化: LLM 介入留 W3.5)
//   - HolidayLead:  节假日 lead_days 窗口内的应季 SKU → severity=info
//
// 设计:
//   - 规则 = 纯 Go interface (无 LLM 依赖, 性能 + 可测)
//   - 上下文 RuleContext 一次性加载所有供应商政策 + 节假日窗口, 避免 N+1 查询
//   - Service.Apply() 顺序应用所有规则, 同一 row 多个规则各产 1 条 alert
//
// W3.2 范围: 4 规则 + Service.Apply + handler 集成 + 单测
// W3.5 (后续): 接入 GraphAgent 处理"应季判定"语义模糊场景
package purchasealert

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tinkler/collect-ai/internal/model"
)

// Alert 提醒 (落库 + 推 H5 + 推企微)
type Alert struct {
	AlertID  int64     `json:"alert_id"`
	SessID   string    `json:"session_id"`
	RowID    int64     `json:"row_id"` // 0 = 整张 session 级别(非 row-specific)
	Rule     string    `json:"rule"`
	Severity string    `json:"severity"`
	// 2026-09-03 W4.1: 决定前端 icon 段位
	//   block            红色感叹号
	//   warn             橙色感叹号
	//   info             灰普通感叹号
	//   highlight_dui    绿色"贴切"标志
	//   highlight_others 绿色"其它"标志
	Category  string    `json:"category"`
	Message   string    `json:"message"`
	AckedAt   time.Time `json:"acked_at,omitempty"`
	AckedBy   string    `json:"acked_by,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// Severity 等级
const (
	SeverityBlock = "block" // 阻断 (限入场)
	SeverityWarn  = "warn"  // 警告 (不允许退货/高库存)
	SeverityInfo  = "info"  // 提示 (季节/节假日/堆头/快讯)
)

// Category 前端 icon 段位 (W4.1)
const (
	CategoryBlock         = "block"            // 红色感叹号
	CategoryWarn          = "warn"             // 橙色感叹号
	CategoryInfo          = "info"             // 灰普通感叹号
	CategoryHighlightDui  = "highlight_dui"    // 绿色"贴切"标志 (堆头)
	CategoryHighlightOthers = "highlight_others" // 绿色"其它"标志 (快讯/端架)
)

// Rule 规则接口
type Rule interface {
	Name() string
	Apply(ctx context.Context, sess *model.Session, row *model.SkuRow, rc RuleContext) []Alert
}

// RuleContext 一次性加载的上下文(避免 N+1)
type RuleContext struct {
	// SupplierPolicies key=supplier_name, value=该 supplier 的所有 policy
	SupplierPolicies map[string][]PolicyKV
	// Holidays 接下来 N 天的节假日(已过滤)
	Holidays []Holiday
	// 2026-09-03 W4.1: 高库存/难消化阈值,从 app_settings 拉 (数据, 不是硬编码)
	HighStockThreshold   float64
	LowMovementThreshold float64
	// 2026-09-03 W4.1: 促销 kind 分类,决定 highlight_dui / highlight_others
	//   数据来源: app_settings.duitou_kinds / others_kinds
	DuitouKinds  []string
	OthersKinds  []string
	// Now 当前时间(便于单测注入)
	Now time.Time
}

// PolicyKV 供应商政策 (k, v) — v 是任意 JSON
type PolicyKV struct {
	Key string
	Val any
}

// Holiday 节假日
type Holiday struct {
	Date     time.Time
	Type     string // holiday / promo / season_start / season_end
	Name     string
	LeadDays int
}

// LoadSupplierPoliciesFromPG 读 supplier_policy → []PolicyKV (按 supplier 分组)
//   简化: W3.2 直接 inline SQL, 不复用 internal/agent/tools (避免循环依赖)
func LoadSupplierPoliciesFromPG(ctx context.Context, pool interface {
	Query(ctx context.Context, sql string, args ...any) (interface{ Close() }, error)
}) (map[string][]PolicyKV, error) {
	// 实际调用方用 pgxpool.Pool — 这里 interface 仅为签名约束
	// 实现见 repo.go
	return nil, fmt.Errorf("placeholder, see alerts_repo.go")
}

// ============================================================
// 规则 1: BlockEntry
//   供应商 policy block_entry=true → severity=block
// ============================================================

type BlockEntryRule struct{}

func (BlockEntryRule) Name() string { return "block_entry" }

func (BlockEntryRule) Apply(_ context.Context, sess *model.Session, row *model.SkuRow, rc RuleContext) []Alert {
	if row == nil {
		return nil
	}
	supplier := strings.TrimSpace(row.MatchedSupp)
	if supplier == "" {
		return nil
	}
	policies, ok := rc.SupplierPolicies[supplier]
	if !ok {
		return nil
	}
	for _, p := range policies {
		if p.Key == "block_entry" {
			if v, ok := p.Val.(bool); ok && v {
				msg := fmt.Sprintf("供应商 [%s] 已被限制入场(block_entry=true),本单据不审请勿入库", supplier)
				return []Alert{{
					SessID:   sess.ID,
					RowID:    row.RowID,
					Rule:     "block_entry",
					Severity: SeverityBlock,
					Category: CategoryBlock, // 红色感叹号
					Message:  msg,
				}}
			}
		}
	}
	return nil
}

// ============================================================
// 规则 2: NoReturn
//   供应商 allow_return=false → severity=warn (新单需店长确认)
// ============================================================

type NoReturnRule struct{}

func (NoReturnRule) Name() string { return "no_return" }

func (NoReturnRule) Apply(_ context.Context, sess *model.Session, row *model.SkuRow, rc RuleContext) []Alert {
	if row == nil {
		return nil
	}
	supplier := strings.TrimSpace(row.MatchedSupp)
	if supplier == "" {
		return nil
	}
	policies, ok := rc.SupplierPolicies[supplier]
	if !ok {
		return nil
	}
	for _, p := range policies {
		if p.Key == "allow_return" {
			if v, ok := p.Val.(bool); ok && !v {
				msg := fmt.Sprintf("供应商 [%s] 不支持退货(allow_return=false),请确认本次采购数量", supplier)
				return []Alert{{
					SessID:   sess.ID,
					RowID:    row.RowID,
					Rule:     "no_return",
					Severity: SeverityWarn,
					Category: CategoryWarn, // 橙色感叹号
					Message:  msg,
				}}
			}
		}
	}
	return nil
}

// ============================================================
// 规则 3: Offseason (本期简化 — 关键词匹配, W3.5 接 LLM 语义判定)
//   行 name 命中"反季"词(冰品 → 冬季 / 电热 → 夏季 / ...) → severity=info
//   W3.5: 改用 GraphAgent + LLM 判定
// ============================================================

type OffseasonRule struct{}

func (OffseasonRule) Name() string { return "offseason" }

// 反季映射 (词 → 适合的季节)
var seasonWords = map[string][]string{
	"冰品":  {"summer"},
	"冰棍":  {"summer"},
	"冰激凌": {"summer"},
	"冰淇淋": {"summer"},
	"电热":  {"winter"},
	"暖手宝": {"winter"},
	"棉衣":  {"winter"},
	"毛毯":  {"winter"},
	"火锅":  {"winter"},
	"凉席":  {"summer"},
	"风扇":  {"summer"},
	"空调":  {"summer"},
	"西瓜":  {"summer"},
	"冰粉":  {"summer"},
}

// 当前月份 → 季节 (W3.2 简化, 实际用 special_calendar)
func currentSeason(t time.Time) string {
	m := t.Month()
	switch {
	case m >= 3 && m <= 5:
		return "spring"
	case m >= 6 && m <= 8:
		return "summer"
	case m >= 9 && m <= 11:
		return "autumn"
	default:
		return "winter"
	}
}

func (OffseasonRule) Apply(_ context.Context, sess *model.Session, row *model.SkuRow, rc RuleContext) []Alert {
	if row == nil {
		return nil
	}
	// 用 matched_name(已识别 SKU) 作为判定依据
	name := row.MatchedName
	if name == "" {
		name = row.RawName
	}
	if name == "" {
		return nil
	}
	cur := currentSeason(rc.Now)
	for word, seasons := range seasonWords {
		if !strings.Contains(name, word) {
			continue
		}
		// 命中关键词,看是否当前季节
		ok := false
		for _, s := range seasons {
			if s == cur {
				ok = true
				break
			}
		}
		if !ok {
			msg := fmt.Sprintf("商品 [%s] 包含应季词 [%s],但当前是 [%s],可能是反季补货", name, word, cur)
			return []Alert{{
				SessID:   sess.ID,
				RowID:    row.RowID,
				Rule:     "offseason",
				Severity: SeverityInfo,
				Category: CategoryInfo, // 灰普通感叹号
				Message:  msg,
			}}
		}
	}
	return nil
}

// ============================================================
// 规则 4: HolidayLead
//   节假日 lead_days 窗口内, 提醒"需提前备货"
//   本期: 整 session 级别 (RowID=0) 推送 1 条汇总
// ============================================================

type HolidayLeadRule struct{}

func (HolidayLeadRule) Name() string { return "holiday_lead" }

func (HolidayLeadRule) Apply(_ context.Context, sess *model.Session, _ *model.SkuRow, rc RuleContext) []Alert {
	if len(rc.Holidays) == 0 {
		return nil
	}
	// 找最近 lead_days 最大的节假日
	var nearest *Holiday
	for i := range rc.Holidays {
		h := &rc.Holidays[i]
		if h.Type != "holiday" {
			continue
		}
		daysUntil := int(h.Date.Sub(rc.Now).Hours() / 24)
		if daysUntil < 0 || daysUntil > h.LeadDays {
			continue
		}
		if nearest == nil || h.Date.Before(nearest.Date) {
			nearest = h
		}
	}
	if nearest == nil {
		return nil
	}
	daysUntil := int(nearest.Date.Sub(rc.Now).Hours() / 24)
	msg := fmt.Sprintf("距 [%s] 还有 %d 天(lead_days=%d),建议提前备货", nearest.Name, daysUntil, nearest.LeadDays)
	return []Alert{{
		SessID:   sess.ID,
		RowID:    0, // session 级别
		Rule:     "holiday_lead",
		Severity: SeverityInfo,
		Category: CategoryInfo, // 灰普通感叹号 (总结栏展示)
		Message:  msg,
	}}
}

// DefaultRules 默认注册 4 个规则(顺序敏感 — block 优先)
var DefaultRules = []Rule{
	BlockEntryRule{},
	NoReturnRule{},
	OffseasonRule{},
	HolidayLeadRule{},
}

// ============================================================
// 规则 5: LLMSeasonRule (W3.5 新增)
//   用 SeasonClassifier 语义判定 SKU 是否反季
//   优先于 OffseasonRule (关键词), 用于 W3.5+ LLM 介入场景
//   classifier == nil → 跳过本规则(兼容 W3.2 部署)
// ============================================================

type LLMSeasonRule struct {
	Classifier SeasonClassifier
}

func (LLMSeasonRule) Name() string { return "llm_offseason" }

func (r LLMSeasonRule) Apply(_ context.Context, sess *model.Session, row *model.SkuRow, _ RuleContext) []Alert {
	if r.Classifier == nil {
		return nil
	}
	if row == nil {
		return nil
	}
	name := row.MatchedName
	if name == "" {
		name = row.RawName
	}
	if name == "" {
		return nil
	}
	season := r.Classifier.Classify(context.Background(), name) // 简单 ctx (W3.5 暂不传 ctx 上下文)
	if season == SeasonOffSeason {
		return []Alert{{
			SessID:   sess.ID,
			RowID:    row.RowID,
			Rule:     "llm_offseason",
			Severity: SeverityInfo,
			Category: CategoryInfo, // 灰普通感叹号
			Message:  fmt.Sprintf("LLM 判定 [%s] 当前反季(类目性反季,非简单关键词),请确认是否真要补货", name),
		}}
	}
	return nil
}

// ============================================================
// 规则 6: HighStockRule (W4.1 新增)
//   阈值从 app_settings.high_stock_threshold 拉 (数据, 不硬编码)
//   row.StockQty > 阈值 → severity=warn, category=warn (橙感叹号)
// ============================================================

type HighStockRule struct{}

func (HighStockRule) Name() string { return "high_stock" }

func (HighStockRule) Apply(_ context.Context, sess *model.Session, row *model.SkuRow, rc RuleContext) []Alert {
	if row == nil {
		return nil
	}
	if rc.HighStockThreshold <= 0 {
		return nil
	}
	stock := float64(0)
	if row.StockQty != nil {
		stock = *row.StockQty
	}
	if stock <= rc.HighStockThreshold {
		return nil
	}
	name := row.MatchedName
	if name == "" {
		name = row.RawName
	}
	return []Alert{{
		SessID:   sess.ID,
		RowID:    row.RowID,
		Rule:     "high_stock",
		Severity: SeverityWarn,
		Category: CategoryWarn, // 橙色感叹号
		Message: fmt.Sprintf("商品 [%s] 当前库存 %.0f,超过阈值 %.0f,本次采购需谨慎(可能压库存)",
			name, stock, rc.HighStockThreshold),
	}}
}

// ============================================================
// 规则 7: HasDuitouRule (W4.1 新增)
//   supplier_policy.has_duitou=true (堆头费他们出/有堆头协议)
//   AND 当前日期在 promotion_fee 期内 (period_start..period_end)
//   → 总结栏 (row_id=0) 1 条 alert, 标 highlight_dui (绿色"贴切"标志)
//   同一 supplier 命中多条 promotion_fee → 合并 1 条, 列出 kind+amount
// ============================================================

type HasDuitouRule struct {
	// 用 PG 一次性查 promotion_fee 当前期内, 按 supplier 分组
	// service.loadContext 里调用 RefreshActivePromos 预加载
	// 这里只读 cache
	ActivePromos map[string][]ActivePromo
}

// ActivePromo promotion_fee 当前期内
type ActivePromo struct {
	Kind   string
	Amount float64
	End    time.Time
}

func (HasDuitouRule) Name() string { return "has_duitou" }

func (r HasDuitouRule) Apply(_ context.Context, sess *model.Session, row *model.SkuRow, rc RuleContext) []Alert {
	// 总结栏规则,只跑 1 次 (row=nil) — 已在 service.Apply 调度
	if row != nil {
		return nil
	}
	// 收集 session 内所有 supplier
	suppliers := map[string]struct{}{}
	for _, r := range sess.Rows {
		if r.IsDeleted || r.MatchedSupp == "" {
			continue
		}
		suppliers[r.MatchedSupp] = struct{}{}
	}
	// 收集本期有堆头协议的 supplier
	duiSups := map[string]struct{}{}
	for sup, pols := range rc.SupplierPolicies {
		for _, p := range pols {
			if p.Key == "has_duitou" {
				if v, ok := p.Val.(bool); ok && v {
					duiSups[sup] = struct{}{}
				}
			}
		}
	}
	// 取交集
	var hits []string
	for sup := range suppliers {
		if _, ok := duiSups[sup]; ok {
			hits = append(hits, sup)
		}
	}
	if len(hits) == 0 {
		return nil
	}
	// 拼堆头明细
	var parts []string
	for _, sup := range hits {
		promos := r.ActivePromos[sup]
		if len(promos) == 0 {
			parts = append(parts, fmt.Sprintf("[%s] 已签堆头", sup))
			continue
		}
		// 合并同 supplier 的所有堆头
		var sub []string
		for _, p := range promos {
			if !containsString(rc.DuitouKinds, p.Kind) {
				continue
			}
			sub = append(sub, fmt.Sprintf("%s ¥%.0f(至 %s)",
				p.Kind, p.Amount, p.End.Format("01-02")))
		}
		if len(sub) > 0 {
			parts = append(parts, fmt.Sprintf("[%s] %s", sup, joinComma(sub)))
		}
	}
	if len(parts) == 0 {
		return nil
	}
	msg := "本期堆头陈列: " + joinComma(parts)
	return []Alert{{
		SessID:   sess.ID,
		RowID:    0, // 总结栏
		Rule:     "has_duitou",
		Severity: SeverityInfo,
		Category: CategoryHighlightDui, // 绿色"贴切"标志
		Message:  msg,
	}}
}

// ============================================================
// 规则 8: FlashPromoRule (W4.1 新增)
//   promotion_fee 命中, kind 在 app_settings.others_kinds (快讯/端架/DM/特价/海报)
//   按 supplier 分组, 每个 supplier + kind 1 条
//   → category=highlight_others (绿色"其它"标志)
// ============================================================

type FlashPromoRule struct {
	ActivePromos map[string][]ActivePromo
}

func (FlashPromoRule) Name() string { return "flash_promo" }

func (r FlashPromoRule) Apply(_ context.Context, sess *model.Session, row *model.SkuRow, rc RuleContext) []Alert {
	if row == nil {
		return nil
	}
	supplier := strings.TrimSpace(row.MatchedSupp)
	if supplier == "" {
		return nil
	}
	promos, ok := r.ActivePromos[supplier]
	if !ok {
		return nil
	}
	// 找该 supplier 当前在期内的 "其它" kind
	var kinds []string
	for _, p := range promos {
		if containsString(rc.OthersKinds, p.Kind) {
			kinds = append(kinds, p.Kind)
		}
	}
	if len(kinds) == 0 {
		return nil
	}
	name := row.MatchedName
	if name == "" {
		name = row.RawName
	}
	return []Alert{{
		SessID:   sess.ID,
		RowID:    row.RowID,
		Rule:     "flash_promo",
		Severity: SeverityInfo,
		Category: CategoryHighlightOthers, // 绿色"其它"标志
		Message: fmt.Sprintf("商品 [%s] 供应商 [%s] 正在做 %s,注意陈列位置",
			name, supplier, joinComma(kinds)),
	}}
}

// ============================================================
// 默认规则集 (W4.1)
//   顺序敏感 — block 优先, 总结栏规则在最后
// ============================================================

// DefaultRulesWithDeps 默认注册 4 个规则 (无外部依赖)
//   复杂规则 (HasDuitouRule / FlashPromoRule) 需走 NewServiceWithPromos 注入

// helpers
func containsString(arr []string, s string) bool {
	for _, x := range arr {
		if x == s {
			return true
		}
	}
	return false
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
