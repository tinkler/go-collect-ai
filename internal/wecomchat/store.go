package wecomchat

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store PG 访问
type Store struct {
	Pool *pgxpool.Pool
}

// NewStore 构造
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{Pool: pool}
}

// ErrNotFound 找不到
var ErrNotFound = errors.New("wecomchat: not found")

// ListBindings 列出所有 (按 updated_at DESC)
func (s *Store) ListBindings(ctx context.Context, purpose string) ([]*ChatBinding, error) {
	q := `SELECT chat_id, purpose, label, enabled, COALESCE(note,''), first_seen, created_by, created_at, updated_at
		FROM wecom_chat_binding`
	args := []any{}
	if purpose != "" {
		q += " WHERE purpose = $1"
		args = append(args, purpose)
	}
	q += " ORDER BY updated_at DESC"

	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query wecom_chat_binding: %w", err)
	}
	defer rows.Close()

	out := []*ChatBinding{}
	for rows.Next() {
		b := &ChatBinding{}
		var firstSeen *time.Time
		if err := rows.Scan(&b.ChatID, &b.Purpose, &b.Label, &b.Enabled, &b.Note, &firstSeen, &b.CreatedBy, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		b.FirstSeen = firstSeen
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetBinding 查单条
func (s *Store) GetBinding(ctx context.Context, chatID string) (*ChatBinding, error) {
	row := s.Pool.QueryRow(ctx, `SELECT chat_id, purpose, label, enabled, COALESCE(note,''), first_seen, created_by, created_at, updated_at
		FROM wecom_chat_binding WHERE chat_id = $1`, chatID)
	b := &ChatBinding{}
	var firstSeen *time.Time
	if err := row.Scan(&b.ChatID, &b.Purpose, &b.Label, &b.Enabled, &b.Note, &firstSeen, &b.CreatedBy, &b.CreatedAt, &b.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	b.FirstSeen = firstSeen
	return b, nil
}

// UpsertBinding upsert (chat_id 唯一)
//
//   - 新增: created_by = operator
//   - 更新: updated_at = NOW()
//   - 字段部分更新: Label / Purpose / Enabled / Note 都可空
func (s *Store) UpsertBinding(ctx context.Context, b *ChatBinding, operator string) error {
	if !AllowedPurposes[b.Purpose] {
		return fmt.Errorf("purpose %q 不在白名单 (允许: agent/fee/promo_alert/owner/office/floor/log/other)", b.Purpose)
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO wecom_chat_binding (chat_id, purpose, label, enabled, note, created_by, first_seen)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (chat_id) DO UPDATE SET
			purpose    = EXCLUDED.purpose,
			label      = EXCLUDED.label,
			enabled    = EXCLUDED.enabled,
			note       = EXCLUDED.note,
			updated_at = NOW()
	`, b.ChatID, b.Purpose, b.Label, b.Enabled, b.Note, operator, b.FirstSeen)
	if err != nil {
		return fmt.Errorf("upsert wecom_chat_binding: %w", err)
	}
	return nil
}

// DeleteBinding 删
func (s *Store) DeleteBinding(ctx context.Context, chatID string) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM wecom_chat_binding WHERE chat_id = $1`, chatID)
	if err != nil {
		return fmt.Errorf("delete wecom_chat_binding: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchFirstSeen 仅在 auto-discover 时调用 — 不改 purpose / label 等业务字段
func (s *Store) TouchFirstSeen(ctx context.Context, chatID string) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO wecom_chat_binding (chat_id, purpose, first_seen, created_by)
		VALUES ($1, 'log', NOW(), 'auto')
		ON CONFLICT (chat_id) DO UPDATE SET first_seen = COALESCE(wecom_chat_binding.first_seen, NOW())
	`, chatID)
	return err
}

// CountByPurpose 各 purpose 的已配数 (status 接口用)
func (s *Store) CountByPurpose(ctx context.Context) (map[string]int, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT purpose, COUNT(*)::INT FROM wecom_chat_binding WHERE enabled GROUP BY purpose
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var p string
		var n int
		if err := rows.Scan(&p, &n); err != nil {
			return nil, err
		}
		out[p] = n
	}
	return out, rows.Err()
}

// SeedFromEnv 启动时从 env seed (无则不改)
//
//   - PROMOTION_ALERT_CHAT_ID        → purpose=promo_alert
//   - OWNER_CHAT_ID                  → purpose=owner
//   - COLLECTAI_AGENT_CHAT_IDS       → 多个,逗号或空格分隔,purpose=agent
//   - 都遵循: 仅当 PG 没配过才 seed, 不覆盖现有配置
func (s *Store) SeedFromEnv(ctx context.Context, getEnv func(string) string) error {
	type seedItem struct {
		chatIDs []string
		purpose string
		label   string
	}
	get := func(k string) string { return getEnv(k) }

	seeds := []seedItem{
		{splitIDs(get("PROMOTION_ALERT_CHAT_ID")), "promo_alert", "堆头费预警群(env seed)"},
		{splitIDs(get("OWNER_CHAT_ID")), "owner", "店主私享群(env seed)"},
		{splitIDs(get("COLLECTAI_AGENT_CHAT_IDS")), "agent", "智能对话群(env seed)"},
	}

	for _, item := range seeds {
		for _, cid := range item.chatIDs {
			if cid == "" {
				continue
			}
			// ON CONFLICT (chat_id) DO NOTHING — 已存在则保留
			_, err := s.Pool.Exec(ctx, `
				INSERT INTO wecom_chat_binding (chat_id, purpose, label, created_by)
				VALUES ($1, $2, $3, 'env_seed')
				ON CONFLICT (chat_id) DO NOTHING
			`, cid, item.purpose, item.label)
			if err != nil {
				return fmt.Errorf("seed %s -> %s: %w", cid, item.purpose, err)
			}
		}
	}
	return nil
}

// splitIDs 切逗号/空格分隔
func splitIDs(s string) []string {
	out := []string{}
	cur := ""
	for _, r := range s {
		if r == ',' || r == ' ' || r == '\n' || r == '\t' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
