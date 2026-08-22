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
	r := api.NewRouter(h, cfg)
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
}
