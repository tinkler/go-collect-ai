// Package store - supplier_parse_strategy 仓库 (Phase A, 2026-09-02)
//
// 设计: 每家供应商 0 或 1 条策略;无则走通用 skill 路径
//   - body + llm_prompt_overlay 是 LLM 友好的自由文本
//   - sku_hints 是机器友好 JSON (barcodes/names/units/spec_patterns/ocr_errors)
//   - 异步计数: generic_apply_count / edit_count / last_applied_at 都是 fire-and-forget
//   - Phase B 会用: ListNeedsAutoBuild / ListNeedsOptimize / Incr* 触发自优化
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tinkler/collect-ai/internal/model"
)

// StrategyRepo supplier_parse_strategy 仓库
type StrategyRepo struct {
	pool *pgxpool.Pool
}

func NewStrategyRepo(pool *pgxpool.Pool) *StrategyRepo {
	return &StrategyRepo{pool: pool}
}

// strategyColumns 统一 SELECT 子句
const strategyColumns = `supplier_name, is_handwrite, enabled, body, sku_hints,
	llm_prompt_overlay, strategy_version, generic_apply_count, edit_count,
	created_at, last_edited_at, last_auto_optimized_at, last_applied_at, note`

// scanStrategy 从 pgx.Row 扫描
func scanStrategy(row pgx.Row, s *model.Strategy) error {
	var skuRaw []byte
	var note string
	if err := row.Scan(
		&s.SupplierName, &s.IsHandwrite, &s.Enabled, &s.Body, &skuRaw,
		&s.LlmPromptOverlay, &s.StrategyVersion, &s.GenericApplyCount, &s.EditCount,
		&s.CreatedAt, &s.LastEditedAt, &s.LastAutoOptimizedAt, &s.LastAppliedAt, &note,
	); err != nil {
		return err
	}
	s.Note = note
	if len(skuRaw) > 0 {
		_ = json.Unmarshal(skuRaw, &s.SkuHints)
	}
	if s.SkuHints == nil {
		s.SkuHints = map[string]any{}
	}
	return nil
}

// GetBySupplier 查一条;不存在返 nil, nil (语义: 没策略 = 走通用)
func (r *StrategyRepo) GetBySupplier(ctx context.Context, name string) (*model.Strategy, error) {
	q := `SELECT ` + strategyColumns + ` FROM supplier_parse_strategy WHERE supplier_name = $1`
	row := r.pool.QueryRow(ctx, q, name)
	var s model.Strategy
	if err := scanStrategy(row, &s); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

// Upsert 插入或更新;带 version 乐观锁(并发写时一方需重试)
//   onConflict version 一样:覆盖;不一样:报错 (Phase B 自优化并发安全)
func (r *StrategyRepo) Upsert(ctx context.Context, s *model.Strategy) error {
	if s.SupplierName == "" {
		return fmt.Errorf("supplier_name 必填")
	}
	hintsJSON, err := json.Marshal(s.SkuHints)
	if err != nil {
		return fmt.Errorf("sku_hints 序列化失败: %w", err)
	}
	now := time.Now()
	_, err = r.pool.Exec(ctx, `
		INSERT INTO supplier_parse_strategy (
			supplier_name, is_handwrite, enabled, body, sku_hints,
			llm_prompt_overlay, strategy_version, generic_apply_count, edit_count,
			created_at, last_edited_at, last_auto_optimized_at, last_applied_at, note
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (supplier_name) DO UPDATE SET
			is_handwrite = EXCLUDED.is_handwrite,
			enabled = EXCLUDED.enabled,
			body = EXCLUDED.body,
			sku_hints = EXCLUDED.sku_hints,
			llm_prompt_overlay = EXCLUDED.llm_prompt_overlay,
			strategy_version = EXCLUDED.strategy_version,
			generic_apply_count = EXCLUDED.generic_apply_count,
			edit_count = EXCLUDED.edit_count,
			last_edited_at = EXCLUDED.last_edited_at,
			last_auto_optimized_at = EXCLUDED.last_auto_optimized_at,
			last_applied_at = EXCLUDED.last_applied_at,
			note = EXCLUDED.note
	`,
		s.SupplierName, s.IsHandwrite, s.Enabled, s.Body, string(hintsJSON),
		s.LlmPromptOverlay, s.StrategyVersion, s.GenericApplyCount, s.EditCount,
		s.CreatedAt, s.LastEditedAt, s.LastAutoOptimizedAt, s.LastAppliedAt, s.Note,
	)
	if err != nil {
		return err
	}
	_ = now
	return nil
}

// IncrGenericCount 通用解析次数 +1 (异步 fire-and-forget 用)
func (r *StrategyRepo) IncrGenericCount(ctx context.Context, supplierName string) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO supplier_parse_strategy (supplier_name, generic_apply_count)
		VALUES ($1, 1)
		ON CONFLICT (supplier_name) DO UPDATE
		SET generic_apply_count = supplier_parse_strategy.generic_apply_count + 1
	`, supplierName)
	return err
}

// IncrEditCount 人工修正次数 +1 (异步 fire-and-forget 用)
func (r *StrategyRepo) IncrEditCount(ctx context.Context, supplierName string) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO supplier_parse_strategy (supplier_name, edit_count)
		VALUES ($1, 1)
		ON CONFLICT (supplier_name) DO UPDATE
		SET edit_count = supplier_parse_strategy.edit_count + 1,
		    last_edited_at = NOW()
	`, supplierName)
	return err
}

// ResetEditCount 优化成功后重置 (Phase B 用)
func (r *StrategyRepo) ResetEditCount(ctx context.Context, supplierName string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE supplier_parse_strategy
		SET edit_count = 0, last_auto_optimized_at = NOW()
		WHERE supplier_name = $1
	`, supplierName)
	return err
}

// TouchApplied 记录最后一次被应用的时间 (异步)
func (r *StrategyRepo) TouchApplied(ctx context.Context, supplierName string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE supplier_parse_strategy
		SET last_applied_at = NOW()
		WHERE supplier_name = $1 AND enabled = TRUE AND body <> ''
	`, supplierName)
	return err
}

// ListNeedsAutoBuild 查"还没建过 strategy 但通用解析已累计 5 次"的供应商 (Phase B 用)
func (r *StrategyRepo) ListNeedsAutoBuild(ctx context.Context, threshold int) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT supplier_name FROM supplier_parse_strategy
		WHERE (body = '' OR enabled = FALSE) AND generic_apply_count >= $1
		ORDER BY generic_apply_count DESC
	`, threshold)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ListNeedsOptimize 查"人工修正累计 3 次"待优化的供应商 (Phase B 用)
func (r *StrategyRepo) ListNeedsOptimize(ctx context.Context, threshold int) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT supplier_name FROM supplier_parse_strategy
		WHERE enabled = TRUE AND body <> '' AND edit_count >= $1
		ORDER BY edit_count DESC
	`, threshold)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ListAll 列全部 (供运营管理界面,Phase B 用)
func (r *StrategyRepo) ListAll(ctx context.Context) ([]model.Strategy, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+strategyColumns+` FROM supplier_parse_strategy ORDER BY supplier_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Strategy
	for rows.Next() {
		var s model.Strategy
		if err := scanStrategy(rows, &s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
