// Package wecomchat HTTP admin API
//
// 端点 (全部走 user:manage perm, 在 router.go 注册):
//
//   GET    /api/v1/admin/wecom/chats             列出所有绑定 (可 ?purpose=xxx 过滤)
//   GET    /api/v1/admin/wecom/chats/:chat_id    查单条
//   PUT    /api/v1/admin/wecom/chats/:chat_id    upsert (body: {purpose,label,enabled,note})
//   DELETE /api/v1/admin/wecom/chats/:chat_id    删
//   GET    /api/v1/admin/wecom/discovered        已发现但未配置 (从 wecom 客户端读 + PG 合并)
//   GET    /api/v1/admin/wecom/status            连接状态 + 汇总
//   POST   /api/v1/admin/wecom/reload            强制重载 cache
//   GET    /api/v1/admin/wecom/purposes          purpose 白名单 + 描述 (前端下拉框用)
package wecomchat

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// ChatLister 抽象从 wecom.Client 读已发现 chat
//   - 实现: wecom.Client.DiscoveredChats()  ([]ChatBinding)
//   - 这样 wecomchat 包不必 import wecom 包 (反向依赖)
type ChatLister interface {
	DiscoveredChats() []DiscoveredWeComChat
}

// DiscoveredWeComChat wecom 客户端返回的发现项 (避免直接 import wecom.ChatBinding 类型)
type DiscoveredWeComChat struct {
	ChatID    string
	FirstSeen time.Time
}

// ConnStatus 抽象 wecom.Client 连接状态查询
type ConnStatus interface {
	Connected() bool
	BotID() string
}

// AdminHandler HTTP handler
type AdminHandler struct {
	Store   *Store
	Router  *ChatRouter
	Lister  ChatLister
	Conn    ConnStatus
}

// NewAdminHandler 构造
func NewAdminHandler(store *Store, router *ChatRouter, lister ChatLister, conn ConnStatus) *AdminHandler {
	return &AdminHandler{Store: store, Router: router, Lister: lister, Conn: conn}
}

// operatorFromCtx 拿操作用户 (跟 freshcheck / rbac 一致)
func operatorFromCtx(c *gin.Context) string {
	if v, ok := c.Get("user_id"); ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	if v, ok := c.Get("username"); ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return "system"
}

// ============== 1. List Bindings ==============

// ListChats GET /admin/wecom/chats?purpose=xxx
func (h *AdminHandler) ListChats(c *gin.Context) {
	purpose := c.Query("purpose")
	if purpose != "" && !AllowedPurposes[purpose] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "purpose 不在白名单: " + purpose})
		return
	}
	bs, err := h.Store.ListBindings(c.Request.Context(), purpose)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": bs, "count": len(bs), "purpose": purpose})
}

// GetChat GET /admin/wecom/chats/:chat_id
func (h *AdminHandler) GetChat(c *gin.Context) {
	chatID := c.Param("chat_id")
	b, err := h.Store.GetBinding(c.Request.Context(), chatID)
	if err == ErrNotFound {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, b)
}

// UpsertChat PUT /admin/wecom/chats/:chat_id
//   body: {"purpose":"agent", "label":"...", "enabled":true, "note":"..."}
func (h *AdminHandler) UpsertChat(c *gin.Context) {
	chatID := c.Param("chat_id")
	var req struct {
		Purpose string `json:"purpose"`
		Label   string `json:"label"`
		Enabled *bool  `json:"enabled"`
		Note    string `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	if req.Purpose == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "purpose 必填"})
		return
	}
	if !AllowedPurposes[req.Purpose] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "purpose 不在白名单 (允许: agent/fee/promo_alert/owner/office/floor/log/other)"})
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	b := &ChatBinding{
		ChatID:  chatID,
		Purpose: req.Purpose,
		Label:   req.Label,
		Enabled: enabled,
		Note:    req.Note,
	}
	operator := operatorFromCtx(c)
	if err := h.Store.UpsertBinding(c.Request.Context(), b, operator); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 立即重载 cache, 不必等下次 reload
	if err := h.Router.Reload(c.Request.Context()); err != nil {
		// 写库成功但 cache 失败 — 不算严重, 下次手动 reload
		c.JSON(http.StatusOK, gin.H{"ok": true, "chat_id": chatID, "warning": "cache reload failed: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "chat_id": chatID, "purpose": req.Purpose, "enabled": enabled})
}

// DeleteChat DELETE /admin/wecom/chats/:chat_id
func (h *AdminHandler) DeleteChat(c *gin.Context) {
	chatID := c.Param("chat_id")
	if err := h.Store.DeleteBinding(c.Request.Context(), chatID); err != nil {
		if err == ErrNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	_ = h.Router.Reload(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"ok": true, "chat_id": chatID})
}

// ============== 2. Discovered (auto-discover) ==============

// ListDiscovered GET /admin/wecom/discovered
//   合并 wecom client 自动发现 + PG 已配, 标记 bound / unbound
func (h *AdminHandler) ListDiscovered(c *gin.Context) {
	// 1) 从 wecom client 读 (在内存 + YAML)
	fromClient := []DiscoveredChat{}
	if h.Lister != nil {
		for _, d := range h.Lister.DiscoveredChats() {
			fromClient = append(fromClient, DiscoveredChat{
				ChatID:    d.ChatID,
				FirstSeen: d.FirstSeen,
			})
		}
	}

	// 2) 从 PG 读所有 binding (含未启用的, 给管理端看全貌)
	all, err := h.Store.ListBindings(c.Request.Context(), "")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	boundByID := make(map[string]*ChatBinding, len(all))
	for _, b := range all {
		boundByID[b.ChatID] = b
	}

	// 3) 合并: client 有 + pg 有 → 标 bound; client 有 pg 没 → 标 unbound
	out := []DiscoveredChat{}
	seen := map[string]bool{}
	for _, d := range fromClient {
		seen[d.ChatID] = true
		if b, ok := boundByID[d.ChatID]; ok {
			out = append(out, DiscoveredChat{
				ChatID:    d.ChatID,
				FirstSeen: d.FirstSeen,
				Purpose:   b.Purpose,
				Label:     b.Label,
				Enabled:   b.Enabled,
				Bound:     true,
			})
		} else {
			out = append(out, DiscoveredChat{
				ChatID:    d.ChatID,
				FirstSeen: d.FirstSeen,
				Bound:     false,
			})
		}
	}
	// 4) PG 有但 client 没 (历史遗留或手工配的) 也展示
	for _, b := range all {
		if !seen[b.ChatID] {
			var fs *time.Time
			if b.FirstSeen != nil {
				fs = b.FirstSeen
			}
			dc := DiscoveredChat{
				ChatID:  b.ChatID,
				Purpose: b.Purpose,
				Label:   b.Label,
				Enabled: b.Enabled,
				Bound:   true,
			}
			if fs != nil {
				dc.FirstSeen = *fs
			}
			out = append(out, dc)
		}
	}

	c.JSON(http.StatusOK, gin.H{"items": out, "count": len(out)})
}

// ============== 3. Status ==============

// GetStatus GET /admin/wecom/status
func (h *AdminHandler) GetStatus(c *gin.Context) {
	connected := false
	botID := ""
	if h.Conn != nil {
		connected = h.Conn.Connected()
		botID = h.Conn.BotID()
	}
	byPurpose, _ := h.Store.CountByPurpose(c.Request.Context())
	discoveredCount := 0
	if h.Lister != nil {
		discoveredCount = len(h.Lister.DiscoveredChats())
	}
	boundAll, _ := h.Store.ListBindings(c.Request.Context(), "")
	boundCount := 0
	for _, b := range boundAll {
		if b.Enabled {
			boundCount++
		}
	}
	_, lastReload := h.Router.Stats()
	c.JSON(http.StatusOK, StatusResponse{
		Connected:       connected,
		BotID:           botID,
		DiscoveredCount: discoveredCount,
		BoundCount:      boundCount,
		BoundByPurpose:  byPurpose,
		LastReloadAt:    lastReload,
	})
}

// ============== 4. Reload ==============

// ReloadCache POST /admin/wecom/reload
func (h *AdminHandler) ReloadCache(c *gin.Context) {
	if err := h.Router.Reload(c.Request.Context()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	loaded, lastReload := h.Router.Stats()
	c.JSON(http.StatusOK, gin.H{
		"ok":             true,
		"loaded":         loaded,
		"last_reload_at": lastReload,
	})
}

// ============== 5. Purpose 白名单 ==============

// ListPurposes GET /admin/wecom/purposes
func (h *AdminHandler) ListPurposes(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"items": PurposeDescription,
		"count": len(PurposeDescription),
	})
}

// 避免未使用的 import 警告
var _ = time.Time{}
