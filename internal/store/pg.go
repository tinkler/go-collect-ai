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

		// ============================================================
		// OCR 解析供应商特定策略 (Phase A, 2026-09-02) — docs/ocr-purchase-skill-architecture.md
		// 每家供应商一条;is_handwrite=true 走纯启发式不开 LLM
		// 通用解析累计 5 次触发自动建策略;edit_count>=3 触发自优化 (Phase B)
		// ============================================================
		`CREATE TABLE IF NOT EXISTS supplier_parse_strategy (
			supplier_name        TEXT PRIMARY KEY,
			is_handwrite         BOOLEAN NOT NULL DEFAULT FALSE,
			enabled              BOOLEAN NOT NULL DEFAULT TRUE,
			body                 TEXT NOT NULL DEFAULT '',
			sku_hints            JSONB NOT NULL DEFAULT '{}'::jsonb,
			llm_prompt_overlay   TEXT NOT NULL DEFAULT '',
			strategy_version     INT  NOT NULL DEFAULT 0,
			generic_apply_count  INT  NOT NULL DEFAULT 0,
			edit_count           INT  NOT NULL DEFAULT 0,
			created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			last_edited_at       TIMESTAMPTZ,
			last_auto_optimized_at TIMESTAMPTZ,
			last_applied_at      TIMESTAMPTZ,
			note                 TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sps_handwrite ON supplier_parse_strategy(is_handwrite) WHERE is_handwrite = TRUE`,
		`CREATE INDEX IF NOT EXISTS idx_sps_needs_build ON supplier_parse_strategy(generic_apply_count) WHERE body = '' OR enabled = FALSE`,

		// 2026-09-02: parse_session 加 strategy_version (Phase A 新增, 替代 template_id)
		//   - 落库时记本次解析用的 strategy 版本(0 = 通用解析)
		`ALTER TABLE parse_session ADD COLUMN IF NOT EXISTS strategy_version INT NOT NULL DEFAULT 0`,

		// 2026-09-02 Phase A: 删旧 template 表 / parse_session.template_id / parse_session.template_name
		//   - 旧 schema 残留 2 个 NOT NULL 列, drop 掉让 Phase A 的 INSERT 能跑
		//   - CASCADE 防止有 FK 引用(虽然 phase A 已经不引用了)
		`ALTER TABLE parse_session DROP COLUMN IF EXISTS template_id CASCADE`,
		`ALTER TABLE parse_session DROP COLUMN IF EXISTS template_name CASCADE`,
		`DROP TABLE IF EXISTS template CASCADE`,

		// ============================================================
		// 2026-09-03: 重复图去重 + 异步策略分析 (W4.1)
		// 需求:
		//   1) 上传重复图不重复处理 (image_hashes 数组 + image_index)
		//   2) 解析后不等策略分析 (analysis_status: pending|running|done|failed)
		//   3) 总结栏 + 行内图标 (alert.category)
		// ============================================================

		// parse_session: 加 image_hashes (JSONB 数组, 元素 = sha256 hex)
		`ALTER TABLE parse_session ADD COLUMN IF NOT EXISTS image_hashes JSONB NOT NULL DEFAULT '[]'::jsonb`,
		// parse_session: 加 analysis_status (分析状态)
		`ALTER TABLE parse_session ADD COLUMN IF NOT EXISTS analysis_status TEXT NOT NULL DEFAULT 'pending'`,
		// parse_session: 加 analysis_at (最近完成时间)
		`ALTER TABLE parse_session ADD COLUMN IF NOT EXISTS analysis_at TIMESTAMPTZ`,
		// parse_session: 加 analysis_error (失败原因)
		`ALTER TABLE parse_session ADD COLUMN IF NOT EXISTS analysis_error TEXT NOT NULL DEFAULT ''`,
		// GIN 索引: image_hashes 数组包含查询 (@>)
		`CREATE INDEX IF NOT EXISTS idx_session_image_hashes ON parse_session USING GIN (image_hashes)`,
		// 状态索引: 找 pending/running 的 session (cron 重试用)
		`CREATE INDEX IF NOT EXISTS idx_session_analysis_status ON parse_session(analysis_status) WHERE analysis_status IN ('pending', 'running')`,

		// parse_row: 加 image_index (属于第几张图, 0-based)
		`ALTER TABLE parse_row ADD COLUMN IF NOT EXISTS image_index INT NOT NULL DEFAULT 0`,
		// 索引: 按 image_index 查 (append 后回查用)
		`CREATE INDEX IF NOT EXISTS idx_row_session_image ON parse_row(session_id, image_index)`,

		// purchase_session_alert: 加 category (决定前端 icon 段位)
		//   block            → 红色感叹号 (限入场)
		//   warn             → 橙色感叹号 (高库存/不允许退货)
		//   info             → 灰普通感叹号 (难消化/反季/节假日)
		//   highlight_dui    → 绿色"贴切"标志 (堆头陈列)
		//   highlight_others → 绿色"其它"标志 (快讯/端架/特殊活动)
		`ALTER TABLE purchase_session_alert ADD COLUMN IF NOT EXISTS category TEXT NOT NULL DEFAULT 'info'`,
		`CREATE INDEX IF NOT EXISTS idx_psalert_category ON purchase_session_alert(session_id, category)`,

		// app_settings: 阈值配置 (K-V, 替换 Go 端硬编码)
		//   - high_stock_threshold: 库存数 > 阈值 → 高库存
		//   - low_movement_threshold: 30/60/90 天销量 < 阈值 → 难消化
		//   - duitou_kinds: 算"堆头陈列"的 kind 集合 (JSON 数组)
		//   - others_kinds: 算"快讯/其它活动"的 kind 集合
		`CREATE TABLE IF NOT EXISTS app_settings (
			key             TEXT PRIMARY KEY,
			value           JSONB NOT NULL,
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		// 默认值: 阈值放数据, Go 端 0 业务判断
		`INSERT INTO app_settings (key, value) VALUES
			('high_stock_threshold', '50'::jsonb),
			('low_movement_threshold_30d', '3'::jsonb),
			('duitou_kinds', '["堆头"]'::jsonb),
			('others_kinds', '["端架", "快讯", "DM", "特价", "海报"]'::jsonb)
		ON CONFLICT (key) DO NOTHING`,

		// ============================================================
		// freshcheck 模块 RBAC perm seed (W1.4d, 2026-09-09)
		// 4 个新 perm, 给前端 perm guard + 后端 RequirePerm 用
		// owner 通配 * 自动覆盖, 不需要单独 seed
		// ============================================================
		`INSERT INTO permissions (id, domain, action, description) VALUES
			('freshcheck:pool:write',     'freshcheck', 'pool:write',     '入框/出框事件录入 (H5 录单用)'),
			('freshcheck:settle:read',    'freshcheck', 'settle:read',    '查看周期结算结果 (R1-R5 报表)'),
			('freshcheck:settle:run',     'freshcheck', 'settle:run',     '触发周期结算 + 重算 + 跨期重算'),
			('freshcheck:config:write',   'freshcheck', 'config:write',   '改生鲜SKU映射/特价码/损耗率/阈值/轨道配置'),
			('freshcheck:override',       'freshcheck', 'override',       '周期锁超线豁免 + 告警处置')
		ON CONFLICT (id) DO NOTHING`,

		// ----- role_permissions 关联 -----
		// 角色策略 (跟 plan.md §6.2 表格对齐):
		//   - owner    拿 * (不需 seed)
		//   - manager  拿全部 4 个 freshcheck perm
		//   - buyer    拿 settle:read + settle:run + config:write
		//   - floor    只拿 pool:write
		//   - office   拿 settle:read
		//   - cashier  无
		`INSERT INTO role_permissions (role_id, perm_id) VALUES
			('manager', 'freshcheck:pool:write'),
			('manager', 'freshcheck:settle:read'),
			('manager', 'freshcheck:settle:run'),
			('manager', 'freshcheck:config:write'),
			('manager', 'freshcheck:override'),
			('buyer',   'freshcheck:settle:read'),
			('buyer',   'freshcheck:settle:run'),
			('buyer',   'freshcheck:config:write'),
			('floor',   'freshcheck:pool:write'),
			('office',  'freshcheck:settle:read')
		ON CONFLICT DO NOTHING`,

		// ============================================================
		// freshcheck 生鲜免日盘管理 (W1, 2026-09-09)
		// 需求: docs/生鲜免日盘管理扩展子系统设计需求文档.md v1.0
		// 设计: docs/freshcheck-{architecture,data-model,settlement}.md
		// 13 张表: 5 配置 + 2 人工 + 6 派生
		// 业务阈值/损耗率/品类轨道走表, 改阈值免 build
		// 派生表: 只 INSERT 不 UPDATE (重算走"先清后写")
		// ============================================================

		// ----- 1. freshcheck_sku_map (配置: 生鲜SKU映射) -----
		`CREATE TABLE IF NOT EXISTS freshcheck_sku_map (
			id                BIGSERIAL PRIMARY KEY,
			branch_no         TEXT NOT NULL DEFAULT '0001',
			item_no           TEXT NOT NULL,
			item_name         TEXT NOT NULL DEFAULT '',
			fresh_category    TEXT NOT NULL,
			turnover_class    TEXT NOT NULL CHECK (turnover_class IN ('fast','slow')),
			shelf_life_days   INT  NOT NULL DEFAULT 7,
			default_pool_code TEXT,
			is_active         BOOLEAN NOT NULL DEFAULT TRUE,
			created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (branch_no, item_no)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_fcsm_category ON freshcheck_sku_map(branch_no, fresh_category) WHERE is_active`,
		`CREATE INDEX IF NOT EXISTS idx_fcsm_pool ON freshcheck_sku_map(branch_no, default_pool_code) WHERE default_pool_code IS NOT NULL`,

		// ----- 2. freshcheck_pool_code (配置: 特价码定义) -----
		`CREATE TABLE IF NOT EXISTS freshcheck_pool_code (
			id              BIGSERIAL PRIMARY KEY,
			branch_no       TEXT NOT NULL DEFAULT '0001',
			pool_code       TEXT NOT NULL,
			pool_name       TEXT NOT NULL,
			pricing_mode    TEXT NOT NULL CHECK (pricing_mode IN ('weight','piece')),
			unit_price      NUMERIC(10,2) NOT NULL,
			piece_weight    NUMERIC(10,4),
			priority_rank   INT NOT NULL DEFAULT 0,
			is_active       BOOLEAN NOT NULL DEFAULT TRUE,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (branch_no, pool_code)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_fcpc_priority ON freshcheck_pool_code(branch_no, priority_rank) WHERE is_active`,

		// ----- 3. freshcheck_loss_rate (配置: 损耗率规则) -----
		`CREATE TABLE IF NOT EXISTS freshcheck_loss_rate (
			id                  BIGSERIAL PRIMARY KEY,
			fresh_category      TEXT NOT NULL,
			turnover_class      TEXT NOT NULL CHECK (turnover_class IN ('fast','slow')),
			loss_type           TEXT NOT NULL CHECK (loss_type IN ('natural','spoilage','process')),
			daily_rate          NUMERIC(8,6) NOT NULL,
			effective_from      DATE NOT NULL DEFAULT CURRENT_DATE,
			effective_to        DATE,
			is_calibrated       BOOLEAN NOT NULL DEFAULT FALSE,
			last_calibrate_at   TIMESTAMPTZ,
			last_calibrate_by   TEXT NOT NULL DEFAULT '',
			created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (fresh_category, turnover_class, loss_type, effective_from)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_fclr_active ON freshcheck_loss_rate(fresh_category, turnover_class, loss_type) WHERE effective_to IS NULL`,

		// ----- 4. freshcheck_threshold (配置: 业务阈值 K-V) -----
		`CREATE TABLE IF NOT EXISTS freshcheck_threshold (
			key             TEXT PRIMARY KEY,
			value           NUMERIC(10,4) NOT NULL,
			unit            TEXT NOT NULL DEFAULT '',
			description     TEXT NOT NULL DEFAULT '',
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_by      TEXT NOT NULL DEFAULT ''
		)`,

		// ----- 5. freshcheck_pool_event (人工: 入框/出框事件) -----
		`CREATE TABLE IF NOT EXISTS freshcheck_pool_event (
			id              BIGSERIAL PRIMARY KEY,
			branch_no       TEXT NOT NULL DEFAULT '0001',
			pool_code       TEXT NOT NULL,
			item_no         TEXT NOT NULL,
			event_kind      TEXT NOT NULL CHECK (event_kind IN ('in','out')),
			event_time      TIMESTAMPTZ NOT NULL,
			recorded_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			weight_kg       NUMERIC(12,4) NOT NULL DEFAULT 0,
			piece_count     INT NOT NULL DEFAULT 0,
			out_destination TEXT CHECK (out_destination IN ('sold_out','spoiled','return_to_shelf','downgrade')),
			downgrade_to    TEXT,
			operator        TEXT NOT NULL,
			confidence      TEXT NOT NULL DEFAULT 'high' CHECK (confidence IN ('high','low')),
			source          TEXT NOT NULL DEFAULT 'h5' CHECK (source IN ('h5','admin','import')),
			note            TEXT NOT NULL DEFAULT '',
			UNIQUE (branch_no, pool_code, item_no, event_kind, event_time)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_fcpe_pool_time ON freshcheck_pool_event(branch_no, pool_code, event_time DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_fcpe_item_time ON freshcheck_pool_event(branch_no, item_no, event_time DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_fcpe_low_conf ON freshcheck_pool_event(confidence) WHERE confidence = 'low'`,

		// ----- 6. freshcheck_period_stock (人工: 周期盘点) -----
		`CREATE TABLE IF NOT EXISTS freshcheck_period_stock (
			id              BIGSERIAL PRIMARY KEY,
			branch_no       TEXT NOT NULL DEFAULT '0001',
			period_id       BIGINT NOT NULL,
			item_no         TEXT NOT NULL,
			item_name       TEXT NOT NULL DEFAULT '',
			qty             NUMERIC(12,4) NOT NULL,
			unit            TEXT NOT NULL DEFAULT '',
			stock_time      TIMESTAMPTZ NOT NULL,
			operator        TEXT NOT NULL,
			confidence      TEXT NOT NULL DEFAULT 'high' CHECK (confidence IN ('high','low')),
			source          TEXT NOT NULL DEFAULT 'h5' CHECK (source IN ('h5','admin','import')),
			note            TEXT NOT NULL DEFAULT '',
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (period_id, item_no)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_fcps_period ON freshcheck_period_stock(period_id)`,

		// ----- 7. freshcheck_category_track (配置: 品类结算轨道) -----
		`CREATE TABLE IF NOT EXISTS freshcheck_category_track (
			id                    BIGSERIAL PRIMARY KEY,
			fresh_category        TEXT NOT NULL,
			branch_no             TEXT NOT NULL DEFAULT '0001',
			track_code            TEXT NOT NULL,
			period_lock_days      INT  NOT NULL,
			stock_freq            TEXT NOT NULL CHECK (stock_freq IN ('daily','weekly','monthly','none')),
			require_stock         BOOLEAN NOT NULL DEFAULT TRUE,
			last_settle_at        TIMESTAMPTZ,
			next_settle_deadline  TIMESTAMPTZ,
			is_active             BOOLEAN NOT NULL DEFAULT TRUE,
			created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (branch_no, fresh_category)
		)`,

		// ----- 8. freshcheck_settlement (派生: R1 周期单品毛利) -----
		`CREATE TABLE IF NOT EXISTS freshcheck_settlement (
			id                    BIGSERIAL PRIMARY KEY,
			branch_no             TEXT NOT NULL DEFAULT '0001',
			period_id             BIGINT NOT NULL,
			track_code            TEXT NOT NULL,
			fresh_category        TEXT NOT NULL,
			item_no               TEXT NOT NULL,
			item_name             TEXT NOT NULL DEFAULT '',
			begin_qty             NUMERIC(12,4) NOT NULL DEFAULT 0,
			purchase_qty          NUMERIC(12,4) NOT NULL DEFAULT 0,
			normal_sale_qty       NUMERIC(12,4) NOT NULL DEFAULT 0,
			normal_sale_amt       NUMERIC(12,2) NOT NULL DEFAULT 0,
			pool_alloc_qty        NUMERIC(12,4) NOT NULL DEFAULT 0,
			pool_alloc_amt        NUMERIC(12,2) NOT NULL DEFAULT 0,
			end_qty               NUMERIC(12,4) NOT NULL DEFAULT 0,
			backflush_qty         NUMERIC(12,4) NOT NULL DEFAULT 0,
			loss_qty              NUMERIC(12,4) NOT NULL DEFAULT 0,
			box_loss_qty          NUMERIC(12,4) NOT NULL DEFAULT 0,
			avg_cost              NUMERIC(12,4) NOT NULL,
			sale_cost             NUMERIC(12,2) NOT NULL DEFAULT 0,
			total_revenue         NUMERIC(12,2) NOT NULL DEFAULT 0,
			gross_profit          NUMERIC(12,2) NOT NULL DEFAULT 0,
			gross_profit_rate     NUMERIC(8,4),
			confidence            TEXT NOT NULL DEFAULT 'high',
			is_overridden         BOOLEAN NOT NULL DEFAULT FALSE,
			override_reason       TEXT NOT NULL DEFAULT '',
			window_start          TIMESTAMPTZ NOT NULL,
			window_end            TIMESTAMPTZ NOT NULL,
			window_days           INT  NOT NULL,
			status                TEXT NOT NULL DEFAULT 'finalized' CHECK (status IN ('finalized','recalculating','overridden','voided')),
			settled_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			settled_by            TEXT NOT NULL,
			conservation_ok       BOOLEAN NOT NULL DEFAULT TRUE,
			conservation_msg      TEXT NOT NULL DEFAULT '',
			idempotency_key       TEXT NOT NULL,
			UNIQUE (idempotency_key)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_fcs_period ON freshcheck_settlement(period_id, item_no)`,
		`CREATE INDEX IF NOT EXISTS idx_fcs_track ON freshcheck_settlement(track_code, window_end DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_fcs_item ON freshcheck_settlement(branch_no, item_no, window_end DESC)`,

		// ----- 9. freshcheck_alloc (派生: R2 特价归因明细) -----
		`CREATE TABLE IF NOT EXISTS freshcheck_alloc (
			id                  BIGSERIAL PRIMARY KEY,
			period_id           BIGINT NOT NULL,
			pool_code           TEXT NOT NULL,
			segment_start       TIMESTAMPTZ NOT NULL,
			segment_end         TIMESTAMPTZ NOT NULL,
			item_no             TEXT NOT NULL,
			in_weight_kg        NUMERIC(12,4) NOT NULL DEFAULT 0,
			out_weight_kg       NUMERIC(12,4) NOT NULL DEFAULT 0,
			spoiled_weight_kg   NUMERIC(12,4) NOT NULL DEFAULT 0,
			weight_diff_kg      NUMERIC(12,4) NOT NULL DEFAULT 0,
			pool_pos_qty        NUMERIC(12,4) NOT NULL DEFAULT 0,
			pool_pos_amt        NUMERIC(12,2) NOT NULL DEFAULT 0,
			share_weight        NUMERIC(8,6) NOT NULL DEFAULT 0,
			confidence_factor   NUMERIC(4,2) NOT NULL DEFAULT 1.00,
			alloc_qty           NUMERIC(12,4) NOT NULL DEFAULT 0,
			alloc_amt           NUMERIC(12,2) NOT NULL DEFAULT 0,
			backflush_alloc_qty NUMERIC(12,4) NOT NULL DEFAULT 0,
			deviation_qty       NUMERIC(12,4) NOT NULL DEFAULT 0,
			deviation_rate      NUMERIC(8,4),
			needs_review        BOOLEAN NOT NULL DEFAULT FALSE,
			created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (period_id, pool_code, segment_start, item_no)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_fca_pool ON freshcheck_alloc(period_id, pool_code)`,

		// ----- 10. freshcheck_box_recon (派生: R3 框内对账) -----
		`CREATE TABLE IF NOT EXISTS freshcheck_box_recon (
			id                  BIGSERIAL PRIMARY KEY,
			period_id           BIGINT NOT NULL,
			branch_no           TEXT NOT NULL DEFAULT '0001',
			pool_code           TEXT NOT NULL,
			segment_start       TIMESTAMPTZ NOT NULL,
			segment_end         TIMESTAMPTZ NOT NULL,
			in_total_kg         NUMERIC(12,4) NOT NULL DEFAULT 0,
			out_total_kg        NUMERIC(12,4) NOT NULL DEFAULT 0,
			out_sold_out_kg     NUMERIC(12,4) NOT NULL DEFAULT 0,
			out_spoiled_kg      NUMERIC(12,4) NOT NULL DEFAULT 0,
			out_return_kg       NUMERIC(12,4) NOT NULL DEFAULT 0,
			out_downgrade_kg    NUMERIC(12,4) NOT NULL DEFAULT 0,
			pos_total_kg        NUMERIC(12,4) NOT NULL DEFAULT 0,
			box_loss_kg         NUMERIC(12,4) NOT NULL DEFAULT 0,
			box_loss_amt        NUMERIC(12,2) NOT NULL DEFAULT 0,
			responsible_user    TEXT NOT NULL DEFAULT '',
			responsible_at      TIMESTAMPTZ,
			needs_investigate   BOOLEAN NOT NULL DEFAULT FALSE,
			investigate_note    TEXT NOT NULL DEFAULT '',
			created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (period_id, pool_code, segment_start)
		)`,

		// ----- 11. freshcheck_alert (派生: C1-C8 告警日志) -----
		`CREATE TABLE IF NOT EXISTS freshcheck_alert (
			id              BIGSERIAL PRIMARY KEY,
			branch_no       TEXT NOT NULL DEFAULT '0001',
			period_id       BIGINT,
			rule_code       TEXT NOT NULL,
			severity        TEXT NOT NULL CHECK (severity IN ('info','warn','block')),
			entity_type     TEXT NOT NULL,
			entity_id       TEXT NOT NULL,
			message         TEXT NOT NULL,
			payload         JSONB NOT NULL DEFAULT '{}'::jsonb,
			status          TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','overridden','fixed','ignored')),
			override_by     TEXT NOT NULL DEFAULT '',
			override_reason TEXT NOT NULL DEFAULT '',
			override_at     TIMESTAMPTZ,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_fca_status ON freshcheck_alert(branch_no, status, created_at DESC) WHERE status = 'open'`,
		`CREATE INDEX IF NOT EXISTS idx_fca_period ON freshcheck_alert(period_id) WHERE period_id IS NOT NULL`,

		// ----- 12. freshcheck_loss_calibrate (派生+人工: 损耗率校准) -----
		`CREATE TABLE IF NOT EXISTS freshcheck_loss_calibrate (
			id                  BIGSERIAL PRIMARY KEY,
			branch_no           TEXT NOT NULL DEFAULT '0001',
			fresh_category      TEXT NOT NULL,
			turnover_class      TEXT NOT NULL,
			period_window_start DATE NOT NULL,
			period_window_end   DATE NOT NULL,
			measured_loss_qty   NUMERIC(12,4) NOT NULL,
			measured_throughput NUMERIC(12,4) NOT NULL,
			measured_rate       NUMERIC(8,6) NOT NULL,
			preset_rate         NUMERIC(8,6) NOT NULL,
			deviation_pct       NUMERIC(8,4) NOT NULL,
			action              TEXT NOT NULL DEFAULT 'pending' CHECK (action IN ('pending','update_preset','investigate','discard')),
			action_by           TEXT NOT NULL DEFAULT '',
			action_at           TIMESTAMPTZ,
			action_note         TEXT NOT NULL DEFAULT '',
			created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (branch_no, fresh_category, turnover_class, period_window_end)
		)`,

		// ----- 13. freshcheck_config_snap (审计: 配置变更快照) -----
		`CREATE TABLE IF NOT EXISTS freshcheck_config_snap (
			id              BIGSERIAL PRIMARY KEY,
			table_name      TEXT NOT NULL,
			row_pk          TEXT NOT NULL,
			action          TEXT NOT NULL CHECK (action IN ('create','update','delete')),
			old_value       JSONB,
			new_value       JSONB,
			changed_by      TEXT NOT NULL,
			changed_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			rollback_to     TIMESTAMPTZ,
			rollback_by     TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_fcs_snap ON freshcheck_config_snap(table_name, row_pk, changed_at DESC)`,

		// ----- seed: 业务阈值 11 行默认值 -----
		// 2026-09-09: 移除 c7_sync_diff_pct (零同步架构下不适用, 详情见 pr-summary.md "设计反思")
		// 改阈值只需 UPDATE freshcheck_threshold, 不需要 build
		`INSERT INTO freshcheck_threshold (key, value, unit, description) VALUES
			('c1_pool_saturation_pct', 15.00, 'pct', 'C1 池内饱和度容差 (|sum_alloc - pos_qty| / pos_qty)'),
			('c2_leakage_multiplier_weekly', 2.00, 'multiplier', 'C2 周结池外泄漏倍数 (backflush > loss*k 告警)'),
			('c2_leakage_multiplier_long', 1.80, 'multiplier', 'C2 月结/长窗口池外泄漏倍数'),
			('c6_period_lock_leaf_days', 7, 'days', 'C6 叶菜周期锁 (最长不结算天数)'),
			('c6_period_lock_root_days', 30, 'days', 'C6 根茎周期锁'),
			('c6_period_lock_aquatic_days', 14, 'days', 'C6 水产周期锁'),
			('c6_period_lock_meat_days', 14, 'days', 'C6 肉类周期锁'),
			('c6_period_lock_frozen_days', 30, 'days', 'C6 冻品周期锁'),
			('c8_period_stock_coverage_pct', 100.00, 'pct', 'C8 盘点覆盖率 (生鲜 SKU 期末盘点必须 100%)'),
			('loss_calibrate_deviation_pct', 20.00, 'pct', '损耗率校准偏差阈值 (实测 vs 预设)'),
			('low_confidence_weight', 0.50, 'factor', '低置信度事件分摊权重 (1.0 - confidence_factor)')
		ON CONFLICT (key) DO NOTHING`,

		// ----- seed: 5 个品类结算轨道 -----
		// period_lock_days 默认从 freshcheck_threshold 读; 这里只填 track_code + stock_freq
		// last_settle_at = NULL (首次结算由首次盘点触发)
		`INSERT INTO freshcheck_category_track (fresh_category, branch_no, track_code, period_lock_days, stock_freq, require_stock) VALUES
			('leaf',    '0001', 'leaf-weekly',     7,  'weekly',  TRUE),
			('root',    '0001', 'root-monthly',    30, 'monthly', TRUE),
			('aquatic', '0001', 'aquatic-biweekly',14, 'weekly',  TRUE),
			('meat',    '0001', 'meat-biweekly',   14, 'weekly',  TRUE),
			('frozen',  '0001', 'frozen-monthly',  30, 'monthly', TRUE)
		ON CONFLICT (branch_no, fresh_category) DO NOTHING`,

		// ============================================================
		// 企微群绑定 (2026-09-11, 任务: 群用途配置化 + LLM 推断)
		//   替代原先 3 个 env 注入 (PROMOTION_ALERT_CHAT_ID / OWNER_CHAT_ID /
		//   COLLECTAI_AGENT_CHAT_IDS),UI 在 admin/system.html 配
		// ============================================================
		// purpose 白名单: agent (智能对话) | fee (费用录入) | promo_alert (堆头费到期预警) |
		//                owner (店主私享) | office (办公室) | floor (卖场) | log (仅记录) | other
		`CREATE TABLE IF NOT EXISTS wecom_chat_binding (
			chat_id      TEXT PRIMARY KEY,
			purpose      TEXT NOT NULL DEFAULT 'log'
			              CHECK (purpose IN ('agent','fee','promo_alert','owner','office','floor','log','other')),
			label        TEXT NOT NULL DEFAULT '',
			enabled      BOOLEAN NOT NULL DEFAULT TRUE,
			note         TEXT NOT NULL DEFAULT '',
			first_seen   TIMESTAMPTZ,                    -- 自动发现时间 (wecom 客户端上报)
			created_by   TEXT NOT NULL DEFAULT 'system', -- 配置人 user_id
			created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_wcb_purpose ON wecom_chat_binding(purpose) WHERE enabled`,
		`CREATE INDEX IF NOT EXISTS idx_wcb_updated ON wecom_chat_binding(updated_at DESC)`,
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
