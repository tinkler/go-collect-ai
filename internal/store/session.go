package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
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
//   W4.1: 同步写 image_hashes / image_index / analysis_status='pending'
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
	// W4.1: 默认 analysis_status = pending
	if s.AnalysisStatus == "" {
		s.AnalysisStatus = "pending"
	}

	imagePathsJSON, _ := json.Marshal(s.ImagePaths)
	imageURLsJSON, _ := json.Marshal(s.ImageURLs)
	imageHashesJSON, _ := json.Marshal(s.ImageHashes)
	rawOCR, _ := json.Marshal(s.Rows) // 暂存 rows 到 raw 字段 (debug 用)

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		INSERT INTO parse_session
		(id, supplier_name, mode, image_path, image_url, image_paths, image_urls, image_hashes, source, raw_ocr_json, note, strategy_version, analysis_status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
	`,
		s.ID, s.SupplierName, string(s.Mode),
		s.ImagePath, s.ImageURL, string(imagePathsJSON), string(imageURLsJSON), string(imageHashesJSON),
		s.Source, rawOCR, s.Note, s.StrategyVersion, s.AnalysisStatus, s.CreatedAt, s.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}

	for _, row := range s.Rows {
		if _, err := tx.Exec(ctx, `
			INSERT INTO parse_row
			(session_id, seq, image_index, raw_barcode, raw_name, raw_qty, matched_barcode, matched_name, matched_supp, matched_src, qty, unit_price, status, is_new, stock_qty, stock_diff, stock_mismatch, is_deleted)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		`,
			s.ID, row.Seq, row.ImageIndex,
			nullStr(row.RawBarcode), nullStr(row.RawName), nullStr(row.RawQty),
			nullStr(row.MatchedBarcode), nullStr(row.MatchedName), nullStr(row.MatchedSupp), nullStr(row.MatchedSrc),
			row.Qty, row.UnitPrice, nullStr(row.Status), row.IsNew, row.StockQty, row.StockDiff, row.StockMismatch, row.IsDeleted); err != nil {
			return fmt.Errorf("insert row %d: %w", row.Seq, err)
		}
	}
	return tx.Commit(ctx)
}

// Get 拉单条 (含 rows + image_paths/urls + image_hashes + analysis_status)
func (r *SessionRepo) Get(ctx context.Context, id string) (*model.Session, error) {
	var s model.Session
	var mode string
	var imagePathsJSON, imageURLsJSON, imageHashesJSON []byte
	var analysisStatus, analysisError string
	var analysisAt *time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT id, supplier_name, mode, image_path, image_url, image_paths, image_urls, image_hashes, source, COALESCE(note,''), strategy_version, analysis_status, analysis_at, COALESCE(analysis_error,''), created_at, updated_at
		FROM parse_session WHERE id = $1
	`, id).Scan(&s.ID, &s.SupplierName, &mode, &s.ImagePath, &s.ImageURL, &imagePathsJSON, &imageURLsJSON, &imageHashesJSON, &s.Source, &s.Note, &s.StrategyVersion, &analysisStatus, &analysisAt, &analysisError, &s.CreatedAt, &s.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.Mode = model.Mode(mode)
	s.AnalysisStatus = analysisStatus
	s.AnalysisAt = analysisAt
	s.AnalysisError = analysisError
	// image_paths / image_urls / image_hashes 兼容: 老库可能 NULL 或 '[]'
	_ = json.Unmarshal(imagePathsJSON, &s.ImagePaths)
	_ = json.Unmarshal(imageURLsJSON, &s.ImageURLs)
	_ = json.Unmarshal(imageHashesJSON, &s.ImageHashes)
	if len(s.ImagePaths) == 0 && s.ImagePath != "" {
		s.ImagePaths = []string{s.ImagePath}
	}
	if len(s.ImageURLs) == 0 && s.ImageURL != "" {
		s.ImageURLs = []string{s.ImageURL}
	}
	if s.ImageHashes == nil {
		s.ImageHashes = []string{}
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
	q := `SELECT s.id, s.supplier_name, s.strategy_version, s.mode,
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
		if err := rows.Scan(&s.ID, &s.SupplierName, &s.StrategyVersion, &mode, &s.RowCount, &s.ImageCount, &s.Source, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		s.Mode = model.Mode(mode)
		out = append(out, s)
	}
	return out, rows.Err()
}

// UpdateSessionRows 2026-09-04 异步 VLM 模式 (新):
//   - 删 session 关联的所有 parse_row
//   - 插新的 parse_row
//   - 更新 parse_session 的 analysis_status, raw_ocr_json, updated_at
//   整体在一个事务里, 失败时回滚
//
// 用法: handler 立即 CreateSession(空 rows, status='pending'),
//   后台 goroutine 跑 VLM, 完成后调本方法一次性写入 rows + 改 status='done'
func (r *SessionRepo) UpdateSessionRows(ctx context.Context, sessionID string, s *model.Session, status string) error {
	if s == nil || s.ID != sessionID {
		return fmt.Errorf("sessionID 不匹配")
	}

	rawOCR, _ := json.Marshal(s.Rows)

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// 1) 删旧 rows
	if _, err := tx.Exec(ctx, "DELETE FROM parse_row WHERE session_id = $1", sessionID); err != nil {
		return fmt.Errorf("delete old rows: %w", err)
	}

	// 2) 插新 rows
	for _, row := range s.Rows {
		if _, err := tx.Exec(ctx, `
			INSERT INTO parse_row
			(session_id, seq, image_index, raw_barcode, raw_name, raw_qty, matched_barcode, matched_name, matched_supp, matched_src, qty, unit_price, status, is_new, stock_qty, stock_diff, stock_mismatch, is_deleted)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		`,
			sessionID, row.Seq, row.ImageIndex,
			nullStr(row.RawBarcode), nullStr(row.RawName), nullStr(row.RawQty),
			nullStr(row.MatchedBarcode), nullStr(row.MatchedName), nullStr(row.MatchedSupp), nullStr(row.MatchedSrc),
			row.Qty, row.UnitPrice, nullStr(row.Status), row.IsNew, row.StockQty, row.StockDiff, row.StockMismatch, row.IsDeleted); err != nil {
			return fmt.Errorf("insert row %d: %w", row.Seq, err)
		}
	}

	// 3) 更新 session 状态 + raw_ocr_json + updated_at
	if _, err := tx.Exec(ctx, `
		UPDATE parse_session
		SET analysis_status = $2, raw_ocr_json = $3, updated_at = NOW()
		WHERE id = $1
	`, sessionID, status, rawOCR); err != nil {
		return fmt.Errorf("update session: %w", err)
	}

	return tx.Commit(ctx)
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

// ImageCandidate AppendImages 入参 (caller 算好 hash + 准备 file 落盘)
type ImageCandidate struct {
	Hash     string // sha256 hex
	FileName string
	ImgBytes []byte
}

// HashImageBytes SHA-256 哈希 (W4.1 重复图去重用)
//   返回 64 字符 hex 字符串
func HashImageBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// AppendImages 追加图片到已有 session (W4.1 重复图去重)
//   行为:
//     1) 算每张图 sha256, 跟 session 现存 image_hashes 比对
//     2) 重复的 → 跳过, 记到 skippedHashes
//     3) 新的 → 调 orchestrator 解析(调用方传 ParseFn), 续接 seq
//     4) 写新 image_hashes 到 session
//   签名:
//     id: sessionID
//     hashesWithPaths: caller 算好 hash + path/url (前台可看到, 便于 UI 提示)
//     parseFn: 对每个 (hash, fileName, imgBytes) 调 VLM 解析, 返回 []SkuRow
//   返回:
//     addedRows: 新加的行 (含新 row_id)
//     skippedHashes: 已存在的 hash 列表 (UI 可提示"该图已识别过, 跳过")
//     newHashes: 本次新加的 hash (用于更新 session.image_hashes)
func (r *SessionRepo) AppendImages(
	ctx context.Context,
	id string,
	candidates []ImageCandidate,
	parseFn func(hash, fileName string, imgBytes []byte) ([]model.SkuRow, error),
) (addedRows []model.SkuRow, skippedHashes []string, newHashes []string, err error) {
	// 1) 拉 session 当前 image_hashes
	var imageHashesJSON []byte
	if err = r.pool.QueryRow(ctx, `SELECT image_hashes FROM parse_session WHERE id=$1`, id).Scan(&imageHashesJSON); err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil, nil, fmt.Errorf("session 不存在: %s", id)
		}
		return nil, nil, nil, fmt.Errorf("read image_hashes: %w", err)
	}
	existing := map[string]struct{}{}
	if len(imageHashesJSON) > 0 {
		_ = json.Unmarshal(imageHashesJSON, &existing)
	}

	// 2) 分类: 新图 vs 重复
	var toParse []ImageCandidate
	for _, c := range candidates {
		if c.Hash == "" {
			return nil, nil, nil, fmt.Errorf("candidate.hash 必填")
		}
		if _, ok := existing[c.Hash]; ok {
			skippedHashes = append(skippedHashes, c.Hash)
			continue
		}
		newHashes = append(newHashes, c.Hash)
		existing[c.Hash] = struct{}{}
		toParse = append(toParse, c)
	}

	if len(toParse) == 0 {
		return nil, skippedHashes, nil, nil
	}

	// 3) 拉最大 seq 和 image_index (续接)
	var maxSeq, maxImgIdx int
	if err = r.pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(seq), 0), COALESCE(MAX(image_index), -1)
		FROM parse_row WHERE session_id = $1
	`, id).Scan(&maxSeq, &maxImgIdx); err != nil {
		return nil, nil, nil, fmt.Errorf("read max seq: %w", err)
	}

	// 4) 解析每张新图 + 插入 rows
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	defer tx.Rollback(ctx)

	for _, c := range toParse {
		// 图片索引 (从 max+1 开始)
		maxImgIdx++
		imageIndex := maxImgIdx

		// 调 parseFn (handler 注入, 调 Orchestrator.Parse)
		rows, err := parseFn(c.Hash, c.FileName, c.ImgBytes)
		if err != nil {
			log.Printf("[AppendImages] parseFn err (hash=%s): %v", c.Hash, err)
			// 单图失败不阻断后续图, 仅记 0 rows
			rows = nil
		}

		// 续接 seq
		baseSeq := maxSeq
		for i := range rows {
			baseSeq++
			rows[i].Seq = baseSeq
			rows[i].ImageIndex = imageIndex
			if _, err := tx.Exec(ctx, `
				INSERT INTO parse_row
				(session_id, seq, image_index, raw_barcode, raw_name, raw_qty, matched_barcode, matched_name, matched_supp, matched_src, qty, unit_price, status, is_new, stock_qty, stock_diff, stock_mismatch, is_deleted)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
			`,
				id, rows[i].Seq, rows[i].ImageIndex,
				nullStr(rows[i].RawBarcode), nullStr(rows[i].RawName), nullStr(rows[i].RawQty),
				nullStr(rows[i].MatchedBarcode), nullStr(rows[i].MatchedName), nullStr(rows[i].MatchedSupp), nullStr(rows[i].MatchedSrc),
				rows[i].Qty, rows[i].UnitPrice, nullStr(rows[i].Status), rows[i].IsNew, rows[i].StockQty, rows[i].StockDiff, rows[i].StockMismatch, rows[i].IsDeleted); err != nil {
				return nil, nil, nil, fmt.Errorf("insert row (img=%d): %w", imageIndex, err)
			}
		}
		addedRows = append(addedRows, rows...)
		maxSeq = baseSeq
	}

	// 5) 更新 session.image_hashes + image_paths/image_urls + analysis_status=pending (重跑分析)
	mergedHashes := make([]string, 0, len(existing))
	for k := range existing {
		mergedHashes = append(mergedHashes, k)
	}
	mergedHashesJSON, _ := json.Marshal(mergedHashes)

	if _, err = tx.Exec(ctx, `
		UPDATE parse_session
		SET image_hashes = $2,
		    analysis_status = 'pending',
		    analysis_at = NULL,
		    analysis_error = '',
		    updated_at = NOW()
		WHERE id = $1
	`, id, string(mergedHashesJSON)); err != nil {
		return nil, nil, nil, fmt.Errorf("update session: %w", err)
	}

	if err = tx.Commit(ctx); err != nil {
		return nil, nil, nil, fmt.Errorf("commit: %w", err)
	}
	return addedRows, skippedHashes, newHashes, nil
}
func (r *SessionRepo) queryRows(ctx context.Context, sessionID string, _ bool) ([]model.SkuRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, seq, image_index, COALESCE(raw_barcode,''), COALESCE(raw_name,''), COALESCE(raw_qty,''),
			COALESCE(matched_barcode,''), COALESCE(matched_name,''), COALESCE(matched_supp,''), COALESCE(matched_src,''),
			qty, unit_price, COALESCE(status,''), COALESCE(is_new,false), stock_qty, stock_diff, COALESCE(stock_mismatch,false), COALESCE(is_deleted,false)
		FROM parse_row WHERE session_id = $1 ORDER BY image_index, seq
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.SkuRow
	for rows.Next() {
		var r model.SkuRow
		if err := rows.Scan(&r.RowID, &r.Seq, &r.ImageIndex, &r.RawBarcode, &r.RawName, &r.RawQty,
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
