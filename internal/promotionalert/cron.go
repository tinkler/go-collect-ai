// Package promotionalert 促销费用到期预警 (W3.3)
//
// 职责: 每日扫描 promotion_fee, 未来 N 天内 period_end 到期的费用 → 推企微群
//
// 数据源: promotion_fee 表 (W1 已建, A 模块录入)
//
// 推送策略:
//   - 按 supplier_name 分组汇总 (避免一供应商多条刷屏)
//   - 推 office 群 (解耦于 Agent Bridge)
//   - 每天 21:00 跑一次 + 启动时立即跑一次
package promotionalert

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Sender 抽象发消息接口 (跟 agent.Bridge 一样, 便于 mock)
type Sender interface {
	SendText(ctx context.Context, chatID, text string) error
}

// Service cron 服务
type Service struct {
	pool *pgxpool.Pool
	// ChatID 推哪个群 (office 群, env 显式注入)
	ChatID string
	// DaysAhead 预警窗口 (默认 7 天)
	DaysAhead int
	// now 注入便于单测
	now func() time.Time
}

// NewService 默认 7 天窗口
func NewService(pool *pgxpool.Pool, chatID string) *Service {
	return &Service{
		pool:      pool,
		ChatID:    chatID,
		DaysAhead: 7,
		now:       time.Now,
	}
}

// SetNow 注入 now (单测)
func (s *Service) SetNow(now func() time.Time) {
	s.now = now
}

// ExpiringFee 单条到期费用
type ExpiringFee struct {
	FeeID       int64
	Supplier    string
	Kind        string
	Amount      float64
	PeriodStart time.Time
	PeriodEnd   time.Time
	Note        string
	DaysLeft    int // 距今天数
}

// RunOnce 跑一次扫描, 返回需要推送的费用清单 (按 supplier 分组)
func (s *Service) RunOnce(ctx context.Context) (map[string][]ExpiringFee, error) {
	if s.pool == nil {
		return nil, fmt.Errorf("pg pool nil")
	}
	now := s.now()
	cutoff := now.AddDate(0, 0, s.DaysAhead)

	rows, err := s.pool.Query(ctx, `
		SELECT id, supplier_name, kind, amount, period_start, period_end, COALESCE(note,'')
		FROM promotion_fee
		WHERE period_end >= $1 AND period_end <= $2
		ORDER BY supplier_name ASC, period_end ASC
	`, now, cutoff)
	if err != nil {
		return nil, fmt.Errorf("query expiring fees: %w", err)
	}
	defer rows.Close()

	grouped := make(map[string][]ExpiringFee)
	for rows.Next() {
		var f ExpiringFee
		var start, end time.Time
		if err := rows.Scan(&f.FeeID, &f.Supplier, &f.Kind, &f.Amount, &start, &end, &f.Note); err != nil {
			return nil, err
		}
		f.PeriodStart = start
		f.PeriodEnd = end
		f.DaysLeft = int(end.Sub(now).Hours() / 24)
		grouped[f.Supplier] = append(grouped[f.Supplier], f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return grouped, nil
}

// Push 推送到指定 chat_id (按 supplier 分组生成短消息, 总数 ≤ 5 条)
func (s *Service) Push(ctx context.Context, sender Sender, grouped map[string][]ExpiringFee) error {
	if s.ChatID == "" {
		log.Printf("[promotionalert] ChatID 未配置, 跳过推送")
		return nil
	}
	if sender == nil {
		return fmt.Errorf("sender nil")
	}
	if len(grouped) == 0 {
		log.Printf("[promotionalert] 无即将到期费用, 跳过推送")
		return nil
	}
	// 每天最多 5 条 (频控, 避免多供应商刷屏)
	const maxMsgs = 5
	sent := 0
	for sup, fees := range grouped {
		if sent >= maxMsgs {
			log.Printf("[promotionalert] 达到 %d 条上限, 剩余 %d 供应商不推", maxMsgs, len(grouped)-sent)
			break
		}
		msg := formatExpiringMsg(sup, fees)
		if err := sender.SendText(ctx, s.ChatID, msg); err != nil {
			log.Printf("[promotionalert] SendText err sup=%s: %v", sup, err)
			continue
		}
		sent++
	}
	return nil
}

// formatExpiringMsg 单供应商多笔费用汇总成一条短消息
func formatExpiringMsg(supplier string, fees []ExpiringFee) string {
	if len(fees) == 1 {
		f := fees[0]
		return fmt.Sprintf("⏰ [%s] %s %.0f 元 还有 %d 天到期 (到 %s)",
			supplier, f.Kind, f.Amount, f.DaysLeft, f.PeriodEnd.Format("01-02"))
	}
	// 多笔: 列表
	var totalAmount float64
	minDays := fees[0].DaysLeft
	var sb fmt.Stringer = nil
	_ = sb
	msg := fmt.Sprintf("⏰ [%s] 即将到期的促销费用 (%d 笔):\n", supplier, len(fees))
	for i, f := range fees {
		if i >= 4 { // 最多列 4 笔
			msg += fmt.Sprintf("  ...还有 %d 笔\n", len(fees)-4)
			break
		}
		msg += fmt.Sprintf("  • %s %.0f 元 (剩 %d 天, 到 %s)\n",
			f.Kind, f.Amount, f.DaysLeft, f.PeriodEnd.Format("01-02"))
		totalAmount += f.Amount
		if f.DaysLeft < minDays {
			minDays = f.DaysLeft
		}
	}
	msg += fmt.Sprintf("合计 %.0f 元, 最近一笔 %d 天后到期", totalAmount, minDays)
	return msg
}

// RunAndPush 一次: scan + push (主入口, cron 调用)
func (s *Service) RunAndPush(ctx context.Context, sender Sender) error {
	grouped, err := s.RunOnce(ctx)
	if err != nil {
		return err
	}
	return s.Push(ctx, sender, grouped)
}
