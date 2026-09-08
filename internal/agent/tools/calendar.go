package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
)

// ============================================================
// 工具 3: record_special_date  (UPSERT 幂等)
// ============================================================

// SpecialDateType 合法类型(白名单,防止 LLM 瞎写)
var SpecialDateType = struct {
	Holiday     string
	Promo       string
	Blackout    string
	SeasonStart string
	SeasonEnd   string
}{
	Holiday:     "holiday",
	Promo:       "promo",
	Blackout:    "blackout",
	SeasonStart: "season_start",
	SeasonEnd:   "season_end",
}

var allowedSpecialTypes = map[string]bool{
	SpecialDateType.Holiday:     true,
	SpecialDateType.Promo:       true,
	SpecialDateType.Blackout:    true,
	SpecialDateType.SeasonStart: true,
	SpecialDateType.SeasonEnd:   true,
}

// RecordSpecialDateReq 输入
type RecordSpecialDateReq struct {
	Date      string `json:"date" jsonschema:"description=日期 YYYY-MM-DD(必填),required"`
	Type      string `json:"type" jsonschema:"description=类型(必填): holiday|promo|blackout|season_start|season_end,required"`
	Name      string `json:"name" jsonschema:"description=名称(必填): 中秋节|国庆|春节|618|双11 等,required"`
	LeadDays  int    `json:"lead_days,omitempty" jsonschema:"description=提前备货天数(默认 0,holiday/promo 建议 3-15)"`
	Note      string `json:"note,omitempty" jsonschema:"description=备注"`
	DryRun    bool   `json:"dry_run,omitempty" jsonschema:"description=二次确认模式,默认 false"`
	Source    string `json:"source,omitempty" jsonschema:"description=来源标识,默认 wecom_agent"`
}

// RecordSpecialDateResp 输出
type RecordSpecialDateResp struct {
	Date    string `json:"date"`
	Type    string `json:"name_type"`
	Name    string `json:"name"`
	Action  string `json:"action"` // "dry_run" | "created" | "updated" | "unchanged"
	LeadDays int   `json:"lead_days"`
}

// RecordSpecialDate 工具函数
//   UNIQUE (date, type, name): 同一 (日期,类型,名称) 三元组唯一
func RecordSpecialDate(pool *pgxpool.Pool) *function.FunctionTool[RecordSpecialDateReq, RecordSpecialDateResp] {
	fn := func(ctx context.Context, req RecordSpecialDateReq) (RecordSpecialDateResp, error) {
		if pool == nil {
			return RecordSpecialDateResp{}, fmt.Errorf("record_special_date: pg pool 未初始化")
		}
		// 1) 校验
		dateStr := trimSpace(req.Date)
		date, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			return RecordSpecialDateResp{}, fmt.Errorf("date 格式错误,期望 YYYY-MM-DD: %w", err)
		}
		typ := trimSpace(req.Type)
		if !allowedSpecialTypes[typ] {
			return RecordSpecialDateResp{}, fmt.Errorf("type %q 不在白名单(允许: %v)", typ, keysOf(allowedSpecialTypes))
		}
		name := trimSpace(req.Name)
		if name == "" {
			return RecordSpecialDateResp{}, fmt.Errorf("name 必填")
		}
		if req.LeadDays < 0 {
			return RecordSpecialDateResp{}, fmt.Errorf("lead_days 不能为负")
		}
		source := orDefault(req.Source, "wecom_agent")

		// 2) 拉旧值,判断 action
		var existingID int64
		var existingLead int
		err = pool.QueryRow(ctx, `
			SELECT id, lead_days FROM special_calendar
			WHERE date=$1 AND type=$2 AND name=$3
		`, date, typ, name).Scan(&existingID, &existingLead)
		exists := err == nil
		if err != nil && err != pgx.ErrNoRows {
			return RecordSpecialDateResp{}, fmt.Errorf("read existing: %w", err)
		}

		// 3) DryRun
		if req.DryRun {
			return RecordSpecialDateResp{
				Date:     dateStr,
				Type:     typ,
				Name:     name,
				Action:   "dry_run",
				LeadDays: req.LeadDays,
			}, nil
		}

		// 4) UPSERT
		var action string
		if exists && existingLead == req.LeadDays {
			action = "unchanged"
		} else {
			_, err = pool.Exec(ctx, `
				INSERT INTO special_calendar (date, type, name, lead_days, note, source)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (date, type, name) DO UPDATE
				SET lead_days = EXCLUDED.lead_days,
				    note = EXCLUDED.note,
				    source = EXCLUDED.source
			`, date, typ, name, req.LeadDays, req.Note, source)
			if err != nil {
				return RecordSpecialDateResp{}, fmt.Errorf("upsert special_calendar: %w", err)
			}
			action = "updated"
			if !exists {
				action = "created"
			}
		}

		return RecordSpecialDateResp{
			Date:     dateStr,
			Type:     typ,
			Name:     name,
			Action:   action,
			LeadDays: req.LeadDays,
		}, nil
	}

	return function.NewFunctionTool(fn,
		function.WithName("record_special_date"),
		function.WithDescription("记一个特殊日期(节假日/促销/季节),dry_run=true 时只返回待写入内容. 同一 (date,type,name) 三元组唯一."),
	)
}

// ============================================================
// 工具 4: query_upcoming_dates
// ============================================================

// QueryUpcomingDatesReq 输入
type QueryUpcomingDatesReq struct {
	Type       string `json:"type,omitempty" jsonschema:"description=按类型过滤(可选),空=所有类型"`
	DaysAhead  int    `json:"days_ahead" jsonschema:"description=从今天起算的天数窗口(必填,建议 7/30/90),required"`
	FromDate   string `json:"from_date,omitempty" jsonschema:"description=起始日期 YYYY-MM-DD(可选,默认今天)"`
}

// QueryUpcomingDatesItem 单条
type QueryUpcomingDatesItem struct {
	Date     string `json:"date"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	LeadDays int    `json:"lead_days"`
	Note     string `json:"note"`
}

// QueryUpcomingDatesResp 输出
type QueryUpcomingDatesResp struct {
	FromDate string                    `json:"from_date"`
	ToDate   string                    `json:"to_date"`
	Count    int                       `json:"count"`
	Items    []QueryUpcomingDatesItem  `json:"items"`
}

// QueryUpcomingDates 工具函数
//   取 [from_date, from_date + days_ahead) 内的所有 special_calendar,按日期升序
func QueryUpcomingDates(pool *pgxpool.Pool) *function.FunctionTool[QueryUpcomingDatesReq, QueryUpcomingDatesResp] {
	fn := func(ctx context.Context, req QueryUpcomingDatesReq) (QueryUpcomingDatesResp, error) {
		if pool == nil {
			return QueryUpcomingDatesResp{}, fmt.Errorf("query_upcoming_dates: pg pool 未初始化")
		}
		if req.DaysAhead <= 0 {
			return QueryUpcomingDatesResp{}, fmt.Errorf("days_ahead 必须 > 0")
		}
		var fromDate time.Time
		if req.FromDate != "" {
			d, err := time.Parse("2006-01-02", req.FromDate)
			if err != nil {
				return QueryUpcomingDatesResp{}, fmt.Errorf("from_date 格式错误: %w", err)
			}
			fromDate = d
		} else {
			fromDate = time.Now().UTC().Truncate(24 * time.Hour)
		}
		toDate := fromDate.AddDate(0, 0, req.DaysAhead)

		var (
			rows pgx.Rows
			err  error
		)
		if req.Type != "" {
			rows, err = pool.Query(ctx, `
				SELECT date, type, name, lead_days, COALESCE(note,'')
				FROM special_calendar
				WHERE date >= $1 AND date < $2 AND type = $3
				ORDER BY date ASC
			`, fromDate, toDate, req.Type)
		} else {
			rows, err = pool.Query(ctx, `
				SELECT date, type, name, lead_days, COALESCE(note,'')
				FROM special_calendar
				WHERE date >= $1 AND date < $2
				ORDER BY date ASC
			`, fromDate, toDate)
		}
		if err != nil {
			return QueryUpcomingDatesResp{}, fmt.Errorf("query special_calendar: %w", err)
		}
		defer rows.Close()

		out := QueryUpcomingDatesResp{
			FromDate: fromDate.Format("2006-01-02"),
			ToDate:   toDate.Format("2006-01-02"),
			Items:    []QueryUpcomingDatesItem{},
		}
		for rows.Next() {
			var it QueryUpcomingDatesItem
			var d time.Time
			if err := rows.Scan(&d, &it.Type, &it.Name, &it.LeadDays, &it.Note); err != nil {
				return QueryUpcomingDatesResp{}, fmt.Errorf("scan: %w", err)
			}
			it.Date = d.Format("2006-01-02")
			out.Items = append(out.Items, it)
		}
		if err := rows.Err(); err != nil {
			return QueryUpcomingDatesResp{}, fmt.Errorf("rows err: %w", err)
		}
		out.Count = len(out.Items)
		return out, nil
	}

	return function.NewFunctionTool(fn,
		function.WithName("query_upcoming_dates"),
		function.WithDescription("查从今天/指定日期起 N 天内的特殊日期清单(按日期升序). type 可选过滤."),
	)
}
