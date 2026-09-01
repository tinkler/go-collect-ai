// Package supplierpayment 供应商结算自动化 (W4.3)
//
// 4 个 cron 任务:
//   1. DailyForecast        每日 21:00 — 算 N 天 forecast → 写 supplier_forecast
//   2. WeeklySuggestions    每周一 09:00 — 跑 suggest_supplier_payment → 写 supplier_payment_suggestion (供 H5 pending 列表)
//   3. MonthlyShare         每月 1 号 02:00 — 跑 promotion_fee_share → 写分摊
//   4. DailyCashCheck       每日 22:00 — cash_balance < sum(pending) → 推 owner 群
//
// 设计: 跟 promotionalert 一致 — 自实现 ticker + goroutine
//   启动时立即跑一次 (立即捕已到期的)
//   每日 21:00 整点
package supplierpayment

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tinkler/collect-ai/internal/store"
)

// Sender 抽象发消息 (跟 agent.Bridge 一致, mock + 默认 wcm)
type Sender interface {
	SendText(ctx context.Context, chatID, text string) error
}

// Service cron 服务
type Service struct {
	pool  *pgxpool.Pool
	cash  *store.CashBalanceRepo
	pay   *store.SupplierPaymentRepo
	fore  *store.SupplierForecastRepo
	share *store.PromotionFeeShareRepo

	// OwnerChatID 现金不足时推的群
	OwnerChatID string

	now func() time.Time
}

// NewService 构造
func NewService(pool *pgxpool.Pool, ownerChatID string) *Service {
	return &Service{
		pool:        pool,
		cash:        store.NewCashBalanceRepo(pool),
		pay:         store.NewSupplierPaymentRepo(pool),
		fore:        store.NewSupplierForecastRepo(pool),
		share:       store.NewPromotionFeeShareRepo(pool),
		OwnerChatID: ownerChatID,
		now:         time.Now,
	}
}

// SetNow 注入 now (单测)
func (s *Service) SetNow(now func() time.Time) {
	s.now = now
}

// ============================================================
// 1. DailyForecast: 跑近 30 天有采购的 supplier, 写 7/30/90 天 forecast
// ============================================================

// RunDailyForecast 跑 daily forecast (供 cron 调用)
//   扫近 30 天有 parse_row 的 supplier
//   对每个 supplier 算 forecast(7/30/90) → 写 supplier_forecast
//   返回: 处理的 supplier 数
func (s *Service) RunDailyForecast(ctx context.Context) (int, error) {
	if s.pool == nil {
		return 0, fmt.Errorf("pool nil")
	}
	// 拉近 30 天 distinct supplier
	cutoff := s.now().AddDate(0, 0, -30)
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ps.supplier_name
		FROM parse_session ps
		WHERE ps.mode = 'purchase' AND ps.created_at >= $1
	`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("query distinct suppliers: %w", err)
	}
	defer rows.Close()

	suppliers := []string{}
	for rows.Next() {
		var sup string
		if err := rows.Scan(&sup); err != nil {
			return 0, err
		}
		suppliers = append(suppliers, sup)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	// 对每个 supplier 算 forecast
	count := 0
	horizons := []int{7, 30, 90}
	for _, sup := range suppliers {
		// 算 base_daily + window_total
		var windowTotal float64
		var sampleRows int
		err := s.pool.QueryRow(ctx, `
			SELECT COALESCE(SUM(pr.qty * pr.unit_price), 0), COUNT(pr.id)
			FROM parse_row pr
			JOIN parse_session ps ON pr.session_id = ps.id
			WHERE ps.supplier_name = $1
			  AND ps.mode = 'purchase'
			  AND pr.qty IS NOT NULL AND pr.unit_price IS NOT NULL
			  AND pr.is_deleted = FALSE
			  AND ps.created_at >= $2
		`, sup, cutoff).Scan(&windowTotal, &sampleRows)
		if err != nil {
			log.Printf("[supplierpayment.DailyForecast] %s query err: %v", sup, err)
			continue
		}
		baseDaily := windowTotal / 30.0
		for _, h := range horizons {
			f := store.SupplierForecast{
				SupplierName: sup,
				ForecastDate: s.now(),
				HorizonDays:  h,
				Amount:       baseDaily * float64(h),
				Basis:        fmt.Sprintf("base_daily=%.2f, sample_rows=%d, window=30d", baseDaily, sampleRows),
			}
			if _, err := s.fore.Insert(ctx, f); err != nil {
				log.Printf("[supplierpayment.DailyForecast] %s horizon=%d insert err: %v", sup, h, err)
			} else {
				count++
			}
		}
	}
	return count, nil
}

// ============================================================
// 2. WeeklySuggestions: 跑 suggest_supplier_payment(30 天周期) → 写 supplier_payment_suggestion
// ============================================================

// RunWeeklySuggestions 跑 weekly 结算建议
//   对每个近 30 天有采购的 supplier, 跑一个 30 天周期结算建议
//   返回: 处理的 supplier 数
func (s *Service) RunWeeklySuggestions(ctx context.Context) (int, error) {
	if s.pool == nil {
		return 0, fmt.Errorf("pool nil")
	}
	cutoff := s.now().AddDate(0, 0, -30)
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ps.supplier_name
		FROM parse_session ps
		WHERE ps.mode = 'purchase' AND ps.created_at >= $1
	`, cutoff)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	suppliers := []string{}
	for rows.Next() {
		var sup string
		if err := rows.Scan(&sup); err != nil {
			return 0, err
		}
		suppliers = append(suppliers, sup)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	count := 0
	for _, sup := range suppliers {
		sugg, err := s.computePaymentSuggestion(ctx, sup, 30)
		if err != nil {
			log.Printf("[supplierpayment.WeeklySuggestions] %s compute err: %v", sup, err)
			continue
		}
		if _, err := s.pay.Insert(ctx, sugg); err != nil {
			log.Printf("[supplierpayment.WeeklySuggestions] %s insert err: %v", sup, err)
			continue
		}
		count++
	}
	return count, nil
}

// computePaymentSuggestion 复用 W4.2 三维度算法 (内联版本, 不调 LLM tool)
func (s *Service) computePaymentSuggestion(ctx context.Context, supplier string, periodDays int) (store.SupplierPaymentSuggestion, error) {
	cutoff := s.now().AddDate(0, 0, -30)
	var windowTotal float64
	var sampleRows int
	err := s.pool.QueryRow(ctx, `
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
		return store.SupplierPaymentSuggestion{}, err
	}
	baseDaily := windowTotal / 30.0
	baseForecast := baseDaily * float64(periodDays)

	// 当月 investment_weight
	now := s.now()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	monthEnd := monthStart.AddDate(0, 1, 0)
	var totalShare float64
	_ = s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount * GREATEST(0, LEAST(period_end, $2::date) - GREATEST(period_start, $1::date))::numeric / NULLIF((period_end - period_start + 1), 0)), 0)
		FROM promotion_fee
		WHERE supplier_name = $3 AND period_end >= $1 AND period_start < $2
	`, monthStart, monthEnd, supplier).Scan(&totalShare)
	monthForecast := baseDaily * 30
	invWeight := 1.0
	if monthForecast > 0 {
		ratio := totalShare / monthForecast
		invWeight = 0.8 + ratio*1.5
		if invWeight < 0.8 {
			invWeight = 0.8
		}
		if invWeight > 1.5 {
			invWeight = 1.5
		}
	}
	promoWeight := 1.0
	sellthroughWeight := 1.0
	amount := baseForecast * invWeight * promoWeight * sellthroughWeight

	basis := map[string]any{
		"base_daily":         baseDaily,
		"window_total_30d":   windowTotal,
		"sample_rows":        sampleRows,
		"investment_total":   totalShare,
		"month_forecast":     monthForecast,
		"weights_formula":    "amount = base × inv × promo × sellthru",
		"computed_at":        now.UTC().Format(time.RFC3339),
	}
	return store.SupplierPaymentSuggestion{
		SupplierName:      supplier,
		PeriodDays:        periodDays,
		BaseForecast:      baseForecast,
		InvestmentWeight:  invWeight,
		PromoWeight:       promoWeight,
		SellthroughWeight: sellthroughWeight,
		PaymentCycleDays:  30,
		Amount:            amount,
		Basis:             basis,
		Status:            "pending",
	}, nil
}

// ============================================================
// 3. MonthlyShare: 跑 promotion_fee_share 上月分摊
// ============================================================

// RunMonthlyShare 跑上月 promotion_fee 分摊
//   对每条 promotion_fee (period 与上月重叠), 写 share 记录
//   返回: 处理的 fee 数
func (s *Service) RunMonthlyShare(ctx context.Context) (int, error) {
	if s.pool == nil {
		return 0, fmt.Errorf("pool nil")
	}
	// 上月
	now := s.now()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).AddDate(0, -1, 0)
	monthEnd := monthStart.AddDate(0, 1, 0)

	rows, err := s.pool.Query(ctx, `
		SELECT id, supplier_name, kind, amount, period_start, period_end
		FROM promotion_fee
		WHERE period_end >= $1 AND period_start < $2
	`, monthStart, monthEnd)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var (
			id, sup, kind string
			amount        float64
			ps, pe        time.Time
		)
		if err := rows.Scan(&id, &sup, &kind, &amount, &ps, &pe); err != nil {
			return count, err
		}
		// 重叠天数
		overlapStart := maxTime(ps, monthStart)
		overlapEnd := minTime(pe.AddDate(0, 0, 1), monthEnd)
		overlapDays := int(overlapEnd.Sub(overlapStart).Hours() / 24)
		if overlapDays < 0 {
			overlapDays = 0
		}
		if err := s.share.Insert(ctx, store.PromotionFeeShare{
			SupplierName: sup,
			ShareMonth:   monthStart,
			Kind:         kind,
			Amount:       amount,
			PeriodStart:  ps,
			PeriodEnd:    pe,
			DaysInMonth:  overlapDays,
			Note:         fmt.Sprintf("fee_id=%s", id),
		}); err != nil {
			log.Printf("[supplierpayment.MonthlyShare] %s insert err: %v", id, err)
			continue
		}
		count++
	}
	return count, rows.Err()
}

// ============================================================
// 4. DailyCashCheck: cash_balance < sum(pending) → 推 owner
// ============================================================

// CashCheckResult 现金检查结果
type CashCheckResult struct {
	BalanceDate  time.Time
	Cash         float64
	PendingTotal float64
	ShortBy      float64 // 缺口 (负数 = 不足)
	Suppliers    int     // pending supplier 数
	AlertPushed  bool
}

// RunDailyCashCheck 检查现金是否够, 不足时推 owner
//   返回 CashCheckResult (供单测/监控)
func (s *Service) RunDailyCashCheck(ctx context.Context, sender Sender) (*CashCheckResult, error) {
	if s.pool == nil {
		return nil, fmt.Errorf("pool nil")
	}
	// 1) 拉最新 cash_balance
	today := s.now().UTC().Truncate(24 * time.Hour)
	cb, err := s.cash.GetByDate(ctx, today)
	if err != nil {
		return nil, fmt.Errorf("get cash: %w", err)
	}
	cash := 0.0
	if cb != nil {
		cash = cb.Amount
	}

	// 2) 求 sum(pending supplier_payment_suggestion)
	pendingList, err := s.pay.ListPending(ctx, 10000)
	if err != nil {
		return nil, fmt.Errorf("list pending: %w", err)
	}
	pendingTotal := 0.0
	for _, p := range pendingList {
		pendingTotal += p.Amount
	}

	res := &CashCheckResult{
		BalanceDate:  today,
		Cash:         cash,
		PendingTotal: pendingTotal,
		Suppliers:    len(pendingList),
		ShortBy:      cash - pendingTotal,
	}

	// 3) 不足 → 推 owner
	if res.ShortBy < 0 && s.OwnerChatID != "" && sender != nil {
		msg := fmt.Sprintf("💸 现金日报 [%s] %.0f 元, 待结算 supplier 货款 %.0f 元, 缺口 %.0f 元 (共 %d 家)\n建议: 增加现金缓冲 / 与供应商协商延期 / 优先结算高 investment 供应商",
			today.Format("01-02"), cash, pendingTotal, -res.ShortBy, len(pendingList))
		if err := sender.SendText(ctx, s.OwnerChatID, msg); err != nil {
			log.Printf("[supplierpayment.DailyCashCheck] SendText err: %v", err)
		} else {
			res.AlertPushed = true
		}
	}
	return res, nil
}

// ============================================================
// helpers
// ============================================================

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
