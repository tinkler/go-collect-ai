package purchasealert

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tinkler/collect-ai/internal/model"
)

// ============================================================
// W4.2 smoke tests: service.Apply 路径选择
//   - 无 AgentRunner → 走 Go rules
//   - AgentRunner 不可用 → 走 Go rules
//   - AgentRunner 跑失败 → fallback Go rules
//   - AgentRunner + skill 齐 → 走 skill (失败时 fallback)
// ============================================================

// stubAgentRunner 模拟 Runner 接口
type stubAgentRunner struct {
	enabled  bool
	reply    string
	err      error
	called   int
	gotPromp string
}

func (s *stubAgentRunner) Enabled() bool { return s.enabled }

func (s *stubAgentRunner) RunAnalysis(ctx context.Context, userID, sessionID, prompt string) (string, int, error) {
	s.called++
	s.gotPromp = prompt
	if s.err != nil {
		return "", 0, s.err
	}
	return s.reply, 3, nil
}

// stubSkillLoader 模拟 SkillLoader 接口
type stubSkillLoader struct {
	skills map[string]string // name -> body
}

func (l *stubSkillLoader) GetBody(name string) (string, bool) {
	b, ok := l.skills[name]
	return b, ok
}

// makeSession 构造一个最小 session
func makeSession(id, supplier string, rows ...model.SkuRow) *model.Session {
	return &model.Session{
		ID:           id,
		SupplierName: supplier,
		Mode:         model.ModePurchase,
		Rows:         rows,
	}
}

func makeRow(rowID int64, supp, name string, qty *int) model.SkuRow {
	return model.SkuRow{
		RowID:       rowID,
		Seq:         int(rowID),
		MatchedSupp: supp,
		MatchedName: name,
		Qty:         qty,
	}
}

func TestApply_NoAgentRunner_GoRulesOnly(t *testing.T) {
	// nil pool: 跑不到 SQL 路径, 但 Go rules 也会失败 (因为拿不到 context)
	// 关键: 验证不调 agentRunner
	svc := NewService(nil)
	svc.SetSkillLoader(&stubSkillLoader{skills: map[string]string{"purchase-alert": "SKILL BODY"}})

	qty := 1
	sess := makeSession("sess-1", "汇一", makeRow(1, "汇一", "可口可乐", &qty))

	_, err := svc.Apply(context.Background(), sess)
	// 期望: 不调 agentRunner (它为 nil), 走 Go rules, 因 nil pool loadContext 失败
	if err == nil {
		t.Fatal("nil pool should fail at loadContext")
	}
}

func TestApply_AgentRunnerDisabled_GoRulesOnly(t *testing.T) {
	runner := &stubAgentRunner{enabled: false}
	loader := &stubSkillLoader{skills: map[string]string{"purchase-alert": "SKILL BODY"}}
	svc := NewService(nil)
	svc.SetAgentRunner(runner)
	svc.SetSkillLoader(loader)

	qty := 1
	sess := makeSession("sess-2", "汇一", makeRow(1, "汇一", "可口可乐", &qty))
	_, _ = svc.Apply(context.Background(), sess)

	// runner.enabled = false → 走 Go rules 路径, 不调 RunAnalysis
	if runner.called != 0 {
		t.Errorf("AgentRunner.RunAnalysis should not be called (enabled=false), got called=%d", runner.called)
	}
}

func TestApply_AgentRunnerError_FallbackToGoRules(t *testing.T) {
	runner := &stubAgentRunner{
		enabled: true,
		err:     errors.New("LLM network timeout"),
	}
	loader := &stubSkillLoader{skills: map[string]string{"purchase-alert": "SKILL BODY"}}
	svc := NewService(nil)
	svc.SetAgentRunner(runner)
	svc.SetSkillLoader(loader)

	qty := 1
	sess := makeSession("sess-3", "汇一", makeRow(1, "汇一", "可口可乐", &qty))
	_, _ = svc.Apply(context.Background(), sess)

	// 期望: runner 跑一次 (失败), 然后 fallback Go rules (失败因 nil pool)
	if runner.called != 1 {
		t.Errorf("AgentRunner.RunAnalysis should be called once, got called=%d", runner.called)
	}
}

func TestApply_SkillMissing_FallbackToGoRules(t *testing.T) {
	runner := &stubAgentRunner{enabled: true, reply: "ok"}
	loader := &stubSkillLoader{skills: map[string]string{}} // 空, 没有 purchase-alert
	svc := NewService(nil)
	svc.SetAgentRunner(runner)
	svc.SetSkillLoader(loader)

	qty := 1
	sess := makeSession("sess-4", "汇一", makeRow(1, "汇一", "可口可乐", &qty))
	_, _ = svc.Apply(context.Background(), sess)

	// skill 缺失 → 走 fallback, 不调 runner
	if runner.called != 0 {
		t.Errorf("AgentRunner.RunAnalysis should not be called (skill missing), got called=%d", runner.called)
	}
}

func TestApply_AgentRunnerSuccess_NoFallback(t *testing.T) {
	runner := &stubAgentRunner{
		enabled: true,
		reply:   "OK",
	}
	loader := &stubSkillLoader{skills: map[string]string{"purchase-alert": "FAKE BODY"}}
	svc := NewService(nil)
	svc.SetAgentRunner(runner)
	svc.SetSkillLoader(loader)

	qty := 1
	sess := makeSession("sess-5", "汇一", makeRow(1, "汇一", "可口可乐", &qty))
	_, _ = svc.Apply(context.Background(), sess)

	// 期望: 调 runner 一次, 但因为 nil pool, ListAlertsBySession 失败 → Apply 返 err
	// fallback 不跑 (因为 skill 路径没"成功")
	if runner.called != 1 {
		t.Errorf("AgentRunner.RunAnalysis should be called once, got called=%d", runner.called)
	}
	// prompt 验证: 应该包含 skill body
	if runner.gotPromp == "" {
		t.Error("prompt should be set")
	}
	// prompt 包含 "FAKE BODY" + session 数据
	if !contains(runner.gotPromp, "FAKE BODY") {
		t.Error("prompt should contain skill body")
	}
	if !contains(runner.gotPromp, "sess-5") {
		t.Error("prompt should contain session_id")
	}
}

func TestApply_AgentRunnerSuccess_AlertsReadFromDB(t *testing.T) {
	pool, cleanup := testPoolFallback(t)
	defer cleanup()
	if pool == nil {
		t.Skip("PG unavailable, skipping integration test")
	}

	// 准备: 1 个 session + 2 个 alert (LLM 模拟已落库)
	sessID := "00000000-0000-0000-0000-000000000abc"
	setupAlertsTable(t, pool)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO purchase_session_alert (session_id, row_id, rule, severity, category, message) VALUES
		 ($1, 0, 'block_entry', 'block', 'block', 'mock alert 1'),
		 ($1, 1, 'high_stock', 'warn', 'warn', 'mock alert 2')`, sessID)
	if err != nil {
		t.Fatalf("insert mock alerts: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM purchase_session_alert WHERE session_id = $1`, sessID)
	})

	runner := &stubAgentRunner{enabled: true, reply: "OK"}
	loader := &stubSkillLoader{skills: map[string]string{"purchase-alert": "BODY"}}
	svc := NewService(pool)
	svc.SetAgentRunner(runner)
	svc.SetSkillLoader(loader)

	qty := 1
	sess := makeSession(sessID, "汇一", makeRow(1, "汇一", "可口可乐", &qty))
	alerts, err := svc.Apply(context.Background(), sess)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(alerts) != 2 {
		t.Errorf("alerts count = %d, want 2", len(alerts))
	}
}

// helpers

// testPoolFallback 起一个临时 PG pool (跟其他测试共享 testhelper 模式)
//   失败时返 nil, 调用方 Skip
func testPoolFallback(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	dsn := getTestDSN(t)
	if dsn == "" {
		return nil, func() {}
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		return nil, func() {}
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		return nil, func() {}
	}
	return pool, func() { pool.Close() }
}

func getTestDSN(t *testing.T) string {
	t.Helper()
	// 跟其他工具 testhelper 共用, 这里简单实现
	// 实际项目已有 testhelper_test.go 提供 testPool, 这里只取 DSN
	// 为简洁起见, 直接从 env 读
	if v := getenv("PG_TEST_DSN"); v != "" {
		return v
	}
	return ""
}

func setupAlertsTable(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, _ = pool.Exec(context.Background(), `
		CREATE TABLE IF NOT EXISTS purchase_session_alert (
			id              BIGSERIAL PRIMARY KEY,
			session_id      UUID NOT NULL,
			row_id          BIGINT,
			rule            TEXT NOT NULL,
			severity        TEXT NOT NULL,
			category        TEXT NOT NULL DEFAULT 'info',
			message         TEXT NOT NULL,
			acked_at        TIMESTAMPTZ,
			acked_by        TEXT NOT NULL DEFAULT '',
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`)
}

func getenv(k string) string {
	return os.Getenv(k)
}

// contains 简单字符串包含 (reuses rules_test.go's contains)
func indexOfStr(haystack, needle string) int {
	if needle == "" {
		return 0
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
