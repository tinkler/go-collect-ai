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
			ocr_model       TEXT NOT NULL DEFAULT '',
			llm_model       TEXT NOT NULL DEFAULT '',
			use_llm         BOOLEAN,
			fuzzy_distance  INT,
			header_keywords JSONB NOT NULL DEFAULT '[]'::jsonb,
			footer_keywords JSONB NOT NULL DEFAULT '[]'::jsonb,
			subtitle_keywords JSONB NOT NULL DEFAULT '[]'::jsonb,
			is_default      BOOLEAN NOT NULL DEFAULT FALSE,
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			note            TEXT NOT NULL DEFAULT ''
		)`,
		// 兼容老库
		`ALTER TABLE template ADD COLUMN IF NOT EXISTS ocr_model TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE template ADD COLUMN IF NOT EXISTS llm_model TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE template ADD COLUMN IF NOT EXISTS use_llm BOOLEAN`,
		`ALTER TABLE template ADD COLUMN IF NOT EXISTS fuzzy_distance INT`,
		// 删历史死代码字段
		`ALTER TABLE template DROP COLUMN IF EXISTS use_glm_ocr`,
		`CREATE INDEX IF NOT EXISTS idx_template_supplier ON template(supplier_name)`,
		`CREATE INDEX IF NOT EXISTS idx_template_default ON template(is_default)`,

		// ============================================================
		// 智能采购模块 (W1, 2026-09-01) — agent-purchase-plan.md §6
		// 依赖 trpc-agent-go; 工具/Agent 入口: internal/agent/
		// ============================================================

		// 供应商政策 (A 模块) — 一家供应商同一 key 唯一
		`CREATE TABLE IF NOT EXISTS supplier_policy (
			id              BIGSERIAL PRIMARY KEY,
			supplier_name   TEXT NOT NULL,
			key             TEXT NOT NULL,
			value           JSONB NOT NULL,
			source          TEXT NOT NULL,
			chat_id         TEXT NOT NULL DEFAULT '',
			message_id      TEXT NOT NULL DEFAULT '',
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (supplier_name, key)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_supplier_policy_supplier ON supplier_policy(supplier_name)`,
		`CREATE INDEX IF NOT EXISTS idx_supplier_policy_key ON supplier_policy(key)`,

		// 特殊日历 (A 模块) — 节假日/促销/季节 决策辅助
		`CREATE TABLE IF NOT EXISTS special_calendar (
			id              BIGSERIAL PRIMARY KEY,
			date            DATE NOT NULL,
			type            TEXT NOT NULL,    -- 'holiday' | 'promo' | 'blackout' | 'season_start' | 'season_end'
			name            TEXT NOT NULL,
			lead_days       INT NOT NULL DEFAULT 0,
			note            TEXT NOT NULL DEFAULT '',
			source          TEXT NOT NULL,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (date, type, name)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_special_calendar_date ON special_calendar(date)`,
		`CREATE INDEX IF NOT EXISTS idx_special_calendar_type ON special_calendar(type, date)`,

		// 促销费用 (A 模块) — 堆头/端架/陈列/DM
		`CREATE TABLE IF NOT EXISTS promotion_fee (
			id              BIGSERIAL PRIMARY KEY,
			supplier_name   TEXT NOT NULL,
			kind            TEXT NOT NULL,    -- '堆头' | '端架' | '陈列' | 'DM' | '条码费'
			amount          NUMERIC(12,2) NOT NULL,
			period_start    DATE NOT NULL,
			period_end      DATE NOT NULL,
			note            TEXT NOT NULL DEFAULT '',
			source          TEXT NOT NULL,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_promotion_fee_supplier ON promotion_fee(supplier_name, period_end DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_promotion_fee_period ON promotion_fee(period_start, period_end)`,

		// ============================================================
		// 采购订单智能提醒 (W3.2, 2026-09-01) — agent-purchase-plan.md §4
		// 规则引擎产出: 限入场 / 季节不匹配 / 节假日 lead_days
		// ============================================================
		`CREATE TABLE IF NOT EXISTS purchase_session_alert (
			id              BIGSERIAL PRIMARY KEY,
			session_id      UUID NOT NULL REFERENCES parse_session(id) ON DELETE CASCADE,
			row_id          BIGINT REFERENCES parse_row(id) ON DELETE CASCADE,
			rule            TEXT NOT NULL,    -- 'block_entry' | 'no_return' | 'offseason' | 'holiday_lead'
			severity        TEXT NOT NULL,    -- 'block' | 'warn' | 'info'
			message         TEXT NOT NULL,
			acked_at        TIMESTAMPTZ,
			acked_by        TEXT NOT NULL DEFAULT '',
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_psalert_session ON purchase_session_alert(session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_psalert_rule ON purchase_session_alert(rule, severity)`,
		`CREATE INDEX IF NOT EXISTS idx_psalert_pending ON purchase_session_alert(session_id) WHERE acked_at IS NULL`,

		// ============================================================
		// 现金日报 + 供应商结算 (W4, 2026-09-01) — agent-purchase-plan.md §5
		// D 模块数据源: cash_balance (短期手动 / 中期 RPA / 长期 cube)
		// ============================================================
		`CREATE TABLE IF NOT EXISTS cash_balance (
			id              BIGSERIAL PRIMARY KEY,
			balance_date    DATE NOT NULL UNIQUE,
			amount          NUMERIC(14,2) NOT NULL,
			source          TEXT NOT NULL,    -- 'manual' | 'rpa' | 'cube'
			note            TEXT NOT NULL DEFAULT '',
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_cash_balance_date ON cash_balance(balance_date DESC)`,

		// 供应商结算建议 (W4)
		`CREATE TABLE IF NOT EXISTS supplier_forecast (
			id              BIGSERIAL PRIMARY KEY,
			supplier_name   TEXT NOT NULL,
			forecast_date   DATE NOT NULL,
			horizon_days    INT NOT NULL,    -- 7 / 30 / 90
			amount          NUMERIC(12,2) NOT NULL,
			basis           TEXT NOT NULL DEFAULT '',
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_supplier_forecast_supplier ON supplier_forecast(supplier_name, created_at DESC)`,

		`CREATE TABLE IF NOT EXISTS supplier_payment_suggestion (
			id                    BIGSERIAL PRIMARY KEY,
			supplier_name         TEXT NOT NULL,
			period_days           INT NOT NULL,
			base_forecast         NUMERIC(12,2) NOT NULL,
			investment_weight     NUMERIC(4,2) NOT NULL,
			promo_weight          NUMERIC(4,2) NOT NULL,
			sellthrough_weight    NUMERIC(4,2) NOT NULL,
			payment_cycle_days    INT NOT NULL,
			amount                NUMERIC(12,2) NOT NULL,
			basis                 JSONB NOT NULL DEFAULT '{}'::jsonb,
			status                TEXT NOT NULL DEFAULT 'pending',
			acked_by              TEXT NOT NULL DEFAULT '',
			acked_at              TIMESTAMPTZ,
			created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sps_supplier ON supplier_payment_suggestion(supplier_name, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_sps_status ON supplier_payment_suggestion(status) WHERE status = 'pending'`,

		`CREATE TABLE IF NOT EXISTS promotion_fee_share (
			id              BIGSERIAL PRIMARY KEY,
			supplier_name   TEXT NOT NULL,
			share_month     DATE NOT NULL,    -- 月初, e.g. 2026-09-01
			kind            TEXT NOT NULL,    -- 堆头/端架/陈列/DM/条码费
			amount          NUMERIC(12,2) NOT NULL,
			period_start    DATE NOT NULL,
			period_end      DATE NOT NULL,
			days_in_month   INT NOT NULL,    -- 当月在 period 内的天数 (按月分摊)
			note            TEXT NOT NULL DEFAULT '',
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_pfs_supplier ON promotion_fee_share(supplier_name, share_month DESC)`,

		// ============== restock 模块 (2026-09-02 重构后精简) ==============
		// 保留 4 张表:
		//   restock_display_suggest  陈列补货建议
		//   restock_short_state      短补锁定
		//   restock_need_purchase    采购计划单
		//   restock_tick_log         tick 执行日志
		`CREATE TABLE IF NOT EXISTS restock_display_suggest (
			branch_no      TEXT NOT NULL,
			item_no        TEXT NOT NULL,
			period_date    DATE NOT NULL,
			suggest_qty    INT NOT NULL DEFAULT 0,
			inv_snapshot   INT NOT NULL DEFAULT 0,
			last_period    TEXT NOT NULL DEFAULT '',
			last_sale_at   TIMESTAMPTZ,
			last_update_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			item_name      TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (branch_no, item_no, period_date)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_rds_suggest ON restock_display_suggest(branch_no, period_date DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_rds_item ON restock_display_suggest(item_no)`,

		`CREATE TABLE IF NOT EXISTS restock_short_state (
			branch_no  TEXT NOT NULL,
			item_no    TEXT NOT NULL,
			is_short   BOOLEAN NOT NULL DEFAULT FALSE,
			short_at   TIMESTAMPTZ,
			short_user TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (branch_no, item_no)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_rss_short ON restock_short_state(branch_no) WHERE is_short = TRUE`,

		`CREATE TABLE IF NOT EXISTS restock_need_purchase (
			id              BIGSERIAL PRIMARY KEY,
			branch_no       TEXT NOT NULL,
			item_no         TEXT NOT NULL,
			item_name       TEXT NOT NULL DEFAULT '',
			barcode         TEXT NOT NULL DEFAULT '',
			supplier_name   TEXT NOT NULL DEFAULT '',
			suggest_qty     INT NOT NULL DEFAULT 0,
			trigger_kind    TEXT NOT NULL,
			trigger_task_id TEXT NOT NULL DEFAULT '',
			status          TEXT NOT NULL DEFAULT 'pending',
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			exported_at     TIMESTAMPTZ
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_rnp_branch_item_pending
			ON restock_need_purchase(branch_no, item_no) WHERE status = 'pending'`,
		`CREATE INDEX IF NOT EXISTS idx_rnp_status ON restock_need_purchase(branch_no, status)`,
		`CREATE INDEX IF NOT EXISTS idx_rnp_supplier ON restock_need_purchase(supplier_name, created_at DESC)`,

		`CREATE TABLE IF NOT EXISTS restock_tick_log (
			id           BIGSERIAL PRIMARY KEY,
			branch_no    TEXT NOT NULL,
			period       TEXT NOT NULL,
			tick_at      TIMESTAMPTZ NOT NULL,
			window_from  TIMESTAMPTZ NOT NULL,
			window_to    TIMESTAMPTZ NOT NULL,
			status       TEXT NOT NULL,
			error_msg    TEXT,
			items_count  INT NOT NULL DEFAULT 0,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_rtl_branch_status ON restock_tick_log(branch_no, status, created_at DESC)`,

		// 2026-09-02 重构: 删 4 张旧表
		//   - restock_task           旧 ROP 触发 task 体系
		//   - restock_feedback       旧反馈审计 (新版不再需要, 写 display_suggest.last_update_at 已能体现)
		//   - restock_sales_watch    旧 R2/R2b 24h 销售观测
		//   - supplier_reliability   旧 LLM 调量用 fill_rate
		`DROP TABLE IF EXISTS restock_task CASCADE`,
		`DROP TABLE IF EXISTS restock_feedback CASCADE`,
		`DROP TABLE IF EXISTS restock_sales_watch CASCADE`,
		`DROP TABLE IF EXISTS supplier_reliability CASCADE`,
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
