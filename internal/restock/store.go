package restock

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store 4 张 restock 表的 PG 仓库
//   - restock_display_suggest  (陈列补货建议)
//   - restock_short_state      (短补锁定)
//   - restock_need_purchase    (采购计划单)
//   - restock_tick_log         (tick 执行日志)
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// ============== restock_display_suggest ==============

// UpsertDisplaySuggest 累加陈列补充建议
//   saleQty: 本次 tick 窗口内的销售量(累加值)
//   invSnapshot: 当前 hbpos 库存(覆盖式,总是最新)
//   period: 'eve' / 'morn' / 'aft' / 'manual'
//   2026-09-02: 重构后不再用 period_date 做 UNIQUE 约束,改为 (branch_no, item_no)
//   多次 cron tick 累加 suggest_qty,跨日期时新建一行
func (s *Store) UpsertDisplaySuggest(ctx context.Context, d *DisplaySuggest, saleQty int) error {
	now := time.Now()
	dateStr := d.PeriodDate.Format("2006-01-02")
	_, err := s.pool.Exec(ctx, `
		INSERT INTO restock_display_suggest
			(branch_no, item_no, period_date, suggest_qty, inv_snapshot,
			 last_period, last_sale_at, last_update_at, item_name)
		VALUES ($1, $2, $3::date, $4, $5, $6, $7, $7, $8)
		ON CONFLICT (branch_no, item_no, period_date) DO UPDATE SET
			suggest_qty    = restock_display_suggest.suggest_qty + EXCLUDED.suggest_qty,
			inv_snapshot   = EXCLUDED.inv_snapshot,
			last_period    = EXCLUDED.last_period,
			last_sale_at   = EXCLUDED.last_sale_at,
			last_update_at = EXCLUDED.last_update_at,
			item_name      = COALESCE(EXCLUDED.item_name, restock_display_suggest.item_name)
	`, d.BranchNo, d.ItemNo, dateStr, saleQty, d.InvSnapshot,
		d.LastPeriod, now, d.ItemName)
	return err
}

// GetDisplaySuggest 拉单条
func (s *Store) GetDisplaySuggest(ctx context.Context, branchNo, itemNo string, date time.Time) (*DisplaySuggest, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT branch_no, item_no, period_date, suggest_qty, inv_snapshot,
			COALESCE(last_period,''), last_sale_at, last_update_at, COALESCE(item_name,'')
		FROM restock_display_suggest
		WHERE branch_no=$1 AND item_no=$2 AND period_date=$3::date
	`, branchNo, itemNo, date.Format("2006-01-02"))
	d := &DisplaySuggest{}
	err := row.Scan(&d.BranchNo, &d.ItemNo, &d.PeriodDate, &d.SuggestQty, &d.InvSnapshot,
		&d.LastPeriod, &d.LastSaleAt, &d.LastUpdateAt, &d.ItemName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return d, err
}

// ListActiveItems 拉某门店所有"待办" item (suggest_qty>0 或 is_short=true)
//   2026-09-02 重构:
//   - 不再按 date 过滤
//   - 跨天累加: GROUP BY item_no 把多日期行的 suggest_qty 累加
//   - 排序: 短补中的优先,然后按累加量降序
func (s *Store) ListActiveItems(ctx context.Context, branchNo string) ([]*H5TaskItem, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT
			d.item_no,
			COALESCE(MAX(d.item_name), ''),
			COALESCE(SUM(d.suggest_qty), 0),
			COALESCE(MAX(d.inv_snapshot), 0),
			COALESCE(BOOL_OR(s.is_short), FALSE) AS is_short,
			COALESCE(MAX(s.short_at), NULL) AS short_at,
			COALESCE(MAX(s.short_user), '') AS short_user,
			COALESCE(MAX(np.suggest_qty), 0) AS need_qty,
			COALESCE(MAX(np.status), '') AS need_status,
			COALESCE(MAX(d.last_period), '') AS last_period,
			COALESCE(MAX(d.last_sale_at), NULL) AS last_sale_at,
			COALESCE(MAX(d.last_update_at), NOW()) AS last_update_at
		FROM restock_display_suggest d
		LEFT JOIN restock_short_state s
			ON d.branch_no=s.branch_no AND d.item_no=s.item_no
		LEFT JOIN restock_need_purchase np
			ON d.branch_no=np.branch_no AND d.item_no=np.item_no
			AND np.status IN ('pending', 'sent_to_supplier')
		WHERE d.branch_no=$1
		GROUP BY d.item_no
		HAVING SUM(d.suggest_qty) > 0 OR BOOL_OR(s.is_short) = TRUE
		ORDER BY BOOL_OR(s.is_short) DESC, SUM(d.suggest_qty) DESC
	`, branchNo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*H5TaskItem
	for rows.Next() {
		it := &H5TaskItem{BranchNo: branchNo}
		var shortAt, lastSale *time.Time
		var lastUpdate time.Time
		if err := rows.Scan(&it.ItemNo, &it.ItemName, &it.SuggestQty, &it.InvSnapshot,
			&it.IsShort, &shortAt, &it.ShortUser, &it.NeedQty, &it.NeedStatus,
			&it.LastPeriod, &lastSale, &lastUpdate); err != nil {
			return nil, err
		}
		if shortAt != nil {
			it.ShortAt = shortAt.Format(time.RFC3339)
		}
		if lastSale != nil {
			it.LastSaleAt = lastSale.Format(time.RFC3339)
		}
		it.LastUpdateAt = lastUpdate.Format(time.RFC3339)
		out = append(out, it)
	}
	return out, rows.Err()
}

// ClearDisplaySuggestQty 员工点 DONE 时清零
//   清 suggest_qty(关闭建议),inv_snapshot / last_period 保留(审计用)
func (s *Store) ClearDisplaySuggestQty(ctx context.Context, branchNo, itemNo string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE restock_display_suggest
		SET suggest_qty = 0, last_update_at = NOW()
		WHERE branch_no=$1 AND item_no=$2
	`, branchNo, itemNo)
	return err
}

// ============== restock_short_state ==============

// GetShortState 拉短补状态(nil 表示不存在)
func (s *Store) GetShortState(ctx context.Context, branchNo, itemNo string) (*ShortState, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT branch_no, item_no, is_short, short_at, COALESCE(short_user,'')
		FROM restock_short_state WHERE branch_no=$1 AND item_no=$2
	`, branchNo, itemNo)
	st := &ShortState{}
	err := row.Scan(&st.BranchNo, &st.ItemNo, &st.IsShort, &st.ShortAt, &st.ShortUser)
	if errors.Is(err, pgx.ErrNoRows) {
		return &ShortState{BranchNo: branchNo, ItemNo: itemNo, IsShort: false}, nil
	}
	return st, err
}

// SetShortState 员工点 SHORT 时 upsert
//   isShort=true → 写入 short_at + short_user
//   isShort=false → 清空 short_at + short_user
func (s *Store) SetShortState(ctx context.Context, branchNo, itemNo, userID string, isShort bool) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO restock_short_state (branch_no, item_no, is_short, short_at, short_user)
		VALUES ($1, $2, $3, CASE WHEN $3 THEN NOW() ELSE NULL END, CASE WHEN $3 THEN $4 ELSE '' END)
		ON CONFLICT (branch_no, item_no) DO UPDATE SET
			is_short   = EXCLUDED.is_short,
			short_at   = EXCLUDED.short_at,
			short_user = EXCLUDED.short_user
	`, branchNo, itemNo, isShort, userID)
	return err
}

// ClearShortState 员工点 DONE 时解除短补
func (s *Store) ClearShortState(ctx context.Context, branchNo, itemNo string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE restock_short_state
		SET is_short=FALSE, short_at=NULL, short_user=''
		WHERE branch_no=$1 AND item_no=$2
	`, branchNo, itemNo)
	return err
}

// ============== restock_need_purchase ==============

// UpsertNeedPurchase 短补覆盖:用当前 suggest_qty 覆盖
//   同 (branch, item) pending 状态 upsert;已 sent_to_supplier 不动
func (s *Store) UpsertNeedPurchase(ctx context.Context, np *PurchasePlan) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO restock_need_purchase
			(branch_no, item_no, item_name, barcode, supplier_name, suggest_qty,
			 trigger_kind, trigger_task_id, status, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending', NOW())
		ON CONFLICT (branch_no, item_no) WHERE status='pending'
		DO UPDATE SET
			item_name      = EXCLUDED.item_name,
			barcode        = EXCLUDED.barcode,
			supplier_name  = EXCLUDED.supplier_name,
			suggest_qty    = EXCLUDED.suggest_qty,
			trigger_kind   = EXCLUDED.trigger_kind,
			trigger_task_id = EXCLUDED.trigger_task_id,
			updated_at     = NOW()
	`, np.BranchNo, np.ItemNo, np.ItemName, np.Barcode, np.SupplierName,
		np.SuggestQty, np.TriggerKind, np.TriggerTaskID)
	return err
}

// ClearNeedPurchase 员工点 DONE 时关闭采购单
//   pending → cancelled + suggest_qty=0
//   sent_to_supplier → 不动
func (s *Store) ClearNeedPurchase(ctx context.Context, branchNo, itemNo string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE restock_need_purchase
		SET suggest_qty = 0, status = 'cancelled', updated_at = NOW()
		WHERE branch_no=$1 AND item_no=$2 AND status = 'pending'
	`, branchNo, itemNo)
	return err
}

// ListPendingPlans 拉所有 pending need_purchase
func (s *Store) ListPendingPlans(ctx context.Context, branchNo string) ([]*PurchasePlan, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, branch_no, item_no, COALESCE(item_name,''), COALESCE(barcode,''),
			COALESCE(supplier_name,''), suggest_qty, trigger_kind,
			COALESCE(trigger_task_id,''), status, created_at, updated_at, exported_at
		FROM restock_need_purchase WHERE branch_no=$1 AND status='pending'
		ORDER BY created_at DESC
	`, branchNo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PurchasePlan
	for rows.Next() {
		np := &PurchasePlan{}
		if err := rows.Scan(&np.ID, &np.BranchNo, &np.ItemNo, &np.ItemName, &np.Barcode,
			&np.SupplierName, &np.SuggestQty, &np.TriggerKind, &np.TriggerTaskID,
			&np.Status, &np.CreatedAt, &np.UpdatedAt, &np.ExportedAt); err != nil {
			return nil, err
		}
		out = append(out, np)
	}
	return out, rows.Err()
}

// ListPlansBySupplier 按 supplier 过滤(办公室用)
func (s *Store) ListPlansBySupplier(ctx context.Context, supplierName, branchNo string) ([]*PurchasePlan, error) {
	q := `
		SELECT id, branch_no, item_no, COALESCE(item_name,''), COALESCE(barcode,''),
			COALESCE(supplier_name,''), suggest_qty, trigger_kind,
			COALESCE(trigger_task_id,''), status, created_at, updated_at, exported_at
		FROM restock_need_purchase
		WHERE supplier_name = $1 AND status = 'pending'`
	args := []any{supplierName}
	if branchNo != "" {
		q += " AND branch_no = $2"
		args = append(args, branchNo)
	}
	q += " ORDER BY created_at DESC"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PurchasePlan
	for rows.Next() {
		np := &PurchasePlan{}
		if err := rows.Scan(&np.ID, &np.BranchNo, &np.ItemNo, &np.ItemName, &np.Barcode,
			&np.SupplierName, &np.SuggestQty, &np.TriggerKind, &np.TriggerTaskID,
			&np.Status, &np.CreatedAt, &np.UpdatedAt, &np.ExportedAt); err != nil {
			return nil, err
		}
		out = append(out, np)
	}
	return out, rows.Err()
}

// ============== restock_tick_log ==============

// RecordTickLog 记一次 tick 结果
func (s *Store) RecordTickLog(ctx context.Context, l *TickLog) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO restock_tick_log
			(branch_no, period, tick_at, window_from, window_to, status, error_msg, items_count)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8)
	`, l.BranchNo, l.Period, l.TickAt, l.WindowFrom, l.WindowTo, l.Status, l.ErrorMsg, l.ItemsCount)
	return err
}

// ListRecentTickErrors 启动时扫描最近错误(告警)
func (s *Store) ListRecentTickErrors(ctx context.Context, branchNo string, since time.Time) ([]*TickLog, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, branch_no, period, tick_at, window_from, window_to, status,
			COALESCE(error_msg,''), items_count, created_at
		FROM restock_tick_log
		WHERE branch_no=$1 AND status='error' AND created_at >= $2
		ORDER BY created_at DESC LIMIT 50
	`, branchNo, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TickLog
	for rows.Next() {
		l := &TickLog{}
		if err := rows.Scan(&l.ID, &l.BranchNo, &l.Period, &l.TickAt,
			&l.WindowFrom, &l.WindowTo, &l.Status, &l.ErrorMsg, &l.ItemsCount, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
