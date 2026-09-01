package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
)

// ============================================================
// 工具 5: record_promotion_fee  (INSERT, 不 UPSERT — 同一笔费用应保持独立记录)
// ============================================================

// PromotionFeeKind 白名单(LLM 只能写这 5 种)
var PromotionFeeKind = struct {
	Duitou  string
	Duanjia string
	Chenlie string
	DM      string
	Tiaoma  string
}{
	Duitou:  "堆头",
	Duanjia: "端架",
	Chenlie: "陈列",
	DM:      "DM",
	Tiaoma:  "条码费",
}

var allowedFeeKinds = map[string]bool{
	PromotionFeeKind.Duitou:  true,
	PromotionFeeKind.Duanjia: true,
	PromotionFeeKind.Chenlie: true,
	PromotionFeeKind.DM:      true,
	PromotionFeeKind.Tiaoma:  true,
}

// RecordPromotionFeeReq 输入
type RecordPromotionFeeReq struct {
	Supplier    string  `json:"supplier" jsonschema:"description=供应商名称(必填),required"`
	Kind        string  `json:"kind" jsonschema:"description=费用种类(必填): 堆头|端架|陈列|DM|条码费,required"`
	Amount      float64 `json:"amount" jsonschema:"description=金额(必填,元),required,minimum=0"`
	PeriodStart string  `json:"period_start" jsonschema:"description=费用周期起始 YYYY-MM-DD(必填),required"`
	PeriodEnd   string  `json:"period_end" jsonschema:"description=费用周期结束 YYYY-MM-DD(必填),required"`
	Note        string  `json:"note,omitempty" jsonschema:"description=备注"`
	DryRun      bool    `json:"dry_run,omitempty" jsonschema:"description=二次确认模式,默认 false"`
	Source      string  `json:"source,omitempty" jsonschema:"description=来源标识,默认 wecom_agent"`
}

// RecordPromotionFeeResp 输出
type RecordPromotionFeeResp struct {
	FeeID       int64   `json:"fee_id"`
	Supplier    string  `json:"supplier"`
	Kind        string  `json:"kind"`
	Amount      float64 `json:"amount"`
	PeriodStart string  `json:"period_start"`
	PeriodEnd   string  `json:"period_end"`
	Action      string  `json:"action"` // "dry_run" | "inserted"
}

// RecordPromotionFee 工具函数
//   同一笔费用可重复插,不复用 — 不同月份/不同谈判的"堆头费"语义不同
func RecordPromotionFee(pool *pgxpool.Pool) *function.FunctionTool[RecordPromotionFeeReq, RecordPromotionFeeResp] {
	fn := func(ctx context.Context, req RecordPromotionFeeReq) (RecordPromotionFeeResp, error) {
		if pool == nil {
			return RecordPromotionFeeResp{}, fmt.Errorf("record_promotion_fee: pg pool 未初始化")
		}
		supplier := trimSpace(req.Supplier)
		if supplier == "" {
			return RecordPromotionFeeResp{}, fmt.Errorf("supplier 必填")
		}
		kind := trimSpace(req.Kind)
		if !allowedFeeKinds[kind] {
			return RecordPromotionFeeResp{}, fmt.Errorf("kind %q 不在白名单(允许: %v)", kind, keysOf(allowedFeeKinds))
		}
		if req.Amount <= 0 {
			return RecordPromotionFeeResp{}, fmt.Errorf("amount 必须 > 0")
		}
		start, err := time.Parse("2006-01-02", req.PeriodStart)
		if err != nil {
			return RecordPromotionFeeResp{}, fmt.Errorf("period_start 格式错误: %w", err)
		}
		end, err := time.Parse("2006-01-02", req.PeriodEnd)
		if err != nil {
			return RecordPromotionFeeResp{}, fmt.Errorf("period_end 格式错误: %w", err)
		}
		if end.Before(start) {
			return RecordPromotionFeeResp{}, fmt.Errorf("period_end 不能早于 period_start")
		}
		source := orDefault(req.Source, "wecom_agent")

		if req.DryRun {
			return RecordPromotionFeeResp{
				Supplier:    supplier,
				Kind:        kind,
				Amount:      req.Amount,
				PeriodStart: req.PeriodStart,
				PeriodEnd:   req.PeriodEnd,
				Action:      "dry_run",
			}, nil
		}

		var feeID int64
		err = pool.QueryRow(ctx, `
			INSERT INTO promotion_fee (supplier_name, kind, amount, period_start, period_end, note, source)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING id
		`, supplier, kind, req.Amount, start, end, req.Note, source).Scan(&feeID)
		if err != nil {
			return RecordPromotionFeeResp{}, fmt.Errorf("insert promotion_fee: %w", err)
		}

		return RecordPromotionFeeResp{
			FeeID:       feeID,
			Supplier:    supplier,
			Kind:        kind,
			Amount:      req.Amount,
			PeriodStart: req.PeriodStart,
			PeriodEnd:   req.PeriodEnd,
			Action:      "inserted",
		}, nil
	}

	return function.NewFunctionTool(fn,
		function.WithName("record_promotion_fee"),
		function.WithDescription("记一笔供应商促销费用(堆头/端架/陈列/DM/条码费). 每次写入是独立记录,不复用 — 不同月份/不同谈判的'堆头费'语义不同. dry_run=true 时只返回待写入内容."),
	)
}

// ============================================================
// 工具 6: list_promotion_fee
// ============================================================

// ListPromotionFeeReq 输入
type ListPromotionFeeReq struct {
	Supplier    string `json:"supplier,omitempty" jsonschema:"description=按供应商过滤(可选)"`
	PeriodStart string `json:"period_start,omitempty" jsonschema:"description=只看 period_start >= 此日期 (YYYY-MM-DD)"`
	PeriodEnd   string `json:"period_end,omitempty" jsonschema:"description=只看 period_end <= 此日期 (YYYY-MM-DD)"`
	Kind        string `json:"kind,omitempty" jsonschema:"description=按种类过滤(可选)"`
	Limit       int    `json:"limit,omitempty" jsonschema:"description=最多返回条数,默认 100,上限 500"`
}

// ListPromotionFeeItem 单条
type ListPromotionFeeItem struct {
	FeeID       int64   `json:"fee_id"`
	Supplier    string  `json:"supplier"`
	Kind        string  `json:"kind"`
	Amount      float64 `json:"amount"`
	PeriodStart string  `json:"period_start"`
	PeriodEnd   string  `json:"period_end"`
	Note        string  `json:"note"`
	Source      string  `json:"source"`
	CreatedAt   string  `json:"created_at"`
}

// ListPromotionFeeResp 输出
type ListPromotionFeeResp struct {
	Count  int                    `json:"count"`
	Total  float64                `json:"total"`
	Items  []ListPromotionFeeItem `json:"items"`
}

// ListPromotionFee 工具函数
//   按过滤条件查 promotion_fee 记录,默认 limit=100
//   Total = 返回的 Items 金额求和(不是全表 sum,仅供 LLM 理解)
func ListPromotionFee(pool *pgxpool.Pool) *function.FunctionTool[ListPromotionFeeReq, ListPromotionFeeResp] {
	fn := func(ctx context.Context, req ListPromotionFeeReq) (ListPromotionFeeResp, error) {
		if pool == nil {
			return ListPromotionFeeResp{}, fmt.Errorf("list_promotion_fee: pg pool 未初始化")
		}
		limit := req.Limit
		if limit <= 0 {
			limit = 100
		}
		if limit > 500 {
			limit = 500
		}
		// 动态拼 WHERE(避免空过滤)
		q := `SELECT id, supplier_name, kind, amount, period_start, period_end, COALESCE(note,''), source, created_at
			FROM promotion_fee WHERE 1=1`
		args := []any{}
		if req.Supplier != "" {
			args = append(args, trimSpace(req.Supplier))
			q += fmt.Sprintf(" AND supplier_name = $%d", len(args))
		}
		if req.Kind != "" {
			args = append(args, req.Kind)
			q += fmt.Sprintf(" AND kind = $%d", len(args))
		}
		if req.PeriodStart != "" {
			d, err := time.Parse("2006-01-02", req.PeriodStart)
			if err != nil {
				return ListPromotionFeeResp{}, fmt.Errorf("period_start 格式错误: %w", err)
			}
			args = append(args, d)
			q += fmt.Sprintf(" AND period_start >= $%d", len(args))
		}
		if req.PeriodEnd != "" {
			d, err := time.Parse("2006-01-02", req.PeriodEnd)
			if err != nil {
				return ListPromotionFeeResp{}, fmt.Errorf("period_end 格式错误: %w", err)
			}
			args = append(args, d)
			q += fmt.Sprintf(" AND period_end <= $%d", len(args))
		}
		args = append(args, limit)
		q += fmt.Sprintf(" ORDER BY period_end DESC LIMIT $%d", len(args))

		rows, err := pool.Query(ctx, q, args...)
		if err != nil {
			return ListPromotionFeeResp{}, fmt.Errorf("query promotion_fee: %w", err)
		}
		defer rows.Close()

		out := ListPromotionFeeResp{Items: []ListPromotionFeeItem{}}
		for rows.Next() {
			var it ListPromotionFeeItem
			var start, end, created time.Time
			if err := rows.Scan(&it.FeeID, &it.Supplier, &it.Kind, &it.Amount, &start, &end, &it.Note, &it.Source, &created); err != nil {
				return ListPromotionFeeResp{}, fmt.Errorf("scan: %w", err)
			}
			it.PeriodStart = start.Format("2006-01-02")
			it.PeriodEnd = end.Format("2006-01-02")
			it.CreatedAt = created.UTC().Format(time.RFC3339)
			out.Total += it.Amount
			out.Items = append(out.Items, it)
		}
		if err := rows.Err(); err != nil {
			return ListPromotionFeeResp{}, fmt.Errorf("rows err: %w", err)
		}
		out.Count = len(out.Items)
		return out, nil
	}

	return function.NewFunctionTool(fn,
		function.WithName("list_promotion_fee"),
		function.WithDescription("查供应商促销费用(堆头/端架/陈列/DM/条码费). 支持 supplier/kind/period_start/period_end 过滤. 默认 limit=100,上限 500."),
	)
}
