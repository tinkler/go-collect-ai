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
			image_paths     JSONB NOT NULL DEFAULT '[]'::jsonb,
			image_urls      JSONB NOT NULL DEFAULT '[]'::jsonb,
			source          TEXT NOT NULL,
			raw_ocr_json    JSONB,
			raw_llm_json    JSONB,
			note            TEXT,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_session_created ON parse_session(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_session_supplier ON parse_session(supplier_name)`,
		// 多图字段兼容老库(2026-08-28 加入, 用于企微 H5 多图采购收货单)
		`ALTER TABLE parse_session ADD COLUMN IF NOT EXISTS image_paths JSONB NOT NULL DEFAULT '[]'::jsonb`,
		`ALTER TABLE parse_session ADD COLUMN IF NOT EXISTS image_urls  JSONB NOT NULL DEFAULT '[]'::jsonb`,
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

		// ============== restock 模块 5 张表(追加于 2026-08-26) ==============

		`CREATE TABLE IF NOT EXISTS restock_task (
			task_id         TEXT PRIMARY KEY,
			branch_no       TEXT NOT NULL,
			item_no         TEXT NOT NULL,
			item_name       TEXT NOT NULL DEFAULT '',
			supplier_name   TEXT,
			current_stock   INT NOT NULL DEFAULT 0,
			safety_stock    INT NOT NULL DEFAULT 0,
			yesterday_sales INT NOT NULL DEFAULT 0,
			suggest_qty     INT NOT NULL DEFAULT 0,
			reason          TEXT,
			priority        TEXT NOT NULL DEFAULT 'P2',
			status          TEXT NOT NULL DEFAULT 'open',
			first_push_at   TIMESTAMPTZ,
			last_push_at    TIMESTAMPTZ,
			last_update_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			closed_at       TIMESTAMPTZ,
			closed_reason   TEXT,
			push_count      INT NOT NULL DEFAULT 0
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_open_task
			ON restock_task (branch_no, item_no) WHERE status='open'`,
		`CREATE INDEX IF NOT EXISTS idx_task_status
			ON restock_task (status, last_push_at)`,

		`CREATE TABLE IF NOT EXISTS restock_feedback (
			id            BIGSERIAL PRIMARY KEY,
			task_id       TEXT NOT NULL,
			feedback_type TEXT NOT NULL,
			feedback_user TEXT NOT NULL,
			feedback_time TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_feedback_task ON restock_feedback(task_id)`,
		`CREATE INDEX IF NOT EXISTS idx_feedback_time ON restock_feedback(feedback_time DESC)`,

		`CREATE TABLE IF NOT EXISTS restock_sales_watch (
			branch_no    TEXT NOT NULL,
			item_no      TEXT NOT NULL,
			window_start TIMESTAMPTZ NOT NULL,
			window_end   TIMESTAMPTZ NOT NULL,
			sale_qnty    INT NOT NULL DEFAULT 0,
			PRIMARY KEY (branch_no, item_no, window_start)
		)`,

		`CREATE TABLE IF NOT EXISTS restock_need_purchase (
			id              BIGSERIAL PRIMARY KEY,
			branch_no       TEXT NOT NULL,
			item_no         TEXT NOT NULL,
			item_name       TEXT NOT NULL DEFAULT '',
			barcode         TEXT,
			supplier_name   TEXT,
			suggest_qty     INT NOT NULL DEFAULT 0,
			trigger_kind    TEXT NOT NULL,
			trigger_task_id TEXT,
			status          TEXT NOT NULL DEFAULT 'pending',
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			exported_at     TIMESTAMPTZ
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_pending_need
			ON restock_need_purchase (branch_no, item_no) WHERE status='pending'`,
		`CREATE INDEX IF NOT EXISTS idx_need_pending
			ON restock_need_purchase (branch_no, status, created_at DESC) WHERE status='pending'`,

		`CREATE TABLE IF NOT EXISTS supplier_reliability (
			supplier_name TEXT NOT NULL,
			item_no       TEXT NOT NULL,
			requested_qty NUMERIC(12,2) NOT NULL DEFAULT 0,
			supplied_qty  NUMERIC(12,2) NOT NULL DEFAULT 0,
			fill_rate     NUMERIC(5,2)  NOT NULL DEFAULT 1.0,
			avg_lead_days NUMERIC(5,1)  NOT NULL DEFAULT 1.0,
			last_order_at TIMESTAMPTZ,
			updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (supplier_name, item_no)
		)`,

		// ============== 鉴权 (2026-08-29) ==============
		// users / role_permissions / auth_sessions / audit_log
		`CREATE TABLE IF NOT EXISTS users (
			id              TEXT PRIMARY KEY,
			name            TEXT NOT NULL,
			role            TEXT NOT NULL,
			tenant_id       TEXT NOT NULL DEFAULT 't_dev',
			external_id     TEXT,
			source          TEXT NOT NULL DEFAULT 'wecom',
			status          TEXT NOT NULL DEFAULT 'active',
			"group"         TEXT NOT NULL DEFAULT '',   -- 2026-08-30: 'floor' / 'office' / ''
			created_at      TIMESTAMPTZ DEFAULT now(),
			updated_at      TIMESTAMPTZ DEFAULT now()
		)`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS "group" TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_users_role ON users(role)`,
		`CREATE INDEX IF NOT EXISTS idx_users_tenant ON users(tenant_id)`,
		`CREATE INDEX IF NOT EXISTS idx_users_group ON users("group")`,

		// ============== RBAC 增强 (2026-08-30) ==============
		// 部门缓存 + 用户多角色 + 权限点字典 + 审计
		`CREATE TABLE IF NOT EXISTS wecom_departments (
			id              BIGINT PRIMARY KEY,
			parent_id       BIGINT,
			name            TEXT NOT NULL,
			path            TEXT NOT NULL DEFAULT '',
			order_idx       INT DEFAULT 0,
			synced_at       TIMESTAMPTZ DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_dept_path ON wecom_departments(path)`,

		`CREATE TABLE IF NOT EXISTS roles (
			id              TEXT PRIMARY KEY,
			name            TEXT NOT NULL,
			scope           TEXT NOT NULL DEFAULT 'platform',  -- 'platform' / 'store' / 'dept'
			description     TEXT DEFAULT '',
			is_builtin      BOOLEAN NOT NULL DEFAULT false,
			created_at      TIMESTAMPTZ DEFAULT now()
		)`,

		`CREATE TABLE IF NOT EXISTS permissions (
			id              TEXT PRIMARY KEY,        -- 'session:create' / 'restock:feedback' / 'user:manage' ...
			domain          TEXT NOT NULL,           -- 'session' / 'restock' / 'inventory' / 'user' / 'report'
			action          TEXT NOT NULL,           -- 'create' / 'read' / 'update' / 'delete' / 'manage' / 'feedback'
			description     TEXT DEFAULT ''
		)`,

		`CREATE TABLE IF NOT EXISTS role_permissions (
			role_id         TEXT NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
			perm_id         TEXT NOT NULL REFERENCES permissions(id) ON DELETE CASCADE,
			PRIMARY KEY (role_id, perm_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_role_perm_perm ON role_permissions(perm_id)`,

		`CREATE TABLE IF NOT EXISTS user_roles (
			user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			role_id         TEXT NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
			scope_type      TEXT NOT NULL DEFAULT 'all',   -- 'all' / 'store' / 'dept'
			scope_id        TEXT NOT NULL DEFAULT '',     -- 门店号 '0001' / 部门 path '/5/12/'
			is_primary      BOOLEAN NOT NULL DEFAULT false,
			granted_by      TEXT DEFAULT '',
			granted_at      TIMESTAMPTZ DEFAULT now(),
			expires_at      TIMESTAMPTZ,                 -- 临时授权到期 (NULL=永久)
			PRIMARY KEY (user_id, role_id, scope_type, scope_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_user_roles_role ON user_roles(role_id)`,
		`CREATE INDEX IF NOT EXISTS idx_user_roles_expires ON user_roles(expires_at) WHERE expires_at IS NOT NULL`,

		`CREATE TABLE IF NOT EXISTS permission_audit (
			id              BIGSERIAL PRIMARY KEY,
			actor_id        TEXT DEFAULT '',             -- 操作人
			target_user     TEXT DEFAULT '',             -- 被操作用户
			action          TEXT NOT NULL,               -- 'grant' / 'revoke' / 'create_role' / 'update_role' / 'delete_role' / 'left' / 'sync'
			detail          JSONB DEFAULT '{}'::jsonb,
			reason          TEXT DEFAULT '',
			ts              TIMESTAMPTZ DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_target_ts ON permission_audit(target_user, ts DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_actor_ts ON permission_audit(actor_id, ts DESC)`,

		// users 表加新字段 (部门/手机/职位/在职)
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS mobile TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS department_id BIGINT`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS department_path TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS position TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS hired_at TIMESTAMPTZ`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS left_at TIMESTAMPTZ`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS sync_at TIMESTAMPTZ`,
		`CREATE INDEX IF NOT EXISTS idx_users_dept ON users(department_id)`,
		`CREATE INDEX IF NOT EXISTS idx_users_left ON users(left_at)`,

		`CREATE TABLE IF NOT EXISTS role_permissions (
			role            TEXT NOT NULL,
			perm            TEXT NOT NULL,
			PRIMARY KEY (role, perm)
		)`,

		`CREATE TABLE IF NOT EXISTS auth_sessions (
			id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			refresh_hash    TEXT NOT NULL,
			expires_at      TIMESTAMPTZ NOT NULL,
			created_at      TIMESTAMPTZ DEFAULT now(),
			revoked_at      TIMESTAMPTZ
		)`,
		`CREATE INDEX IF NOT EXISTS idx_auth_sessions_user ON auth_sessions(user_id) WHERE revoked_at IS NULL`,

		`CREATE TABLE IF NOT EXISTS audit_log (
			id              BIGSERIAL PRIMARY KEY,
			user_id         TEXT,
			method          TEXT,
			path            TEXT,
			status          INT,
			ts              TIMESTAMPTZ DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_log_user_ts ON audit_log(user_id, ts DESC)`,

		// seed 4 个 dev 账号 (2026-08-30: 加 group 分组: office=管理者,floor=卖场员工)
		`INSERT INTO users (id, name, role, tenant_id, source, "group") VALUES
			('u_owner',   '梁老板(店主)', 'owner',   't_dev', 'dev', 'office'),
			('u_manager', '李店长',       'manager', 't_dev', 'dev', 'office'),
			('u_buyer',   '王采购',       'buyer',   't_dev', 'dev', 'office'),
			('u_cashier', '陈收银',       'cashier', 't_dev', 'dev', 'floor')
		ON CONFLICT (id) DO NOTHING`,
		// 已有账号补 group 字段 (UPDATE 而不是 INSERT 触发 ON CONFLICT)
		`UPDATE users SET "group" = 'office' WHERE id IN ('u_owner','u_manager','u_buyer') AND ("group" IS NULL OR "group" = '')`,
		`UPDATE users SET "group" = 'floor'  WHERE id = 'u_cashier'               AND ("group" IS NULL OR "group" = '')`,

		// ============== RBAC 增强 seed (2026-08-30) ==============
		// 内置 6 个角色 (旧的 'role' 字段 + 新的 roles 表双轨)
		`INSERT INTO roles (id, name, scope, description, is_builtin) VALUES
			('owner',   '店主',     'platform', '平台总管理员, 所有权限',   true),
			('manager', '店长',     'store',   '门店管理 (单店范围)',      true),
			('buyer',   '采购',     'platform', '采购员 (跨店)',           true),
			('cashier', '收银',     'store',   '收银员 (单店, 只读)',     true),
			('floor',   '卖场员工', 'store',   '卖场员工 (反馈补货)',      true),
			('office',  '办公室',   'platform', '办公室人员 (看板)',       true)
		ON CONFLICT (id) DO NOTHING`,

		// 权限点字典 (跟现有 role_permissions 表保持一致)
		`INSERT INTO permissions (id, domain, action, description) VALUES
			('*',               '_wildcard', 'all',     '通配 (所有权限)'),
			('session:create',   'session',   'create',  '创建收货单'),
			('session:read',     'session',   'read',    '查看收货单'),
			('session:update',   'session',   'update',  '编辑收货单'),
			('session:delete',   'session',   'delete',  '删除收货单'),
			('row:update',       'row',       'update',  '修改明细行'),
			('row:delete',       'row',       'delete',  '删除明细行'),
			('plan:read',        'plan',      'read',    '查看补货计划'),
			('plan:create',      'plan',      'create',  '生成补货计划'),
			('plan:approve',     'plan',      'approve', '审批补货计划'),
			('restock:feedback', 'restock',   'feedback','反馈补货 (已补/缺货)'),
			('inventory:view',   'inventory', 'view',    '查看库存'),
			('inventory:adjust', 'inventory', 'adjust',  '调整库存'),
			('report:view',      'report',    'view',    '查看报表'),
			('user:manage',      'user',      'manage',  '管理员工/授权'),
			('role:manage',      'role',      'manage',  '管理角色'),
			('admin',            'admin',     'manage',  '系统管理 (同步/审计)')
		ON CONFLICT (id) DO NOTHING`,

		// 内置角色-权限映射 (新表)
		`INSERT INTO role_permissions (role_id, perm_id) VALUES
			('owner',   '*'),
			('owner',   'admin'),
			('manager', 'session:create'), ('manager', 'session:read'),
			('manager', 'session:update'), ('manager', 'session:delete'),
			('manager', 'row:update'),     ('manager', 'row:delete'),
			('manager', 'plan:read'),      ('manager', 'plan:create'),
			('manager', 'restock:feedback'),
			('manager', 'inventory:view'), ('manager', 'inventory:adjust'),
			('manager', 'report:view'),    ('manager', 'user:manage'),
			('buyer',   'session:create'), ('buyer',   'session:read'),
			('buyer',   'session:update'), ('buyer',   'session:delete'),
			('buyer',   'row:update'),     ('buyer',   'row:delete'),
			('buyer',   'plan:read'),      ('buyer',   'plan:create'),
			('buyer',   'plan:approve'),   ('buyer',   'inventory:view'),
			('cashier', 'session:read'),   ('cashier', 'plan:read'),
			('cashier', 'restock:feedback'),
			('floor',   'plan:read'),      ('floor',   'restock:feedback'),
			('office',  'plan:read'),      ('office',  'plan:create'),
			('office',  'inventory:view'), ('office',  'report:view'),
			('office',  'restock:feedback')
		ON CONFLICT (role_id, perm_id) DO NOTHING`,

		// 现有 dev 账号绑定到内置角色 (1:1)
		`INSERT INTO user_roles (user_id, role_id, scope_type, scope_id, is_primary) VALUES
			('u_owner',   'owner',   'platform', '', true),
			('u_manager', 'manager', 'store',   '0001', true),
			('u_buyer',   'buyer',   'platform', '', true),
			('u_cashier', 'cashier', 'store',   '0001', true),
			('u_cashier', 'floor',   'store',   '0001', false)
		ON CONFLICT DO NOTHING`,
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
