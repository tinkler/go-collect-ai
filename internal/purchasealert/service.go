// Package purchasealert 规则引擎服务
package purchasealert

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tinkler/collect-ai/internal/model"
)

// Service 规则引擎服务
type Service struct {
	pool  *pgxpool.Pool
	rules []Rule
	now   func() time.Time // 注入便于单测
}

// NewService 默认 4 规则
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{
		pool:  pool,
		rules: DefaultRules,
		now:   time.Now,
	}
}

// SetNow 注入 now (单测用)
func (s *Service) SetNow(now func() time.Time) {
	s.now = now
}

// Apply 应用所有规则,返回产生的 alerts (已落库)
func (s *Service) Apply(ctx context.Context, sess *model.Session) ([]Alert, error) {
	if sess == nil {
		return nil, fmt.Errorf("session is nil")
	}
	// 1) 加载上下文
	rc, err := s.loadContext(ctx, sess)
	if err != nil {
		return nil, fmt.Errorf("load context: %w", err)
	}

	// 2) 应用规则
	var alerts []Alert
	// 整 session 级别规则(HolidayLead)只跑一次
	for _, rule := range s.rules {
		if rule.Name() == "holiday_lead" {
			alerts = append(alerts, rule.Apply(ctx, sess, nil, rc)...)
		}
	}
	// row-specific 规则
	for i := range sess.Rows {
		row := &sess.Rows[i]
		if row.IsDeleted {
			continue
		}
		for _, rule := range s.rules {
			if rule.Name() == "holiday_lead" {
				continue // 已跑过
			}
			alerts = append(alerts, rule.Apply(ctx, sess, row, rc)...)
		}
	}

	// 3) 落库
	if len(alerts) > 0 {
		if err := s.insertAlerts(ctx, alerts); err != nil {
			log.Printf("[purchasealert] insertAlerts err: %v", err)
			return alerts, fmt.Errorf("insert alerts: %w", err)
		}
	}
	return alerts, nil
}

// loadContext 加载供应商政策 + 节假日
func (s *Service) loadContext(ctx context.Context, sess *model.Session) (RuleContext, error) {
	rc := RuleContext{
		SupplierPolicies: make(map[string][]PolicyKV),
		Holidays:         []Holiday{},
		Now:              s.now(),
	}

	// 1) 拉 session 内涉及到的所有 supplier 的政策
	suppliers := make(map[string]struct{})
	for _, row := range sess.Rows {
		if !row.IsDeleted && row.MatchedSupp != "" {
			suppliers[row.MatchedSupp] = struct{}{}
		}
	}
	if len(suppliers) > 0 {
		supList := make([]string, 0, len(suppliers))
		for k := range suppliers {
			supList = append(supList, k)
		}
		rows, err := s.pool.Query(ctx, `
			SELECT supplier_name, key, value
			FROM supplier_policy
			WHERE supplier_name = ANY($1)
		`, supList)
		if err != nil {
			return rc, fmt.Errorf("query supplier_policy: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var sup, key string
			var raw []byte
			if err := rows.Scan(&sup, &key, &raw); err != nil {
				return rc, err
			}
			var val any
			_ = json.Unmarshal(raw, &val)
			rc.SupplierPolicies[sup] = append(rc.SupplierPolicies[sup], PolicyKV{Key: key, Val: val})
		}
	}

	// 2) 拉接下来 90 天内的节假日
	rows, err := s.pool.Query(ctx, `
		SELECT date, type, name, lead_days
		FROM special_calendar
		WHERE date >= $1 AND date < $1::date + INTERVAL '90 days'
		ORDER BY date ASC
	`, rc.Now)
	if err != nil {
		return rc, fmt.Errorf("query special_calendar: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var h Holiday
		var d time.Time
		if err := rows.Scan(&d, &h.Type, &h.Name, &h.LeadDays); err != nil {
			return rc, err
		}
		h.Date = d
		rc.Holidays = append(rc.Holidays, h)
	}
	return rc, nil
}

// insertAlerts 批量写 purchase_session_alert
func (s *Service) insertAlerts(ctx context.Context, alerts []Alert) error {
	if s.pool == nil {
		return fmt.Errorf("pg pool nil")
	}
	for _, a := range alerts {
		_, err := s.pool.Exec(ctx, `
			INSERT INTO purchase_session_alert (session_id, row_id, rule, severity, message)
			VALUES ($1, $2, $3, $4, $5)
		`, a.SessID, a.RowID, a.Rule, a.Severity, a.Message)
		if err != nil {
			return fmt.Errorf("insert alert: %w", err)
		}
	}
	return nil
}

// ListAlertsBySession 拉 session 的所有 alerts
func (s *Service) ListAlertsBySession(ctx context.Context, sessionID string) ([]Alert, error) {
	if s.pool == nil {
		return nil, fmt.Errorf("pg pool nil")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, session_id, COALESCE(row_id, 0), rule, severity, message,
		       acked_at, COALESCE(acked_by, ''), created_at
		FROM purchase_session_alert
		WHERE session_id = $1
		ORDER BY severity DESC, created_at ASC
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		var a Alert
		var rowID int64
		if err := rows.Scan(&a.AlertID, &a.SessID, &rowID, &a.Rule, &a.Severity, &a.Message,
			&a.AckedAt, &a.AckedBy, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.RowID = rowID
		if !a.AckedAt.IsZero() {
			a.AckedAt = a.AckedAt
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AckAlert 标记已读
func (s *Service) AckAlert(ctx context.Context, alertID int64, by string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE purchase_session_alert
		SET acked_at = NOW(), acked_by = $2
		WHERE id = $1
	`, alertID, by)
	return err
}
