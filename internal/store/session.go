package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tinkler/collect-ai/internal/model"
)

// SessionRepo 会话仓库
type SessionRepo struct {
	pool *pgxpool.Pool
}

func NewSessionRepo(pool *pgxpool.Pool) *SessionRepo {
	return &SessionRepo{pool: pool}
}

// Create 创建会话 + 批量插入 rows
//   多图: s.ImagePaths / s.ImageURLs 数组,ImagePath/ImageURL 兼容字段(取第一张)
func (r *SessionRepo) Create(ctx context.Context, s *model.Session) error {
	if s.ID == "" {
		s.ID = uuid.NewString()
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now()
	}
	s.UpdatedAt = time.Now()

	// 多图兼容: 数组为空时, 用单图字段; 数组非空时, 单图字段取首项
	if len(s.ImagePaths) == 0 && s.ImagePath != "" {
		s.ImagePaths = []string{s.ImagePath}
	}
	if len(s.ImageURLs) == 0 && s.ImageURL != "" {
		s.ImageURLs = []string{s.ImageURL}
	}
	if s.ImagePath == "" && len(s.ImagePaths) > 0 {
		s.ImagePath = s.ImagePaths[0]
	}
	if s.ImageURL == "" && len(s.ImageURLs) > 0 {
		s.ImageURL = s.ImageURLs[0]
	}

	imagePathsJSON, _ := json.Marshal(s.ImagePaths)
	imageURLsJSON, _ := json.Marshal(s.ImageURLs)
	rawOCR, _ := json.Marshal(s.Rows) // 暂存 rows 到 raw 字段 (debug 用)

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		INSERT INTO parse_session
		(id, supplier_name, template_id, template_name, mode, image_path, image_url, image_paths, image_urls, source, raw_ocr_json, note, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
	`,
		s.ID, s.SupplierName, s.TemplateID, s.TemplateName, string(s.Mode),
		s.ImagePath, s.ImageURL, string(imagePathsJSON), string(imageURLsJSON),
		s.Source, rawOCR, s.Note, s.CreatedAt, s.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}

	for _, row := range s.Rows {
		if _, err := tx.Exec(ctx, `
			INSERT INTO parse_row
			(session_id, seq, raw_barcode, raw_name, raw_qty, matched_barcode, matched_name, matched_supp, matched_src, qty, unit_price, status, is_new, stock_qty, stock_diff, stock_mismatch, is_deleted)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		`,
			s.ID, row.Seq, nullStr(row.RawBarcode), nullStr(row.RawName), nullStr(row.RawQty),
			nullStr(row.MatchedBarcode), nullStr(row.MatchedName), nullStr(row.MatchedSupp), nullStr(row.MatchedSrc),
			row.Qty, row.UnitPrice, nullStr(row.Status), row.IsNew, row.StockQty, row.StockDiff, row.StockMismatch, row.IsDeleted); err != nil {
			return fmt.Errorf("insert row %d: %w", row.Seq, err)
		}
	}
	return tx.Commit(ctx)
}

// Get 拉单条 (含 rows + image_paths/urls)
func (r *SessionRepo) Get(ctx context.Context, id string) (*model.Session, error) {
	var s model.Session
	var mode string
	var imagePathsJSON, imageURLsJSON []byte
	err := r.pool.QueryRow(ctx, `
		SELECT id, supplier_name, template_id, template_name, mode, image_path, image_url, image_paths, image_urls, source, COALESCE(note,''), created_at, updated_at
		FROM parse_session WHERE id = $1
	`, id).Scan(&s.ID, &s.SupplierName, &s.TemplateID, &s.TemplateName, &mode, &s.ImagePath, &s.ImageURL, &imagePathsJSON, &imageURLsJSON, &s.Source, &s.Note, &s.CreatedAt, &s.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.Mode = model.TemplateMode(mode)
	// image_paths / image_urls 兼容: 老库可能 NULL 或 '[]'
	_ = json.Unmarshal(imagePathsJSON, &s.ImagePaths)
	_ = json.Unmarshal(imageURLsJSON, &s.ImageURLs)
	if len(s.ImagePaths) == 0 && s.ImagePath != "" {
		s.ImagePaths = []string{s.ImagePath}
	}
	if len(s.ImageURLs) == 0 && s.ImageURL != "" {
		s.ImageURLs = []string{s.ImageURL}
	}

	rows, err := r.queryRows(ctx, id, true)
	if err != nil {
		return nil, err
	}
	s.Rows = rows
	return &s, nil
}

// ListSummaries 列表 (不含 rows, 性能)
//   image_count: 用 jsonb_array_length 优先, 0 退化到 1 (老库只有 image_path)
func (r *SessionRepo) ListSummaries(ctx context.Context, supplier string, from time.Time, limit int) ([]model.SessionSummary, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q := `SELECT s.id, s.supplier_name, s.template_name, s.mode,
		COALESCE((SELECT COUNT(*) FROM parse_row r WHERE r.session_id=s.id AND NOT r.is_deleted), 0),
		CASE WHEN jsonb_array_length(s.image_paths) > 0
		     THEN jsonb_array_length(s.image_paths)
		     ELSE CASE WHEN s.image_path <> '' THEN 1 ELSE 0 END
		END AS image_count,
		s.source, s.created_at, s.updated_at
		FROM parse_session s WHERE 1=1`
	args := []any{}
	if supplier != "" {
		args = append(args, supplier)
		q += fmt.Sprintf(" AND s.supplier_name ILIKE $%d", len(args))
	}
	if !from.IsZero() {
		args = append(args, from)
		q += fmt.Sprintf(" AND s.created_at >= $%d", len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY s.created_at DESC LIMIT $%d", len(args))

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.SessionSummary
	for rows.Next() {
		var s model.SessionSummary
		var mode string
		if err := rows.Scan(&s.ID, &s.SupplierName, &s.TemplateName, &mode, &s.RowCount, &s.ImageCount, &s.Source, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		s.Mode = model.TemplateMode(mode)
		out = append(out, s)
	}
	return out, rows.Err()
}

// UpdateRow 改某行 (Patch)
func (r *SessionRepo) UpdateRow(ctx context.Context, sessionID string, rowID int64, patch map[string]any) error {
	if len(patch) == 0 {
		return nil
	}
	// 过滤白名单字段
	allowed := map[string]bool{
		"matched_barcode": true, "matched_name": true, "matched_supp": true,
		"qty": true, "unit_price": true, "is_deleted": true, "status": true, "is_new": true,
	}
	set := make([]string, 0, len(patch))
	args := []any{sessionID, rowID}
	idx := 3
	for k, v := range patch {
		if !allowed[k] {
			continue
		}
		set = append(set, fmt.Sprintf("%s = $%d", k, idx))
		args = append(args, v)
		idx++
	}
	if len(set) == 0 {
		return nil
	}
	q := fmt.Sprintf("UPDATE parse_row SET %s WHERE session_id = $1 AND id = $2",
		strings.Join(set, ", "))
	tag, err := r.pool.Exec(ctx, q, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("行不存在 (session_id=%s, row_id=%d)", sessionID, rowID)
	}
	// 触发表 updated_at
	_, _ = r.pool.Exec(ctx, "UPDATE parse_session SET updated_at = NOW() WHERE id = $1", sessionID)
	return nil
}

// DeleteRow 软删 (is_deleted=true)
func (r *SessionRepo) DeleteRow(ctx context.Context, sessionID string, rowID int64) error {
	tag, err := r.pool.Exec(ctx, "UPDATE parse_row SET is_deleted = TRUE WHERE session_id = $1 AND id = $2", sessionID, rowID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("行不存在 (session_id=%s, row_id=%d)", sessionID, rowID)
	}
	_, _ = r.pool.Exec(ctx, "UPDATE parse_session SET updated_at = NOW() WHERE id = $1", sessionID)
	return nil
}

// DeleteSession 整条删
func (r *SessionRepo) DeleteSession(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, "DELETE FROM parse_session WHERE id = $1", id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("session 不存在: %s", id)
	}
	return nil
}

// queryRows 拉所有行 (含 is_deleted)
func (r *SessionRepo) queryRows(ctx context.Context, sessionID string, _ bool) ([]model.SkuRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, seq, COALESCE(raw_barcode,''), COALESCE(raw_name,''), COALESCE(raw_qty,''),
			COALESCE(matched_barcode,''), COALESCE(matched_name,''), COALESCE(matched_supp,''), COALESCE(matched_src,''),
			qty, unit_price, COALESCE(status,''), COALESCE(is_new,false), stock_qty, stock_diff, COALESCE(stock_mismatch,false), COALESCE(is_deleted,false)
		FROM parse_row WHERE session_id = $1 ORDER BY seq
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.SkuRow
	for rows.Next() {
		var r model.SkuRow
		if err := rows.Scan(&r.RowID, &r.Seq, &r.RawBarcode, &r.RawName, &r.RawQty,
			&r.MatchedBarcode, &r.MatchedName, &r.MatchedSupp, &r.MatchedSrc,
			&r.Qty, &r.UnitPrice, &r.Status, &r.IsNew, &r.StockQty, &r.StockDiff, &r.StockMismatch, &r.IsDeleted); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
