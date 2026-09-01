// Package tools 提供 trpc-agent-go Function Tool 实现,挂接 collect-ai 现有 PG。
//
// 范围(模块 A, 智能采购 方案 2.3):
//   - remember_supplier_policy / query_supplier_policy        (supplier_policy)
//   - record_special_date    / query_upcoming_dates           (special_calendar)
//   - record_promotion_fee   / list_promotion_fee             (promotion_fee)
//
// 约束:
//   - 工具名只允许 [a-zA-Z0-9_-]+,不能含中文(DeepSeek 严格校验)
//   - 写入路径全部 UPSERT / 幂等,LLM 二次确认时不会重复创建
//   - 失败回返 error,LLM 收到后自我修正,不静默丢错
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
)

// ============================================================
// 工具 1: remember_supplier_policy  (UPSERT 幂等)
// ============================================================

// RememberSupplierPolicyReq 输入 schema (与工具 JSON schema 1:1)
type RememberSupplierPolicyReq struct {
	Supplier string         `json:"supplier" jsonschema:"description=供应商名称(必填),required"`
	Key      string         `json:"key" jsonschema:"description=属性键(必填): is_self_procure|allow_return|has_duitou|has_duanjia|block_entry|note,required"`
	Value    any            `json:"value" jsonschema:"description=属性值(bool/string/number/array/object 任意 JSON 类型),required"`
	DryRun   bool           `json:"dry_run,omitempty" jsonschema:"description=二次确认模式: true=只返回待写入内容不落库, false=真写. 默认 false"`
	Source   string         `json:"source,omitempty" jsonschema:"description=来源标识: wecom_agent|manual|import. 默认 wecom_agent"`
	ChatID   string         `json:"chat_id,omitempty" jsonschema:"description=来源企微群 chat_id,用于溯源"`
	MessageID string        `json:"message_id,omitempty" jsonschema:"description=来源企微消息 ID,幂等用"`
}

// RememberSupplierPolicyResp 输出
type RememberSupplierPolicyResp struct {
	Supplier    string `json:"supplier"`
	Key         string `json:"key"`
	Value       any    `json:"value"`
	Action      string `json:"action"`      // "dry_run" | "updated" | "created" | "unchanged"
	PreviousVal any    `json:"previous_value,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

// 允许的 key 白名单(LLM 不能瞎写)
var allowedPolicyKeys = map[string]bool{
	"is_self_procure": true,
	"allow_return":    true,
	"has_duitou":      true,
	"has_duanjia":     true,
	"block_entry":     true,
	"block_reason":    true,
	"note":            true,
}

// RememberSupplierPolicy 工具函数
//   行为:
//     1) key 白名单校验
//     2) 拉旧值(若存在)
//     3) DryRun=true → 返回 action=dry_run,不入库
//     4) DryRun=false → UPSERT,返回 action=created/updated/unchanged
func RememberSupplierPolicy(pool *pgxpool.Pool) *function.FunctionTool[RememberSupplierPolicyReq, RememberSupplierPolicyResp] {
	fn := func(ctx context.Context, req RememberSupplierPolicyReq) (RememberSupplierPolicyResp, error) {
		if pool == nil {
			return RememberSupplierPolicyResp{}, fmt.Errorf("remember_supplier_policy: pg pool 未初始化")
		}
		// 1) 校验
		supplier := trimSpace(req.Supplier)
		key := trimSpace(req.Key)
		if supplier == "" {
			return RememberSupplierPolicyResp{}, fmt.Errorf("supplier 必填")
		}
		if key == "" {
			return RememberSupplierPolicyResp{}, fmt.Errorf("key 必填")
		}
		if !allowedPolicyKeys[key] {
			return RememberSupplierPolicyResp{}, fmt.Errorf("key %q 不在白名单(允许: %v)", key, keysOf(allowedPolicyKeys))
		}
		if req.Value == nil {
			return RememberSupplierPolicyResp{}, fmt.Errorf("value 必填")
		}
		source := orDefault(req.Source, "wecom_agent")

		// 2) 拉旧值
		var oldJSON []byte
		row := pool.QueryRow(ctx, `SELECT value FROM supplier_policy WHERE supplier_name=$1 AND key=$2`, supplier, key)
		if err := row.Scan(&oldJSON); err != nil && err != pgx.ErrNoRows {
			return RememberSupplierPolicyResp{}, fmt.Errorf("read old value: %w", err)
		}
		var prev any
		if len(oldJSON) > 0 {
			_ = json.Unmarshal(oldJSON, &prev)
		}

		// 序列化为 JSONB
		newJSON, err := json.Marshal(req.Value)
		if err != nil {
			return RememberSupplierPolicyResp{}, fmt.Errorf("value 序列化失败: %w", err)
		}
		sameJSON := len(oldJSON) > 0 && string(oldJSON) == string(newJSON)

		// 3) DryRun
		if req.DryRun {
			return RememberSupplierPolicyResp{
				Supplier:    supplier,
				Key:         key,
				Value:       req.Value,
				Action:      "dry_run",
				PreviousVal: prev,
			}, nil
		}

		// 4) UPSERT
		var (
			updatedAt time.Time
			action    string
		)
		if sameJSON {
			action = "unchanged"
			row2 := pool.QueryRow(ctx, `SELECT updated_at FROM supplier_policy WHERE supplier_name=$1 AND key=$2`, supplier, key)
			if err := row2.Scan(&updatedAt); err != nil {
				return RememberSupplierPolicyResp{}, fmt.Errorf("read updated_at: %w", err)
			}
		} else {
			tag, err := pool.Exec(ctx, `
				INSERT INTO supplier_policy (supplier_name, key, value, source, chat_id, message_id, updated_at)
				VALUES ($1, $2, $3::jsonb, $4, $5, $6, NOW())
				ON CONFLICT (supplier_name, key) DO UPDATE
				SET value = EXCLUDED.value,
				    source = EXCLUDED.source,
				    chat_id = EXCLUDED.chat_id,
				    message_id = EXCLUDED.message_id,
				    updated_at = NOW()
			`, supplier, key, string(newJSON), source, req.ChatID, req.MessageID)
			if err != nil {
				return RememberSupplierPolicyResp{}, fmt.Errorf("upsert supplier_policy: %w", err)
			}
			// RowsAffected: 1=insert, 2=update (PG 约定;但 ON CONFLICT DO UPDATE 总是 1)
			_ = tag
			action = "updated"
			if prev == nil {
				action = "created"
			}
			updatedAt = time.Now()
		}

		return RememberSupplierPolicyResp{
			Supplier:    supplier,
			Key:         key,
			Value:       req.Value,
			Action:      action,
			PreviousVal: prev,
			UpdatedAt:   updatedAt.UTC().Format(time.RFC3339),
		}, nil
	}

	return function.NewFunctionTool(fn,
		function.WithName("remember_supplier_policy"),
		function.WithDescription("记下某供应商的某条政策(如 is_self_procure=true/allow_return=false/block_entry=true 等). dry_run=true 时只返回待写入内容,不入库,用于二次确认. 同一 supplier+key 唯一,二次写入会覆盖."),
	)
}

// ============================================================
// 工具 2: query_supplier_policy
// ============================================================

// QuerySupplierPolicyReq 输入
type QuerySupplierPolicyReq struct {
	Supplier string `json:"supplier" jsonschema:"description=供应商名称(必填),required"`
	Key      string `json:"key,omitempty" jsonschema:"description=属性键(可选),空=返回该供应商所有属性"`
}

// QuerySupplierPolicyResp 单条属性
type QuerySupplierPolicyItem struct {
	Key       string `json:"key"`
	Value     any    `json:"value"`
	Source    string `json:"source"`
	UpdatedAt string `json:"updated_at"`
}

// QuerySupplierPolicyResp 输出
type QuerySupplierPolicyResp struct {
	Supplier string                     `json:"supplier"`
	Policies []QuerySupplierPolicyItem  `json:"policies"`
	Count    int                        `json:"count"`
}

// QuerySupplierPolicy 工具函数
func QuerySupplierPolicy(pool *pgxpool.Pool) *function.FunctionTool[QuerySupplierPolicyReq, QuerySupplierPolicyResp] {
	fn := func(ctx context.Context, req QuerySupplierPolicyReq) (QuerySupplierPolicyResp, error) {
		if pool == nil {
			return QuerySupplierPolicyResp{}, fmt.Errorf("query_supplier_policy: pg pool 未初始化")
		}
		supplier := trimSpace(req.Supplier)
		if supplier == "" {
			return QuerySupplierPolicyResp{}, fmt.Errorf("supplier 必填")
		}

		var (
			rows pgx.Rows
			err  error
		)
		if req.Key != "" {
			rows, err = pool.Query(ctx, `
				SELECT key, value, source, updated_at
				FROM supplier_policy
				WHERE supplier_name=$1 AND key=$2
				ORDER BY key
			`, supplier, req.Key)
		} else {
			rows, err = pool.Query(ctx, `
				SELECT key, value, source, updated_at
				FROM supplier_policy
				WHERE supplier_name=$1
				ORDER BY key
			`, supplier)
		}
		if err != nil {
			return QuerySupplierPolicyResp{}, fmt.Errorf("query supplier_policy: %w", err)
		}
		defer rows.Close()

		out := QuerySupplierPolicyResp{Supplier: supplier, Policies: []QuerySupplierPolicyItem{}}
		for rows.Next() {
			var it QuerySupplierPolicyItem
			var raw []byte
			var updatedAt time.Time
			if err := rows.Scan(&it.Key, &raw, &it.Source, &updatedAt); err != nil {
				return QuerySupplierPolicyResp{}, fmt.Errorf("scan: %w", err)
			}
			_ = json.Unmarshal(raw, &it.Value)
			it.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
			out.Policies = append(out.Policies, it)
		}
		if err := rows.Err(); err != nil {
			return QuerySupplierPolicyResp{}, fmt.Errorf("rows err: %w", err)
		}
		out.Count = len(out.Policies)
		return out, nil
	}

	return function.NewFunctionTool(fn,
		function.WithName("query_supplier_policy"),
		function.WithDescription("查某供应商的政策清单(可按 key 过滤). 找不到返回空数组,不是错误."),
	)
}
