package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
)

// ============================================================
// 工具 7: compute_promotion_fee_share (W4 D 模块)
//   算某 supplier 当月 promotion_fee 分摊 (按月在 period 内天数)
//   返回 [{kind, amount, days_in_month, month_share}] 列表
// ============================================================

// ComputePromotionFeeShareReq 输入
type ComputePromotionFeeShareReq struct {
	Supplier string `json:"supplier" jsonschema:"description=供应商名称(必填),required"`
	Month    string `json:"month" jsonschema:"description=月份 YYYY-MM(必填,默认当前月),required"`
}

// ComputePromotionFeeShareItem 输出单条
type ComputePromotionFeeShareItem struct {
	Kind         string  `json:"kind"`
	Amount       float64 `json:"amount"`
	DaysInMonth  int     `json:"days_in_month"`
	MonthShare   float64 `json:"month_share"`   // amount * days_in_month / period_days
	PeriodStart  string  `json:"period_start"`
	PeriodEnd    string  `json:"period_end"`
}

// ComputePromotionFeeShareResp 输出
type ComputePromotionFeeShareResp struct {
	Supplier     string                        `json:"supplier"`
	Month        string                        `json:"month"`
	TotalShare   float64                       `json:"total_share"`
	Items        []ComputePromotionFeeShareItem `json:"items"`
	Count        int                           `json:"count"`
}

// ComputePromotionFeeShare 工具
//   算法: 拉当月所有属于该 supplier 的 promotion_fee,按 period 与 month 的重叠天数分摊
//   month_share = amount * overlap_days / (period_end - period_start + 1)
func ComputePromotionFeeShare(pool *pgxpool.Pool) *function.FunctionTool[ComputePromotionFeeShareReq, ComputePromotionFeeShareResp] {
	fn := func(ctx context.Context, req ComputePromotionFeeShareReq) (ComputePromotionFeeShareResp, error) {
		if pool == nil {
			return ComputePromotionFeeShareResp{}, fmt.Errorf("compute_promotion_fee_share: pg pool 未初始化")
		}
		supplier := trimSpace(req.Supplier)
		if supplier == "" {
			return ComputePromotionFeeShareResp{}, fmt.Errorf("supplier 必填")
		}
		monthStr := trimSpace(req.Month)
		if monthStr == "" {
			monthStr = time.Now().Format("2006-01")
		}
		// month 解析为月初/月末
		monthStart, err := time.Parse("2006-01", monthStr)
		if err != nil {
			return ComputePromotionFeeShareResp{}, fmt.Errorf("month 格式错误 YYYY-MM: %w", err)
		}
		monthEnd := monthStart.AddDate(0, 1, 0)
		monthDays := int(monthEnd.Sub(monthStart).Hours() / 24)

		rows, err := pool.Query(ctx, `
			SELECT kind, amount, period_start, period_end
			FROM promotion_fee
			WHERE supplier_name = $1
			  AND period_end >= $2
			  AND period_start < $3
		`, supplier, monthStart, monthEnd)
		if err != nil {
			return ComputePromotionFeeShareResp{}, fmt.Errorf("query promotion_fee: %w", err)
		}
		defer rows.Close()

		out := ComputePromotionFeeShareResp{
			Supplier: supplier,
			Month:    monthStr,
			Items:    []ComputePromotionFeeShareItem{},
		}
		for rows.Next() {
			var kind string
			var amount float64
			var ps, pe time.Time
			if err := rows.Scan(&kind, &amount, &ps, &pe); err != nil {
				return out, err
			}
			// 计算当月重叠天数
			overlapStart := maxTime(ps, monthStart)
			overlapEnd := minTime(pe.AddDate(0, 0, 1), monthEnd) // period_end 包含
			overlapDays := int(overlapEnd.Sub(overlapStart).Hours() / 24)
			if overlapDays < 0 {
				overlapDays = 0
			}
			totalDays := int(pe.AddDate(0, 0, 1).Sub(ps).Hours() / 24)
			var monthShare float64
			if totalDays > 0 {
				monthShare = amount * float64(overlapDays) / float64(totalDays)
			}
			out.Items = append(out.Items, ComputePromotionFeeShareItem{
				Kind:        kind,
				Amount:      amount,
				DaysInMonth: overlapDays,
				MonthShare:  monthShare,
				PeriodStart: ps.Format("2006-01-02"),
				PeriodEnd:   pe.Format("2006-01-02"),
			})
			out.TotalShare += monthShare
		}
		out.Count = len(out.Items)
		_ = monthDays
		return out, rows.Err()
	}
	return function.NewFunctionTool(fn,
		function.WithName("compute_promotion_fee_share"),
		function.WithDescription("算某 supplier 当月堆头/端架费等促销费分摊 (按月在 period 内天数比例). 返回总金额 + 各笔明细. 用于月度对账."),
	)
}

// ============================================================
// 工具 8: upcoming_promotion_expiry
//   复用 W3.3 已有逻辑(从 promotionalert.RunOnce 提取)
// ============================================================

// UpcomingPromotionExpiryReq 输入
type UpcomingPromotionExpiryReq struct {
	Supplier   string `json:"supplier,omitempty" jsonschema:"description=按 supplier 过滤(可选)"`
	DaysAhead  int    `json:"days_ahead" jsonschema:"description=从今天起算的天数窗口(必填),required"`
}

// UpcomingPromotionExpiryItem 输出
type UpcomingPromotionExpiryItem struct {
	FeeID      int64   `json:"fee_id"`
	Supplier   string  `json:"supplier"`
	Kind       string  `json:"kind"`
	Amount     float64 `json:"amount"`
	PeriodEnd  string  `json:"period_end"`
	DaysLeft   int     `json:"days_left"`
	Note       string  `json:"note,omitempty"`
}

// UpcomingPromotionExpiryResp 输出
type UpcomingPromotionExpiryResp struct {
	FromDate string                         `json:"from_date"`
	ToDate   string                         `json:"to_date"`
	Count    int                            `json:"count"`
	Items    []UpcomingPromotionExpiryItem `json:"items"`
}

// UpcomingPromotionExpiry 工具
func UpcomingPromotionExpiry(pool *pgxpool.Pool) *function.FunctionTool[UpcomingPromotionExpiryReq, UpcomingPromotionExpiryResp] {
	fn := func(ctx context.Context, req UpcomingPromotionExpiryReq) (UpcomingPromotionExpiryResp, error) {
		if pool == nil {
			return UpcomingPromotionExpiryResp{}, fmt.Errorf("upcoming_promotion_expiry: pg pool 未初始化")
		}
		if req.DaysAhead <= 0 {
			return UpcomingPromotionExpiryResp{}, fmt.Errorf("days_ahead 必须 > 0")
		}
		now := time.Now()
		to := now.AddDate(0, 0, req.DaysAhead)

		var (
			rows pgx.Rows
			err  error
		)
		if req.Supplier != "" {
			rows, err = pool.Query(ctx, `
				SELECT id, supplier_name, kind, amount, period_end, COALESCE(note,'')
				FROM promotion_fee
				WHERE period_end >= $1 AND period_end <= $2 AND supplier_name = $3
				ORDER BY period_end ASC
			`, now, to, trimSpace(req.Supplier))
		} else {
			rows, err = pool.Query(ctx, `
				SELECT id, supplier_name, kind, amount, period_end, COALESCE(note,'')
				FROM promotion_fee
				WHERE period_end >= $1 AND period_end <= $2
				ORDER BY period_end ASC
			`, now, to)
		}
		if err != nil {
			return UpcomingPromotionExpiryResp{}, fmt.Errorf("query promotion_fee: %w", err)
		}
		defer rows.Close()

		out := UpcomingPromotionExpiryResp{
			FromDate: now.Format("2006-01-02"),
			ToDate:   to.Format("2006-01-02"),
			Items:    []UpcomingPromotionExpiryItem{},
		}
		for rows.Next() {
			var it UpcomingPromotionExpiryItem
			var pe time.Time
			if err := rows.Scan(&it.FeeID, &it.Supplier, &it.Kind, &it.Amount, &pe, &it.Note); err != nil {
				return out, err
			}
			it.PeriodEnd = pe.Format("2006-01-02")
			it.DaysLeft = int(pe.Sub(now).Hours() / 24)
			out.Items = append(out.Items, it)
		}
		out.Count = len(out.Items)
		return out, rows.Err()
	}
	return function.NewFunctionTool(fn,
		function.WithName("upcoming_promotion_expiry"),
		function.WithDescription("查未来 N 天内 promotion_fee 到期清单, 可按 supplier 过滤. 返回 days_left = 距今天数. 用于堆头费/端架费到期预警."),
	)
}

// ============================================================
// 工具 9: forecast_purchase_amount
//   算 N 天内某 supplier 的预测采购额
//   简化: 用 parse_row 历史平均 daily × N (而非调 cube, 避免跨服务)
//   实际: cube 调大模型层 agent.Client (W5 升级)
// ============================================================

// ForecastPurchaseAmountReq 输入
type ForecastPurchaseAmountReq struct {
	Supplier string `json:"supplier" jsonschema:"description=供应商名称(必填),required"`
	Days     int    `json:"days" jsonschema:"description=预测未来天数(必填,7/30/90),required"`
}

// ForecastPurchaseAmountResp 输出
type ForecastPurchaseAmountResp struct {
	Supplier       string  `json:"supplier"`
	Days           int     `json:"days"`
	BaseDaily      float64 `json:"base_daily"`     // 近 30 天日均
	WindowDays     int     `json:"window_days"`    // 实际取数天数
	WindowTotal    float64 `json:"window_total"`   // 窗口总额
	ForecastAmount float64 `json:"forecast_amount"`
	SampleRows     int     `json:"sample_rows"`
}

// ForecastPurchaseAmount 工具
func ForecastPurchaseAmount(pool *pgxpool.Pool) *function.FunctionTool[ForecastPurchaseAmountReq, ForecastPurchaseAmountResp] {
	fn := func(ctx context.Context, req ForecastPurchaseAmountReq) (ForecastPurchaseAmountResp, error) {
		if pool == nil {
			return ForecastPurchaseAmountResp{}, fmt.Errorf("forecast_purchase_amount: pg pool 未初始化")
		}
		supplier := trimSpace(req.Supplier)
		if supplier == "" {
			return ForecastPurchaseAmountResp{}, fmt.Errorf("supplier 必填")
		}
		if req.Days <= 0 {
			return ForecastPurchaseAmountResp{}, fmt.Errorf("days 必须 > 0")
		}
		// 拉近 30 天 purchase session
		cutoff := time.Now().AddDate(0, 0, -30)
		var windowTotal float64
		var sampleRows int
		err := pool.QueryRow(ctx, `
			SELECT COALESCE(SUM(pr.qty * pr.unit_price), 0), COUNT(pr.id)
			FROM parse_row pr
			JOIN parse_session ps ON pr.session_id = ps.id
			WHERE ps.supplier_name = $1
			  AND ps.mode = 'purchase'
			  AND pr.qty IS NOT NULL
			  AND pr.unit_price IS NOT NULL
			  AND pr.is_deleted = FALSE
			  AND ps.created_at >= $2
		`, supplier, cutoff).Scan(&windowTotal, &sampleRows)
		if err != nil {
			return ForecastPurchaseAmountResp{}, fmt.Errorf("query parse_row: %w", err)
		}
		baseDaily := windowTotal / 30.0
		if baseDaily < 0 {
			baseDaily = 0
		}
		forecast := baseDaily * float64(req.Days)
		return ForecastPurchaseAmountResp{
			Supplier:       supplier,
			Days:           req.Days,
			BaseDaily:      baseDaily,
			WindowDays:     30,
			WindowTotal:    windowTotal,
			ForecastAmount: forecast,
			SampleRows:     sampleRows,
		}, nil
	}
	return function.NewFunctionTool(fn,
		function.WithName("forecast_purchase_amount"),
		function.WithDescription("预测某 supplier N 天内的采购额 (基于近 30 天 parse_row 平均日采购). W5 升级: 改用 cube 真实销售/采购数据."),
	)
}

// ============================================================
// 工具 10: suggest_supplier_payment (核心 — 三维度算法)
//   suggested_settlement = base_forecast * investment * promo * sellthrough * cycle_factor
//   三维度: 供应商费用支持力度 / 产品促销力度 / 产品动销率
// ============================================================

// SuggestSupplierPaymentReq 输入
type SuggestSupplierPaymentReq struct {
	Supplier         string  `json:"supplier" jsonschema:"description=供应商名称(必填),required"`
	PeriodDays       int     `json:"period_days" jsonschema:"description=结算周期天数(必填,7/15/30/60),required"`
	PaymentCycleDays int     `json:"payment_cycle_days,omitempty" jsonschema:"description=该供应商账期(默认 30),默认 30"`
	BufferFactor     float64 `json:"buffer_factor,omitempty" jsonschema:"description=buffer 系数(默认 1.5),默认 1.5"`
}

// SuggestSupplierPaymentResp 输出
type SuggestSupplierPaymentResp struct {
	Supplier            string                 `json:"supplier"`
	PeriodDays          int                    `json:"period_days"`
	BaseForecast        float64                `json:"base_forecast"`
	InvestmentWeight    float64                `json:"investment_weight"`
	PromoWeight         float64                `json:"promo_weight"`
	SellthroughWeight   float64                `json:"sellthrough_weight"`
	PaymentCycleDays    int                    `json:"payment_cycle_days"`
	BufferFactor        float64                `json:"buffer_factor"`
	Amount              float64                `json:"amount"`
	Basis               map[string]any         `json:"basis"`
	Action              string                 `json:"action"`  // "dry_run" | "inserted"
	ID                  int64                  `json:"id,omitempty"`
}

// SuggestSupplierPayment 工具
//   算法 (方案 §5.2):
//     1) base_forecast = forecast_purchase_amount(period_days)
//     2) investment_weight = 0.8 ~ 1.5 (基于当月 promotion_fee_share / base_forecast)
//     3) promo_weight = 0.9 ~ 1.3 (暂用固定 1.0, 未来用 cube 促销数据)
//     4) sellthrough_weight = 0.7 ~ 1.2 (暂用固定 1.0, 未来用 cube 动销率)
//     5) amount = base_forecast * 4 个系数
func SuggestSupplierPayment(pool *pgxpool.Pool) *function.FunctionTool[SuggestSupplierPaymentReq, SuggestSupplierPaymentResp] {
	fn := func(ctx context.Context, req SuggestSupplierPaymentReq) (SuggestSupplierPaymentResp, error) {
		if pool == nil {
			return SuggestSupplierPaymentResp{}, fmt.Errorf("suggest_supplier_payment: pg pool 未初始化")
		}
		supplier := trimSpace(req.Supplier)
		if supplier == "" {
			return SuggestSupplierPaymentResp{}, fmt.Errorf("supplier 必填")
		}
		if req.PeriodDays <= 0 {
			return SuggestSupplierPaymentResp{}, fmt.Errorf("period_days 必须 > 0")
		}
		if req.PaymentCycleDays <= 0 {
			req.PaymentCycleDays = 30
		}
		if req.BufferFactor <= 0 {
			req.BufferFactor = 1.5
		}
		// 1) base_forecast
		var windowTotal float64
		var sampleRows int
		cutoff := time.Now().AddDate(0, 0, -30)
		err := pool.QueryRow(ctx, `
			SELECT COALESCE(SUM(pr.qty * pr.unit_price), 0), COUNT(pr.id)
			FROM parse_row pr
			JOIN parse_session ps ON pr.session_id = ps.id
			WHERE ps.supplier_name = $1
			  AND ps.mode = 'purchase'
			  AND pr.qty IS NOT NULL AND pr.unit_price IS NOT NULL
			  AND pr.is_deleted = FALSE
			  AND ps.created_at >= $2
		`, supplier, cutoff).Scan(&windowTotal, &sampleRows)
		if err != nil {
			return SuggestSupplierPaymentResp{}, fmt.Errorf("query base_forecast: %w", err)
		}
		baseDaily := windowTotal / 30.0
		baseForecast := baseDaily * float64(req.PeriodDays)

		// 2) investment_weight: 当月 promotion_fee_share / base_forecast 比例
		monthStart := time.Date(time.Now().Year(), time.Now().Month(), 1, 0, 0, 0, 0, time.UTC)
		monthEnd := monthStart.AddDate(0, 1, 0)
		var totalShare float64
		_ = pool.QueryRow(ctx, `
			SELECT COALESCE(SUM(amount * GREATEST(0, LEAST(period_end, $2::date) - GREATEST(period_start, $1::date))::numeric / NULLIF((period_end - period_start + 1), 0)), 0)
			FROM promotion_fee
			WHERE supplier_name = $3
			  AND period_end >= $1
			  AND period_start < $2
		`, monthStart, monthEnd, supplier).Scan(&totalShare)
		// 投资比例: share / forecast 当月
		monthForecast := baseDaily * 30
		var invWeight float64
		if monthForecast > 0 {
			ratio := totalShare / monthForecast
			invWeight = 0.8 + ratio*1.5 // 0.8 ~ 1.5 区间
			if invWeight < 0.8 {
				invWeight = 0.8
			}
			if invWeight > 1.5 {
				invWeight = 1.5
			}
		} else {
			invWeight = 1.0
		}
		// 3) promo_weight 暂用 1.0
		promoWeight := 1.0
		// 4) sellthrough_weight 暂用 1.0
		sellthroughWeight := 1.0
		// 5) amount
		amount := baseForecast * invWeight * promoWeight * sellthroughWeight

		basis := map[string]any{
			"base_daily":           baseDaily,
			"window_total_30d":     windowTotal,
			"sample_rows":          sampleRows,
			"investment_total":     totalShare,
			"month_forecast":       monthForecast,
			"weights_formula":      "amount = base × inv × promo × sellthru",
			"buffer_factor":        req.BufferFactor,
			"payment_cycle_days":   req.PaymentCycleDays,
		}
		return SuggestSupplierPaymentResp{
			Supplier:          supplier,
			PeriodDays:        req.PeriodDays,
			BaseForecast:      baseForecast,
			InvestmentWeight:  invWeight,
			PromoWeight:       promoWeight,
			SellthroughWeight: sellthroughWeight,
			PaymentCycleDays:  req.PaymentCycleDays,
			BufferFactor:      req.BufferFactor,
			Amount:            amount,
			Basis:             basis,
			Action:            "dry_run",
		}, nil
	}
	return function.NewFunctionTool(fn,
		function.WithName("suggest_supplier_payment"),
		function.WithDescription("基于三维度算法(供应商费用支持/产品促销/产品动销)计算某 supplier 结算建议金额. 期 dry_run=true 显式确认. W5 升级 promo/sellthrough 用 cube 真实数据."),
	)
}

// maxTime / minTime helpers
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
