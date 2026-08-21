package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool 创建 PG 连接池
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 10
	cfg.MinConns = 1
	cfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	// 健康检查
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pg ping: %w", err)
	}
	return pool, nil
}

// Migrate 建表 (幂等)
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS parse_session (
			id              UUID PRIMARY KEY,
			supplier_name   TEXT NOT NULL,
			template_id     TEXT NOT NULL,
			template_name   TEXT NOT NULL,
			mode            TEXT NOT NULL,
			image_path      TEXT NOT NULL,
			image_url       TEXT NOT NULL DEFAULT '',
			source          TEXT NOT NULL,
			raw_ocr_json    JSONB,
			raw_llm_json    JSONB,
			note            TEXT,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_session_created ON parse_session(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_session_supplier ON parse_session(supplier_name)`,
		`CREATE TABLE IF NOT EXISTS parse_row (
			id              BIGSERIAL PRIMARY KEY,
			session_id      UUID NOT NULL REFERENCES parse_session(id) ON DELETE CASCADE,
			seq             INT NOT NULL,
			raw_barcode     TEXT,
			raw_name        TEXT,
			raw_qty         TEXT,
			matched_barcode TEXT,
			matched_name    TEXT,
			matched_supp    TEXT,
			matched_src     TEXT,
			qty             INT,
			unit_price      NUMERIC(12,2),
			status          TEXT,
			is_new          BOOLEAN,
			stock_qty       NUMERIC(12,2),
			stock_diff      NUMERIC(12,2),
			stock_mismatch  BOOLEAN,
			is_deleted      BOOLEAN NOT NULL DEFAULT FALSE,
			UNIQUE (session_id, seq)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_row_session ON parse_row(session_id)`,
		`CREATE TABLE IF NOT EXISTS template (
			id              TEXT PRIMARY KEY,
			name            TEXT NOT NULL,
			supplier_name   TEXT NOT NULL DEFAULT '',
			mode            TEXT NOT NULL,
			llm_prompt      TEXT NOT NULL DEFAULT '',
			use_glm_ocr     BOOLEAN NOT NULL DEFAULT FALSE,
			header_keywords JSONB NOT NULL DEFAULT '[]'::jsonb,
			footer_keywords JSONB NOT NULL DEFAULT '[]'::jsonb,
			subtitle_keywords JSONB NOT NULL DEFAULT '[]'::jsonb,
			is_default      BOOLEAN NOT NULL DEFAULT FALSE,
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			note            TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_template_supplier ON template(supplier_name)`,
		`CREATE INDEX IF NOT EXISTS idx_template_default ON template(is_default)`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return fmt.Errorf("migrate (%s...): %w", trim(s, 60), err)
		}
	}
	return nil
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
