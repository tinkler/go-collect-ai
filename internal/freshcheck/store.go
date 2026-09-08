package freshcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store freshcheck 模块 PG 仓库 (W1.4 实现)
//
// 职责:
//   - 配置类 CRUD: SkuMap / PoolCode / LossRate / Threshold / CategoryTrack
//   - 人工录入: PoolEvent / PeriodStock (W2 实现)
//   - 配置变更前自动写 ConfigSnap (审计)
//
// 派生表 (Settlement / Alloc / BoxRecon / Alert / LossCalibrate) 走 settle.go 写
type Store struct {
	pool *pgxpool.Pool
}

// NewStore 构造
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// ============== 通用错误 ==============

var (
	ErrNotFound     = errors.New("freshcheck: not found")
	ErrDuplicateKey = errors.New("freshcheck: duplicate key")
	ErrInvalidInput = errors.New("freshcheck: invalid input")
	ErrPeriodLocked = errors.New("freshcheck: period lock exceeded")
	ErrNoStock      = errors.New("freshcheck: period stock missing")
)

// ============== 13 张表存在性验证 (W1.1 smoke test) ==============

// AllFreshcheckTables W1.1 必须存在的 13 张表
var AllFreshcheckTables = []string{
	"freshcheck_sku_map",
	"freshcheck_pool_code",
	"freshcheck_loss_rate",
	"freshcheck_threshold",
	"freshcheck_pool_event",
	"freshcheck_period_stock",
	"freshcheck_category_track",
	"freshcheck_settlement",
	"freshcheck_alloc",
	"freshcheck_box_recon",
	"freshcheck_alert",
	"freshcheck_loss_calibrate",
	"freshcheck_config_snap",
}

// VerifyTablesExist 验证所有 13 张表都建好 (启动 sanity check)
func (s *Store) VerifyTablesExist(ctx context.Context) ([]string, error) {
	missing := []string{}
	for _, t := range AllFreshcheckTables {
		var exists bool
		err := s.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_name = $1 AND table_schema = 'public'
			)
		`, t).Scan(&exists)
		if err != nil {
			return nil, err
		}
		if !exists {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return missing, nil
	}
	return nil, nil
}

// ============== 配置变更快照 (审计) ==============

// WriteConfigSnap 写配置变更快照
//   所有配置表 UPDATE/DELETE 前必调, 失败需回滚业务操作
//   oldValue / newValue: map[string]any, 内部序列化为 JSONB
func (s *Store) WriteConfigSnap(ctx context.Context, tableName, rowPK, action string, oldValue, newValue map[string]any, operator string) error {
	var oldJSON, newJSON []byte
	var err error
	if oldValue != nil {
		oldJSON, err = json.Marshal(oldValue)
		if err != nil {
			return fmt.Errorf("snap marshal old: %w", err)
		}
	}
	if newValue != nil {
		newJSON, err = json.Marshal(newValue)
		if err != nil {
			return fmt.Errorf("snap marshal new: %w", err)
		}
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO freshcheck_config_snap
			(table_name, row_pk, action, old_value, new_value, changed_by)
		VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6)
	`, tableName, rowPK, action, nullableJSON(oldJSON), nullableJSON(newJSON), operator)
	return err
}

func nullableJSON(b []byte) any {
	if b == nil {
		return nil
	}
	return string(b)
}

// ============== 1. freshcheck_sku_map ==============

// UpsertSkuMap 插入或更新 SKU 映射
//   写配置表前先抓旧值, 写 snap 审计, 再 UPDATE
func (s *Store) UpsertSkuMap(ctx context.Context, m *SkuMap) error {
	if m.ItemNo == "" {
		return fmt.Errorf("%w: item_no required", ErrInvalidInput)
	}
	if m.FreshCategory == "" {
		return fmt.Errorf("%w: fresh_category required", ErrInvalidInput)
	}

	// 1. 抓旧值 (for snap)
	var oldRaw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT to_jsonb(t) FROM freshcheck_sku_map t
		WHERE branch_no = $1 AND item_no = $2
	`, m.BranchNo, m.ItemNo).Scan(&oldRaw)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("snap read old: %w", err)
	}

	// 2. UPSERT
	_, err = s.pool.Exec(ctx, `
		INSERT INTO freshcheck_sku_map
			(branch_no, item_no, item_name, fresh_category, turnover_class,
			 shelf_life_days, default_pool_code, is_active, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		ON CONFLICT (branch_no, item_no) DO UPDATE SET
			item_name         = EXCLUDED.item_name,
			fresh_category    = EXCLUDED.fresh_category,
			turnover_class    = EXCLUDED.turnover_class,
			shelf_life_days   = EXCLUDED.shelf_life_days,
			default_pool_code = EXCLUDED.default_pool_code,
			is_active         = EXCLUDED.is_active,
			updated_at        = NOW()
	`, m.BranchNo, m.ItemNo, m.ItemName, m.FreshCategory, m.TurnoverClass,
		m.ShelfLifeDays, m.DefaultPoolCode, m.IsActive)
	if err != nil {
		return err
	}

	// 3. 写 snap
	action := "create"
	var oldMap map[string]any
	if oldRaw != nil {
		action = "update"
		_ = json.Unmarshal(oldRaw, &oldMap)
	}
	newMap := map[string]any{
		"item_no": m.ItemNo, "fresh_category": m.FreshCategory,
		"turnover_class": m.TurnoverClass, "shelf_life_days": m.ShelfLifeDays,
	}
	return s.WriteConfigSnap(ctx, "freshcheck_sku_map", m.ItemNo, action, oldMap, newMap, "")
}

// GetSkuMap 单条查
func (s *Store) GetSkuMap(ctx context.Context, branchNo, itemNo string) (*SkuMap, error) {
	m := &SkuMap{}
	var defaultPool *string
	err := s.pool.QueryRow(ctx, `
		SELECT id, branch_no, item_no, item_name, fresh_category, turnover_class,
		       shelf_life_days, default_pool_code, is_active, created_at, updated_at
		FROM freshcheck_sku_map
		WHERE branch_no = $1 AND item_no = $2
	`, branchNo, itemNo).Scan(
		&m.ID, &m.BranchNo, &m.ItemNo, &m.ItemName, &m.FreshCategory, &m.TurnoverClass,
		&m.ShelfLifeDays, &defaultPool, &m.IsActive, &m.CreatedAt, &m.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.DefaultPoolCode = defaultPool
	return m, nil
}

// ListSkuMapByCategory 列某子类下所有 active SKU
func (s *Store) ListSkuMapByCategory(ctx context.Context, branchNo, freshCategory string) ([]*SkuMap, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, branch_no, item_no, item_name, fresh_category, turnover_class,
		       shelf_life_days, default_pool_code, is_active, created_at, updated_at
		FROM freshcheck_sku_map
		WHERE branch_no = $1 AND fresh_category = $2 AND is_active = TRUE
		ORDER BY item_no
	`, branchNo, freshCategory)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*SkuMap{}
	for rows.Next() {
		m := &SkuMap{}
		var defaultPool *string
		if err := rows.Scan(
			&m.ID, &m.BranchNo, &m.ItemNo, &m.ItemName, &m.FreshCategory, &m.TurnoverClass,
			&m.ShelfLifeDays, &defaultPool, &m.IsActive, &m.CreatedAt, &m.UpdatedAt,
		); err != nil {
			return nil, err
		}
		m.DefaultPoolCode = defaultPool
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListAllSkuMap 列出某门店所有 active SKU
func (s *Store) ListAllSkuMap(ctx context.Context, branchNo string) ([]*SkuMap, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, branch_no, item_no, item_name, fresh_category, turnover_class,
		       shelf_life_days, default_pool_code, is_active, created_at, updated_at
		FROM freshcheck_sku_map
		WHERE branch_no = $1 AND is_active = TRUE
		ORDER BY fresh_category, item_no
	`, branchNo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*SkuMap{}
	for rows.Next() {
		m := &SkuMap{}
		var defaultPool *string
		if err := rows.Scan(
			&m.ID, &m.BranchNo, &m.ItemNo, &m.ItemName, &m.FreshCategory, &m.TurnoverClass,
			&m.ShelfLifeDays, &defaultPool, &m.IsActive, &m.CreatedAt, &m.UpdatedAt,
		); err != nil {
			return nil, err
		}
		m.DefaultPoolCode = defaultPool
		out = append(out, m)
	}
	return out, rows.Err()
}

// ============== 2. freshcheck_pool_code ==============

// UpsertPoolCode 插入或更新特价码
func (s *Store) UpsertPoolCode(ctx context.Context, p *PoolCode) error {
	if p.PoolCode == "" {
		return fmt.Errorf("%w: pool_code required", ErrInvalidInput)
	}

	var oldRaw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT to_jsonb(t) FROM freshcheck_pool_code t
		WHERE branch_no = $1 AND pool_code = $2
	`, p.BranchNo, p.PoolCode).Scan(&oldRaw)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO freshcheck_pool_code
			(branch_no, pool_code, pool_name, pricing_mode, unit_price,
			 piece_weight, priority_rank, is_active, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		ON CONFLICT (branch_no, pool_code) DO UPDATE SET
			pool_name     = EXCLUDED.pool_name,
			pricing_mode  = EXCLUDED.pricing_mode,
			unit_price    = EXCLUDED.unit_price,
			piece_weight  = EXCLUDED.piece_weight,
			priority_rank = EXCLUDED.priority_rank,
			is_active     = EXCLUDED.is_active,
			updated_at    = NOW()
	`, p.BranchNo, p.PoolCode, p.PoolName, p.PricingMode, p.UnitPrice,
		p.PieceWeight, p.PriorityRank, p.IsActive)
	if err != nil {
		return err
	}

	action := "create"
	var oldMap map[string]any
	if oldRaw != nil {
		action = "update"
		_ = json.Unmarshal(oldRaw, &oldMap)
	}
	newMap := map[string]any{
		"pool_code": p.PoolCode, "unit_price": p.UnitPrice,
		"priority_rank": p.PriorityRank,
	}
	return s.WriteConfigSnap(ctx, "freshcheck_pool_code", p.PoolCode, action, oldMap, newMap, "")
}

// ListPoolCodes 列出某门店所有 active 特价码 (按单价降序, 即归因优先级)
func (s *Store) ListPoolCodes(ctx context.Context, branchNo string) ([]*PoolCode, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, branch_no, pool_code, pool_name, pricing_mode, unit_price,
		       piece_weight, priority_rank, is_active, created_at, updated_at
		FROM freshcheck_pool_code
		WHERE branch_no = $1 AND is_active = TRUE
		ORDER BY unit_price DESC
	`, branchNo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*PoolCode{}
	for rows.Next() {
		p := &PoolCode{}
		if err := rows.Scan(
			&p.ID, &p.BranchNo, &p.PoolCode, &p.PoolName, &p.PricingMode, &p.UnitPrice,
			&p.PieceWeight, &p.PriorityRank, &p.IsActive, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPoolCode 单条查
func (s *Store) GetPoolCode(ctx context.Context, branchNo, poolCode string) (*PoolCode, error) {
	p := &PoolCode{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, branch_no, pool_code, pool_name, pricing_mode, unit_price,
		       piece_weight, priority_rank, is_active, created_at, updated_at
		FROM freshcheck_pool_code
		WHERE branch_no = $1 AND pool_code = $2
	`, branchNo, poolCode).Scan(
		&p.ID, &p.BranchNo, &p.PoolCode, &p.PoolName, &p.PricingMode, &p.UnitPrice,
		&p.PieceWeight, &p.PriorityRank, &p.IsActive, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// ============== 3. freshcheck_loss_rate ==============

// UpsertLossRate 插入或更新损耗率
//   同一 (category, turnover, type, effective_from) 唯一
//   每次更新 = 旧记录 effective_to 置 effective_from, 新记录 effective_to = NULL
func (s *Store) UpsertLossRate(ctx context.Context, l *LossRate) error {
	if l.FreshCategory == "" || l.TurnoverClass == "" || l.LossType == "" {
		return fmt.Errorf("%w: category/turnover/loss_type required", ErrInvalidInput)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// 关旧版本: 当前 effective_to IS NULL 的置为新 effective_from
	if l.EffectiveFrom.IsZero() {
		l.EffectiveFrom = time.Now()
	}
	_, err = tx.Exec(ctx, `
		UPDATE freshcheck_loss_rate
		SET effective_to = $1::date
		WHERE fresh_category = $2 AND turnover_class = $3 AND loss_type = $4
		  AND effective_to IS NULL
	`, l.EffectiveFrom.Format("2006-01-02"), l.FreshCategory, l.TurnoverClass, l.LossType)
	if err != nil {
		return fmt.Errorf("close old loss rate: %w", err)
	}

	// 插新版本
	_, err = tx.Exec(ctx, `
		INSERT INTO freshcheck_loss_rate
			(fresh_category, turnover_class, loss_type, daily_rate, effective_from, effective_to,
			 is_calibrated, last_calibrate_at, last_calibrate_by)
		VALUES ($1, $2, $3, $4, $5::date, $6, $7, $8, $9)
	`, l.FreshCategory, l.TurnoverClass, l.LossType, l.DailyRate,
		l.EffectiveFrom.Format("2006-01-02"), l.EffectiveTo,
		l.IsCalibrated, l.LastCalibrateAt, l.LastCalibrateBy)
	if err != nil {
		return fmt.Errorf("insert new loss rate: %w", err)
	}

	return tx.Commit(ctx)
}

// GetActiveLossRate 查当前生效的损耗率
//   effective_to IS NULL AND effective_from <= NOW
//   同 (cat, turnover, type) 多条: 取 effective_from 最大者
func (s *Store) GetActiveLossRate(ctx context.Context, freshCategory, turnoverClass, lossType string) (*LossRate, error) {
	l := &LossRate{}
	var effectiveTo *time.Time
	var lastCalibrateAt *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT id, fresh_category, turnover_class, loss_type, daily_rate,
		       effective_from, effective_to, is_calibrated, last_calibrate_at, last_calibrate_by, created_at
		FROM freshcheck_loss_rate
		WHERE fresh_category = $1 AND turnover_class = $2 AND loss_type = $3
		  AND effective_to IS NULL
		ORDER BY effective_from DESC
		LIMIT 1
	`, freshCategory, turnoverClass, lossType).Scan(
		&l.ID, &l.FreshCategory, &l.TurnoverClass, &l.LossType, &l.DailyRate,
		&l.EffectiveFrom, &effectiveTo, &l.IsCalibrated, &lastCalibrateAt, &l.LastCalibrateBy, &l.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	l.EffectiveTo = effectiveTo
	l.LastCalibrateAt = lastCalibrateAt
	return l, nil
}

// ListLossRates 列出某子类/周转类下所有版本
func (s *Store) ListLossRates(ctx context.Context, freshCategory, turnoverClass string) ([]*LossRate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, fresh_category, turnover_class, loss_type, daily_rate,
		       effective_from, effective_to, is_calibrated, last_calibrate_at, last_calibrate_by, created_at
		FROM freshcheck_loss_rate
		WHERE ($1 = '' OR fresh_category = $1)
		  AND ($2 = '' OR turnover_class = $2)
		ORDER BY fresh_category, turnover_class, loss_type, effective_from DESC
	`, freshCategory, turnoverClass)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*LossRate{}
	for rows.Next() {
		l := &LossRate{}
		var effectiveTo *time.Time
		var lastCalibrateAt *time.Time
		if err := rows.Scan(
			&l.ID, &l.FreshCategory, &l.TurnoverClass, &l.LossType, &l.DailyRate,
			&l.EffectiveFrom, &effectiveTo, &l.IsCalibrated, &lastCalibrateAt, &l.LastCalibrateBy, &l.CreatedAt,
		); err != nil {
			return nil, err
		}
		l.EffectiveTo = effectiveTo
		l.LastCalibrateAt = lastCalibrateAt
		out = append(out, l)
	}
	return out, rows.Err()
}

// ============== 4. freshcheck_threshold ==============

// GetThreshold 查单个阈值
func (s *Store) GetThreshold(ctx context.Context, key string) (float64, error) {
	var v float64
	err := s.pool.QueryRow(ctx, `SELECT value FROM freshcheck_threshold WHERE key = $1`, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return v, err
}

// ListThresholds 列所有阈值
func (s *Store) ListThresholds(ctx context.Context) ([]Threshold, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT key, value, unit, description, updated_at, updated_by
		FROM freshcheck_threshold
		ORDER BY key
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Threshold{}
	for rows.Next() {
		t := Threshold{}
		if err := rows.Scan(&t.Key, &t.Value, &t.Unit, &t.Description, &t.UpdatedAt, &t.UpdatedBy); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateThreshold 改阈值, 写 snap
func (s *Store) UpdateThreshold(ctx context.Context, key string, value float64, operator string) error {
	var oldValue float64
	err := s.pool.QueryRow(ctx, `SELECT value FROM freshcheck_threshold WHERE key = $1`, key).Scan(&oldValue)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: threshold %q", ErrNotFound, key)
		}
		return err
	}

	_, err = s.pool.Exec(ctx, `
		UPDATE freshcheck_threshold
		SET value = $1, updated_at = NOW(), updated_by = $2
		WHERE key = $3
	`, value, operator, key)
	if err != nil {
		return err
	}

	oldMap := map[string]any{"key": key, "value": oldValue}
	newMap := map[string]any{"key": key, "value": value}
	return s.WriteConfigSnap(ctx, "freshcheck_threshold", key, "update", oldMap, newMap, operator)
}

// ============== 5. freshcheck_category_track ==============

// UpsertCategoryTrack 插入或更新品类轨道
func (s *Store) UpsertCategoryTrack(ctx context.Context, t *CategoryTrack) error {
	if t.FreshCategory == "" {
		return fmt.Errorf("%w: fresh_category required", ErrInvalidInput)
	}

	var oldRaw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT to_jsonb(t) FROM freshcheck_category_track t
		WHERE branch_no = $1 AND fresh_category = $2
	`, t.BranchNo, t.FreshCategory).Scan(&oldRaw)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO freshcheck_category_track
			(fresh_category, branch_no, track_code, period_lock_days, stock_freq,
			 require_stock, is_active, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (branch_no, fresh_category) DO UPDATE SET
			track_code       = EXCLUDED.track_code,
			period_lock_days = EXCLUDED.period_lock_days,
			stock_freq       = EXCLUDED.stock_freq,
			require_stock    = EXCLUDED.require_stock,
			is_active        = EXCLUDED.is_active,
			updated_at       = NOW()
	`, t.FreshCategory, t.BranchNo, t.TrackCode, t.PeriodLockDays, t.StockFreq,
		t.RequireStock, t.IsActive)
	if err != nil {
		return err
	}

	action := "create"
	var oldMap map[string]any
	if oldRaw != nil {
		action = "update"
		_ = json.Unmarshal(oldRaw, &oldMap)
	}
	newMap := map[string]any{
		"fresh_category": t.FreshCategory, "track_code": t.TrackCode,
		"period_lock_days": t.PeriodLockDays,
	}
	return s.WriteConfigSnap(ctx, "freshcheck_category_track", t.FreshCategory, action, oldMap, newMap, "")
}

// GetTrack 按 track_code 查
func (s *Store) GetTrack(ctx context.Context, branchNo, trackCode string) (*CategoryTrack, error) {
	t := &CategoryTrack{}
	var lastSettleAt, nextDeadline *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT id, fresh_category, branch_no, track_code, period_lock_days, stock_freq,
		       require_stock, last_settle_at, next_settle_deadline, is_active, created_at, updated_at
		FROM freshcheck_category_track
		WHERE branch_no = $1 AND track_code = $2
	`, branchNo, trackCode).Scan(
		&t.ID, &t.FreshCategory, &t.BranchNo, &t.TrackCode, &t.PeriodLockDays, &t.StockFreq,
		&t.RequireStock, &lastSettleAt, &nextDeadline, &t.IsActive, &t.CreatedAt, &t.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.LastSettleAt = lastSettleAt
	t.NextSettleDeadline = nextDeadline
	return t, nil
}

// GetTrackByCategory 按 fresh_category 查 (1 品类 = 1 轨道)
func (s *Store) GetTrackByCategory(ctx context.Context, branchNo, freshCategory string) (*CategoryTrack, error) {
	t := &CategoryTrack{}
	var lastSettleAt, nextDeadline *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT id, fresh_category, branch_no, track_code, period_lock_days, stock_freq,
		       require_stock, last_settle_at, next_settle_deadline, is_active, created_at, updated_at
		FROM freshcheck_category_track
		WHERE branch_no = $1 AND fresh_category = $2
	`, branchNo, freshCategory).Scan(
		&t.ID, &t.FreshCategory, &t.BranchNo, &t.TrackCode, &t.PeriodLockDays, &t.StockFreq,
		&t.RequireStock, &lastSettleAt, &nextDeadline, &t.IsActive, &t.CreatedAt, &t.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.LastSettleAt = lastSettleAt
	t.NextSettleDeadline = nextDeadline
	return t, nil
}

// ListTracks 列某门店所有 active 轨道
func (s *Store) ListTracks(ctx context.Context, branchNo string) ([]*CategoryTrack, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, fresh_category, branch_no, track_code, period_lock_days, stock_freq,
		       require_stock, last_settle_at, next_settle_deadline, is_active, created_at, updated_at
		FROM freshcheck_category_track
		WHERE branch_no = $1 AND is_active = TRUE
		ORDER BY fresh_category
	`, branchNo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CategoryTrack{}
	for rows.Next() {
		t := &CategoryTrack{}
		var lastSettleAt, nextDeadline *time.Time
		if err := rows.Scan(
			&t.ID, &t.FreshCategory, &t.BranchNo, &t.TrackCode, &t.PeriodLockDays, &t.StockFreq,
			&t.RequireStock, &lastSettleAt, &nextDeadline, &t.IsActive, &t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, err
		}
		t.LastSettleAt = lastSettleAt
		t.NextSettleDeadline = nextDeadline
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateLastSettleAt 结算完成后更新轨道结算时间
//   重新计算 next_settle_deadline = last_settle_at + period_lock_days
func (s *Store) UpdateLastSettleAt(ctx context.Context, id int64, t time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE freshcheck_category_track
		SET last_settle_at = $1,
		    next_settle_deadline = $1 + (period_lock_days || ' days')::interval,
		    updated_at = NOW()
		WHERE id = $2
	`, t, id)
	return err
}
