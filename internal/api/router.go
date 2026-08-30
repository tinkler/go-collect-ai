package api

import (
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tinkler/collect-ai/internal/api/handler"
	"github.com/tinkler/collect-ai/internal/api/middleware"
	"github.com/tinkler/collect-ai/internal/auth"
	"github.com/tinkler/collect-ai/internal/config"
	"github.com/tinkler/collect-ai/internal/restock"
)

// NewRouter 构造 router
//   authSvc: 鉴权 service, 含 store + signer + wecom
//
// Gin 路由中间件顺序约定: 中间件在前, handler 在最后
//   r.GET("/x", AuthMiddleware(), RequirePerm("p"), handler)
//   → AuthMiddleware → RequirePerm → handler
func NewRouter(h *handler.Handler, cfg *config.Config, restockSvc *restock.Service, authSvc *auth.Service, authSign *auth.Signer) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery(), gin.Logger())

	// CORS (飞书 H5 / 企微 H5 跨域)
	//   2026-08-28: 反射 Origin 头, 兼容 credentials=include
	r.Use(func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		if origin == "" {
			origin = "*"
		}
		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type,Authorization,X-Requested-With")
		c.Header("Access-Control-Allow-Credentials", "true")
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

	// 鉴权 handler
	authH := auth.NewHandler(authSvc, authSign, cfg.CookieDomain, cfg.CookieSecure)

	// ============== /api/v1 ==============
	api := r.Group("/api/v1")
	{
		// health (公开)
		api.GET("/health", h.Health)

		// ============== 鉴权公开路由 ==============
		api.POST("/auth/wecom/callback", authH.WeComCallback)
		if cfg.DevMode {
			api.POST("/auth/dev-login", authH.DevLogin)
		}
		api.POST("/auth/refresh", authH.Refresh)

		// ratelimit stats (公开, 监控用)
		api.GET("/ratelimit/stats", func(c *gin.Context) {
			active, max, wait, block := limiter.Stats()
			c.JSON(200, gin.H{"active": active, "max": max, "total_wait": wait, "total_block": block})
		})

		// ============== 已登录路由 (AuthMiddleware) ==============
		// 重要: gin 顺序: 中间件在前, handler 在最后
		authed := api.Group("", auth.AuthMiddleware(authSvc, authSign))
		{
			// /auth/* (已登录部分)
			authed.POST("/auth/logout", authH.Logout)
			authed.GET("/auth/me", authH.Me)

			// suppliers
			authed.GET("/suppliers", auth.RequirePerm("session:read"), h.ListSuppliers)
			authed.GET("/suppliers/by-brand", auth.RequirePerm("session:read"), h.ListSuppliersByBrand)

			// templates
			authed.GET("/templates", auth.RequirePerm("session:read"), h.ListTemplates)
			authed.GET("/templates/all", auth.RequirePerm("session:read"), h.ListAllTemplates)
			authed.POST("/templates/sync", auth.RequirePerm("admin"), h.SyncTemplates)

			// parse (受限流保护)
			authed.POST("/parse", limiter.Middleware(wait), auth.RequirePerm("session:create"), h.Parse)
			authed.POST("/rematch", limiter.Middleware(wait), auth.RequirePerm("session:update"), h.Rematch)
			authed.POST("/sessions", limiter.Middleware(wait), auth.RequirePerm("session:create"), h.CreateSession)

			// sessions
			authed.GET("/sessions", auth.RequirePerm("session:read"), h.ListSessions)
			authed.GET("/sessions/:id", auth.RequirePerm("session:read"), h.GetSession)
			authed.DELETE("/sessions/:id", auth.RequirePerm("session:delete"), h.DeleteSession)
			authed.GET("/sessions/:id/export", auth.RequirePerm("session:read"), h.ExportSession)
			authed.PUT("/sessions/:id/rows/:rowId", auth.RequirePerm("row:update"), h.UpdateRow)
			authed.DELETE("/sessions/:id/rows/:rowId", auth.RequirePerm("row:delete"), h.DeleteRow)

			// 采购计划
			authed.GET("/purchase-plans", auth.RequirePerm("plan:read"), restock.PurchasePlansList(restockSvc))

			// 数据源切换 (admin)
			authed.GET("/datasource", auth.RequirePerm("admin"), h.GetDataSource)
			authed.POST("/datasource", auth.RequirePerm("admin"), h.SetDataSource)

			// 业务层 API
			authed.GET("/datasources", auth.RequirePerm("session:read"), h.ListDatasources)
			authed.GET("/products/search", auth.RequirePerm("session:read"), h.SearchProducts)

			// restock
			authed.GET("/restock/tasks", auth.RequirePerm("plan:read"), restock.RestockTasksList(restockSvc))
			authed.GET("/restock/need-purchase", auth.RequirePerm("plan:read"), restock.RestockNeedPurchaseList(restockSvc))
			authed.POST("/restock/cron/tick", auth.RequirePerm("admin"), restock.RestockManualTick(restockSvc))
			authed.GET("/restock/llm/plan", auth.RequirePerm("plan:read"), restock.RestockLlmPlanNow(restockSvc))

			// H5 视图 (2026-08-30): 替代企微群 button 交互, 按 user.group 决定返回粒度
			authed.GET("/restock/h5/tasks", auth.RequirePerm("plan:read"), restock.RestockH5TasksList(restockSvc))
			authed.POST("/restock/h5/tasks/:task_id/feedback", auth.RequirePerm("plan:read"), restock.RestockH5Feedback(restockSvc))
			authed.GET("/restock/h5/categories", auth.RequirePerm("plan:read"), restock.RestockH5Categories(restockSvc))
			authed.GET("/restock/h5/cls-map", auth.RequirePerm("plan:read"), restock.RestockH5ClsMap(restockSvc))
			authed.GET("/restock/h5/purchase-plans", auth.RequirePerm("plan:read"), restock.RestockH5PurchasePlans(restockSvc))

			// wecom chat 管理 (admin)
			authed.GET("/restock/wecom/chats", auth.RequirePerm("admin"), restock.RestockListChats(restockSvc))
			authed.POST("/restock/wecom/chats/bind", auth.RequirePerm("admin"), restock.RestockBindChat(restockSvc))
			authed.POST("/restock/wecom/chats/bulk-bind", auth.RequirePerm("admin"), restock.RestockBulkBindChat(restockSvc))
			authed.POST("/restock/wecom/chats/test", auth.RequirePerm("admin"), restock.RestockTestChat(restockSvc))
		}
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
