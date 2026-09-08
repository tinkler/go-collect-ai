// Package tools - purchase-alert skill 专用 tool (W4.2, 2026-09-03)
//
// 范围 (对应 skills/purchase-alert/SKILL.md + references/query-tools.md):
//   - query_app_settings       : 读阈值/分类白名单 (K-V JSONB)
//   - query_sku_stock          : 查 SKU 当前库存 (走 cube t_im_branch_stock)
//   - query_sku_sales          : 查 SKU N 天销量 (走 cube siss_saleflow, W4.2 启用)
//   - query_return_order       : 查供应商退货单 (W4.4 新增, 走 cube t_rm_returnflow, 等 cube 数据源)
//   - insert_purchase_alert    : LLM 决定报 alert 后落库
//   - update_analysis_status   : 收尾写 parse_session.analysis_status
//
// 设计:
//   - 跟现有 tool 同款 (function.FunctionTool[Req, Resp] + jsonschema tag)
//   - 入参/出参尽量简单,减少 LLM 调错几率
//   - 工具名跟 query-tools.md 一一对应,LLM 按文档调
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tinkler/collect-ai/internal/business"
	"github.com/jackc/pgx/v5/pgxpool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
)

// ============================================================
// 工具 1: query_app_settings
//   读阈值/分类白名单 (K-V JSONB 表)
// ============================================================

// QueryAppSettingsReq 入参
type QueryAppSettingsReq struct {
	Key string `json:"key" jsonschema:"description=配置 key: high_stock_threshold|low_movement_threshold_30d|duitou_kinds|others_kinds|season_words_override,required"`
}

// QueryAppSettingsResp 出参
type QueryAppSettingsResp struct {
	Key       string      `json:"key"`
	Value     interface{} `json:"value,omitempty"`
	UpdatedAt string      `json:"updated_at,omitempty"`
}

// QueryAppSettings 工具函数
func QueryAppSettings(pool *pgxpool.Pool) *function.FunctionTool[QueryAppSettingsReq, QueryAppSettingsResp] {
	fn := func(ctx context.Context, req QueryAppSettingsReq) (QueryAppSettingsResp, error) {
		if pool == nil {
			return QueryAppSettingsResp{}, fmt.Errorf("query_app_settings: pg pool 未初始化")
		}
		key := trimSpace(req.Key)
		if key == "" {
			return QueryAppSettingsResp{}, fmt.Errorf("key 必填")
		}
		var raw []byte
		var updatedAt time.Time
		err := pool.QueryRow(ctx, `
			SELECT value, updated_at FROM app_settings WHERE key = $1
		`, key).Scan(&raw, &updatedAt)
		if err == pgx.ErrNoRows {
			// 找不到返空, 不报错 (LLM 自行降级)
			return QueryAppSettingsResp{Key: key}, nil
		}
		if err != nil {
			return QueryAppSettingsResp{}, fmt.Errorf("query app_settings: %w", err)
		}
		var v interface{}
		_ = json.Unmarshal(raw, &v)
		return QueryAppSettingsResp{
			Key:       key,
			Value:     v,
			UpdatedAt: updatedAt.Format(time.RFC3339),
		}, nil
	}
	return function.NewFunctionTool(fn,
		function.WithName("query_app_settings"),
		function.WithDescription("读系统配置(阈值/分类白名单), 走 app_settings 表 (K-V JSONB). 找不到返空对象, 不报错. 用于 purchase-alert skill 跑规则时拿最新阈值. LLM 在判定前必须调一次拿 high_stock_threshold/duitou_kinds/others_kinds. 不允许 LLM 硬编码阈值."),
	)
}

// ============================================================
// 工具 2: query_sku_stock
//   查 SKU 当前库存 (走 cube t_im_branch_stock via business.Gateway)
// ============================================================

// QuerySkuStockReq 入参
type QuerySkuStockReq struct {
	ItemNo  string `json:"item_no,omitempty" jsonschema:"description=商品 item_no (item_info.item_no), 与 barcode 二选一"`
	Barcode string `json:"barcode,omitempty" jsonschema:"description=商品条码 (item_info.item_no 或 13位 barcode), 与 item_no 二选一"`
}

// QuerySkuStockResp 出参
type QuerySkuStockResp struct {
	ItemNo   string  `json:"item_no,omitempty"`
	ItemName string  `json:"item_name,omitempty"`
	StockQty float64 `json:"stock_qty"`
	BranchNo string  `json:"branch_no,omitempty"`
	AsOf     string  `json:"as_of"`
	NotFound bool    `json:"not_found,omitempty"`
}

// QuerySkuStockFn 查询函数 (caller 注入, 走 cube)
//   走 business.Gateway, 返回 (item_no, item_name, stock_qty, branch_no, as_of, not_found)
//   由 main.go 注入实际实现
type QuerySkuStockFn func(ctx context.Context, itemNo, barcode string) (string, string, float64, string, time.Time, bool, error)

// QuerySkuStock 工具函数
func QuerySkuStock(queryFn QuerySkuStockFn) *function.FunctionTool[QuerySkuStockReq, QuerySkuStockResp] {
	fn := func(ctx context.Context, req QuerySkuStockReq) (QuerySkuStockResp, error) {
		return querySkuStockImpl(ctx, queryFn, req)
	}
	return function.NewFunctionTool(fn,
		function.WithName("query_sku_stock"),
		function.WithDescription("查 SKU 当前库存, 走 cube t_im_branch_stock (via business.Gateway). 优先用 barcode (13位 EAN-13), 没匹配用 item_no. 用于 purchase-alert skill 跑 high_stock 规则时验证库存最新值. 找不到返 not_found=true, LLM 降级用 row.stock_qty."),
	)
}

// querySkuStockImpl 抽出实现,方便单测直接调
func querySkuStockImpl(ctx context.Context, queryFn QuerySkuStockFn, req QuerySkuStockReq) (QuerySkuStockResp, error) {
	if queryFn == nil {
		return QuerySkuStockResp{NotFound: true}, fmt.Errorf("query_sku_stock: cube querier 未注入 (启动时缺配置)")
	}
	itemNo := trimSpace(req.ItemNo)
	barcode := trimSpace(req.Barcode)
	if itemNo == "" && barcode == "" {
		return QuerySkuStockResp{NotFound: true}, fmt.Errorf("item_no 或 barcode 必填其一")
	}
	resItemNo, resItemName, stock, branch, asOf, notFound, err := queryFn(ctx, itemNo, barcode)
	if err != nil {
		return QuerySkuStockResp{NotFound: true}, fmt.Errorf("cube query: %w", err)
	}
	if notFound {
		return QuerySkuStockResp{NotFound: true, AsOf: asOf.Format(time.RFC3339)}, nil
	}
	return QuerySkuStockResp{
		ItemNo:   resItemNo,
		ItemName: resItemName,
		StockQty: stock,
		BranchNo: branch,
		AsOf:     asOf.Format(time.RFC3339),
	}, nil
}

// ============================================================
// 工具 3: query_sku_sales
//   查 SKU N 天销量 (走 cube siss_saleflow, W4.2 启用, 难消化规则用)
// ============================================================

// QuerySkuSalesReq 入参
type QuerySkuSalesReq struct {
	ItemNo string `json:"item_no" jsonschema:"description=商品 item_no,required"`
	Days   int    `json:"days" jsonschema:"description=窗口天数: 30|60|90,required,enum=30,enum=60,enum=90"`
}

// QuerySkuSalesResp 出参
type QuerySkuSalesResp struct {
	ItemNo     string  `json:"item_no"`
	ItemName   string  `json:"item_name,omitempty"`
	Days       int     `json:"days"`
	TotalQty   float64 `json:"total_qty"`
	TotalMoney float64 `json:"total_money"`
	DailyAvg   float64 `json:"daily_avg"`
	NotFound   bool    `json:"not_found,omitempty"`
}

// QuerySkuSalesFn 查询函数 (caller 注入, 走 cube)
type QuerySkuSalesFn func(ctx context.Context, itemNo string, days int) (string, float64, float64, bool, error)

// QuerySkuSales 工具函数
func QuerySkuSales(queryFn QuerySkuSalesFn) *function.FunctionTool[QuerySkuSalesReq, QuerySkuSalesResp] {
	fn := func(ctx context.Context, req QuerySkuSalesReq) (QuerySkuSalesResp, error) {
		return querySkuSalesImpl(ctx, queryFn, req)
	}
	return function.NewFunctionTool(fn,
		function.WithName("query_sku_sales"),
		function.WithDescription("查 SKU N 天销量, 走 cube siss_saleflow view (via business.Gateway). 用于 purchase-alert skill 跑 low_movement (难消化) 规则 (W4.2 启用). days 必须 30/60/90. 找不到返 not_found=true, LLM 降级 (不报难消化)."),
	)
}

// querySkuSalesImpl 抽出实现,方便单测直接调
func querySkuSalesImpl(ctx context.Context, queryFn QuerySkuSalesFn, req QuerySkuSalesReq) (QuerySkuSalesResp, error) {
	if queryFn == nil {
		return QuerySkuSalesResp{NotFound: true}, fmt.Errorf("query_sku_sales: cube querier 未注入")
	}
	itemNo := trimSpace(req.ItemNo)
	days := req.Days
	if itemNo == "" {
		return QuerySkuSalesResp{NotFound: true}, fmt.Errorf("item_no 必填")
	}
	if days != 30 && days != 60 && days != 90 {
		return QuerySkuSalesResp{NotFound: true}, fmt.Errorf("days 必须 30/60/90, 收到 %d", days)
	}
	name, qty, money, notFound, err := queryFn(ctx, itemNo, days)
	if err != nil {
		return QuerySkuSalesResp{NotFound: true}, fmt.Errorf("cube query: %w", err)
	}
	if notFound {
		return QuerySkuSalesResp{ItemNo: itemNo, Days: days, NotFound: true}, nil
	}
	return QuerySkuSalesResp{
		ItemNo:     itemNo,
		ItemName:   name,
		Days:       days,
		TotalQty:   qty,
		TotalMoney: money,
		DailyAvg:   qty / float64(days),
	}, nil
}

// ============================================================
// 工具 3.5: query_return_order
//   查供应商退货单 (W4.4 新增, cube 端 supplier_returns 已建好)
//   用途: purchase-alert skill 跑 "未审批退货单" 规则 (pending_return)
//   数据源: cube 端 supplier_returns (HBPoS t_pm_sheet_master WHERE trans_no='RO')
//         mapping 走 configs/mappings.yaml entities.returns 段 (业务字段名 → 物理字段)
//         严禁直接 import parser/agent; 必须经 business.Executor.SearchReturnsBySupplier
//
//   ReturnOrder 类型: 复用 business.ReturnOrder (W4.4 统一, 避免重复类型)
//   业务字段: bill_no / supplier_id / supplier_name / status / return_money / create_date / branch_no
//   状态值业务: pending (未审核) / approved (已审核) / rejected (永不命中, HBPoS 无此状态)
// ============================================================

// QueryReturnOrderReq 入参
type QueryReturnOrderReq struct {
	Supplier string `json:"supplier" jsonschema:"description=供应商名 (用于过滤), 必填,required"`
	Status   string `json:"status,omitempty" jsonschema:"description=过滤状态: pending|approved|rejected, 留空返所有状态,enum=pending,enum=approved,enum=rejected"`
	Days     int    `json:"days,omitempty" jsonschema:"description=窗口天数, 默认 30, LLM 可传 60/90"`
}

// QueryReturnOrderResp 出参
type QueryReturnOrderResp struct {
	Supplier     string                  `json:"supplier_name"`
	Status       string                  `json:"status,omitempty"`
	Days         int                     `json:"days"`
	Count        int                     `json:"count"`
	TotalMoney   float64                 `json:"total_money"`
	Returns      []business.ReturnOrder  `json:"returns"`
	NotAvailable bool                    `json:"not_available,omitempty"` // true = cube 数据源未配置, LLM 降级
	Hint         string                  `json:"hint,omitempty"`          // 提示(如"数据源未接入, 规则自动降级")
}

// QueryReturnOrderFn 查询函数 (caller 注入, 走 cube supplier_returns via business.Executor)
//   返 (退货单 list, hint, 错误)
//   如果 cube 数据源未配置 / mapping 缺失, 返 ([], "未配置提示", nil) → queryReturnOrderImpl 走降级路径
type QueryReturnOrderFn func(ctx context.Context, supplier, status string, days int) ([]business.ReturnOrder, string, error)

// QueryReturnOrder 工具函数
func QueryReturnOrder(queryFn QueryReturnOrderFn) *function.FunctionTool[QueryReturnOrderReq, QueryReturnOrderResp] {
	fn := func(ctx context.Context, req QueryReturnOrderReq) (QueryReturnOrderResp, error) {
		return queryReturnOrderImpl(ctx, queryFn, req)
	}
	return function.NewFunctionTool(fn,
		function.WithName("query_return_order"),
		function.WithDescription("查供应商退货单, 走 cube 端 t_rm_returnflow (via business.Gateway, W4.4). 用途: purchase-alert skill 跑 pending_return (未审批退货单) 规则. 必填 supplier, status 留空查所有, days 默认 30. 数据源未配置返 not_available=true + hint, LLM 应降级(不报 pending_return). 严禁直接 import parser/agent."),
	)
}

// queryReturnOrderImpl 抽出实现,方便单测
func queryReturnOrderImpl(ctx context.Context, queryFn QueryReturnOrderFn, req QueryReturnOrderReq) (QueryReturnOrderResp, error) {
	if queryFn == nil {
		// 没注入 Fn = cube 数据源未接入, LLM 降级
		return QueryReturnOrderResp{
			Supplier:     trimSpace(req.Supplier),
			Status:       trimSpace(req.Status),
			Days:         defaultDays(req.Days, 30),
			NotAvailable: true,
			Hint:         "query_return_order: cube 数据源未注入, pending_return 规则自动降级 (不报)",
		}, nil
	}
	supplier := trimSpace(req.Supplier)
	if supplier == "" {
		return QueryReturnOrderResp{}, fmt.Errorf("supplier 必填")
	}
	status := trimSpace(req.Status)
	days := defaultDays(req.Days, 30)
	if days != 7 && days != 30 && days != 60 && days != 90 {
		return QueryReturnOrderResp{}, fmt.Errorf("days 必须 7/30/60/90, 收到 %d", days)
	}
	orders, hint, err := queryFn(ctx, supplier, status, days)
	if err != nil {
		return QueryReturnOrderResp{
			Supplier:     supplier,
			Status:       status,
			Days:         days,
			NotAvailable: true,
			Hint:         fmt.Sprintf("cube query 失败: %v (规则降级)", err),
		}, nil
	}
	if hint != "" {
		// Fn 主动报告"未配置"等提示
		return QueryReturnOrderResp{
			Supplier:     supplier,
			Status:       status,
			Days:         days,
			NotAvailable: true,
			Hint:         hint,
		}, nil
	}
	total := 0.0
	for _, o := range orders {
		total += o.ReturnMoney
	}
	return QueryReturnOrderResp{
		Supplier:   supplier,
		Status:     status,
		Days:       days,
		Count:      len(orders),
		TotalMoney: total,
		Returns:    orders,
	}, nil
}

// defaultDays 工具: d<=0 用 fallback
func defaultDays(d, fallback int) int {
	if d <= 0 {
		return fallback
	}
	return d
}

// ============================================================
// 工具 4: insert_purchase_alert
//   LLM 决定报 alert 后落库
// ============================================================

// InsertPurchaseAlertReq 入参
type InsertPurchaseAlertReq struct {
	SessionID string `json:"session_id" jsonschema:"description=parse_session.id (UUID),required"`
	RowID     int64  `json:"row_id" jsonschema:"description=行 id (DB id, parse_row.id), 0=session 级,required"`
	Rule      string `json:"rule" jsonschema:"description=规则名: block_entry|no_return|offseason|holiday_lead|high_stock|has_duitou|flash_promo|low_movement,required"`
	Severity  string `json:"severity" jsonschema:"description=严重等级: block|warn|info,required,enum=block,enum=warn,enum=info"`
	Category  string `json:"category" jsonschema:"description=前端 icon 段位: block|warn|info|highlight_dui|highlight_others,required,enum=block,enum=warn,enum=info,enum=highlight_dui,enum=highlight_others"`
	Message   string `json:"message" jsonschema:"description=中文消息, 1-2 句, 含商品名/数量/关键阈值/政策,required"`
	DedupKey  string `json:"dedup_key,omitempty" jsonschema:"description=可选去重 key (如 high_stock:row_1:2026-09-03), 防止同 session+row+rule 重复插入"`
}

// InsertPurchaseAlertResp 出参
type InsertPurchaseAlertResp struct {
	AlertID   int64  `json:"alert_id"`
	CreatedAt string `json:"created_at"`
	Action    string `json:"action"` // "inserted" | "updated" | "skipped"
}

// InsertPurchaseAlert 工具函数
func InsertPurchaseAlert(pool *pgxpool.Pool) *function.FunctionTool[InsertPurchaseAlertReq, InsertPurchaseAlertResp] {
	fn := func(ctx context.Context, req InsertPurchaseAlertReq) (InsertPurchaseAlertResp, error) {
		if pool == nil {
			return InsertPurchaseAlertResp{}, fmt.Errorf("insert_purchase_alert: pg pool 未初始化")
		}
		sessID := trimSpace(req.SessionID)
		if sessID == "" {
			return InsertPurchaseAlertResp{}, fmt.Errorf("session_id 必填")
		}
		rule := trimSpace(req.Rule)
		if rule == "" {
			return InsertPurchaseAlertResp{}, fmt.Errorf("rule 必填")
		}
		severity := trimSpace(req.Severity)
		if severity == "" {
			return InsertPurchaseAlertResp{}, fmt.Errorf("severity 必填")
		}
		category := trimSpace(req.Category)
		if category == "" {
			category = "info" // 默认
		}
		msg := trimSpace(req.Message)
		if msg == "" {
			return InsertPurchaseAlertResp{}, fmt.Errorf("message 必填")
		}
		now := time.Now()

		// 1) dedup: 查同 session+row+rule 的最新 1 条
		if req.DedupKey != "" {
			// 简化: dedup_key 作为 message 前缀存, LLM 后续可 query
			// 本期不实现完整 dedup, 只 INSERT (LLM 自己保证不重复)
		}

		// 2) INSERT
		var alertID int64
		err := pool.QueryRow(ctx, `
			INSERT INTO purchase_session_alert (session_id, row_id, rule, severity, category, message, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING id
		`, sessID, req.RowID, rule, severity, category, msg, now).Scan(&alertID)
		if err != nil {
			return InsertPurchaseAlertResp{}, fmt.Errorf("insert alert: %w", err)
		}
		return InsertPurchaseAlertResp{
			AlertID:   alertID,
			CreatedAt: now.Format(time.RFC3339),
			Action:    "inserted",
		}, nil
	}
	return function.NewFunctionTool(fn,
		function.WithName("insert_purchase_alert"),
		function.WithDescription("purchase-alert skill 落库 alert. LLM 跑完规则后, 对每个判定要报的 alert 调一次. severity: block|warn|info (硬规则等级). category: block|warn|info|highlight_dui|highlight_others (前端 icon 段位, 见 icon-mapping.md). LLM 必须自己保证不重复调 (dedup_key 字段预留, W4.3 启用)."),
	)
}

// ============================================================
// 工具 5: update_analysis_status
//   收尾写 parse_session.analysis_status
// ============================================================

// UpdateAnalysisStatusReq 入参
type UpdateAnalysisStatusReq struct {
	SessionID string `json:"session_id" jsonschema:"description=parse_session.id (UUID),required"`
	Status    string `json:"status" jsonschema:"description=状态: pending|running|done|failed,required,enum=pending,enum=running,enum=done,enum=failed"`
	Error     string `json:"error,omitempty" jsonschema:"description=失败原因, status=failed 时必填"`
}

// UpdateAnalysisStatusResp 出参
type UpdateAnalysisStatusResp struct {
	SessionID      string  `json:"session_id"`
	AnalysisStatus string  `json:"analysis_status"`
	AnalysisAt     *string `json:"analysis_at,omitempty"`
	AnalysisError  string  `json:"analysis_error,omitempty"`
}

// UpdateAnalysisStatus 工具函数
func UpdateAnalysisStatus(pool *pgxpool.Pool) *function.FunctionTool[UpdateAnalysisStatusReq, UpdateAnalysisStatusResp] {
	fn := func(ctx context.Context, req UpdateAnalysisStatusReq) (UpdateAnalysisStatusResp, error) {
		if pool == nil {
			return UpdateAnalysisStatusResp{}, fmt.Errorf("update_analysis_status: pg pool 未初始化")
		}
		sessID := trimSpace(req.SessionID)
		if sessID == "" {
			return UpdateAnalysisStatusResp{}, fmt.Errorf("session_id 必填")
		}
		status := trimSpace(req.Status)
		if status != "pending" && status != "running" && status != "done" && status != "failed" {
			return UpdateAnalysisStatusResp{}, fmt.Errorf("status 必须 pending|running|done|failed, 收到 %q", status)
		}
		errMsg := trimSpace(req.Error)

		// done 状态写 analysis_at = NOW()
		// failed 状态写 analysis_error
		var at interface{}
		if status == "done" {
			at = time.Now()
		} else {
			at = nil
		}

		_, err := pool.Exec(ctx, `
			UPDATE parse_session
			SET analysis_status = $2,
			    analysis_at = $3,
			    analysis_error = $4,
			    updated_at = NOW()
			WHERE id = $1
		`, sessID, status, at, errMsg)
		if err != nil {
			return UpdateAnalysisStatusResp{}, fmt.Errorf("update status: %w", err)
		}
		out := UpdateAnalysisStatusResp{
			SessionID:      sessID,
			AnalysisStatus: status,
			AnalysisError:  errMsg,
		}
		if status == "done" {
			t := time.Now().Format(time.RFC3339)
			out.AnalysisAt = &t
		}
		return out, nil
	}
	return function.NewFunctionTool(fn,
		function.WithName("update_analysis_status"),
		function.WithDescription("purchase-alert skill 收尾: 更新 parse_session.analysis_status. done 状态自动写 analysis_at = NOW(). failed 状态必须传 error 字段 (会被前端展示). 跑完 LLM 必须调一次 (无论成功失败)."),
	)
}
