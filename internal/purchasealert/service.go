// Package purchasealert 规则引擎服务
package purchasealert

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tinkler/collect-ai/internal/model"
)

// Service 规则引擎服务 (W4.2 重构: 7 规则全部迁到 skills/purchase-alert/)
//
// 历史: W3.2 起的 4 规则 + W4.1 加 3 规则, 全部在 rules.go 写死 if/else
// 现状: Apply 走两步:
//   1) LLM 跑 skills/purchase-alert/ (RunAnalysis)
//      LLM 调 8 个 tool 查数据 + 自己决定 alert + 调 insert_purchase_alert 落库
//   2) 失败 fallback: LLM 不可用 (无 API key / 网络错) → 跑 Go rules (rules.go 的 7 规则 stub)
//      保证 W4.1 已上线功能不破
type Service struct {
	pool  *pgxpool.Pool
	rules []Rule // W4.2: 仅 fallback 用, LLM 跑成功后这些 rule 不跑
	now   func() time.Time
	// W4.2: LLM 调度器
	agentRunner AgentRunner // interface, 避免循环依赖
	skillLoader SkillLoader
	// W4.1: 异步分析跟踪
	analysisMu  sync.Mutex
	analysisRun map[string]context.CancelFunc
}

// AgentRunner 抽象 (用于 RunAnalysis), 避免 service 直接 import agent 包
type AgentRunner interface {
	RunAnalysis(ctx context.Context, userID, sessionID, prompt string) (string, int, error)
	Enabled() bool
}

// SkillLoader 抽象, 避免 service 直接 import skill 包
type SkillLoader interface {
	// GetBody 拿 skill 的 markdown body (LLM 用)
	// 返回 (body, exists)
	GetBody(name string) (string, bool)
}

// NewService 默认 fallback 4 规则 (W3.2)
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{
		pool:        pool,
		rules:       DefaultRules,
		now:         time.Now,
		analysisRun: map[string]context.CancelFunc{},
	}
}

// NewServiceWithClassifier W3.5+ 启用 LLMSeasonRule (仅 fallback 用)
func NewServiceWithClassifier(pool *pgxpool.Pool, classifier SeasonClassifier) *Service {
	rules := make([]Rule, 0, len(DefaultRules)+1)
	rules = append(rules, DefaultRules...)
	if classifier != nil {
		rules = append(rules, LLMSeasonRule{Classifier: classifier})
	}
	return &Service{
		pool:        pool,
		rules:       rules,
		now:         time.Now,
		analysisRun: map[string]context.CancelFunc{},
	}
}

// NewServiceWithPromos W4.1 启用 堆头 + 快讯规则
//   promos 预加载的 active promotion_fee (按 supplier 分组)
//   nil promos → 不启用堆头/快讯规则
//   顺序: 默认 5 规则 + HasDuitouRule + FlashPromoRule (堆头/快讯)
//   classifier 跟 promos 可同时传, 走 NewServiceWithClassifierAndPromos
func NewServiceWithPromos(pool *pgxpool.Pool, promos map[string][]ActivePromo) *Service {
	rules := make([]Rule, 0, len(DefaultRules)+2)
	rules = append(rules, DefaultRules...)
	if promos != nil {
		rules = append(rules, HasDuitouRule{ActivePromos: promos})
		rules = append(rules, FlashPromoRule{ActivePromos: promos})
	}
	return &Service{
		pool:        pool,
		rules:       rules,
		now:         time.Now,
		analysisRun: map[string]context.CancelFunc{},
	}
}

// SetNow 注入 now (单测用)
func (s *Service) SetNow(now func() time.Time) {
	s.now = now
}

// SetAgentRunner 注入 LLM 调度器 (W4.2)
func (s *Service) SetAgentRunner(r AgentRunner) {
	s.agentRunner = r
}

// SetSkillLoader 注入 skill loader (W4.2)
func (s *Service) SetSkillLoader(l SkillLoader) {
	s.skillLoader = l
}

// Apply 应用所有规则,返回产生的 alerts (已落库)
//
// W4.2 重构: 优先走 LLM skill 路径, 失败 fallback 到 Go rules
//   1) 如果 agentRunner + skillLoader 都注入 → 调 RunAnalysis 跑 purchase-alert skill
//      LLM 自己调 8 个 tool 落库, Go 端只等结果
//   2) 否则 / 失败 → 跑 Go rules (rules.go 7 规则), 数据走 SQL 查询
func (s *Service) Apply(ctx context.Context, sess *model.Session) ([]Alert, error) {
	if sess == nil {
		return nil, fmt.Errorf("session is nil")
	}

	// 1) 尝试 LLM skill 路径
	if s.agentRunner != nil && s.skillLoader != nil && s.agentRunner.Enabled() {
		alerts, err := s.applyViaSkill(ctx, sess)
		if err == nil {
			log.Printf("[purchasealert] session %s LLM 跑完, %d alerts", sess.ID, len(alerts))
			return alerts, nil
		}
		log.Printf("[purchasealert] session %s LLM 失败, fallback Go rules: %v", sess.ID, err)
		// 失败不返, 走 fallback
	} else {
		log.Printf("[purchasealert] session %s LLM 不可用 (agentRunner=%v skillLoader=%v), 走 Go rules",
			sess.ID, s.agentRunner != nil, s.skillLoader != nil)
	}

	// 2) Fallback: Go rules
	return s.applyViaGoRules(ctx, sess)
}

// applyViaSkill 调 LLM 跑 purchase-alert skill (W4.2)
func (s *Service) applyViaSkill(ctx context.Context, sess *model.Session) ([]Alert, error) {
	// 1) 加载 skill body
	body, ok := s.skillLoader.GetBody("purchase-alert")
	if !ok {
		return nil, fmt.Errorf("skill 'purchase-alert' 未加载, 检查 skills/ 目录")
	}

	// 2) 拼 prompt (skill body + 任务数据)
	prompt := buildAnalysisPrompt(body, sess)

	// 3) 调 LLM 跑
	reply, toolCalls, err := s.agentRunner.RunAnalysis(ctx,
		"system", sess.ID, prompt)
	if err != nil {
		return nil, fmt.Errorf("RunAnalysis: %w", err)
	}
	log.Printf("[purchasealert] skill LLM 跑完, tool_calls=%d, reply 前 200: %s",
		toolCalls, truncateString(reply, 200))

	// 4) LLM 已通过 insert_purchase_alert 落库, 重新读 alerts
	alerts, err := s.ListAlertsBySession(ctx, sess.ID)
	if err != nil {
		s.writeAnalysisStatus(ctx, sess.ID, "failed", "read alerts: "+err.Error())
		return nil, fmt.Errorf("read alerts after LLM: %w", err)
	}
	// 5) 写 done (W4.2.1: 跟 fallback 路径行为一致)
	s.writeAnalysisStatus(ctx, sess.ID, "done", "")
	return alerts, nil
}

// buildAnalysisPrompt 拼 LLM 用的 prompt
//   = skill body (含完整 7 规则) + 任务数据 (session.rows)
func buildAnalysisPrompt(skillBody string, sess *model.Session) string {
	rowsJSON, _ := json.Marshal(sess.Rows)
	supplier := sess.SupplierName
	now := time.Now().Format(time.RFC3339)

	return skillBody + `

---

# 任务 (本次跑)

请对下面这张采购收货单跑 7 规则 (block_entry / no_return / offseason / holiday_lead / high_stock / has_duitou / flash_promo), 产出 alerts 并落库。

## 输入

session_id: ` + sess.ID + `
supplier_name: ` + supplier + `
analysis_at: ` + now + `

rows (本次采购商品):
` + string(rowsJSON) + `

## 输出要求

- 读 references/7-rules.md 拿到每条规则的判定 + 降级 + 不报条件
- 调 query_supplier_policy / query_promotion_fee / query_special_calendar / query_app_settings 拿必要数据
- 对每个判定要报的 alert, 调 insert_purchase_alert 落库
- 跑完调 update_analysis_status(session_id, "done") 收尾
- 如果出错, 调 update_analysis_status(session_id, "failed", error_message)

跑完后, 用一句话简短总结"本单命中 X 条 alert (block 0, warn 1, info 2, highlight_dui 1, highlight_others 0)"。
`
}

// applyViaGoRules fallback: 跑 Go rules (W3.2 - W4.1 老路径, 保持兼容)
//
// W4.2.1 fix: 跑完必须写 analysis_status=done/failed, 跟 applyViaSkill 行为一致
//   - 否则前端永远轮询"正在排队分析中", 1 小时后还在 pending
//   - writeAnalysisStatus 在 s.pool 为 nil 时静默跳过 (兼容单测)
func (s *Service) applyViaGoRules(ctx context.Context, sess *model.Session) ([]Alert, error) {
	// 1) 加载上下文
	rc, err := s.loadContext(ctx, sess)
	if err != nil {
		// loadContext 失败: 写 failed, 但仍返 err 让调用方知道
		s.writeAnalysisStatus(ctx, sess.ID, "failed", err.Error())
		return nil, fmt.Errorf("load context: %w", err)
	}

	// 2) 应用规则
	var alerts []Alert
	// 整 session 级别规则(HolidayLead)只跑一次
	for _, rule := range s.rules {
		if rule.Name() == "holiday_lead" {
			alerts = append(alerts, rule.Apply(ctx, sess, nil, rc)...)
		}
	}
	// row-specific 规则
	for i := range sess.Rows {
		row := &sess.Rows[i]
		if row.IsDeleted {
			continue
		}
		for _, rule := range s.rules {
			if rule.Name() == "holiday_lead" {
				continue // 已跑过
			}
			alerts = append(alerts, rule.Apply(ctx, sess, row, rc)...)
		}
	}

	// 3) 落库
	if len(alerts) > 0 {
		if err := s.insertAlerts(ctx, alerts); err != nil {
			log.Printf("[purchasealert] insertAlerts err: %v", err)
			s.writeAnalysisStatus(ctx, sess.ID, "failed", err.Error())
			return alerts, fmt.Errorf("insert alerts: %w", err)
		}
	}
	// 4) 写 done (跟 StartAnalysisAsync 的 LLM 路径行为一致)
	s.writeAnalysisStatus(ctx, sess.ID, "done", "")
	return alerts, nil
}

// writeAnalysisStatus 写 status 到 PG (W4.2.1 修 fallback 不写 done 的 bug)
//   pool nil 静默跳过 (单测不连 DB)
//   error 静默忽略 (主流程已返 alerts, status 写失败不影响业务)
func (s *Service) writeAnalysisStatus(ctx context.Context, sessionID, status, errMsg string) {
	if s.pool == nil || sessionID == "" {
		return
	}
	var at interface{}
	if status == "done" {
		at = time.Now()
	} else {
		at = nil
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE parse_session
		SET analysis_status = $2, analysis_at = $3, analysis_error = $4, updated_at = NOW()
		WHERE id = $1
	`, sessionID, status, at, errMsg)
	if err != nil {
		log.Printf("[purchasealert] writeAnalysisStatus(%s, %s) err: %v", sessionID, status, err)
	}
}

// loadContext 加载供应商政策 + 节假日 + 阈值 (app_settings) + active promos
func (s *Service) loadContext(ctx context.Context, sess *model.Session) (RuleContext, error) {
	// W4.2: nil pool 守卫 (单测常用 nil pool 测 fallback 路径, 不应 panic)
	if s.pool == nil {
		return RuleContext{}, fmt.Errorf("loadContext: pg pool not initialized")
	}
	rc := RuleContext{
		SupplierPolicies: make(map[string][]PolicyKV),
		Holidays:         []Holiday{},
		Now:              s.now(),
	}

	// 1) 拉 session 内涉及到的所有 supplier 的政策
	suppliers := make(map[string]struct{})
	for _, row := range sess.Rows {
		if !row.IsDeleted && row.MatchedSupp != "" {
			suppliers[row.MatchedSupp] = struct{}{}
		}
	}
	if len(suppliers) > 0 {
		supList := make([]string, 0, len(suppliers))
		for k := range suppliers {
			supList = append(supList, k)
		}
		rows, err := s.pool.Query(ctx, `
			SELECT supplier_name, key, value
			FROM supplier_policy
			WHERE supplier_name = ANY($1)
		`, supList)
		if err != nil {
			return rc, fmt.Errorf("query supplier_policy: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var sup, key string
			var raw []byte
			if err := rows.Scan(&sup, &key, &raw); err != nil {
				return rc, err
			}
			var val any
			_ = json.Unmarshal(raw, &val)
			rc.SupplierPolicies[sup] = append(rc.SupplierPolicies[sup], PolicyKV{Key: key, Val: val})
		}
	}

	// 2) 拉接下来 90 天内的节假日
	rows, err := s.pool.Query(ctx, `
		SELECT date, type, name, lead_days
		FROM special_calendar
		WHERE date >= $1 AND date < $1::date + INTERVAL '90 days'
		ORDER BY date ASC
	`, rc.Now)
	if err != nil {
		return rc, fmt.Errorf("query special_calendar: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var h Holiday
		var d time.Time
		if err := rows.Scan(&d, &h.Type, &h.Name, &h.LeadDays); err != nil {
			return rc, err
		}
		h.Date = d
		rc.Holidays = append(rc.Holidays, h)
	}

	// 3) W4.1: 拉 app_settings 阈值 (数据, 不硬编码)
	if err := s.loadSettings(ctx, &rc); err != nil {
		log.Printf("[purchasealert] loadSettings err: %v (继续用零值)", err)
	}

	// 4) W4.1: 拉当前日期在期内的 promotion_fee (按 supplier 分组)
	//   供 HasDuitouRule / FlashPromoRule 用
	promos, err := s.loadActivePromos(ctx, rc.Now)
	if err != nil {
		log.Printf("[purchasealert] loadActivePromos err: %v (继续无堆头/快讯规则)", err)
	}
	// 把 promos 注入到已注册的规则里 (新规则才有这个字段)
	for _, rule := range s.rules {
		switch r := rule.(type) {
		case HasDuitouRule:
			r.ActivePromos = promos
		case FlashPromoRule:
			r.ActivePromos = promos
		}
	}
	return rc, nil
}

// loadSettings 从 app_settings 读阈值 (W4.1)
func (s *Service) loadSettings(ctx context.Context, rc *RuleContext) error {
	if s.pool == nil {
		return nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT key, value FROM app_settings
		WHERE key IN ('high_stock_threshold', 'duitou_kinds', 'others_kinds')
	`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return err
		}
		switch key {
		case "high_stock_threshold":
			var v float64
			_ = json.Unmarshal(raw, &v)
			if v > 0 {
				rc.HighStockThreshold = v
			}
		case "duitou_kinds":
			_ = json.Unmarshal(raw, &rc.DuitouKinds)
		case "others_kinds":
			_ = json.Unmarshal(raw, &rc.OthersKinds)
		}
	}
	return rows.Err()
}

// loadActivePromos 拉当前日期在期内的 promotion_fee
//   按 supplier 分组, 同一 supplier 多个 kind 都列出
//   (W4.1) 供 HasDuitouRule / FlashPromoRule 用
func (s *Service) loadActivePromos(ctx context.Context, now time.Time) (map[string][]ActivePromo, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT supplier_name, kind, amount, period_end
		FROM promotion_fee
		WHERE period_start <= $1 AND period_end >= $1
		ORDER BY supplier_name, period_end ASC
	`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]ActivePromo{}
	for rows.Next() {
		var sup, kind string
		var amount float64
		var end time.Time
		if err := rows.Scan(&sup, &kind, &amount, &end); err != nil {
			return nil, err
		}
		out[sup] = append(out[sup], ActivePromo{Kind: kind, Amount: amount, End: end})
	}
	return out, rows.Err()
}

// insertAlerts 批量写 purchase_session_alert (W4.1: 含 category)
func (s *Service) insertAlerts(ctx context.Context, alerts []Alert) error {
	if s.pool == nil {
		return fmt.Errorf("pg pool nil")
	}
	for _, a := range alerts {
		category := a.Category
		if category == "" {
			category = CategoryInfo // 默认
		}
		_, err := s.pool.Exec(ctx, `
			INSERT INTO purchase_session_alert (session_id, row_id, rule, severity, category, message)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, a.SessID, a.RowID, a.Rule, a.Severity, category, a.Message)
		if err != nil {
			return fmt.Errorf("insert alert: %w", err)
		}
	}
	return nil
}

// ListAlertsBySession 拉 session 的所有 alerts (W4.1: 含 category)
func (s *Service) ListAlertsBySession(ctx context.Context, sessionID string) ([]Alert, error) {
	if s.pool == nil {
		return nil, fmt.Errorf("pg pool nil")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, session_id, COALESCE(row_id, 0), rule, severity, COALESCE(category,'info'), message,
		       acked_at, COALESCE(acked_by, ''), created_at
		FROM purchase_session_alert
		WHERE session_id = $1
		ORDER BY severity DESC, created_at ASC
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		var a Alert
		var rowID int64
		if err := rows.Scan(&a.AlertID, &a.SessID, &rowID, &a.Rule, &a.Severity, &a.Category, &a.Message,
			&a.AckedAt, &a.AckedBy, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.RowID = rowID
		// AckedAt 已由 Scan 填好,不再做多余自赋值 (W4.2 vet 修)
		out = append(out, a)
	}
	return out, rows.Err()
}

// AckAlert 标记已读
func (s *Service) AckAlert(ctx context.Context, alertID int64, by string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE purchase_session_alert
		SET acked_at = NOW(), acked_by = $2
		WHERE id = $1
	`, alertID, by)
	return err
}

// ============================================================
// W4.1: 异步策略分析
//   CreateSession/AppendImages 完成后, 调 StartAnalysisAsync
//   - 立即返回, 不等 LLM
//   - 后台 goroutine 跑 Apply
//   - 完成后写 parse_session.analysis_status='done'
//   - 同一 session 重复调用: 取消旧 run, 启新 run (append-images 时)
// ============================================================

// StartAnalysisAsync 启动后台分析 (W4.1)
//   sessionID: parse_session.id
//   sessLoader: 从 DB 拉 session 数据的函数 (避免 handler 直接 import store 循环依赖)
//   行为:
//     1) 先把 status 置为 running
//     2) cancel 上一次同 session 的 run (如果有)
//     3) 启新 goroutine, 跑 Apply, 写 status=done/failed
func (s *Service) StartAnalysisAsync(
	sessionID string,
	sessLoader func(ctx context.Context, id string) (*model.Session, error),
) {
	if s.pool == nil || sessionID == "" {
		return
	}
	// 1) cancel 上一次 (append-images 重复触发用)
	s.analysisMu.Lock()
	if prev, ok := s.analysisRun[sessionID]; ok {
		prev() // cancel 旧 run
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	s.analysisRun[sessionID] = cancel
	s.analysisMu.Unlock()

	// 2) 写 running
	if _, err := s.pool.Exec(ctx, `
		UPDATE parse_session SET analysis_status='running', analysis_error=''
		WHERE id=$1
	`, sessionID); err != nil {
		log.Printf("[purchasealert] write running err: %v", err)
	}

	// 3) 启后台 goroutine
	go func() {
		defer func() {
			s.analysisMu.Lock()
			delete(s.analysisRun, sessionID)
			s.analysisMu.Unlock()
		}()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[purchasealert] panic in analysis %s: %v", sessionID, r)
				_, _ = s.pool.Exec(context.Background(), `
					UPDATE parse_session SET analysis_status='failed', analysis_error=$2
					WHERE id=$1
				`, sessionID, fmt.Sprintf("panic: %v", r))
			}
		}()

		// 拉最新 session 数据 (含 append 后新 rows)
		sess, err := sessLoader(ctx, sessionID)
		if err != nil {
			log.Printf("[purchasealert] sessLoader err: %v", err)
			_, _ = s.pool.Exec(context.Background(), `
				UPDATE parse_session SET analysis_status='failed', analysis_error=$2
				WHERE id=$1
			`, sessionID, "load session: "+err.Error())
			return
		}
		if sess == nil {
			_, _ = s.pool.Exec(context.Background(), `
				UPDATE parse_session SET analysis_status='failed', analysis_error='session not found'
				WHERE id=$1
			`, sessionID)
			return
		}

		// 跑 Apply
		alerts, err := s.Apply(ctx, sess)
		if err != nil {
			log.Printf("[purchasealert] Apply err: %v", err)
			_, _ = s.pool.Exec(context.Background(), `
				UPDATE parse_session SET analysis_status='failed', analysis_error=$2
				WHERE id=$1
			`, sessionID, err.Error())
			return
		}
		log.Printf("[purchasealert] session %s analyzed: %d alerts", sessionID, len(alerts))

		// 写 done
		_, _ = s.pool.Exec(context.Background(), `
			UPDATE parse_session SET analysis_status='done', analysis_at=NOW(), analysis_error=''
			WHERE id=$1
		`, sessionID)
	}()
}

// truncateString 截断字符串到 n 字符 (用于日志)
//   W4.2: log 日志里避免超长 LLM reply 把日志撑爆
func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
