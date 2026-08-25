package restock

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store 5 张 restock 表的 PG 仓库
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// ============== Task ==============

// UpsertTask open 状态唯一:同 branch+item 只存在 1 个 open task
//   已存在 open 行 → 更新数量/库存/优先级/原因
//   不存在 → 插入新行
//   状态非 open(acked/short/closed) → 不动,让调用方决定
func (s *Store) UpsertTask(ctx context.Context, t *Task) (created bool, err error) {
	// 查现有 open task
	var existing Task
	row := s.pool.QueryRow(ctx, `
		SELECT task_id, status, push_count FROM restock_task
		WHERE branch_no=$1 AND item_no=$2 AND status='open'
	`, t.BranchNo, t.ItemNo)
	err = row.Scan(&existing.TaskID, &existing.Status, &existing.PushCount)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}

	if errors.Is(err, pgx.ErrNoRows) {
		// 全新:插入
		if t.TaskID == "" {
			t.TaskID = "restock-" + t.BranchNo + "-" + t.ItemNo
		}
		t.Status = TaskStatusOpen
		t.LastUpdateAt = time.Now()
		_, err = s.pool.Exec(ctx, `
			INSERT INTO restock_task
			(task_id, branch_no, item_no, item_name, supplier_name,
			 current_stock, safety_stock, yesterday_sales, suggest_qty,
			 reason, priority, status, last_update_at, push_count)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,0)
		`, t.TaskID, t.BranchNo, t.ItemNo, t.ItemName, t.SupplierName,
			t.CurrentStock, t.SafetyStock, t.YesterdaySales, t.SuggestQty,
			t.Reason, t.Priority, t.Status, t.LastUpdateAt)
		return true, err
	}

	// 已存在:更新数量/库存/优先级/原因(不动 task_id / status / push_count)
	_, err = s.pool.Exec(ctx, `
		UPDATE restock_task SET
			item_name=$2, supplier_name=$3, current_stock=$4,
			safety_stock=$5, yesterday_sales=$6, suggest_qty=$7,
			reason=$8, priority=$9, last_update_at=NOW()
		WHERE task_id=$1
	`, existing.TaskID, t.ItemName, t.SupplierName, t.CurrentStock,
		t.SafetyStock, t.YesterdaySales, t.SuggestQty, t.Reason, t.Priority)
	return false, err
}

// ListOpenTasks 拉某门店所有 open 状态 task
func (s *Store) ListOpenTasks(ctx context.Context, branchNo string) ([]*Task, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT task_id, branch_no, item_no, item_name, COALESCE(supplier_name,''),
			current_stock, safety_stock, yesterday_sales, suggest_qty,
			COALESCE(reason,''), priority, status, first_push_at, last_push_at,
			last_update_at, closed_at, COALESCE(closed_reason,''), push_count
		FROM restock_task WHERE branch_no=$1 AND status='open'
		ORDER BY last_update_at DESC
	`, branchNo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t := &Task{}
		if err := rows.Scan(&t.TaskID, &t.BranchNo, &t.ItemNo, &t.ItemName, &t.SupplierName,
			&t.CurrentStock, &t.SafetyStock, &t.YesterdaySales, &t.SuggestQty,
			&t.Reason, &t.Priority, &t.Status, &t.FirstPushAt, &t.LastPushAt,
			&t.LastUpdateAt, &t.ClosedAt, &t.ClosedReason, &t.PushCount); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTask 拉单条
func (s *Store) GetTask(ctx context.Context, taskID string) (*Task, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT task_id, branch_no, item_no, item_name, COALESCE(supplier_name,''),
			current_stock, safety_stock, yesterday_sales, suggest_qty,
			COALESCE(reason,''), priority, status, first_push_at, last_push_at,
			last_update_at, closed_at, COALESCE(closed_reason,''), push_count
		FROM restock_task WHERE task_id=$1
	`, taskID)
	t := &Task{}
	err := row.Scan(&t.TaskID, &t.BranchNo, &t.ItemNo, &t.ItemName, &t.SupplierName,
		&t.CurrentStock, &t.SafetyStock, &t.YesterdaySales, &t.SuggestQty,
		&t.Reason, &t.Priority, &t.Status, &t.FirstPushAt, &t.LastPushAt,
		&t.LastUpdateAt, &t.ClosedAt, &t.ClosedReason, &t.PushCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

// CloseTask 关闭 task
func (s *Store) CloseTask(ctx context.Context, taskID, reason string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE restock_task SET status='closed', closed_at=NOW(), closed_reason=$2
		WHERE task_id=$1 AND status IN ('open','acked','short')
	`, taskID, reason)
	return err
}

// MarkPushed 推送后回写
func (s *Store) MarkPushed(ctx context.Context, taskID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE restock_task SET
			last_push_at = NOW(),
			first_push_at = COALESCE(first_push_at, NOW()),
			push_count = push_count + 1
		WHERE task_id=$1
	`, taskID)
	return err
}

// UpdateStatus 改状态(员工反馈用)
func (s *Store) UpdateStatus(ctx context.Context, taskID, status string) error {
	_, err := s.pool.Exec(ctx, `UPDATE restock_task SET status=$2 WHERE task_id=$1`, taskID, status)
	return err
}

// ============== Feedback ==============

// InsertFeedback 写员工反馈(已存在同 kind 则忽略,避免重复)
func (s *Store) InsertFeedback(ctx context.Context, fb *Feedback) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO restock_feedback (task_id, feedback_type, feedback_user)
		VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING
	`, fb.TaskID, fb.FeedbackType, fb.FeedbackUser)
	return err
}

// HasRecentFeedback 同 SKU 在 since 时间内有某类反馈
func (s *Store) HasRecentFeedback(ctx context.Context, branchNo, itemNo, kind string, since time.Time) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM restock_feedback f
			JOIN restock_task t ON f.task_id = t.task_id
			WHERE t.branch_no=$1 AND t.item_no=$2 AND f.feedback_type=$3
			  AND f.feedback_time >= $4
		)
	`, branchNo, itemNo, kind, since).Scan(&exists)
	return exists, err
}

// CountSalesInWindow 统计 (branch, item) 在 [from, to] 内的销量
func (s *Store) CountSalesInWindow(ctx context.Context, branchNo, itemNo string, from, to time.Time) (int, error) {
	var total int
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(sale_qnty), 0) FROM restock_sales_watch
		WHERE branch_no=$1 AND item_no=$2
		  AND window_start >= $3 AND window_end <= $4
	`, branchNo, itemNo, from, to).Scan(&total)
	return total, err
}

// RecordSalesWatch 记一笔销售观测
func (s *Store) RecordSalesWatch(ctx context.Context, w *SalesWatch) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO restock_sales_watch (branch_no, item_no, window_start, window_end, sale_qnty)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (branch_no, item_no, window_start) DO UPDATE SET sale_qnty = EXCLUDED.sale_qnty
	`, w.BranchNo, w.ItemNo, w.WindowStart, w.WindowEnd, w.SaleQnty)
	return err
}

// ============== Need Purchase ==============

// UpsertNeedPurchase 同 (branch, item) pending 状态 upsert
func (s *Store) UpsertNeedPurchase(ctx context.Context, np *NeedPurchase) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO restock_need_purchase
		(branch_no, item_no, item_name, barcode, supplier_name, suggest_qty,
		 trigger_kind, trigger_task_id, status, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'pending', NOW())
		ON CONFLICT (branch_no, item_no) WHERE status='pending'
		DO UPDATE SET
			item_name=EXCLUDED.item_name, barcode=EXCLUDED.barcode,
			supplier_name=EXCLUDED.supplier_name, suggest_qty=EXCLUDED.suggest_qty,
			trigger_kind=EXCLUDED.trigger_kind, trigger_task_id=EXCLUDED.trigger_task_id,
			updated_at=NOW()
	`, np.BranchNo, np.ItemNo, np.ItemName, np.Barcode, np.SupplierName,
		np.SuggestQty, np.TriggerKind, np.TriggerTaskID)
	return err
}

// ListPendingNeeds 拉某门店所有 pending need_purchase
func (s *Store) ListPendingNeeds(ctx context.Context, branchNo string) ([]*NeedPurchase, error) {
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
	var out []*NeedPurchase
	for rows.Next() {
		np := &NeedPurchase{}
		if err := rows.Scan(&np.ID, &np.BranchNo, &np.ItemNo, &np.ItemName, &np.Barcode,
			&np.SupplierName, &np.SuggestQty, &np.TriggerKind, &np.TriggerTaskID,
			&np.Status, &np.CreatedAt, &np.UpdatedAt, &np.ExportedAt); err != nil {
			return nil, err
		}
		out = append(out, np)
	}
	return out, rows.Err()
}

// MarkNeedsExported 批量标已导出
func (s *Store) MarkNeedsExported(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE restock_need_purchase SET status='sent_to_supplier', exported_at=NOW()
		WHERE id = ANY($1) AND status='pending'
	`, ids)
	return err
}

// ============== Supplier Reliability ==============

// GetSupplierReliability 拉供应商-商品供应能力(fill_rate 默认 1.0)
func (s *Store) GetSupplierReliability(ctx context.Context, supplier, itemNo string) (*SupplierReliability, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT supplier_name, item_no, requested_qty, supplied_qty, fill_rate, avg_lead_days, last_order_at, updated_at
		FROM supplier_reliability WHERE supplier_name=$1 AND item_no=$2
	`, supplier, itemNo)
	r := &SupplierReliability{}
	err := row.Scan(&r.SupplierName, &r.ItemNo, &r.RequestedQty, &r.SuppliedQty, &r.FillRate, &r.AvgLeadDays, &r.LastOrderAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return &SupplierReliability{
			SupplierName: supplier, ItemNo: itemNo,
			FillRate: 1.0, AvgLeadDays: 1.0,
			UpdatedAt: time.Now(),
		}, nil
	}
	return r, err
}
