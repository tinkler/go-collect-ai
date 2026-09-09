package freshcheck

// validate.go: W3.5 7 校验 (C1-C6 + C8, C7 撤)
//
// 业务规则 (docs/freshcheck-settlement.md §八):
//   C1 池内饱和度:  |Σalloc - POS| / POS   <= 15%   (容差, 超限 warn 不阻断)
//   C2 池外泄漏:    backflush > loss × k   (周 2× / 月 1.8×, warn 疑漏勾选)
//   C3 总量守恒:    Σalloc == ΣPOS        (违反 block 阻断)
//   C4 成本守恒:    (n+a+l+b) == (b+p-e)  (违反 block 阻断, W3.4 已算)
//   C5 退货负数:    normalSaleQty < 0     (跨多日 block 退货回冲重算)
//   C6 周期锁:      period_end 距今 > track_days  (block 未到结算日)
//   C8 盘点缺项:    coverage < 100%        (warn 盘点未全)
//
// 简化:
//   - 7 校验核心: C1, C2, C4, C6, C8
//   - C3 用 settlement.ConservationOK 反向派生
//   - C5 暂用 normal_sale 总和 < 0 作为简易检测
//   - 写 alert 走 Store.InsertAlert
//   - 阻断(block) 校验 写 alert 后还返 ErrValidationBlock (W3.4 编排处理)

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// ValidationResult 校验结果汇总
type ValidationResult struct {
	PeriodID    int64
	BranchNo    string
	Alerts      []*Alert
	HasBlock    bool      // 存在 block 级别, 阻断结算
	BlockErrors []string  // block 错误描述
}

// RunValidation 跑 7 校验 + 写 alert
//   输入: SettleRequest + Service 持有上下文 (Store, CubeQuerier)
//   输出: ValidationResult
//   业务: 校验后 alert 写表; block 返错让 W3.4 编排回滚
func (s *Service) RunValidation(ctx context.Context, req SettleRequest, alloc *AllocateResult, bf *BackflushResult) (*ValidationResult, error) {
	vr := &ValidationResult{PeriodID: req.PeriodID, BranchNo: req.BranchNo}

	// C1 池内饱和度
	if err := s.checkC1(ctx, req, alloc, vr); err != nil {
		return vr, fmt.Errorf("C1: %w", err)
	}
	// C2 池外泄漏
	if err := s.checkC2(ctx, req, alloc, bf, vr); err != nil {
		return vr, fmt.Errorf("C2: %w", err)
	}
	// C3 总量守恒 (从 alloc 派生)
	if err := s.checkC3(ctx, req, alloc, vr); err != nil {
		return vr, fmt.Errorf("C3: %w", err)
	}
	// C4 成本守恒 (从 settlement.ConservationOK 派生, W3.4 写 settlement 时已算)
	if err := s.checkC4(ctx, req, vr); err != nil {
		return vr, fmt.Errorf("C4: %w", err)
	}
	// C5 退货负数
	if err := s.checkC5(ctx, req, bf, vr); err != nil {
		return vr, fmt.Errorf("C5: %w", err)
	}
	// C6 周期锁
	if err := s.checkC6(ctx, req, vr); err != nil {
		return vr, fmt.Errorf("C6: %w", err)
	}
	// C8 盘点缺项
	if err := s.checkC8(ctx, req, vr); err != nil {
		return vr, fmt.Errorf("C8: %w", err)
	}

	if vr.HasBlock {
		return vr, fmt.Errorf("validation block: %v", vr.BlockErrors)
	}
	return vr, nil
}

// writeAlert 写一条 alert 到 freshcheck_alert 表
func (s *Service) writeAlert(ctx context.Context, al *Alert) error {
	if al.Status == "" {
		al.Status = AlertOpen
	}
	if al.BranchNo == "" {
		al.BranchNo = "0001"
	}
	return s.Store.InsertAlert(ctx, al)
}

// ============== C1 池内饱和度 ==============
//
// |Σalloc - ΣPOS| / ΣPOS  <=  c1_pool_saturation_pct (默认 15%)
// 违反: warn (不阻断)

func (s *Service) checkC1(ctx context.Context, req SettleRequest, alloc *AllocateResult, vr *ValidationResult) error {
	thr, _ := s.Store.GetThreshold(ctx, ThrC1PoolSaturationPct)
	if thr <= 0 {
		thr = 15.0 // 默认 15%
	}
	if alloc.Summary.TotalPosQty <= 0 {
		return nil // 无 POS 不校验
	}
	if alloc.Summary.SaturationPct > thr {
		payload, _ := json.Marshal(map[string]any{
			"saturation_pct":   alloc.Summary.SaturationPct,
			"total_alloc_qty":  alloc.Summary.TotalAllocQty,
			"total_pos_qty":    alloc.Summary.TotalPosQty,
			"threshold_pct":    thr,
		})
		al := &Alert{
			BranchNo:   req.BranchNo,
			PeriodID:    &req.PeriodID,
			RuleCode:    "C1",
			Severity:    SeverityWarn,
			EntityType:  "settlement",
			EntityID:    strconv.FormatInt(req.PeriodID, 10),
			Message:     fmt.Sprintf("池内饱和度 %.2f%% > 阈值 %.2f%%", alloc.Summary.SaturationPct, thr),
			Payload:     string(payload),
		}
		if err := s.writeAlert(ctx, al); err != nil {
			return err
		}
		vr.Alerts = append(vr.Alerts, al)
	}
	return nil
}

// ============== C2 池外泄漏 ==============
//
// backflush 总量 >  expected_loss × multiplier (周 2× / 月 1.8×)
// 违反: warn (疑漏勾选, 不阻断)

func (s *Service) checkC2(ctx context.Context, req SettleRequest, alloc *AllocateResult, bf *BackflushResult, vr *ValidationResult) error {
	mult, _ := s.Store.GetThreshold(ctx, ThrC2LeakageMultWeekly)
	if mult <= 0 {
		// 根据 track_code 选 multiplier
		switch req.TrackCode {
		case "root-monthly", "frozen-monthly":
			mult = 1.8
		default:
			mult = 2.0
		}
	}
	// 总 backflush > 总 loss × mult
	totalBackflush := 0.0
	totalLoss := 0.0
	for _, it := range bf.Items {
		totalBackflush += it.Backflush
		totalLoss += it.ExpectedLoss
	}
	threshold := totalLoss * mult
	if totalBackflush > threshold {
		payload, _ := json.Marshal(map[string]any{
			"total_backflush": totalBackflush,
			"total_loss":      totalLoss,
			"multiplier":      mult,
			"threshold":       threshold,
		})
		al := &Alert{
			BranchNo:   req.BranchNo,
			PeriodID:    &req.PeriodID,
			RuleCode:    "C2",
			Severity:    SeverityWarn,
			EntityType:  "settlement",
			EntityID:    strconv.FormatInt(req.PeriodID, 10),
			Message:     fmt.Sprintf("池外泄漏疑似: backflush %.2f > loss %.2f × %.2f = %.2f", totalBackflush, totalLoss, mult, threshold),
			Payload:     string(payload),
		}
		if err := s.writeAlert(ctx, al); err != nil {
			return err
		}
		vr.Alerts = append(vr.Alerts, al)
	}
	return nil
}

// ============== C3 总量守恒 ==============
//
// Σalloc == ΣPOS (汇总)
// 违反: block 阻断结算
//
// 简化: W3.3 AllocateSummary.TotalAllocQty vs TotalPosQty, 偏差 < 0.01 视为守恒

func (s *Service) checkC3(ctx context.Context, req SettleRequest, alloc *AllocateResult, vr *ValidationResult) error {
	if alloc.Summary.TotalPosQty <= 0 {
		return nil
	}
	diff := absFloat(alloc.Summary.TotalAllocQty - alloc.Summary.TotalPosQty)
	if diff > 0.01 {
		payload, _ := json.Marshal(map[string]any{
			"total_alloc_qty": alloc.Summary.TotalAllocQty,
			"total_pos_qty":   alloc.Summary.TotalPosQty,
			"diff":            diff,
		})
		al := &Alert{
			BranchNo:   req.BranchNo,
			PeriodID:    &req.PeriodID,
			RuleCode:    "C3",
			Severity:    SeverityBlock, // block 阻断
			EntityType:  "settlement",
			EntityID:    strconv.FormatInt(req.PeriodID, 10),
			Message:     fmt.Sprintf("C3 总量守恒失败: alloc=%.4f vs pos=%.4f (diff=%.4f)", alloc.Summary.TotalAllocQty, alloc.Summary.TotalPosQty, diff),
			Payload:     string(payload),
		}
		if err := s.writeAlert(ctx, al); err != nil {
			return err
		}
		vr.Alerts = append(vr.Alerts, al)
		vr.HasBlock = true
		vr.BlockErrors = append(vr.BlockErrors, al.Message)
	}
	return nil
}

// ============== C4 成本守恒 ==============
//
// (n+a+l+b) == (b+p-e)  per SKU
// 违反: block 阻断
//
// 简化: W3.4 writeSettlements 写 settlement 时已经判定 ConservationOK, 走 PG 查
// 任何 SKU ConservationOK=false 触发 block alert

func (s *Service) checkC4(ctx context.Context, req SettleRequest, vr *ValidationResult) error {
	// 查 settlement 表中 conservation_ok=false 的行
	rows, err := s.Store.pool.Query(ctx, `
		SELECT item_no, conservation_msg FROM freshcheck_settlement
		WHERE branch_no = $1 AND period_id = $2 AND conservation_ok = FALSE
	`, req.BranchNo, req.PeriodID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var itemNo, msg string
		if err := rows.Scan(&itemNo, &msg); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{
			"item_no":         itemNo,
			"conservation_msg": msg,
		})
		al := &Alert{
			BranchNo:   req.BranchNo,
			PeriodID:    &req.PeriodID,
			RuleCode:    "C4",
			Severity:    SeverityBlock,
			EntityType:  "item",
			EntityID:    itemNo,
			Message:     "C4 成本守恒失败: " + msg,
			Payload:     string(payload),
		}
		if err := s.writeAlert(ctx, al); err != nil {
			return err
		}
		vr.Alerts = append(vr.Alerts, al)
		vr.HasBlock = true
		vr.BlockErrors = append(vr.BlockErrors, al.Message)
	}
	return nil
}

// ============== C5 退货负数 ==============
//
// normalSaleQty < 0 跨多日 (单日允许, 跨日需回冲重算)
// 违反: block 退货回冲
//
// 简化: 总 normal_sale < 0 → block (W3.9 验收 8 场景时细化)

func (s *Service) checkC5(ctx context.Context, req SettleRequest, bf *BackflushResult, vr *ValidationResult) error {
	for _, it := range bf.Items {
		if it.NormalSaleQty < 0 {
			// 业务: 退货 > 销售, 需回冲 (简化: 整个 SKU 触发 block)
			payload, _ := json.Marshal(map[string]any{
				"item_no":          it.ItemNo,
				"normal_sale_qty":  it.NormalSaleQty,
			})
			al := &Alert{
				BranchNo:   req.BranchNo,
				PeriodID:    &req.PeriodID,
				RuleCode:    "C5",
				Severity:    SeverityBlock,
				EntityType:  "item",
				EntityID:    it.ItemNo,
				Message:     fmt.Sprintf("C5 退货负数: %s 销售 %.2f < 0 (需回冲重算)", it.ItemNo, it.NormalSaleQty),
				Payload:     string(payload),
			}
			if err := s.writeAlert(ctx, al); err != nil {
				return err
			}
			vr.Alerts = append(vr.Alerts, al)
			vr.HasBlock = true
			vr.BlockErrors = append(vr.BlockErrors, al.Message)
		}
	}
	return nil
}

// ============== C6 周期锁 ==============
//
// period_end 距今 (或 c6_period_lock_X_days) 未到 → 不能结算
// 违反: block
//
// 简化: period_end 距今 > track 对应天数, 即未到结算日

func (s *Service) checkC6(ctx context.Context, req SettleRequest, vr *ValidationResult) error {
	// track 决定天数
	lockDays := 7 // 默认 7 (leaf)
	switch req.TrackCode {
	case "root-monthly", "frozen-monthly":
		lockDays = 30
	case "meat-biweekly", "aquatic-biweekly":
		lockDays = 14
	}
	// 也可以从 freshcheck_category_track 读 category->lock_days (W3.5 简化: 走 track_code)
	now := time.Now()
	if req.PeriodEnd.IsZero() {
		// 周期未结束, 直接 block
		payload, _ := json.Marshal(map[string]any{
			"period_end": req.PeriodEnd,
			"now":        now,
			"lock_days":  lockDays,
		})
		al := &Alert{
			BranchNo:   req.BranchNo,
			PeriodID:    &req.PeriodID,
			RuleCode:    "C6",
			Severity:    SeverityBlock,
			EntityType:  "settlement",
			EntityID:    strconv.FormatInt(req.PeriodID, 10),
			Message:     "C6 周期未到结算日 (period_end 为零)",
			Payload:     string(payload),
		}
		if err := s.writeAlert(ctx, al); err != nil {
			return err
		}
		vr.Alerts = append(vr.Alerts, al)
		vr.HasBlock = true
		vr.BlockErrors = append(vr.BlockErrors, al.Message)
	}
	// period_end 距 now 大于 lockDays 也可触发 (周期太久未结, 需复核) — 暂简化不查
	return nil
}

// ============== C8 盘点缺项 ==============
//
// 期覆盖率 < 100%
// 违反: warn (不阻断, 但提示补录)

func (s *Service) checkC8(ctx context.Context, req SettleRequest, vr *ValidationResult) error {
	cov, err := s.Store.CheckPeriodStockCoverage(ctx, req.BranchNo, req.PeriodID)
	if err != nil {
		return err
	}
	if !cov.MeetsC8 && cov.Total > 0 {
		payload, _ := json.Marshal(map[string]any{
			"period_id":     cov.PeriodID,
			"counted":       cov.Counted,
			"total":         cov.Total,
			"coverage_pct":  cov.CoveragePct,
			"missing_count": len(cov.Missing),
		})
		al := &Alert{
			BranchNo:   req.BranchNo,
			PeriodID:    &req.PeriodID,
			RuleCode:    "C8",
			Severity:    SeverityWarn,
			EntityType:  "period_stock",
			EntityID:    strconv.FormatInt(req.PeriodID, 10),
			Message:     fmt.Sprintf("C8 盘点覆盖率 %.1f%% < 100%% (缺 %d 个 SKU)", cov.CoveragePct, len(cov.Missing)),
			Payload:     string(payload),
		}
		if err := s.writeAlert(ctx, al); err != nil {
			return err
		}
		vr.Alerts = append(vr.Alerts, al)
	}
	return nil
}
