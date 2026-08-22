package api

import (
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tinkler/collect-ai/internal/api/handler"
	"github.com/tinkler/collect-ai/internal/api/middleware"
	"github.com/tinkler/collect-ai/internal/config"
)

func NewRouter(h *handler.Handler, cfg *config.Config) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery(), gin.Logger())

	// CORS (飞书 H5 跨域)
	r.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type,Authorization")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	// 并发限流 (semaphore-based, 阻塞 30s 后 503)
	// 仅对解析类接口 (/parse /rematch /sessions) 限流, 不限管理类
	limiter := middleware.NewSemaphoreLimiter(cfg.MaxConcurrentParse)
	wait := time.Duration(cfg.RateLimitWaitSec) * time.Second

	// 静态 (uploads)
	abs, _ := filepath.Abs(h.UploadDir)
	r.Static("/uploads", abs)

	api := r.Group("/api/v1")
	{
		api.GET("/health", h.Health)

		// suppliers
		api.GET("/suppliers", h.ListSuppliers)
		api.GET("/suppliers/by-brand", h.ListSuppliersByBrand)

		// templates
		api.GET("/templates", h.ListTemplates)         // 飞书: 只看 purchase + default
		api.GET("/templates/all", h.ListAllTemplates)  // C# 管理
		api.POST("/templates/sync", h.SyncTemplates)   // C# 整体同步

		// parse (受限流保护)
		parseGroup := api.Group("", limiter.Middleware(wait))
		parseGroup.POST("/parse", h.Parse)                       // multipart file, 不存库
		parseGroup.POST("/rematch", h.Rematch)                   // 用现有 rows + 新 supplier 重新跑 SkuMatcher
		parseGroup.POST("/sessions", h.CreateSession)           // multipart, 存库 (含解析)

		// 健康检查 + 限流 stats (不限流)
		api.GET("/ratelimit/stats", func(c *gin.Context) {
			active, max, wait, block := limiter.Stats()
			c.JSON(200, gin.H{"active": active, "max": max, "total_wait": wait, "total_block": block})
		})

		// sessions (只读 + 改/删行 不限流, 因为不调 OCR/LLM)
		api.GET("/sessions", h.ListSessions)
		api.GET("/sessions/:id", h.GetSession)
		api.DELETE("/sessions/:id", h.DeleteSession)
		api.GET("/sessions/:id/export", h.ExportSession)
		api.PUT("/sessions/:id/rows/:rowId", h.UpdateRow)
		api.DELETE("/sessions/:id/rows/:rowId", h.DeleteRow)

		// 数据源切换(unified cube)
		//   GET  /datasource              → 当前数据源
		//   POST /datasource {name:"..."} → 切换数据源
		api.GET("/datasource", h.GetDataSource)
		api.POST("/datasource", h.SetDataSource)

		// 业务层 API
		//   GET  /datasources                   → 列出所有 (entity, datasource) 组合
		//   GET  /products/search?datasource=&supplier=&limit= → 业务字段名商品搜索
		api.GET("/datasources", h.ListDatasources)
		api.GET("/products/search", h.SearchProducts)
	}

	r.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"service": "collect-ai",
			"version": "0.1.0",
			"docs":    fmt.Sprintf("see /api/v1/health"),
		})
	})
	return r
}
