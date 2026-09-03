// Package agent 是 collect-ai 智能采购模块的 LLM Agent 编排层。
//
// 基于 trpc-group/trpc-agent-go (Apache-2.0) v1.11+ 实现。
//
// 范围(W1, 2026-09-01):
//   - 模块 A 的 6 个 Function Tool (internal/agent/tools/)
//   - Runner 骨架: 配 LLM (DeepSeek 优先) + 工具注册
//   - 降级: LLM 未配置时,工具仍可单独调用(供单测/HTTP 接口)
//
// 后续 W2+: 接入 wecom OnMessage → Runner.Run 流式回发
package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/model/openai"
	"trpc.group/trpc-go/trpc-agent-go/runner"
	"trpc.group/trpc-go/trpc-agent-go/tool"

	"github.com/tinkler/collect-ai/internal/agent/skill"
	"github.com/tinkler/collect-ai/internal/agent/tools"
	"github.com/tinkler/collect-ai/internal/business"
)

// Config Agent 配置 (env 兜底 + main.go 注入)
type Config struct {
	// Enabled 整体开关 (env COLLECTAI_AGENT_ENABLED, 默认 false — W1 默认关闭)
	Enabled bool

	// BaseURL LLM base URL (env COLLECTAI_LLM_BASE_URL, 默认 https://api.deepseek.com)
	BaseURL string

	// APIKey LLM key (env COLLECTAI_LLM_API_KEY, 必填才能启 LLM)
	APIKey string

	// ModelName 模型名 (env COLLECTAI_LLM_MODEL, 默认 deepseek-chat)
	ModelName string

	// AppName Runner app name (env COLLECTAI_AGENT_APP_NAME, 默认 collect-ai)
	AppName string

	// Instruction System prompt (留空用 defaultInstruction)
	Instruction string

	// SkillRoots skill 扫描根(逗号分隔追加到默认 root,env COLLECTAI_SKILL_ROOTS)
	// 默认包含 ./skills/ + ~/.claude/skills/ + ~/.agents/skills/
	// 推荐:留空,用默认就行(已涵盖项目内 + 两个标准用户级落点)
	SkillRoots string

	// SkillsEnabled 是否启用 skill 系统(env COLLECTAI_AGENT_SKILLS_ENABLED, 默认 true)
	// 设为 false 时不加载 skill、不注入 prompt、不注册 invoke_skill tool
	SkillsEnabled bool
}

// DefaultLLMBaseURL 默认 DeepSeek 端点
const DefaultLLMBaseURL = "https://api.deepseek.com"

// DefaultLLMModel 默认 deepseek-chat
const DefaultLLMModel = "deepseek-chat"

// DefaultAppName 默认 Runner app name
const DefaultAppName = "collect-ai"

// defaultInstruction 默认 System prompt
const defaultInstruction = `你是商超采购助理 AI,服务于店老板。

# 角色
- 帮老板把零散对话里的决策信息结构化(供应商政策、节假日、堆头费等)
- 严格通过我提供的 6 个工具记入数据库,不要在对话里"假装"记下了

# 重要原则
1. **二次确认**: 任何写入操作,先用 dry_run=true 预览,然后用自然语言告诉老板你要写什么,等老板确认后再 dry_run=false 真写
2. **白名单优先**: key / kind / type 必须用我白名单里的字面值;不确定时用 query 工具先查
3. **简洁**: 单条回复 ≤ 200 字,推企微群时尤其重要
4. **失败要明说**: 工具返回 error 时,如实告诉老板哪里错了,不要硬编

# 常见对话
- "汇一是自采供应商,堆头费他们出" → 拆 2 个 key:
    is_self_procure=true
    has_duitou=true (堆头费他们出 = 供应商承担)
- "榄菊不让退" → allow_return=false (可考虑同步 block_entry=true)
- "中秋节前 3 天要备 5 倍量" → record special_calendar 中秋节, type=holiday, lead_days=3
- "汇一堆头 5000/月,到 12 月底" → record_promotion_fee kind=堆头, amount=5000, period_end=2026-12-31

# 措辞模板
确认时: "记下: {supplier} {key}={value}。对吗?"
撤销时: "刚才那条要改吗?当前值: {prev} → 新值: {new}"
拒绝时: "我不懂 {X},只能记: {allowed_keys}"
`

// Runner Agent Runner 封装
type Runner struct {
	cfg         Config
	pool        *pgxpool.Pool
	gateway     *business.Gateway // 2026-09-02: 预留 Gateway 注入,W2+ cube 工具用
	tools       []tool.Tool
	toolsByName map[string]tool.Tool
	llmAgent    agent.Agent
	runner      runner.Runner

	// 2026-09-02: Agent Skills (Anthropic spec) — 推理逻辑热更新
	skillStore  *skill.Store
	skillWatch  *skill.Watcher
	invokeSkill tool.Tool // invoke_skill 暴露给 LLM
}

// NewRunner 构造 Runner
//   当 cfg.Enabled=true 且 cfg.APIKey 非空 → 注册 LLM Agent
//   否则降级: tools 仍可用,但 LLM 调用会返回 ErrLLMNotConfigured
//   gateway: 2026-09-02 预留,当前 6 个 tool 都是写 PG 表,不用 cube
//            W2+ 加 cube 工具 (e.g. QueryProductsTool) 时直接用 r.gateway.Query()
func NewRunner(ctx context.Context, cfg Config, pool *pgxpool.Pool, gateway *business.Gateway) (*Runner, error) {
	if pool == nil {
		return nil, fmt.Errorf("agent: pg pool 必填")
	}

	// 1) 业务工具注册
	bizTools := []tool.Tool{
		tools.RememberSupplierPolicy(pool),
		tools.QuerySupplierPolicy(pool),
		tools.RecordSpecialDate(pool),
		tools.QueryUpcomingDates(pool),
		tools.RecordPromotionFee(pool),
		tools.ListPromotionFee(pool),
	}

	// 2) Skill 系统(Anthropic Agent Skills 规范)
	//    推理逻辑外置,SKILL.md + scripts/ 热更新
	//    失败不阻塞 Runner(降级为不带 skill)
	skillStore := skill.NewStore()
	var skillWatcher *skill.Watcher
	var invokeSkillTool tool.Tool
	if cfg.SkillsEnabled {
		wd, _ := os.Getwd()
		roots := skill.RootsFromEnvOrDefault(wd, cfg.SkillRoots)
		skillStore.SetRoots(roots)

		if result, err := skill.Load(roots); err == nil {
			skillStore.Replace(result.Skills)
			if msg := result.FormatErrors(); msg != "" {
				log.Printf("[agent] %s", strings.TrimSpace(msg))
			}
			log.Printf("[agent] skills 加载: %d 个 skill 从 %d 个 root", len(result.Skills), len(result.Roots))
			for _, sk := range result.Skills {
				log.Printf("[agent]   - %s [%s] (%d scripts, %d refs)", sk.Manifest.Name, sk.Source, len(sk.Scripts), len(sk.References))
			}
		} else {
			log.Printf("[agent] skill 初始加载失败(继续): %v", err)
		}

		invokeSkillTool = skill.NewInvokeSkillTool(skillStore)

		// 启动热更新
		w, err := skill.NewWatcher(skillStore, roots, skill.DefaultLoader())
		if err != nil {
			log.Printf("[agent] skill watcher 启动失败(无热更新): %v", err)
		} else {
			skillWatcher = w
		}
	}

	// 3) 合并工具: 业务 + invoke_skill
	allTools := append([]tool.Tool{}, bizTools...)
	if invokeSkillTool != nil {
		allTools = append(allTools, invokeSkillTool)
	}
	toolsByName := make(map[string]tool.Tool, len(allTools))
	for _, t := range allTools {
		toolsByName[t.Declaration().Name] = t
	}

	r := &Runner{
		cfg:         cfg,
		pool:        pool,
		gateway:     gateway,
		tools:       allTools,
		toolsByName: toolsByName,
		skillStore:  skillStore,
		skillWatch:  skillWatcher,
		invokeSkill: invokeSkillTool,
	}

	// 4) LLM Agent (可选)
	if !cfg.Enabled {
		log.Printf("[agent] LLM 关闭 (Enabled=false),tools 仍可用")
		return r, nil
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		log.Printf("[agent] LLM Enabled 但 APIKey 缺失,降级为 tools-only 模式")
		return r, nil
	}

	baseURL := orDefault(cfg.BaseURL, DefaultLLMBaseURL)
	modelName := orDefault(cfg.ModelName, DefaultLLMModel)
	appName := orDefault(cfg.AppName, DefaultAppName)
	instruction := orDefault(cfg.Instruction, defaultInstruction)

	// 把 skill L1 拼到 instruction 后(让 LLM 看到所有可用 skill 的 name+description)
	// 关键:不修改 llmagent 的 instruction(它构造时固定),而是每次 Run 时再注入
	// 但为了简化 L1 可见性,这里先拼到 instruction 字符串(LLM Agent 构造期)
	if skillBlock := skillStore.L1Prompt(); skillBlock != "" {
		instruction = instruction + "\n\n" + skillBlock
	}

	mdl := openai.New(modelName,
		openai.WithAPIKey(cfg.APIKey),
		openai.WithBaseURL(baseURL),
		openai.WithVariant(openai.VariantDeepSeek),
	)

	ag := llmagent.New("purchase-agent",
		llmagent.WithModel(mdl),
		llmagent.WithTools(allTools),
		llmagent.WithInstruction(instruction),
	)

	r.llmAgent = ag
	r.runner = runner.NewRunner(appName, ag)
	log.Printf("[agent] Runner ready: model=%s base=%s tools=%d skills=%d",
		modelName, baseURL, len(allTools), skillStore.Count())
	return r, nil
}

// Enabled 报告 LLM 是否可用
func (r *Runner) Enabled() bool {
	return r.runner != nil
}

// Tools 暴露工具列表(供 HTTP / CLI 调,无需 LLM)
func (r *Runner) Tools() []tool.Tool {
	return r.tools
}

// ToolByName 按名取工具
func (r *Runner) ToolByName(name string) (tool.Tool, bool) {
	t, ok := r.toolsByName[name]
	return t, ok
}

// AgentName 当前 Agent 名(LLM 不可用时返回 "tools-only")
func (r *Runner) AgentName() string {
	if r.llmAgent == nil {
		return "tools-only"
	}
	return r.llmAgent.Info().Name
}

// ErrLLMNotConfigured LLM 未配置
var ErrLLMNotConfigured = fmt.Errorf("agent: LLM 未配置,无法执行对话;工具可单独调用")

// Close 释放资源(skill watcher 等)
func (r *Runner) Close() {
	if r.skillWatch != nil {
		r.skillWatch.Stop()
		r.skillWatch = nil
	}
}

// SkillStore 暴露 skill 列表(供 HTTP / CLI 直接查询,无需 LLM)
func (r *Runner) SkillStore() *skill.Store {
	return r.skillStore
}

// Run 执行 LLM Agent 一轮对话
//   LLM 未配置时返 ErrLLMNotConfigured
//   userID  / sessionID 语义对齐 trpc-agent-go Runner.Run
func (r *Runner) Run(ctx context.Context, userID, sessionID, message string) (<-chan *RunnerEvent, error) {
	if !r.Enabled() {
		return nil, ErrLLMNotConfigured
	}
	events, err := r.runner.Run(ctx, userID, sessionID, model.NewUserMessage(message))
	if err != nil {
		return nil, fmt.Errorf("runner.Run: %w", err)
	}
	out := make(chan *RunnerEvent, 16)
	go func() {
		defer close(out)
		for ev := range events {
			out <- &RunnerEvent{Raw: ev}
		}
	}()
	return out, nil
}

// RunnerEvent 简化的事件包装 (W1: 仅透传 Raw;W2 会按 EventType 分发)
type RunnerEvent struct {
	Raw *event.Event
}

// =====================================================================
// 公共: 简化 env 读取
// =====================================================================

// LoadConfigFromEnv 从环境变量读配置 (W1 不依赖 collect-ai config,后续并入)
func LoadConfigFromEnv(getEnv func(string) string) Config {
	get := func(key, fallback string) string {
		v := strings.TrimSpace(getEnv(key))
		if v == "" {
			return fallback
		}
		return v
	}
	return Config{
		Enabled:      strings.EqualFold(get("COLLECTAI_AGENT_ENABLED", "false"), "true"),
		BaseURL:      get("COLLECTAI_LLM_BASE_URL", DefaultLLMBaseURL),
		APIKey:       get("COLLECTAI_LLM_API_KEY", ""),
		ModelName:    get("COLLECTAI_LLM_MODEL", DefaultLLMModel),
		AppName:      get("COLLECTAI_AGENT_APP_NAME", DefaultAppName),
		Instruction:  get("COLLECTAI_AGENT_INSTRUCTION", ""),
		SkillRoots:   get("COLLECTAI_SKILL_ROOTS", ""),
		SkillsEnabled: !strings.EqualFold(get("COLLECTAI_AGENT_SKILLS_ENABLED", "true"), "false"),
	}
}

func orDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
