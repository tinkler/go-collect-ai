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
	"github.com/tinkler/collect-ai/internal/rbac"
	"github.com/tinkler/collect-ai/internal/restock"
	"github.com/tinkler/collect-ai/internal/wxsign"
)

// NewRouter 构造 router
//   authSvc: 鉴权 service, 含 store + signer + wecom
//
// Gin 路由中间件顺序约定: 中间件在前, handler 在最后
//   r.GET("/x", AuthMiddleware(), RequirePerm("p"), handler)
//   → AuthMiddleware → RequirePerm → handler
func NewRouter(h *handler.Handler, cfg *config.Config, restockSvc *restock.Service, authSvc *auth.Service, authSign *auth.Signer, rbacStore *rbac.Store, wxSvc *wxsign.Service) *gin.Engine {
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

		// WECOM JS-SDK 签名 (公开, 走企业微信 H5 自建应用调用)
		//   dev 模式: 返 503 + reason=dev_mode, 前端 fallback 手动输入
		//   生产: 需配 WECOM_CORP_ID / WECOM_AGENT_ID / WECOM_CORP_SECRET
		api.GET("/wx/sign", wxsign.SignConfigHandler(wxSvc))
		api.GET("/wx/agent-sign", wxsign.SignAgentHandler(wxSvc))
		api.GET("/wx/status", wxsign.StatusHandler(wxSvc))

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
			// 2026-08-31: 用户最后访问页 (登录后自动跳回)
			authed.GET("/auth/last-page", authH.GetLastPage)
			authed.POST("/auth/last-page", authH.SetLastPage)

			// suppliers
			authed.GET("/suppliers", auth.RequirePerm("session:read"), h.ListSuppliers)
			authed.GET("/suppliers/by-brand", auth.RequirePerm("session:read"), h.ListSuppliersByBrand)

			// Phase A (2026-09-02): supplier_parse_strategy 端点 (取代旧 /templates)
			//   - 查:     GET  /suppliers/:name/strategy
			//   - 改:     PUT  /suppliers/:name/strategy
			//   - 优化:   POST /suppliers/:name/strategy/optimize (Phase A 占位, Phase B 接 LLM)
			authed.GET("/suppliers/:name/strategy", auth.RequirePerm("session:read"), h.GetStrategy)
			authed.PUT("/suppliers/:name/strategy", auth.RequirePerm("session:update"), h.UpsertStrategy)
			authed.POST("/suppliers/:name/strategy/optimize", auth.RequirePerm("admin"), h.OptimizeStrategy)

			// W4: 现金日报 + 供应商结算建议
			authed.POST("/cash/balance", auth.RequirePerm("cash:write"), h.SetCashBalance)
			authed.GET("/cash/balance", auth.RequirePerm("cash:read"), h.GetCashBalance)
			authed.GET("/payments/pending", auth.RequirePerm("payment:read"), h.ListPendingPayments)

			// W2.5: H5 端触发 Agent 跑一轮
			authed.POST("/agent/chat", auth.RequirePerm("agent:write"), h.AgentChat)

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
			// W4.1: 追加图片到已有 session (重复图去重 + 续接 seq)
			authed.POST("/sessions/:id/images", limiter.Middleware(wait), auth.RequirePerm("session:update"), h.AppendImages)
			// W4.1: 轻量状态查询 (前端轮询用, 不拉 rows)
			authed.GET("/sessions/:id/analysis-status", auth.RequirePerm("session:read"), h.GetAnalysisStatus)

			// 采购计划
			authed.GET("/purchase-plans", auth.RequirePerm("plan:read"), restock.RestockPurchasePlansList(restockSvc))

			// 业务层 API(2026-08-31: /datasources 端点删除,数据源启动后即固定)
			authed.GET("/products/search", auth.RequirePerm("session:read"), h.SearchProducts)

			// restock (2026-09-02 重构精简)
			//   4 个端点, 不分 date / office / floor, 不推群
			//     GET  /restock/tasks            任务列表 (H5 主页)
			//     POST /restock/feedback         员工反馈 (DONE/SHORT, 按 kind perm 分)
			//     GET  /restock/purchase-plans   采购计划单 (采购看)
			//     POST /restock/cron/tick        手动触发 (admin)
			authed.GET("/restock/tasks", auth.RequirePerm("plan:read"), restock.RestockTasksList(restockSvc))
			authed.POST("/restock/feedback", restock.RestockFeedback(restockSvc))
			authed.GET("/restock/purchase-plans", auth.RequirePerm("plan:read"), restock.RestockPurchasePlansList(restockSvc))
			authed.POST("/restock/cron/tick", auth.RequirePerm("admin"), restock.RestockManualTick(restockSvc))

			// ============== Admin 权限管理 (2026-08-30) ==============
			rbacH := rbac.NewHandler(rbacStore)
			admin := authed.Group("/admin", auth.RequirePerm("user:manage"))
			{
				admin.GET("/stats", rbacH.Stats)
				admin.GET("/users", rbacH.ListUsers)
				admin.GET("/users/:id", rbacH.GetUser)
				admin.PUT("/users/:id", rbacH.UpdateUser)
				admin.DELETE("/users/:id", rbacH.MarkLeft)
				admin.POST("/users/:id/restore", rbacH.RestoreUser)

				admin.POST("/user-roles", rbacH.GrantRole)
				admin.DELETE("/user-roles", rbacH.RevokeRole)

				admin.GET("/roles", rbacH.ListRoles)
				admin.GET("/roles/:id", rbacH.GetRole)
				admin.POST("/roles", rbacH.CreateRole)
				admin.PUT("/roles/:id", rbacH.UpdateRole)
				admin.DELETE("/roles/:id", rbacH.DeleteRole)

				admin.GET("/permissions", rbacH.ListPermissions)
				admin.GET("/departments", rbacH.ListDepartments)
				admin.GET("/audit", rbacH.ListAudit)
			}
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
