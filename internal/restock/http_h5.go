package restock

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tinkler/collect-ai/internal/auth"
)

// Restock HTTP 层 (2026-09-02 重构)
//
// 4 个端点:
//   GET  /api/v1/restock/tasks              任务列表 (H5 主页, 不分 office/floor, 不分 date)
//   POST /api/v1/restock/feedback           员工反馈 (DONE/SHORT)
//   GET  /api/v1/restock/purchase-plans     采购计划单 (采购看)
//   POST /api/v1/restock/cron/tick          手动触发 (admin)
//
// 权限:
//   - inventory:view   决定是否显示 inv_snapshot
//   - supplier:view    决定是否显示 supplier_name
//   - display:done     员工点已完成
//   - display:short    员工点缺货
//   - admin            手动 tick

// ============== GET /restock/tasks ==============

// RestockTasksList 拉陈列补货任务列表
//   不分 date, 不分 office/floor, 统一返回
//   meta.inv_viewable / meta.supplier_viewable 决定前端是否显示敏感字段
func RestockTasksList(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		branchNo := strings.TrimSpace(c.DefaultQuery("branch_no", svc.Cfg.BranchNo))
		if branchNo == "" {
			c.JSON(400, gin.H{"error": "branch_no required"})
			return
		}

		tasks, err := svc.Store.ListActiveItems(ctx, branchNo)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}

		// 注入 clsno / clsname / unit (从 items cube 启动时加载的内存字典查)
		for _, t := range tasks {
			clsno := svc.ItemClsNoOf(t.ItemNo)
			t.ItemClsno = clsno
			t.ItemClsname = svc.ClsNameOf(clsno)
			t.Unit = svc.UnitOf(t.ItemNo)
		}

		invViewable := permInventoryView(c)
		if !invViewable {
			for _, t := range tasks {
				t.InvSnapshot = 0
			}
		}

		c.JSON(200, gin.H{
			"tasks": tasks,
			"count": len(tasks),
			"branch": branchNo,
			"meta": gin.H{
				"inv_viewable":      invViewable,
				"supplier_viewable": permSupplierView(c),
			},
		})
	}
}

// ============== POST /restock/feedback ==============

// RestockFeedback H5 员工反馈
//   body: { kind: "done" | "short", branch_no, item_no, user_id }
//   2026-09-02 重构: 用 kind 字段代替 event_key,适配 H5
func RestockFeedback(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Kind     string `json:"kind"`
			BranchNo string `json:"branch_no"`
			ItemNo   string `json:"item_no"`
			UserID   string `json:"user_id"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
			return
		}
		kind := strings.ToLower(strings.TrimSpace(req.Kind))
		if kind != FeedbackDone && kind != FeedbackShort {
			c.JSON(400, gin.H{"error": "kind 必须 done 或 short"})
			return
		}

		uid := req.UserID
		if uid == "" {
			uid = userIDFromGin(c)
		}

		// 权限: 按 kind 校验
		required := "display:done"
		if kind == FeedbackShort {
			required = "display:short"
		}
		if !svc.hasPermForUser(c.Request.Context(), uid, required) {
			c.JSON(403, gin.H{
				"code":    "FORBIDDEN",
				"message": "需要 " + required + " 权限",
			})
			return
		}

		if err := svc.HandleFeedback(c.Request.Context(), req.BranchNo, req.ItemNo, uid, kind); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}

		c.JSON(200, gin.H{
			"ok":        true,
			"kind":      kind,
			"branch_no": req.BranchNo,
			"item_no":   req.ItemNo,
			"tick_at":   time.Now().Format(time.RFC3339),
		})
	}
}

// ============== GET /restock/purchase-plans ==============

// RestockPurchasePlansList 采购计划单
//   query:
//     supplier   (空 = 所有)
//     branch_no  (默认 = svc.Cfg.BranchNo)
//     status     (默认 pending)
func RestockPurchasePlansList(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		supplier := strings.TrimSpace(c.Query("supplier"))
		branchNo := strings.TrimSpace(c.DefaultQuery("branch_no", svc.Cfg.BranchNo))
		status := strings.TrimSpace(c.DefaultQuery("status", NeedStatusPending))

		var plans []*PurchasePlan
		var err error
		if supplier != "" {
			plans, err = svc.Store.ListPlansBySupplier(ctx, supplier, branchNo)
		} else {
			plans, err = svc.Store.ListPendingPlans(ctx, branchNo)
		}
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}

		// 按 supplier 聚合
		bySup := map[string]struct {
			Count int
			Qty   int
		}{}
		totalQty := 0
		for _, p := range plans {
			s := bySup[p.SupplierName]
			s.Count++
			s.Qty += p.SuggestQty
			bySup[p.SupplierName] = s
			totalQty += p.SuggestQty
		}
		type supAgg struct {
			Supplier string `json:"supplier"`
			Count    int    `json:"count"`
			Qty      int    `json:"qty"`
		}
		sups := make([]supAgg, 0, len(bySup))
		for k, v := range bySup {
			sups = append(sups, supAgg{Supplier: k, Count: v.Count, Qty: v.Qty})
		}

		c.JSON(200, gin.H{
			"plans":     plans,
			"count":     len(plans),
			"total_qty": totalQty,
			"suppliers": sups,
			"branch":    branchNo,
			"status":    status,
		})
	}
}

// ============== POST /restock/cron/tick ==============

// RestockManualTick 手动触发一次 DisplayRestockTick
//   query: period=morn|aft|eve|manual (默认 manual 拉最近 1h)
func RestockManualTick(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		period := c.DefaultQuery("period", PeriodManual)
		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
		defer cancel()
		if err := svc.DisplayRestockTick(ctx, period); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{
			"ok":     true,
			"period": period,
			"ts":     time.Now().Unix(),
		})
	}
}

// ============== 权限 helpers ==============

// permInventoryView 检查当前用户是否有 inventory:view 权限
func permInventoryView(c *gin.Context) bool {
	role := roleFromGin(c)
	if role == "" {
		return false
	}
	return auth.HasPerm(role, "inventory:view")
}

// permSupplierView 检查当前用户是否有 supplier:view 权限
func permSupplierView(c *gin.Context) bool {
	role := roleFromGin(c)
	if role == "" {
		return false
	}
	return auth.HasPerm(role, "supplier:view")
}

// roleFromGin 从 gin ctx 拿 role
func roleFromGin(c *gin.Context) string {
	if v, ok := c.Get("auth_role"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// userIDFromGin 从 gin ctx 拿 user_id
func userIDFromGin(c *gin.Context) string {
	if v, ok := c.Get("auth_user_id"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	if uid := c.GetHeader("X-User-Id"); uid != "" {
		return uid
	}
	return ""
}

// hasPermForUser 查 user 的 perm (svc 持有 auth context 不可,这里直接走 service.HasPerm)
func (s *Service) hasPermForUser(ctx context.Context, userID, perm string) bool {
	if userID == "" {
		return false
	}
	role, err := s.Store.userRoleOf(ctx, userID)
	if err != nil || role == "" {
		return false
	}
	return auth.HasPerm(role, perm)
}

// userRoleOf 从 pg 查 user 的 role id (owner/manager/...)
//   2026-09-02 修复:
//     1. users 表没有 username 列 (id 就是登录名), 旧 SQL 报 column not exist
//     2. users.role 列存的是 role 的中文 name (e.g. "店主"), 而 role_permissions
//        存的是 role 的 id (e.g. "owner"), 两个命名空间不一致
//     3. 修复: 拿到 users.role (中文) 后再 LEFT JOIN roles 把 name 转 id
//   rolePermissions 缓存的 key 是 role id, HasPerm 用 role id 查 → 一致
func (s *Store) userRoleOf(ctx context.Context, userID string) (string, error) {
	// 优先走 user_roles 多对多 (新权限体系)
	var roleID string
	err := s.pool.QueryRow(ctx, `
		SELECT r.id
		FROM users u
		JOIN user_roles ur ON u.id = ur.user_id
		JOIN roles r ON ur.role_id = r.id
		WHERE u.id = $1
		ORDER BY r.id LIMIT 1
	`, userID).Scan(&roleID)
	if err == nil {
		return roleID, nil
	}

	// 兜底: users.role 列存的是中文 name, 转换为 id
	var roleName string
	if e2 := s.pool.QueryRow(ctx, `SELECT COALESCE(role, '') FROM users WHERE id = $1`, userID).Scan(&roleName); e2 == nil && roleName != "" {
		var id string
		if e3 := s.pool.QueryRow(ctx, `SELECT id FROM roles WHERE name = $1 LIMIT 1`, roleName).Scan(&id); e3 == nil {
			return id, nil
		}
		// 已经是 id 形式 (e.g. "owner") 直接返回
		return roleName, nil
	}
	return "", nil
}

// 避免 unused 警告 (strconv 暂时未用, 留给未来 limit 参数)
var _ = strconv.Atoi

// 避免 unused 警告
var _ = http.StatusOK

// ============== ClsDict 注入 helpers ==============
// LoadItemDict (在 service.go) 启动时从 items cube 加载, 这里走 service.ItemClsNoOf / ClsNameOf
