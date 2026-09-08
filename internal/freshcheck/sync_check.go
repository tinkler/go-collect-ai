package freshcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// SyncChecker C7 同步校验 (W1.6, 2026-09-09)
//
// 职责: 每小时 1 次, 拉 24h 销售总额, 跟思迅源库对账
//   差异 > 0.01% → 写 freshcheck_alert (severity=block, C7)
//   失败 3 次 → 阻断后续 W3 周期结算
//
// 数据流:
//   cube (sales_with_refund 24h sum)  ←  走 CubeQuerier.SalesWithRefundInWindow 聚合
//   source (t_rm_saleflow 24h sum)    ←  走 SourceClient.Aggregate 代理 cube-agent-server
//   两者对比, 差值写入 freshcheck_alert

// SyncChecker C7 同步校验器
type SyncChecker struct {
	Store        *Store
	CubeQuerier  *CubeQuerier
	SourceClient *SourceClient
	BranchNo     string
	// 连续失败计数 (3 次失败告警 block)
	ConsecutiveFails int
	// 上次成功时间
	LastSuccessAt time.Time
}

// NewSyncChecker 构造
func NewSyncChecker(s *Store, cq *CubeQuerier, sc *SourceClient, branchNo string) *SyncChecker {
	return &SyncChecker{
		Store:        s,
		CubeQuerier:  cq,
		SourceClient: sc,
		BranchNo:     branchNo,
	}
}

// SyncResult 一次校验结果
type SyncResult struct {
	WindowStart  time.Time
	WindowEnd    time.Time
	CubeValue    float64
	SourceValue  float64
	DiffAbs      float64
	DiffPct      float64    // 差异百分比 (0.01 = 1%)
	ThresholdPct float64    // 阈值 (从 freshcheck_threshold 读)
	OK           bool       // 差异 < 阈值
	SourceRows   int
	DurationMs   int64
	QuerySQL     string
	ErrorMessage string     // 非空表示整体失败 (cube/source 不可用)
}

// RunOnce 跑一次 C7 校验 (24h 窗口)
//   步骤:
//     1) 拉阈值 c7_sync_diff_pct
//     2) cube 聚合 24h 销售 (sum sale_money)
//     3) source 聚合 24h 销售 (sum sale_money, 走 cube-agent-server /admin/source-direct)
//     4) 对比, 写 alert (失败时)
//   返回 SyncResult, 错误时也返回 (带 ErrorMessage)
func (sc *SyncChecker) RunOnce(ctx context.Context) (*SyncResult, error) {
	// 1. 阈值 (默认 0.01 = 1%)
	threshold, _ := sc.Store.GetThreshold(ctx, ThrC7SyncDiffPct)
	if threshold == 0 {
		threshold = 0.01
	}

	// 2. 窗口: [now - 24h, now]
	end := time.Now()
	start := end.Add(-24 * time.Hour)
	res := &SyncResult{
		WindowStart:  start,
		WindowEnd:    end,
		ThresholdPct: threshold * 100,
	}

	// 3. cube 聚合
	cubeStart := time.Now()
	cubeSum, err := sc.aggregateCubeSales(ctx, start, end)
	if err != nil {
		res.ErrorMessage = "cube aggregate: " + err.Error()
		return res, fmt.Errorf("cube aggregate: %w", err)
	}
	res.CubeValue = cubeSum
	res.DurationMs = time.Since(cubeStart).Milliseconds()

	// 4. source 聚合 (代理)
	sourceStart := time.Now()
	srcResp, err := sc.SourceClient.Aggregate(ctx,
		"t_rm_saleflow", "sum", "sale_money",
		start.Format("2006-01-02 15:04:05"),
		end.Format("2006-01-02 15:04:05"))
	if err != nil {
		res.ErrorMessage = "source aggregate: " + err.Error()
		return res, fmt.Errorf("source aggregate: %w", err)
	}
	res.SourceValue = srcResp.Value
	res.SourceRows = srcResp.RowsCounted
	res.QuerySQL = srcResp.QuerySQL
	res.DurationMs += time.Since(sourceStart).Milliseconds()

	// 5. 对比
	res.DiffAbs = absFloat64(res.CubeValue - res.SourceValue)
	if res.SourceValue != 0 {
		res.DiffPct = (res.DiffAbs / res.SourceValue) * 100
	} else if res.CubeValue == 0 {
		res.DiffPct = 0 // 都是 0, 算 OK
	} else {
		// 源库 0 但 cube 非 0 → 100% 差异 (异常)
		res.DiffPct = 100
	}
	res.OK = res.DiffPct <= threshold*100

	// 6. 失败时写 alert
	if !res.OK {
		sc.ConsecutiveFails++
		severity := "warn"
		if sc.ConsecutiveFails >= 3 {
			severity = "block"
		}
		alert := &Alert{
			BranchNo:   sc.BranchNo,
			RuleCode:   "C7",
			Severity:   severity,
			EntityType: "sync",
			EntityID:   "sales_24h",
			Message: fmt.Sprintf("cube 销售总额 %.2f 跟源库 %.2f 差 %.2f%% (阈值 %.2f%%)",
				res.CubeValue, res.SourceValue, res.DiffPct, res.ThresholdPct),
		}
		_ = sc.Store.WriteAlert(ctx, alert)
		log.Printf("[freshcheck C7] 告警: cube=%.2f source=%.2f diff=%.2f%% severity=%s fails=%d",
			res.CubeValue, res.SourceValue, res.DiffPct, severity, sc.ConsecutiveFails)
	} else {
		// 成功: 重置失败计数
		sc.ConsecutiveFails = 0
		sc.LastSuccessAt = time.Now()
		log.Printf("[freshcheck C7] OK: cube=%.2f source=%.2f diff=%.2f%%", res.CubeValue, res.SourceValue, res.DiffPct)
	}
	return res, nil
}

// aggregateCubeSales 调 cube 拉 24h 销售总额
//   走 SalesWithRefundInWindow (W1.5 加的) + 内存 sum
func (sc *SyncChecker) aggregateCubeSales(ctx context.Context, start, end time.Time) (float64, error) {
	rows, err := sc.CubeQuerier.SalesWithRefundInWindow(ctx, sc.BranchNo, start, end)
	if err != nil {
		return 0, err
	}
	var sum float64
	for _, r := range rows {
		// cube 返回的 key 形如 "sales_with_refund.sale_money"
		// 用 asFloat 兼容 (helper 在 cube.go)
		sum += asFloat(r, "sales_with_refund.sale_money")
	}
	return sum, nil
}

// Start 启动 cron, 每小时一次 (7-21 点)
//   goroutine, ctx 取消时退出
//   不返回 error, 启动失败仅 log
func (sc *SyncChecker) Start(ctx context.Context) {
	go sc.loopHourly(ctx)
}

// loopHourly 每小时一次 (7-21 点)
//   仿 restock 模式
func (sc *SyncChecker) loopHourly(ctx context.Context) {
	log.Printf("[freshcheck C7] cron 启动 (每小时 7-21 点)")
	// 启动后等下一个整点
	now := time.Now()
	next := now.Truncate(time.Hour).Add(time.Hour)
	timer := time.NewTimer(time.Until(next))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[freshcheck C7] cron 停止 (ctx 取消)")
			return
		case <-timer.C:
			hour := time.Now().Hour()
			if hour >= 7 && hour <= 21 {
				runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
				_, err := sc.RunOnce(runCtx)
				if err != nil {
					log.Printf("[freshcheck C7] 校验失败: %v", err)
				}
				cancel()
			}
			// 等下个整点
			timer.Reset(time.Hour)
		}
	}
}

// WriteAlert helper: 直接写 alert (避免暴露 Store.writeAlert 私有)
func (s *Store) WriteAlert(ctx context.Context, a *Alert) error {
	payload, _ := json.Marshal(map[string]any{
		"rule_code":    a.RuleCode,
		"entity_type":  a.EntityType,
		"entity_id":    a.EntityID,
		"severity":     a.Severity,
	})
	_, err := s.pool.Exec(ctx, `
		INSERT INTO freshcheck_alert
			(branch_no, period_id, rule_code, severity, entity_type, entity_id, message, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb)
	`, a.BranchNo, a.PeriodID, a.RuleCode, a.Severity, a.EntityType, a.EntityID, a.Message, payload)
	return err
}

// ============== helpers ==============

func absFloat64(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
