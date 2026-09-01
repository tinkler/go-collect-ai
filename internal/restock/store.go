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

// ListPendingNeedsBySupplier 拉某供应商所有 pending need_purchase
//   branchNo 可选,空字符串 = 不限门店
//   2026-08-28 加入, 用于企微 H5 采购收货单按 supplier 反查计划
func (s *Store) ListPendingNeedsBySupplier(ctx context.Context, supplierName, branchNo string) ([]*NeedPurchase, error) {
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

// ============== Display Restock (2026-08-30 新增) ==============

// UpsertDisplaySuggest 累加陈列补充建议
//   saleQty: 本次 tick 窗口内的销售量(累加值)
//   invSnapshot: 当前 hbpos 库存(覆盖式,总是最新)
//   period: 'eve' / 'morn' / 'aft'
//   ON CONFLICT (branch,item,period_date) DO UPDATE SET suggest_qty = suggest_qty + EXCLUDED.suggest_qty
//   2026-09-01: 同时写 item_name(从 cube WindowSaleRow 拿, ListH5Tasks/ListShortItems 不再 JOIN need_purchase 拿 name)
func (s *Store) UpsertDisplaySuggest(ctx context.Context, d *DisplaySuggest, saleQty int) error {
	now := time.Now()
	dateStr := d.PeriodDate.Format("2006-01-02")
	_, err := s.pool.Exec(ctx, `
		INSERT INTO restock_display_suggest
			(branch_no, item_no, period_date, suggest_qty, inv_snapshot, last_period, last_sale_at, last_update_at, item_name)
		VALUES ($1, $2, $3::date, $4, $5, $6, $7, $7, $8)
		ON CONFLICT (branch_no, item_no, period_date) DO UPDATE SET
			suggest_qty  = restock_display_suggest.suggest_qty + EXCLUDED.suggest_qty,
			inv_snapshot = EXCLUDED.inv_snapshot,
			last_period  = EXCLUDED.last_period,
			last_sale_at = EXCLUDED.last_sale_at,
			last_update_at = EXCLUDED.last_update_at,
			item_name    = COALESCE(EXCLUDED.item_name, restock_display_suggest.item_name)
	`, d.BranchNo, d.ItemNo, dateStr, saleQty, d.InvSnapshot, d.LastPeriod, now, d.ItemName)
	return err
}

// GetDisplaySuggest 拉单条陈列建议
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

// ListDisplaySuggestToday 拉某门店当天的所有陈列建议(待推送 / 关闭清单用)
func (s *Store) ListDisplaySuggestToday(ctx context.Context, branchNo string, date time.Time) ([]*DisplaySuggest, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT branch_no, item_no, period_date, suggest_qty, inv_snapshot,
			COALESCE(last_period,''), last_sale_at, last_update_at, COALESCE(item_name,'')
		FROM restock_display_suggest
		WHERE branch_no=$1 AND period_date=$2::date
		ORDER BY suggest_qty DESC, item_no
	`, branchNo, date.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DisplaySuggest
	for rows.Next() {
		d := &DisplaySuggest{}
		if err := rows.Scan(&d.BranchNo, &d.ItemNo, &d.PeriodDate, &d.SuggestQty, &d.InvSnapshot,
			&d.LastPeriod, &d.LastSaleAt, &d.LastUpdateAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ClearDisplaySuggestQty 员工点完成时清零(关闭建议)
//   只清 suggest_qty 和 last_sale_at,inv_snapshot / last_period 保留(审计用)
func (s *Store) ClearDisplaySuggestQty(ctx context.Context, branchNo, itemNo string, date time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE restock_display_suggest
		SET suggest_qty = 0, last_update_at = NOW()
		WHERE branch_no=$1 AND item_no=$2 AND period_date=$3::date
	`, branchNo, itemNo, date.Format("2006-01-02"))
	return err
}

// GetShortState 拉单条短补状态(nil 表示不存在)
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

// ListShortItems 拉某门店所有短补中的 item
//   JOIN display_suggest 拿 item_name / suggest_qty
//   2026-09-01: item_name 改从 display_suggest 拿(之前 d.item_name 列不存在 → 全空,
//   因为短补中的 item 不一定在当天有销售(可能没 display_suggest 行), 这个 SQL 本来就会丢 item_name,
//   见 ListH5Tasks 注释理解整体修复方向)
func (s *Store) ListShortItems(ctx context.Context, branchNo string, date time.Time) ([]*H5TaskItem, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.item_no,
			COALESCE(MAX(d.item_name), ''),
			s.is_short, s.short_at, s.short_user,
			COALESCE(MAX(d.suggest_qty), 0),
			COALESCE(MAX(d.inv_snapshot), 0),
			COALESCE(MAX(d.last_period), ''),
			$2::date,
			COALESCE(MAX(d.last_update_at), NOW())
		FROM restock_short_state s
		LEFT JOIN restock_display_suggest d
			ON s.branch_no=d.branch_no AND s.item_no=d.item_no AND d.period_date=$2::date
		WHERE s.branch_no=$1 AND s.is_short = TRUE
		GROUP BY s.item_no, s.is_short, s.short_at, s.short_user
		ORDER BY s.short_at DESC
	`, branchNo, date.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*H5TaskItem
	for rows.Next() {
		it := &H5TaskItem{BranchNo: branchNo, IsShort: true}
		var shortAt *time.Time
		var lastUpdate time.Time
		var periodDate time.Time
		if err := rows.Scan(&it.ItemNo, &it.ItemName, &it.IsShort, &shortAt, &it.ShortUser,
			&it.SuggestQty, &it.InvSnapshot, &it.LastPeriod, &periodDate, &lastUpdate); err != nil {
			return nil, err
		}
		if shortAt != nil {
			it.ShortAt = shortAt.Format(time.RFC3339)
		}
		it.PeriodDate = periodDate.Format("2006-01-02")
		it.LastUpdateAt = lastUpdate.Format(time.RFC3339)
		out = append(out, it)
	}
	return out, rows.Err()
}

// SetShortState 员工点缺货时 upsert(ONCE 锁定)
//   isShort=true → 写入 short_at + short_user
//   isShort=false → 清空 short_at + short_user(解除时)
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

// ClearShortState 员工点完成时翻 is_short=FALSE
func (s *Store) ClearShortState(ctx context.Context, branchNo, itemNo string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE restock_short_state
		SET is_short=FALSE, short_at=NULL, short_user=''
		WHERE branch_no=$1 AND item_no=$2
	`, branchNo, itemNo)
	return err
}

// UpsertNeedPurchaseFromDisplay 短补覆盖:用 display_suggest.suggest_qty 作为 need_purchase.suggest_qty
//   同 (branch, item) pending 状态 upsert;已 sent_to_supplier 不动
func (s *Store) UpsertNeedPurchaseFromDisplay(ctx context.Context, np *NeedPurchase) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO restock_need_purchase
			(branch_no, item_no, item_name, barcode, supplier_name, suggest_qty,
			 trigger_kind, trigger_task_id, status, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending', NOW())
		ON CONFLICT (branch_no, item_no) WHERE status='pending'
		DO UPDATE SET
			item_name   = EXCLUDED.item_name,
			barcode     = EXCLUDED.barcode,
			supplier_name = EXCLUDED.supplier_name,
			suggest_qty = EXCLUDED.suggest_qty,
			trigger_kind = EXCLUDED.trigger_kind,
			updated_at  = NOW()
	`, np.BranchNo, np.ItemNo, np.ItemName, np.Barcode, np.SupplierName,
		np.SuggestQty, np.TriggerKind, np.TriggerTaskID)
	return err
}

// ClearNeedPurchase 员工点完成时关闭采购计划单
//   pending → cancelled + suggest_qty=0
//   sent_to_supplier → 不动(已发,改不了)
func (s *Store) ClearNeedPurchase(ctx context.Context, branchNo, itemNo string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE restock_need_purchase
		SET suggest_qty = 0, status = 'cancelled', updated_at = NOW()
		WHERE branch_no=$1 AND item_no=$2 AND status = 'pending'
	`, branchNo, itemNo)
	return err
}

// RecordTickLog 记一次 tick 结果
func (s *Store) RecordTickLog(ctx context.Context, l *TickLog) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO restock_tick_log
			(branch_no, period, tick_at, window_from, window_to, status, error_msg, items_count)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8)
	`, l.BranchNo, l.Period, l.TickAt, l.WindowFrom, l.WindowTo, l.Status, l.ErrorMsg, l.ItemsCount)
	return err
}

// ListRecentTickErrors 启动时扫描最近 24h 的失败 tick(用于告警)
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

// ListH5Tasks H5 任务列表:合并 display_suggest + short_state + need_purchase
//   返回 suggest_qty>0 或 is_short=true 的 item(给员工看的"待办"清单)
//   2026-09-01: item_name 直接从 restock_display_suggest 拿(写入时从 cube WindowSaleRow 缓存)
//   之前从 LEFT JOIN need_purchase 拿 name 是错的 — need_purchase 只有用户点了缺货后才有数据,
//   大部分 task 行 item_name 都是空字符串
func (s *Store) ListH5Tasks(ctx context.Context, branchNo string, date time.Time) ([]*H5TaskItem, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT
			d.item_no,
			COALESCE(MAX(d.item_name), ''),
			COALESCE(MAX(d.suggest_qty), 0),
			COALESCE(MAX(d.inv_snapshot), 0),
			COALESCE(MAX(d.last_period), ''),
			$2::date,
			COALESCE(MAX(d.last_update_at), NOW()),
			COALESCE(BOOL_OR(s.is_short), FALSE) AS is_short,
			COALESCE(MAX(s.short_at), NULL) AS short_at,
			COALESCE(MAX(np.suggest_qty), 0) AS need_qty,
			COALESCE(MAX(np.status), '') AS need_status
		FROM restock_display_suggest d
		LEFT JOIN restock_short_state s
			ON d.branch_no=s.branch_no AND d.item_no=s.item_no
		LEFT JOIN restock_need_purchase np
			ON d.branch_no=np.branch_no AND d.item_no=np.item_no
			AND np.status IN ('pending', 'sent_to_supplier')
		WHERE d.branch_no=$1 AND d.period_date=$2::date
		  AND (d.suggest_qty > 0 OR s.is_short = TRUE)
		GROUP BY d.item_no
		ORDER BY BOOL_OR(s.is_short) DESC, MAX(d.suggest_qty) DESC
	`, branchNo, date.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*H5TaskItem
	for rows.Next() {
		it := &H5TaskItem{BranchNo: branchNo}
		var periodDate, lastUpdate time.Time
		var shortAt *time.Time
		if err := rows.Scan(&it.ItemNo, &it.ItemName, &it.SuggestQty, &it.InvSnapshot,
			&it.LastPeriod, &periodDate, &lastUpdate,
			&it.IsShort, &shortAt, &it.NeedQty, &it.NeedStatus); err != nil {
			return nil, err
		}
		if shortAt != nil {
			it.ShortAt = shortAt.Format(time.RFC3339)
		}
		it.PeriodDate = periodDate.Format("2006-01-02")
		it.LastUpdateAt = lastUpdate.Format(time.RFC3339)
		out = append(out, it)
	}
	return out, rows.Err()
}
