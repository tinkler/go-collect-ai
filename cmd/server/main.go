package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tinkler/collect-ai/internal/api"
	"github.com/tinkler/collect-ai/internal/api/handler"
	"github.com/tinkler/collect-ai/internal/business"
	"github.com/tinkler/collect-ai/internal/config"
	"github.com/tinkler/collect-ai/internal/parser"
	"github.com/tinkler/collect-ai/internal/parser/agent"
	"github.com/tinkler/collect-ai/internal/parser/bigmodel"
	"github.com/tinkler/collect-ai/internal/restock"
	"github.com/tinkler/collect-ai/internal/store"
	"github.com/gin-gonic/gin"
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
	templateRepo := store.NewTemplateRepo(pool)

	ocrClient := bigmodel.NewOcrClient(cfg.BigModelAPIKey, cfg.BigModelBase, cfg.OcrTimeoutSec)
	llmClient := bigmodel.NewLlmClient(cfg.BigModelAPIKey, cfg.BigModelBase, cfg.LlmTimeoutSec)
	agentClient := agent.NewClient(cfg.AgentURL, cfg.AgentToken, 30, cfg.DataSource)
	businessReg := business.NewDefaultRegistry()

	psr := parser.New(ocrClient, llmClient, agentClient)

	h := &handler.Handler{
		UploadDir:        cfg.UploadDir,
		PublicBase:       cfg.PublicBaseURL,
		MaxUpload:        int64(cfg.MaxUploadMB) * 1024 * 1024,
		Parser:           psr,
		Agent:            agentClient,
		BusinessReg:      businessReg,
		Sessions:         sessionRepo,
		Templates:        templateRepo,
		FuzzyDistance:    cfg.FuzzyDistance,
		DefaultOcrModel:  cfg.OCRModel,
		DefaultLlmModel:  cfg.LLMModel,
		DefaultUseLlm:    cfg.UseLlm,
		DefaultFuzzyDist: cfg.FuzzyDistance,
	}

	gin.SetMode(gin.ReleaseMode)

	// ============== restock 模块启动 ==============
	restockCfg := &restock.RestockConfig{
		BranchNo:               cfg.RestockBranchNo,
		HourlyCron:             cfg.RestockHourlyCron,
		AggregateCron:          cfg.RestockAggregateCron,
		LlmPlanCron:            cfg.RestockLlmPlanCron,
		MaxPushPerTick:         cfg.RestockMaxPushPerTick,
		ROPFactor:              cfg.RestockROPFactor,
		OUTDays:                cfg.RestockOUTDays,
		OUTPromoBoost:          cfg.RestockOUTPromoBoost,
		SafetyMin:              cfg.RestockSafetyMin,
		WYesterday:             cfg.RestockWYesterday,
		WSevenDay:              cfg.RestockWSevenDay,
		WThirtyDay:             cfg.RestockWThirtyDay,
		FloorMinIntervalMin:    cfg.RestockFloorMinIntervalMin,
		OfficeP0MinMin:         cfg.RestockOfficeP0MinMin,
		OfficeP1MinMin:         cfg.RestockOfficeP1MinMin,
		OfficeP2MinMin:         cfg.RestockOfficeP2MinMin,
		EscalateP2ToP1Hours:    cfg.RestockEscalateP2ToP1Hours,
		EscalateP1ToP0Hours:    cfg.RestockEscalateP1ToP0Hours,
		CubeSales:              cfg.RestockCubeSales,
		CubeInventory:          cfg.RestockCubeInventory,
		CubePromotion:          cfg.RestockCubePromotion,
		LLMEnabled:             cfg.RestockLLMEnabled,
		LLMPlanEnabled:         cfg.RestockLLMPlanEnabled,
		LLMModel:               cfg.RestockLLMModel,
		LLMPlanCacheHrs:        cfg.RestockLLMPlanCacheHrs,
		WeComBotID:             cfg.WeComBotID,
		WeComBotSecret:         cfg.WeComBotSecret,
		WeComWSURL:             cfg.WeComWSURL,
		WeComBindFile:          cfg.WeComBindFile,
	}
	restockCube := restock.NewCubeQuerier(agentClient, restockCfg)
	restockLLM := restock.NewLlmPlanner(llmClient, cfg.RestockLLMModel, cfg.RestockLLMPlanEnabled, cfg.RestockLLMPlanCacheHrs)
	restockWeCom := restock.NewWeCom(restockCfg)
	restockSvc := restock.NewService(restockCfg, pool, restockCube, restockLLM, restockWeCom)

	// 注册企微按钮点击回调 → 复用 Service.OnButtonClick(写 Feedback + 改状态)
	restockWeCom.OnButtonClick = restockSvc.OnButtonClick
	restockWeCom.OnMessage = func(chatID, userID, text string) {
		log.Printf("[wecom] msg from chat=%s user=%s: %s", chatID, userID, text)
	}

	// restock cron(独立开关: 没门店不跑)
	if cfg.RestockBranchNo != "" {
		if err := restockSvc.Start(); err != nil {
			log.Printf("[main] restock cron start failed: %v (继续运行,restock 不可用)", err)
		}
	} else {
		log.Printf("[main] RESTOCK_BRANCH_NO 未配置,restock cron 不启动")
	}
	defer restockSvc.Stop()

	// 企微长连接(独立开关: 任何时候都能起,用于测试/绑定 chat)
	if cfg.WeComBotID != "" {
		if err := restockWeCom.Start(context.Background()); err != nil {
			log.Printf("[main] wecom ws start failed: %v", err)
		} else {
			log.Printf("[main] wecom ws connecting... (bot_id=%s)", cfg.WeComBotID)
		}
	} else {
		log.Printf("[main] WECOM_BOT_ID 未配置,企微长连接不启动")
	}
	defer restockWeCom.Stop()

	r := api.NewRouter(h, cfg, restockSvc)
	log.Printf("[main] 限流: max_concurrent_parse=%d, wait_sec=%d", cfg.MaxConcurrentParse, cfg.RateLimitWaitSec)

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: r,
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
	// restockSvc.Stop() / restockWeCom.Stop() 通过 line 134/146 的 defer 自动调用
}
