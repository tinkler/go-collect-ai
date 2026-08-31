package wxsign

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// SignConfigHandler GET /api/v1/wx/sign?url=...
//   返回: { corpid, timestamp, nonceStr, signature }
//   公开 (不加 AuthMiddleware), 域名/IP 在 nginx 那一层做白名单
func SignConfigHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawURL := c.Query("url")
		if rawURL == "" {
			c.JSON(400, gin.H{"code": "BAD_REQUEST", "message": "url query 必填"})
			return
		}
		corpid, ts, nonce, sig, err := svc.SignConfig(c.Request.Context(), rawURL)
		if err != nil {
			// dev mode: 没配企微, 给前端 503 + 明确 reason, 前端走手动输入 fallback
			c.JSON(503, gin.H{
				"code":    "WECOM_NOT_CONFIGURED",
				"message": err.Error(),
				"reason":  "dev_mode",
			})
			return
		}
		c.JSON(200, gin.H{
			"corpid":    corpid,
			"timestamp": ts,
			"nonceStr":  nonce,
			"signature": sig,
		})
	}
}

// SignAgentHandler GET /api/v1/wx/agent-sign?url=...
//   返回: { corpid, agentid, timestamp, nonceStr, signature }
//   公开, 供前端调 agentConfig 用
func SignAgentHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawURL := c.Query("url")
		if rawURL == "" {
			c.JSON(400, gin.H{"code": "BAD_REQUEST", "message": "url query 必填"})
			return
		}
		corpid, agentid, ts, nonce, sig, err := svc.SignAgent(c.Request.Context(), rawURL)
		if err != nil {
			c.JSON(503, gin.H{
				"code":    "WECOM_NOT_CONFIGURED",
				"message": err.Error(),
				"reason":  "dev_mode",
			})
			return
		}
		c.JSON(200, gin.H{
			"corpid":    corpid,
			"agentid":   agentid,
			"timestamp": ts,
			"nonceStr":  nonce,
			"signature": sig,
		})
	}
}

// StatusHandler GET /api/v1/wx/status — 前端用来检测是否在企业微信环境 + 是否配好凭证
func StatusHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"configured": svc.IsConfigured(),
		})
	}
}
