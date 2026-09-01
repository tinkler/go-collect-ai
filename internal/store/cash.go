// Package store 现金日报 + 供应商结算仓储 (W4)
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CashBalance 现金日报
type CashBalance struct {
	ID         int64     `json:"id"`
	BalanceDate time.Time `json:"balance_date"`
	Amount     float64   `json:"amount"`
	Source     string    `json:"source"`
	Note       string    `json:"note,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// CashBalanceRepo 现金日报 CRUD
type CashBalanceRepo struct {
	pool *pgxpool.Pool
}

func NewCashBalanceRepo(pool *pgxpool.Pool) *CashBalanceRepo {
	return &CashBalanceRepo{pool: pool}
}

// Upsert 手动录入: 同一天只能 1 行 (UNIQUE balance_date)
func (r *CashBalanceRepo) Upsert(ctx context.Context, date time.Time, amount float64, source, note, by string) error {
	if r.pool == nil {
		return fmt.Errorf("pg pool nil")
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO cash_balance (balance_date, amount, source, note)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (balance_date) DO UPDATE
		SET amount = EXCLUDED.amount,
		    source = EXCLUDED.source,
		    note = EXCLUDED.note
	`, date, amount, source, note)
	return err
}

// GetByDate 拉某天
func (r *CashBalanceRepo) GetByDate(ctx context.Context, date time.Time) (*CashBalance, error) {
	if r.pool == nil {
		return nil, fmt.Errorf("pg pool nil")
	}
	var c CashBalance
	err := r.pool.QueryRow(ctx, `
		SELECT id, balance_date, amount, source, COALESCE(note,''), created_at
		FROM cash_balance WHERE balance_date = $1
	`, date).Scan(&c.ID, &c.BalanceDate, &c.Amount, &c.Source, &c.Note, &c.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// GetLatest 拉最近 N 天
func (r *CashBalanceRepo) GetLatest(ctx context.Context, days int) ([]CashBalance, error) {
	if r.pool == nil {
		return nil, fmt.Errorf("pg pool nil")
	}
	if days <= 0 {
		days = 7
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, balance_date, amount, source, COALESCE(note,''), created_at
		FROM cash_balance
		ORDER BY balance_date DESC
		LIMIT $1
	`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CashBalance
	for rows.Next() {
		var c CashBalance
		if err := rows.Scan(&c.ID, &c.BalanceDate, &c.Amount, &c.Source, &c.Note, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ============================================================
// 供应商结算仓储
// ============================================================

// SupplierForecast 预测记录
type SupplierForecast struct {
	ID           int64     `json:"id"`
	SupplierName string    `json:"supplier_name"`
	ForecastDate time.Time `json:"forecast_date"`
	HorizonDays  int       `json:"horizon_days"`
	Amount       float64   `json:"amount"`
	Basis        string    `json:"basis,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// SupplierForecastRepo 预测 CRUD
type SupplierForecastRepo struct {
	pool *pgxpool.Pool
}

func NewSupplierForecastRepo(pool *pgxpool.Pool) *SupplierForecastRepo {
	return &SupplierForecastRepo{pool: pool}
}

// Insert 写一条预测
func (r *SupplierForecastRepo) Insert(ctx context.Context, f SupplierForecast) (int64, error) {
	if r.pool == nil {
		return 0, fmt.Errorf("pg pool nil")
	}
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO supplier_forecast (supplier_name, forecast_date, horizon_days, amount, basis)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`, f.SupplierName, f.ForecastDate, f.HorizonDays, f.Amount, f.Basis).Scan(&id)
	return id, err
}

// GetLatest 拉某 supplier 最新预测 (按 horizon_days 过滤)
func (r *SupplierForecastRepo) GetLatest(ctx context.Context, supplier string, horizonDays int) (*SupplierForecast, error) {
	if r.pool == nil {
		return nil, fmt.Errorf("pg pool nil")
	}
	var f SupplierForecast
	err := r.pool.QueryRow(ctx, `
		SELECT id, supplier_name, forecast_date, horizon_days, amount, COALESCE(basis,''), created_at
		FROM supplier_forecast
		WHERE supplier_name = $1 AND horizon_days = $2
		ORDER BY created_at DESC
		LIMIT 1
	`, supplier, horizonDays).Scan(&f.ID, &f.SupplierName, &f.ForecastDate, &f.HorizonDays, &f.Amount, &f.Basis, &f.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// ============================================================
// SupplierPaymentSuggestion
// ============================================================

// SupplierPaymentSuggestion 结算建议
type SupplierPaymentSuggestion struct {
	ID                int64                  `json:"id"`
	SupplierName      string                 `json:"supplier_name"`
	PeriodDays        int                    `json:"period_days"`
	BaseForecast      float64                `json:"base_forecast"`
	InvestmentWeight  float64                `json:"investment_weight"`
	PromoWeight       float64                `json:"promo_weight"`
	SellthroughWeight float64                `json:"sellthrough_weight"`
	PaymentCycleDays  int                    `json:"payment_cycle_days"`
	Amount            float64                `json:"amount"`
	Basis             map[string]any         `json:"basis"`
	Status            string                 `json:"status"`
	AckedBy           string                 `json:"acked_by,omitempty"`
	AckedAt           *time.Time             `json:"acked_at,omitempty"`
	CreatedAt         time.Time              `json:"created_at"`
}

type SupplierPaymentRepo struct {
	pool *pgxpool.Pool
}

func NewSupplierPaymentRepo(pool *pgxpool.Pool) *SupplierPaymentRepo {
	return &SupplierPaymentRepo{pool: pool}
}

// Insert 写一条建议
func (r *SupplierPaymentRepo) Insert(ctx context.Context, s SupplierPaymentSuggestion) (int64, error) {
	if r.pool == nil {
		return 0, fmt.Errorf("pg pool nil")
	}
	basisJSON, _ := jsonMarshal(s.Basis)
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO supplier_payment_suggestion
		(supplier_name, period_days, base_forecast, investment_weight, promo_weight, sellthrough_weight, payment_cycle_days, amount, basis)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb)
		RETURNING id
	`,
		s.SupplierName, s.PeriodDays, s.BaseForecast, s.InvestmentWeight, s.PromoWeight, s.SellthroughWeight,
		s.PaymentCycleDays, s.Amount, basisJSON,
	).Scan(&id)
	return id, err
}

// GetLatest 拉某 supplier 最新 pending
func (r *SupplierPaymentRepo) GetLatest(ctx context.Context, supplier string) (*SupplierPaymentSuggestion, error) {
	if r.pool == nil {
		return nil, fmt.Errorf("pg pool nil")
	}
	var s SupplierPaymentSuggestion
	var basisJSON []byte
	var ackedAt *time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT id, supplier_name, period_days, base_forecast, investment_weight, promo_weight, sellthrough_weight, payment_cycle_days, amount, basis, status, COALESCE(acked_by,''), acked_at, created_at
		FROM supplier_payment_suggestion
		WHERE supplier_name = $1
		ORDER BY created_at DESC
		LIMIT 1
	`, supplier).Scan(&s.ID, &s.SupplierName, &s.PeriodDays, &s.BaseForecast, &s.InvestmentWeight, &s.PromoWeight, &s.SellthroughWeight, &s.PaymentCycleDays, &s.Amount, &basisJSON, &s.Status, &s.AckedBy, &ackedAt, &s.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.AckedAt = ackedAt
	_ = jsonUnmarshal(basisJSON, &s.Basis)
	return &s, nil
}

// ListPending 拉所有 pending 建议
func (r *SupplierPaymentRepo) ListPending(ctx context.Context, limit int) ([]SupplierPaymentSuggestion, error) {
	if r.pool == nil {
		return nil, fmt.Errorf("pg pool nil")
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, supplier_name, period_days, base_forecast, investment_weight, promo_weight, sellthrough_weight, payment_cycle_days, amount, basis, status, COALESCE(acked_by,''), acked_at, created_at
		FROM supplier_payment_suggestion
		WHERE status = 'pending'
		ORDER BY amount DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SupplierPaymentSuggestion
	for rows.Next() {
		var s SupplierPaymentSuggestion
		var basisJSON []byte
		var ackedAt *time.Time
		if err := rows.Scan(&s.ID, &s.SupplierName, &s.PeriodDays, &s.BaseForecast, &s.InvestmentWeight, &s.PromoWeight, &s.SellthroughWeight, &s.PaymentCycleDays, &s.Amount, &basisJSON, &s.Status, &s.AckedBy, &ackedAt, &s.CreatedAt); err != nil {
			return nil, err
		}
		s.AckedAt = ackedAt
		_ = jsonUnmarshal(basisJSON, &s.Basis)
		out = append(out, s)
	}
	return out, rows.Err()
}

// Ack 标记已确认
func (r *SupplierPaymentRepo) Ack(ctx context.Context, id int64, by string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE supplier_payment_suggestion
		SET status = 'acknowledged', acked_by = $2, acked_at = NOW()
		WHERE id = $1 AND status = 'pending'
	`, id, by)
	return err
}

// ============================================================
// PromotionFeeShare 分摊
// ============================================================

// PromotionFeeShare 分摊记录
type PromotionFeeShare struct {
	ID            int64     `json:"id"`
	SupplierName  string    `json:"supplier_name"`
	ShareMonth    time.Time `json:"share_month"`
	Kind          string    `json:"kind"`
	Amount        float64   `json:"amount"`
	PeriodStart   time.Time `json:"period_start"`
	PeriodEnd     time.Time `json:"period_end"`
	DaysInMonth   int       `json:"days_in_month"`
	Note          string    `json:"note,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

type PromotionFeeShareRepo struct {
	pool *pgxpool.Pool
}

func NewPromotionFeeShareRepo(pool *pgxpool.Pool) *PromotionFeeShareRepo {
	return &PromotionFeeShareRepo{pool: pool}
}

// Insert 写一条分摊
func (r *PromotionFeeShareRepo) Insert(ctx context.Context, s PromotionFeeShare) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO promotion_fee_share
		(supplier_name, share_month, kind, amount, period_start, period_end, days_in_month, note)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, s.SupplierName, s.ShareMonth, s.Kind, s.Amount, s.PeriodStart, s.PeriodEnd, s.DaysInMonth, s.Note)
	return err
}

// ListByMonth 拉某月所有分摊
func (r *PromotionFeeShareRepo) ListByMonth(ctx context.Context, month time.Time) ([]PromotionFeeShare, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, supplier_name, share_month, kind, amount, period_start, period_end, days_in_month, COALESCE(note,''), created_at
		FROM promotion_fee_share
		WHERE share_month = $1
		ORDER BY supplier_name, kind
	`, month)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PromotionFeeShare
	for rows.Next() {
		var s PromotionFeeShare
		if err := rows.Scan(&s.ID, &s.SupplierName, &s.ShareMonth, &s.Kind, &s.Amount, &s.PeriodStart, &s.PeriodEnd, &s.DaysInMonth, &s.Note, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ============================================================
// JSON 帮手 (避免引入 encoding/json)
// ============================================================

func jsonMarshal(v any) (string, error) {
	if v == nil {
		return "{}", nil
	}
	bs, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(bs), nil
}

func jsonUnmarshal(data []byte, v any) error {
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, v)
}
