package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tinkler/collect-ai/internal/model"
)

// TemplateRepo 模板仓库 (供 C# 端同步 + 飞书端查询)
type TemplateRepo struct {
	pool *pgxpool.Pool
}

func NewTemplateRepo(pool *pgxpool.Pool) *TemplateRepo {
	return &TemplateRepo{pool: pool}
}

// templateColumns 统一所有 SELECT 子句, 避免增列时漏掉
//   use_llm / fuzzy_distance 用 COALESCE 转 nil 语义 (DB NULL → Go nil)
const templateColumns = `id, name, supplier_name, mode, COALESCE(llm_prompt,''), COALESCE(ocr_model,''), COALESCE(llm_model,''),
		use_llm, fuzzy_distance,
		header_keywords, footer_keywords, subtitle_keywords,
		COALESCE(is_default,false), updated_at, COALESCE(note,'')`

// scanTemplate 从 pgx.Row 扫描到 Template
func scanTemplate(row pgx.Row, t *model.Template) error {
	var mode, hdr, foot, sub []byte
	if err := row.Scan(&t.ID, &t.Name, &t.SupplierName, &mode,
		&t.LlmPrompt, &t.OcrModel, &t.LlmModel,
		&t.UseLlm, &t.FuzzyDistance,
		&hdr, &foot, &sub,
		&t.IsDefault, &t.UpdatedAt, &t.Note); err != nil {
		return err
	}
	t.Mode = model.TemplateMode(mode)
	_ = json.Unmarshal(hdr, &t.HeaderKeywords)
	_ = json.Unmarshal(foot, &t.FooterKeywords)
	_ = json.Unmarshal(sub, &t.SubtitleKeywords)
	return nil
}

// Upsert 插入/更新
//   use_llm / fuzzy_distance 用 COALESCE: nil=NULL(未设置) / 非 nil=值
func (r *TemplateRepo) Upsert(ctx context.Context, t *model.Template) error {
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = time.Now()
	}
	hdr, _ := json.Marshal(t.HeaderKeywords)
	foot, _ := json.Marshal(t.FooterKeywords)
	sub, _ := json.Marshal(t.SubtitleKeywords)
	_, err := r.pool.Exec(ctx, `
		INSERT INTO template (id, name, supplier_name, mode, llm_prompt, ocr_model, llm_model,
			use_llm, fuzzy_distance,
			header_keywords, footer_keywords, subtitle_keywords, is_default, updated_at, note)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			supplier_name = EXCLUDED.supplier_name,
			mode = EXCLUDED.mode,
			llm_prompt = EXCLUDED.llm_prompt,
			ocr_model = EXCLUDED.ocr_model,
			llm_model = EXCLUDED.llm_model,
			use_llm = EXCLUDED.use_llm,
			fuzzy_distance = EXCLUDED.fuzzy_distance,
			header_keywords = EXCLUDED.header_keywords,
			footer_keywords = EXCLUDED.footer_keywords,
			subtitle_keywords = EXCLUDED.subtitle_keywords,
			is_default = EXCLUDED.is_default,
			updated_at = EXCLUDED.updated_at,
			note = EXCLUDED.note
	`,
		t.ID, t.Name, t.SupplierName, string(t.Mode),
		t.LlmPrompt, t.OcrModel, t.LlmModel,
		t.UseLlm, t.FuzzyDistance,
		hdr, foot, sub, t.IsDefault, t.UpdatedAt, t.Note)
	return err
}

// UpsertAll 批量 (C# 端一次性同步)
func (r *TemplateRepo) UpsertAll(ctx context.Context, ts []model.Template) error {
	for i := range ts {
		if err := r.Upsert(ctx, &ts[i]); err != nil {
			return fmt.Errorf("upsert %s: %w", ts[i].ID, err)
		}
	}
	return nil
}

// GetByID 单条查询 (解析时根据 template_id 拿配置)
func (r *TemplateRepo) GetByID(ctx context.Context, id string) (*model.Template, error) {
	q := `SELECT ` + templateColumns + ` FROM template WHERE id = $1`
	row := r.pool.QueryRow(ctx, q, id)
	var t model.Template
	if err := scanTemplate(row, &t); err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &t, nil
}

// ListForSupplier 列某供应商的模板 (供飞书端)
func (r *TemplateRepo) ListForSupplier(ctx context.Context, supplierName, mode string, onlyDefault, purchaseOnly bool) ([]model.Template, error) {
	q := `SELECT ` + templateColumns + ` FROM template WHERE 1=1`
	args := []any{}
	if purchaseOnly {
		q += " AND mode = 'purchase'"
	}
	if supplierName != "" {
		args = append(args, supplierName)
		q += fmt.Sprintf(" AND (supplier_name = '' OR supplier_name = $%d)", len(args))
	}
	if onlyDefault {
		q += " AND is_default = TRUE"
	}
	q += " ORDER BY is_default DESC, name"

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.Template
	for rows.Next() {
		var t model.Template
		if err := scanTemplate(rows, &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListAll 全部 (供 C# 端管理界面)
func (r *TemplateRepo) ListAll(ctx context.Context) ([]model.Template, error) {
	q := `SELECT ` + templateColumns + ` FROM template ORDER BY supplier_name, is_default DESC, name`
	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Template
	for rows.Next() {
		var t model.Template
		if err := scanTemplate(rows, &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Delete 删除
func (r *TemplateRepo) Delete(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, "DELETE FROM template WHERE id = $1", id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
