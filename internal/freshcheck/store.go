package freshcheck

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store freshcheck 模块 PG 仓库 (W1.1 骨架, W1.4 填实现)
//
// 职责 (实现顺序见 docs/freshcheck-rollout.md W1.4):
//   - 配置类 CRUD: SkuMap / PoolCode / LossRate / Threshold / CategoryTrack
//   - 人工录入: PoolEvent / PeriodStock
//   - 配置变更前自动写 ConfigSnap (审计)
//
// 派生表 (Settlement / Alloc / BoxRecon / Alert / LossCalibrate) 走 settle.go / validate.go / loss_rate.go 写
type Store struct {
	pool *pgxpool.Pool
}

// NewStore 构造
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// ============== 通用错误 ==============

var (
	ErrNotFound       = errors.New("freshcheck: not found")
	ErrDuplicateKey   = errors.New("freshcheck: duplicate key")
	ErrInvalidInput   = errors.New("freshcheck: invalid input")
	ErrPeriodLocked   = errors.New("freshcheck: period lock exceeded")
	ErrNoStock        = errors.New("freshcheck: period stock missing")
)

// ============== PG 表存在性验证 (W1.1 smoke test) ==============

// AllFreshcheckTables W1.1 必须存在的 13 张表
//   跟 docs/freshcheck-data-model.md 严格对齐
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
//   返回缺失的表名列表; 全部齐返 nil
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

// ============== 配置表通用: 写 snap helper ==============

// WriteConfigSnap 写配置变更快照 (审计)
//   所有配置表 UPDATE/DELETE 前必调
//   oldValue / newValue 序列化为 JSON string
//   W1.4 实现
func (s *Store) WriteConfigSnap(ctx context.Context, tableName, rowPK, action, oldValue, newValue, operator string) error {
	// TODO W1.4
	_ = ctx
	_ = tableName
	_ = rowPK
	_ = action
	_ = oldValue
	_ = newValue
	_ = operator
	return nil
}

// ============== W1.4 实现的 stub 接口 (现在不填, W1.4 来) ==============

// ----- SkuMap -----
func (s *Store) UpsertSkuMap(ctx context.Context, m *SkuMap) error     { return nil }
func (s *Store) GetSkuMap(ctx context.Context, branchNo, itemNo string) (*SkuMap, error) { return nil, ErrNotFound }
func (s *Store) ListSkuMapByCategory(ctx context.Context, branchNo, freshCategory string) ([]*SkuMap, error) {
	return nil, nil
}

// ----- PoolCode -----
func (s *Store) UpsertPoolCode(ctx context.Context, p *PoolCode) error  { return nil }
func (s *Store) ListPoolCodes(ctx context.Context, branchNo string) ([]*PoolCode, error) { return nil, nil }

// ----- LossRate -----
func (s *Store) UpsertLossRate(ctx context.Context, l *LossRate) error  { return nil }
func (s *Store) GetActiveLossRate(ctx context.Context, freshCategory, turnoverClass, lossType string) (*LossRate, error) {
	return nil, ErrNotFound
}

// ----- Threshold -----
func (s *Store) GetThreshold(ctx context.Context, key string) (float64, error) { return 0, ErrNotFound }
func (s *Store) ListThresholds(ctx context.Context) ([]Threshold, error) { return nil, nil }
func (s *Store) UpdateThreshold(ctx context.Context, key string, value float64, operator string) error { return nil }

// ----- CategoryTrack -----
func (s *Store) GetTrack(ctx context.Context, branchNo, trackCode string) (*CategoryTrack, error) { return nil, ErrNotFound }
func (s *Store) GetTrackByCategory(ctx context.Context, branchNo, freshCategory string) (*CategoryTrack, error) { return nil, ErrNotFound }
func (s *Store) ListTracks(ctx context.Context, branchNo string) ([]*CategoryTrack, error) { return nil, nil }
func (s *Store) UpdateLastSettleAt(ctx context.Context, id int64, t interface{}) error { return nil }
