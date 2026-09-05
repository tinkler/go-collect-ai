package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tinkler/collect-ai/internal/agent"
	"github.com/tinkler/collect-ai/internal/agent/skill"
	"github.com/tinkler/collect-ai/internal/api"
	"github.com/tinkler/collect-ai/internal/api/handler"
	"github.com/tinkler/collect-ai/internal/auth"
	"github.com/tinkler/collect-ai/internal/business"
	"github.com/tinkler/collect-ai/internal/config"

	"github.com/gin-gonic/gin"
	"github.com/tinkler/collect-ai/internal/parser"
	parseragent "github.com/tinkler/collect-ai/internal/parser/agent"
	"github.com/tinkler/collect-ai/internal/parser/bigmodel"
	"github.com/tinkler/collect-ai/internal/parser/glmocr"
	"github.com/tinkler/collect-ai/internal/promotionalert"
	"github.com/tinkler/collect-ai/internal/purchasealert"
	"github.com/tinkler/collect-ai/internal/rbac"
	"github.com/tinkler/collect-ai/internal/restock"
	"github.com/tinkler/collect-ai/internal/store"
	"github.com/tinkler/collect-ai/internal/supplierpayment"
	"github.com/tinkler/collect-ai/internal/wecom"
	"github.com/tinkler/collect-ai/internal/wxsign"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// OCR_MODEL / LLM_MODEL 现在是 env 兜底值
	// per-template 覆盖存在 template 表里, 解析时由 handler 按 template_id 解析
	log.Printf("[main] BigModel key len=%d, OCR default=%s, LLM default=%s",
		len(cfg.BigModelAPIKey), cfg.OCRModel, cfg.LLMModel)
	log.Printf("[main] PG=%s:%d/%s, Agent=%s", cfg.PGHost, cfg.PGPort, cfg.PGDatabase, cfg.AgentURL)
	log.Printf("[main] Upload dir=%s, MaxUploadMB=%d", cfg.UploadDir, cfg.MaxUploadMB)

	if err := os.MkdirAll(cfg.UploadDir, 0o755); err != nil {
		log.Fatalf("create upload dir: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := store.NewPool(ctx, cfg.PGDSN())
	if err != nil {
		log.Fatalf("connect pg: %v", err)
	}
	defer pool.Close()

	if err := store.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	log.Printf("[main] PG migrate OK")

	sessionRepo := store.NewSessionRepo(pool)
	strategyRepo := store.NewStrategyRepo(pool) // Phase A (2026-09-02)

	// llmClient 保留给 seasonClassifier 等用
	// llmClient := bigmodel.NewLlmClient(cfg.BigModelAPIKey, cfg.BigModelBase, cfg.LlmTimeoutSec)
	// 数据源: 启动后即固定,只走 .env / cfg 配置 (2026-08-31 简化,移除 dsstate 持久化 + 切换 API)
	initialDS := cfg.DataSource
	log.Printf("[main] datasource: %s (from env/cfg, 启动后即固定)", initialDS)
	agentClient := parseragent.NewClient(cfg.AgentURL, cfg.AgentToken, 30, initialDS)
	// 2026-09-02: 业务字段映射优先从 yaml 加载,失败 fallback 到 NewDefaultRegistry 硬编码
	//   MAPPING_FILE 留空 = 用 hardcode (向后兼容)
	//   推荐: MAPPING_FILE=configs/mappings.yaml
	var businessReg *business.Registry
	if cfg.MappingFile != "" {
		reg, err := business.NewRegistryFromYAML(cfg.MappingFile)
		if err != nil {
			log.Printf("[main] WARN: load mappings yaml %s failed: %v (fallback to NewDefaultRegistry)", cfg.MappingFile, err)
			businessReg = business.NewDefaultRegistry()
		} else {
			businessReg = reg
			log.Printf("[main] mappings loaded from yaml: %s (entities=%v)", cfg.MappingFile, businessReg.List())
		}
	} else {
		businessReg = business.NewDefaultRegistry()
		log.Printf("[main] mappings: NewDefaultRegistry (hardcode). Set MAPPING_FILE=configs/mappings.yaml to use yaml config.")
	}
	// 2026-09-02: 所有 cube 调用统一走 Gateway,业务代码不直接 import parser/agent
	//   Gateway 持 CubeClient interface,*agent.Client 自动满足
	gateway := business.NewGateway(agentClient, businessReg)

	gin.SetMode(gin.ReleaseMode)

	// ============== restock 模块启动 (2026-09-02 重构精简) ==============
	restockCfg := &restock.RestockConfig{
		BranchNo:       cfg.RestockBranchNo,
		CronEve:        cfg.DisplayRestockCronEve,
		CronMorn:       cfg.DisplayRestockCronMorn,
		CronAft:        cfg.DisplayRestockCronAft,
		CubeName:       cfg.DisplayRestockCubeName,
		RetryMax:       cfg.DisplayRestockRetryMax,
		MaxPerTick:     cfg.DisplayRestockMaxPush,
		WeComBotID:     cfg.WeComBotID,
		WeComBotSecret: cfg.WeComBotSecret,
		WeComWSURL:     cfg.WeComWSURL,
		WeComBindFile:  cfg.WeComBindFile,
	}
	restockCube := restock.NewCubeQuerier(gateway, restockCfg)
	// 2026-09-02: 企微长连接从 restock 抽到 internal/wecom/ 通用包
	wecomClient := wecom.New(wecom.Config{
		BotID:     cfg.WeComBotID,
		BotSecret: cfg.WeComBotSecret,
		WSURL:     cfg.WeComWSURL,
		BindFile:  cfg.WeComBindFile,
	})
	restockSvc := restock.NewService(restockCfg, pool, restockCube)
	// W3.5: 季节判定分类器 (关键词快速 + LLM 慢路径 + 6h 缓存)
	// seasonClassifier := buildSeasonClassifier(llmClient)
	// alertSvc := purchasealert.NewServiceWithClassifier(pool, seasonClassifier)                                // W3.2+W3.5
	promoAlertSvc := promotionalert.NewService(pool, strings.TrimSpace(os.Getenv("PROMOTION_ALERT_CHAT_ID"))) // W3.3: 堆头费到期预警 (空=禁用)
	supplierPaySvc := supplierpayment.NewService(pool, strings.TrimSpace(os.Getenv("OWNER_CHAT_ID")))         // W4.3: 供应商结算 cron
	// W5: cube 数据源注入 (默认 Noop, 设 COLLECTAI_CUBE_QUERIER=real 接真实 cube)
	//   2026-09-02: 传 gateway.Client() (CubeClient interface),统一 client
	if cq := buildCubeQuerier(gateway.Client()); cq != nil {
		supplierPaySvc.SetCubeQuerier(cq)
		log.Printf("[main] W5 cube 接入: 模式=%s", cubeMode())
	}
	cashRepo := store.NewCashBalanceRepo(pool)    // W4
	payRepo := store.NewSupplierPaymentRepo(pool) // W4

	// Phase A (2026-09-02): BizExecutor 业务字段执行器
	//   Orchestrator 的 SkuLoader 用它从 cube 拉该 supplier 的 SKU 库
	bizExecutor := business.NewExecutorFromGateway(gateway)

	// W2.5 + W4.3: Agent Runner (W2 wecom 桥 + W2.5 HTTP 触发共用)
	//   agentEnabled 跟 W2 wecom 桥保持一致 (用同一个 env)
	//   agentRunner 可能为 nil (LLM 不可用时 tools-only 模式), handler.AgentChat 走降级
	var agentRunner *agent.Runner
	agentEnabled := !strings.EqualFold(strings.TrimSpace(os.Getenv("COLLECTAI_AGENT_ENABLED")), "false")
	agentChatIDs := splitIDs(os.Getenv("COLLECTAI_AGENT_CHAT_IDS"))

	// ============== 鉴权 (2026-08-29) ==============
	authStore := auth.NewStore(pool)
	// 启动时加载 role_permissions 到内存 (RBAC 热路径用)
	if err := authStore.LoadAllRolePerms(ctx); err != nil {
		log.Printf("[main] WARN: load role_permissions failed: %v (RBAC will deny all)", err)
	} else {
		log.Printf("[main] role_permissions loaded OK")
	}
	authSign := auth.NewSigner(cfg.JWTSecret, cfg.AccessTokenTTLSec, cfg.RefreshTokenTTLSec)
	// 安全: prod + 默认 secret → 警告 (不阻断, 一些环境用 secrets manager 启动后才注入)
	if !cfg.DevMode && cfg.JWTSecret == "dev-secret-change-me-in-prod-32chars" {
		log.Printf("[main] WARNING: PROD mode with default JWT_SECRET — set JWT_SECRET in env!")
	}
	authWeCom := auth.NewWeComClient(cfg.WeComCorpID, cfg.WeComAgentID, cfg.WeComCorpSecret)
	authSvc := auth.NewService(authStore, authSign, authWeCom)

	h := &handler.Handler{
		UploadDir:  cfg.UploadDir,
		PublicBase: cfg.PublicBaseURL,
		MaxUpload:  int64(cfg.MaxUploadMB) * 1024 * 1024,
		// Phase A (2026-09-02): Orchestrator 字段在 agentRunner 初始化后填(下方)
		Orchestrator: nil,
		Agent:        agentClient,
		BizExecutor:  bizExecutor, // 2026-09-02 新增
		BusinessReg:  businessReg,
		Pool:         pool, // W4.1: GetAnalysisStatus 轻量查询
		Sessions:     sessionRepo,
		Strategies:   strategyRepo, // Phase A 新增
		// SkillStore + Orchestrator 在 agentRunner 初始化后注入
		SkillStore:  nil,
		CashRepo:    cashRepo,    // W4
		PayRepo:     payRepo,     // W4
		RestockSvc:  restockSvc,  // 2026-08-28: 采购收货单附加 plan_qty
		AlertSvc:    nil,         // 2026-09-01 W3.2: 采购订单智能提醒
		AgentRunner: agentRunner, // W2.5: H5 端 Agent chat
		// Phase B+ (2026-09-03): 删 DefaultOcrModel/DefaultLlmModel 字段 (VLM 内部固定)
	}

	// 注册企微消息回调 (新版 restock 不推群, 只做消息接收)
	wecomClient.OnMessage(func(chatID, userID, text string) {
		log.Printf("[wecom] msg from chat=%s user=%s: %s", chatID, userID, text)
	})

	// ============== 智能采购 Agent 桥接 (W2, 2026-09-01) ==============
	//   显式白名单 chat_ids 才接管(避免误接管 restock 群)
	//   env: COLLECTAI_AGENT_CHAT_IDS=chat_id1,chat_id2 (逗号或空格分隔)
	//        COLLECTAI_AGENT_ENABLED=true|false (默认 true, 需时显式关)
	// (agentEnabled / agentChatIDs / agentRunner 已在 W2.5 块定义, 这里直接用)
	// W4.2.1 修复: agentRunner 创建条件不再绑 wecom bridge 配置
	//   - 之前:  if agentEnabled && len(agentChatIDs) > 0
	//   - bug:    用户不接管企微群 (没设 COLLECTAI_AGENT_CHAT_IDS) → agentRunner 永远是 nil
	//            → alertSvc 永远走 fallback Go rules, 跟 LLM key 无关
	//   - 修复:  agentRunner 创建条件 = agentEnabled && LLM key 非空
	//            wecom bridge 启动条件独立判断 = agentChatIDs 非空
	agentCfg := agent.LoadConfigFromEnv(os.Getenv)
	if agentEnabled && strings.TrimSpace(agentCfg.APIKey) != "" {
		// W4.4: 构造 cube 类 tool 的 Fn 注入
		//   当前只激活 returnOrderFn (规则 8 pending_return)
		//   skuStockFn / skuSalesFn 留 nil → 对应 tool 内部降级 (NotFound 返 LLM)
		//   未来加 sales / stock_qty cube mapping 时再补
		cubeFns := buildPurchaseAlertCubeFns(bizExecutor, gateway)
		var err error
		agentRunner, err = agent.NewRunner(context.Background(), agentCfg, pool, gateway, cubeFns)
		if err != nil {
			log.Printf("[main] agent.NewRunner 失败: %v (LLM 任务降级为 fallback)", err)
		} else {
			log.Printf("[main] Agent Runner ready: llm=%v model=%s tools=15 (含 purchase_alert 6 个, 含 query_return_order 真注入)",
				agentRunner.Enabled(), agentCfg.ModelName)
		}
	} else {
		log.Printf("[main] Agent Runner 未启动 (enabled=%v, llm_key=%s)",
			agentEnabled, maskAPIKey(agentCfg.APIKey))
	}

	// wecom bridge 独立判断: 接管企微群消息 → 调 agentRunner.Run
	if agentEnabled && len(agentChatIDs) > 0 && agentRunner != nil {
		bridge := agent.NewBridge(agent.DefaultBridgeConfig(), agentRunner, agent.NewWecomSender(wecomClient))
		// 白名单 set 覆盖默认
		bridge = agent.NewBridge(agent.BridgeConfig{
			ChatIDs:       agentChatIDs,
			MaxReplyChars: 200,
			PerMinuteRate: 25,
			RunTimeout:    60 * time.Second,
		}, agentRunner, agent.NewWecomSender(wecomClient))
		wecomClient.OnAgentMessage(bridge.Handle)
		log.Printf("[main] Agent Bridge ready: chats=%d", len(agentChatIDs))
	} else if agentEnabled && len(agentChatIDs) > 0 {
		log.Printf("[main] Agent Bridge 跳过 (agentRunner 未就绪, LLM key 缺失)")
	}

	// Phase A (2026-09-02): 注入 SkillStore + Orchestrator 到 handler
	//   - agentRunner 启动后才有 skillStore (内部用 NewStore + Load)
	//   - Orchestrator 需要 SkuLoader (从 BizExecutor 拿)
	//   - 即使 Agent 未启用 (COLLECTAI_AGENT_ENABLED=false), 也构造一个轻量 skill store
	//     让 OCR 解析不依赖 Agent runner,但 skill 系统是 Loader 的事
	var skillStore *skill.Store
	if agentRunner != nil {
		skillStore = agentRunner.SkillStore()
	} else {
		ss, err := agent.NewStandaloneSkillStore(os.Getenv("COLLECTAI_SKILL_ROOTS"))
		if err != nil {
			log.Printf("[main] NewStandaloneSkillStore 失败: %v (Orchestrator 将不可用)", err)
		} else {
			skillStore = ss
		}
	}
	h.SkillStore = skillStore
	// W4.2: 把 agentRunner + skillStore 注入 alertSvc
	//   alertSvc.Apply 优先调 agentRunner.RunAnalysis 跑 purchase-alert skill
	//   LLM 不可用 / skill 缺失 / 跑失败 → fallback 到 Go rules
	if agentRunner != nil && skillStore != nil {
		// alertSvc.SetAgentRunner(agentRunner)
		// alertSvc.SetSkillLoader(&skillStoreAdapter{store: skillStore})
		log.Printf("[main] alertSvc 走 LLM skill 路径 (purchase-alert), 失败 fallback Go rules")
	} else {
		log.Printf("[main] alertSvc 走 Go rules fallback (agentRunner=%v skillStore=%v)",
			agentRunner != nil, skillStore != nil)
	}
	if skillStore != nil {
		// 2026-09-04 双引擎重构 (对齐 tin-nova):
		//   引擎1 = 智谱 prime-sync 文件解析 (复用 BIGMODEL_API_KEY, 印刷体/表格 OCR)
		//   引擎2 = DeepSeek 视觉模型 (图 + OCR 文本参考 → 结构化 JSON)
		//   不再: SKU hints 注入 / SkuMatcher L1~L3 匹配 (全部行当新 SKU)
		glmocrClient := glmocr.New(cfg.BigModelAPIKey, cfg.LlmTimeoutSec+60)
		// 2026-09-04: 启动诊断 — 不回显 key 内容, 只看长度 (env 未读到时长度=0)
		log.Printf("[main] 双引擎配置: BIGMODEL_API_KEY len=%d, DEEPSEEK_API_KEY len=%d, DEEPSEEK_BASE=%s, DEEPSEEK_VISION_MODEL=%s",
			len(cfg.BigModelAPIKey), len(cfg.DeepseekAPIKey), cfg.DeepseekBase, cfg.DeepseekVisionModel)
		orch, err := parser.NewOrchestrator(
			glmocrClient,
			cfg.DeepseekAPIKey, cfg.DeepseekBase, cfg.DeepseekVisionModel,
			skillStore,
			bizExecutor, // L1 barcode 精确对应供应商商品库 (不做 L2~L5 name/修正匹配)
		)
		if err != nil {
			log.Printf("[main] Orchestrator 构造失败: %v (CreateSession 将不可用)", err)
		} else {
			h.Orchestrator = orch
			log.Printf("[main] Orchestrator ready 双引擎+L1 (glmocr+deepseek-vision=%s, skill=%d, ProductSearcher=bizExecutor)",
				cfg.DeepseekVisionModel, skillStore.Count())
		}
	} else {
		log.Printf("[main] SkillStore 不可用,Orchestrator 暂不可用(创建 session 会报 skill 缺失)")
	}

	// W3.3 堆头费到期 cron: 启动时跑一次 + 每日 21:00 跑
	promoAlertCtx, promoAlertCancel := context.WithCancel(context.Background())
	defer promoAlertCancel()
	if strings.TrimSpace(os.Getenv("PROMOTION_ALERT_CHAT_ID")) != "" {
		// 启动时立即跑一次 (捕获已到期的)
		go func() {
			_ = promoAlertSvc.RunAndPush(promoAlertCtx, agent.NewWecomSender(wecomClient))
		}()
		// 每日 21:00 跑
		go func() {
			ticker := time.NewTicker(24 * time.Hour)
			defer ticker.Stop()
			// 第一次等下次 21:00 整点
			now := time.Now()
			next := time.Date(now.Year(), now.Month(), now.Day(), 21, 0, 0, 0, now.Location())
			if next.Before(now) {
				next = next.Add(24 * time.Hour)
			}
			wait := time.Until(next)
			first := time.NewTimer(wait)
			defer first.Stop()
			select {
			case <-promoAlertCtx.Done():
				return
			case <-first.C:
				_ = promoAlertSvc.RunAndPush(promoAlertCtx, agent.NewWecomSender(wecomClient))
			}
			for {
				select {
				case <-promoAlertCtx.Done():
					return
				case <-ticker.C:
					_ = promoAlertSvc.RunAndPush(promoAlertCtx, agent.NewWecomSender(wecomClient))
				}
			}
		}()
		log.Printf("[main] W3.3 堆头费到期预警: 启动时跑一次 + 每日 21:00 (chat_id=%s)", os.Getenv("PROMOTION_ALERT_CHAT_ID"))
	} else {
		log.Printf("[main] PROMOTION_ALERT_CHAT_ID 未配置, 堆头费到期预警禁用")
	}

	// W4.3: 供应商结算 cron (4 任务, 独立开关)
	supplierPayCtx, supplierPayCancel := context.WithCancel(context.Background())
	defer supplierPayCancel()
	if strings.TrimSpace(os.Getenv("OWNER_CHAT_ID")) != "" {
		go runSupplierPayCron(supplierPayCtx, supplierPaySvc, wecomClient, agent.NewWecomSender(wecomClient))
		log.Printf("[main] W4.3 供应商结算 cron 启动 (owner=%s)", os.Getenv("OWNER_CHAT_ID"))
	} else {
		log.Printf("[main] OWNER_CHAT_ID 未配置, 供应商结算 cron 禁用 (但 weekly/monthly 仍会写库, 只不发群)")
		// 仍然写库(forecast/suggestion/share), 只是不推群
		go runSupplierPayCronNoPush(supplierPayCtx, supplierPaySvc)
	}

	// restock cron(独立开关: 没门店不跑)
	if cfg.RestockBranchNo != "" {
		// 启动时一次性加载 items cube 字典 (item_no → clsno / clsname / unit)
		// 失败不阻断,前端 cls/clsname/unit 字段会空
		dictCtx, dictCancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := restockSvc.LoadItemDict(dictCtx); err != nil {
			log.Printf("[main] restock LoadItemDict: %v (前端 cls/unit 字段会空, 不阻断)", err)
		}
		dictCancel()
		if err := restockSvc.Start(); err != nil {
			log.Printf("[main] restock cron start failed: %v (继续运行,restock 不可用)", err)
		}
	} else {
		log.Printf("[main] RESTOCK_BRANCH_NO 未配置,restock cron 不启动")
	}
	defer restockSvc.Stop()

	// 企微长连接(独立开关: 任何时候都能起,用于测试/绑定 chat)
	if cfg.WeComBotID != "" {
		if err := wecomClient.Start(context.Background()); err != nil {
			log.Printf("[main] wecom ws start failed: %v", err)
		} else {
			log.Printf("[main] wecom ws connecting... (bot_id=%s)", cfg.WeComBotID)
		}
	} else {
		log.Printf("[main] WECOM_BOT_ID 未配置,企微长连接不启动")
	}
	defer wecomClient.Stop()

	rbacStore := rbac.NewStore(pool)

	// 2026-08-31: WECOM JS-SDK 签名 (H5 自建应用调原生扫码用)
	wxSvc := wxsign.New(cfg.WeComCorpID, cfg.WeComAgentID, cfg.WeComCorpSecret)
	log.Printf("[main] wxsign: configured=%v (corp_id=%q agent_id=%q)", wxSvc.IsConfigured(), cfg.WeComCorpID, cfg.WeComAgentID)

	r := api.NewRouter(h, cfg, restockSvc, authSvc, authSign, rbacStore, wxSvc)
	log.Printf("[main] 限流: max_concurrent_parse=%d, wait_sec=%d", cfg.MaxConcurrentParse, cfg.RateLimitWaitSec)
	log.Printf("[main] auth: dev_mode=%v, cookie_domain=%s, cookie_secure=%v, access_ttl=%ds, refresh_ttl=%ds",
		cfg.DevMode, cfg.CookieDomain, cfg.CookieSecure, cfg.AccessTokenTTLSec, cfg.RefreshTokenTTLSec)

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: r,
		// 2026-09-04 修复: 之前 srv 零 timeout,导致慢客户端 / 慢网络(企微浏览器
		//   / 中间代理)能挂住服务端 socket 60s+ 不释放,既不优雅也不安全。
		//   ReadHeaderTimeout: 防 slowloris (header 都不发完就占着 socket)
		//   ReadTimeout:        整个 request body 上传的最大允许时长
		//   WriteTimeout:       响应写出去的最大允许时长(注意: SSE / 长连接要单独覆盖)
		//   IdleTimeout:        keep-alive 空闲超时
		//   handlers 内部已经用 WithoutCancel 兜底关键写库,这里只防资源泄漏
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       90 * time.Second, // > 企微 60s,给客户端 buffer 余量
		WriteTimeout:      30 * time.Second, // 客户端收响应最长 30s
		IdleTimeout:       120 * time.Second,
	}

	// 优雅关停
	go func() {
		log.Printf("[main] listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("[main] shutting down...")
	shutdownCtx, c2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer c2()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	// restockSvc.Stop() / wecomClient.Stop() 通过 line 134/146 的 defer 自动调用
}

// splitIDs 按逗号/空格/分号/换行分隔 ID 列表,trim + 去重
func splitIDs(s string) []string {
	seen := make(map[string]struct{})
	out := []string{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\r' || r == '\t'
	}) {
		v := strings.TrimSpace(f)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// buildCubeQuerier W5 cube 数据源
//
//	env COLLECTAI_CUBE_QUERIER:
//	  "" / "noop" (默认) → NoopCubeQuerier (返回固定占位值, devMode 友好)
//	  "real"            → RealCubeQuerier 包装 business.CubeClient (Gateway 内部用同一个 client)
//	真实接入需在 cube-agent-server 端有 siss_saleflow / v_prom_saleflow cube 定义
//	字段名见 internal/supplierpayment/cube.go RealCubeQuerier 默认值
//
// 2026-09-02 重构: agentClient → gateway.Client() (统一 CubeClient interface)
func buildCubeQuerier(client business.CubeClient) supplierpayment.CubeQuerier {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("COLLECTAI_CUBE_QUERIER")))
	switch mode {
	case "real":
		if client == nil {
			log.Printf("[main] W5 cube: 模式 real 但 client nil, 降级 Noop")
			return supplierpayment.NewNoopCubeQuerier()
		}
		return supplierpayment.NewRealCubeQuerier(client)
	default:
		return supplierpayment.NewNoopCubeQuerier()
	}
}

func cubeMode() string {
	m := strings.ToLower(strings.TrimSpace(os.Getenv("COLLECTAI_CUBE_QUERIER")))
	if m == "" {
		return "noop"
	}
	return m
}

// buildPurchaseAlertCubeFns W4.4 (2026-09-04) 构造 purchase-alert 6 tool 的 cube Fn 注入
//
//	走 business.Executor.SearchReturnsBySupplier (e.query() 私有方法, 走 Registry 翻译)
//	严禁直接 import parser/agent / 严禁暴露物理 cube 字段名 (AGENTS.md §12.3 红线)
//
//	当前 (W4.4 第一阶段):
//	  - QueryReturnOrderFn = 真实现 (规则 8 pending_return 激活, 走 returns mapping)
//	  - QuerySkuStockFn = nil (high_stock 规则 cube 路径走降级, 暂未配 stock cube mapping)
//	  - QuerySkuSalesFn = nil (low_movement 规则 cube 路径走降级, 暂未配 sales cube mapping)
//
//	未来补 stock_qty / sales mapping 时, 在这里加 Fn 实现即可, NewRunner 签名不变
func buildPurchaseAlertCubeFns(bizExec *business.Executor, gateway *business.Gateway) agent.CubeToolFns {
	return agent.CubeToolFns{
		// QuerySkuStockFn / QuerySkuSalesFn 暂留 nil, 对应 tool 内部降级
		// 未来 cube mapping 配齐后, 在这里构造 Fn 闭包:
		//   QuerySkuStockFn: func(ctx, itemNo, barcode) (...) {
		//       rows, _ := bizExec.Query("products", []string{"barcode","product_name","stock_qty","clsno","clsname"},
		//           []business.BusinessFilter{{Field: "barcode", Op: "equals", Values: []any{barcode}}}, 1)
		//       ...
		//   }

		// QueryReturnOrderFn: 真实现 (W4.4 规则 8 走 returns cube, 走 business.Executor.SearchReturnsBySupplier)
		//   mapping ValueMap 把 status="pending" 业务值翻成 approve_flag="0" 物理值
		//   days 由 query 端根据 create_date 二次过滤 (cube SQL 自带近 1 年过滤, 实际够用)
		QueryReturnOrderFn: func(ctx context.Context, supplier, status string, days int) ([]business.ReturnOrder, string, error) {
			orders, err := bizExec.SearchReturnsBySupplier(supplier, status, days, 100)
			if err != nil {
				return nil, "", fmt.Errorf("SearchReturnsBySupplier: %w", err)
			}
			// 二次过滤: create_date 距今 <= days (cube SQL 自带近 1 年, 业务上要更严格的窗口)
			//   简化: 这里只把 list 透传, days 在 Executor.SearchReturnsBySupplier 已说明是"受限 cube 自带 1 年过滤"
			//   真要 days 过滤, Executor.SearchReturnsBySupplier 加 timeDimensions 参数
			return orders, "", nil
		},
	}
}

// buildSeasonClassifier W3.5 季节判定分类器链
//
//	关键词 (W3.2 OffseasonRule 已有表) → LLM (GLM-4-flash) → 6h 缓存
//	LLM 不可用 / 没 key → 返回 nil, Service 跳过 LLMSeasonRule,等同 W3.2
func buildSeasonClassifier(llmClient *bigmodel.LlmClient) purchasealert.SeasonClassifier {
	keyword := purchasealert.NewKeywordSeasonClassifier(nil)
	if llmClient == nil {
		log.Printf("[main] Season classifier: keyword-only (LLM client nil)")
		return keyword
	}
	llm := purchasealert.NewBigModelSeasonClassifier(llmClient, "glm-4-flash", nil)
	cached := purchasealert.NewCachingSeasonClassifier(llm, 6*60*60*1e9, 1000, nil) // 6h
	chained := purchasealert.NewChainedSeasonClassifier(keyword, cached)
	log.Printf("[main] Season classifier: keyword + LLM (cached 6h/1000)")
	return chained
}

// runSupplierPayCron W4.3 启动 4 个 cron 任务
//
//	启动时立即跑 forecast + weekly
//	每日 21:00 跑 forecast
//	每周一 09:00 跑 weekly
//	每月 1 号 02:00 跑 monthly share
//	每日 22:00 跑 cash check
func runSupplierPayCron(ctx context.Context, svc *supplierpayment.Service, _ *wecom.Client, sender agent.Sender) {
	// 启动立即跑
	if _, err := svc.RunDailyForecast(ctx); err != nil {
		log.Printf("[supplierpay.DailyForecast] err: %v", err)
	}
	if _, err := svc.RunWeeklySuggestions(ctx); err != nil {
		log.Printf("[supplierpay.WeeklySuggestions] err: %v", err)
	}

	// 4 个 ticker 共享 24h 大循环 + 各自对齐
	runDaily(ctx, func(ctx context.Context) {
		if _, err := svc.RunDailyForecast(ctx); err != nil {
			log.Printf("[supplierpay.DailyForecast] err: %v", err)
		}
	}, 21, 0) // 每日 21:00

	runWeekly(ctx, func(ctx context.Context) {
		if _, err := svc.RunWeeklySuggestions(ctx); err != nil {
			log.Printf("[supplierpay.WeeklySuggestions] err: %v", err)
		}
	}, time.Monday, 9, 0) // 周一 09:00

	runMonthly(ctx, func(ctx context.Context) {
		if _, err := svc.RunMonthlyShare(ctx); err != nil {
			log.Printf("[supplierpay.MonthlyShare] err: %v", err)
		}
	}, 1, 2, 0) // 每月 1 号 02:00

	runDaily(ctx, func(ctx context.Context) {
		if _, err := svc.RunDailyCashCheck(ctx, sender); err != nil {
			log.Printf("[supplierpay.DailyCashCheck] err: %v", err)
		}
	}, 22, 0) // 每日 22:00
}

// runSupplierPayCronNoPush 同上但 cash check 不推群
func runSupplierPayCronNoPush(ctx context.Context, svc *supplierpayment.Service) {
	if _, err := svc.RunDailyForecast(ctx); err != nil {
		log.Printf("[supplierpay] err: %v", err)
	}
	if _, err := svc.RunWeeklySuggestions(ctx); err != nil {
		log.Printf("[supplierpay] err: %v", err)
	}
	runDaily(ctx, func(ctx context.Context) {
		_, _ = svc.RunDailyForecast(ctx)
	}, 21, 0)
	runWeekly(ctx, func(ctx context.Context) {
		_, _ = svc.RunWeeklySuggestions(ctx)
	}, time.Monday, 9, 0)
	runMonthly(ctx, func(ctx context.Context) {
		_, _ = svc.RunMonthlyShare(ctx)
	}, 1, 2, 0)
	runDaily(ctx, func(ctx context.Context) {
		_, _ = svc.RunDailyCashCheck(ctx, nil) // sender nil = 不推
	}, 22, 0)
}

// runDaily 每日定时 HH:MM (本地时间)
func runDaily(ctx context.Context, fn func(context.Context), hour, minute int) {
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	wait := time.Until(next)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	for {
		fn(ctx)
		timer.Reset(24 * time.Hour)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

// runWeekly 每周周几 HH:MM
func runWeekly(ctx context.Context, fn func(context.Context), weekday time.Weekday, hour, minute int) {
	now := time.Now()
	daysUntil := int(weekday - now.Weekday())
	if daysUntil < 0 || (daysUntil == 0 && (now.Hour() > hour || (now.Hour() == hour && now.Minute() >= minute))) {
		daysUntil += 7
	}
	next := time.Date(now.Year(), now.Month(), now.Day()+daysUntil, hour, minute, 0, 0, now.Location())
	wait := time.Until(next)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	for {
		fn(ctx)
		// 下周同时间
		next = next.Add(7 * 24 * time.Hour)
		wait = time.Until(next)
		if wait < 0 {
			wait = 0
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

// runMonthly 每月第几天 HH:MM
func runMonthly(ctx context.Context, fn func(context.Context), day, hour, minute int) {
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), day, hour, minute, 0, 0, now.Location())
	if !next.After(now) {
		next = time.Date(now.Year(), now.Month()+1, day, hour, minute, 0, 0, now.Location())
	}
	wait := time.Until(next)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	for {
		fn(ctx)
		next = time.Date(next.Year(), next.Month()+1, day, hour, minute, 0, 0, next.Location())
		wait = time.Until(next)
		if wait < 0 {
			wait = 0
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

// skillStoreAdapter 把 *skill.Store 适配成 purchasealert.SkillLoader
//
//	purchasealert 不直接 import skill 包 (避免循环), 通过 interface 注入
type skillStoreAdapter struct {
	store *skill.Store
}

func (a *skillStoreAdapter) GetBody(name string) (string, bool) {
	sk, ok := a.store.Get(name)
	if !ok {
		return "", false
	}
	return sk.Body, true
}

// maskAPIKey 脱敏输出 API key (用于日志, 只显示前后 4 字符)
func maskAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= 8 {
		return "***"
	}
	return key[:4] + "***" + key[len(key)-4:]
}
