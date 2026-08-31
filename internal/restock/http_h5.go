package restock

// H5 视图专用 API (2026-08-30 新增)
//   把原来企微群内 button_interaction 卡片交互,改为 H5 页面交互
//   按 user.group (floor/office) 返回不同字段
//   - floor:  极简,只显示当前库存 + 2 个按钮(已补/缺货)
//   - office: 完整,含昨日销售/安全线/优先级/供应商/状态/触发原因
//
//   分类树: 从 siss_sales_demo cube 拉 item_clsname (带每个分类下的 SKU 数)

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

// userGroupFromDB 查 user.group (由 middleware 设置的 user_id 索引)
func userGroupFromDB(ctx context.Context, pool *pgxpool.Pool, userID string) string {
	if userID == "" || pool == nil {
		return ""
	}
	var g string
	row := pool.QueryRow(ctx, `SELECT COALESCE("group",'') FROM users WHERE id=$1`, userID)
	_ = row.Scan(&g)
	return g
}

// H5TaskView H5 任务视图 (兼容 floor / office)
//   Field 选择原则:
//   - floor  极简: task_id/item_no/item_name/branch_no/current_stock/suggest_qty/priority/status
//   - office 完整: 上面 + supplier_name/safety_stock/yesterday_sales/reason/created_at/last_update_at/push_count
type H5TaskView struct {
	TaskID         string     `json:"task_id"`
	BranchNo       string     `json:"branch_no"`
	ItemNo         string     `json:"item_no"`
	ItemName       string     `json:"item_name"`
	Barcode        string     `json:"barcode,omitempty"`        // 暂未挂载 (siss cube 无此 dim)
	ItemClsname    string     `json:"item_clsname,omitempty"`   // 来自 cube (分类)
	ItemClsno      string     `json:"item_clsno,omitempty"`     // 来自 cube
	ItemBrand      string     `json:"item_brand,omitempty"`     // 来自 cube
	SupplierName   string     `json:"supplier_name,omitempty"`  // office
	CurrentStock   int        `json:"current_stock"`
	SafetyStock    int        `json:"safety_stock,omitempty"`    // office
	YesterdaySales int        `json:"yesterday_sales,omitempty"` // office
	SevenDayAvg    int        `json:"seven_day_avg,omitempty"`
	ThirtyDayAvg   int        `json:"thirty_day_avg,omitempty"`
	SuggestQty     int        `json:"suggest_qty"`
	Reason         string     `json:"reason,omitempty"`        // office
	Priority       string     `json:"priority"`
	Status         string     `json:"status"`
	PushCount      int        `json:"push_count,omitempty"`    // office
	FirstPushAt    *time.Time `json:"first_push_at,omitempty"`
	LastPushAt     *time.Time `json:"last_push_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	LastUpdateAt   time.Time  `json:"last_update_at"`
}

// RestockH5TasksList GET /api/v1/restock/h5/tasks
//   query:
//     status   = open|acked|short|closed|all   (默认 open)
//     group    = floor|office|auto            (默认 auto = 从 user.group 推)
//     branch_no= xxx                          (默认 = svc.Cfg.BranchNo)
//     limit    = N (默认 200, 最大 1000)
//   floor:  只返回必要字段
//   office: 返回完整 + 关联分类
func RestockH5TasksList(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		status := c.DefaultQuery("status", "open")
		branchNo := c.DefaultQuery("branch_no", svc.Cfg.BranchNo)

		limit, _ := strconv.Atoi(c.Query("limit"))
		if limit <= 0 || limit > 1000 {
			limit = 100
		}

		// group 决定返回字段粒度
		reqGroup := strings.ToLower(strings.TrimSpace(c.Query("group")))
		uid := userIDFromGin(c)
		actualGroup := reqGroup
		if actualGroup == "" || actualGroup == "auto" {
			actualGroup = userGroupFromDB(ctx, svc.Store.pool, uid)
		}
		full := actualGroup == "office"

		// with_cls: 默认 true, ?with_cls=false 跳过 cube 分类查询
		//   - floor 列表: 用 task 自身的 item_clsname 聚合分类, 无需 cube 全量 GROUP BY
		//   - office 表格: item_clsname 可选
		withCls := c.DefaultQuery("with_cls", "true") != "false"

		// 1) 拉 task 列表 (按 limit)
		tasks, err := loadTasksByStatus(ctx, svc.Store, branchNo, status, limit)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}

		// 2) 查 item_clsno/clsname - 走内存字典 (启动时已加载, 0 cube 调用)
		//    with_cls=false: 跳过 (用于已知道不需要分类的调用方)
		var clsMap map[string]*itemClsItem
		if withCls {
			clsMap = map[string]*itemClsItem{}
			for _, t := range tasks {
				clsno := svc.ItemClsNoOf(t.ItemNo)
				clsname := svc.ClsNameOf(clsno)
				clsMap[t.ItemNo] = &itemClsItem{
					ClsNo:   clsno,
					ClsName: clsname,
				}
			}
		} else {
			clsMap = map[string]*itemClsItem{}
		}

		// 3) 转 view
		views := make([]H5TaskView, 0, len(tasks))
		for _, t := range tasks {
			v := H5TaskView{
				TaskID:       t.TaskID,
				BranchNo:     t.BranchNo,
				ItemNo:       t.ItemNo,
				ItemName:     t.ItemName,
				CurrentStock: t.CurrentStock,
				SuggestQty:   t.SuggestQty,
				Priority:     t.Priority,
				Status:       t.Status,
				CreatedAt:    t.LastUpdateAt,
				LastUpdateAt: t.LastUpdateAt,
			}
			if cls, ok := clsMap[t.ItemNo]; ok {
				v.ItemClsno = cls.ClsNo
				v.ItemClsname = cls.ClsName
				v.ItemBrand = cls.Brand
			}
			if full {
				v.SupplierName = t.SupplierName
				v.SafetyStock = t.SafetyStock
				v.YesterdaySales = t.YesterdaySales
				v.Reason = t.Reason
				v.PushCount = t.PushCount
				v.FirstPushAt = t.FirstPushAt
				v.LastPushAt = t.LastPushAt
				v.CreatedAt = t.LastUpdateAt
			}
			views = append(views, v)
		}

		// 4) 默认按 suggest_qty desc (H5 floor 用户最关心:补货多的先做)
		sort.Slice(views, func(i, j int) bool {
			if views[i].SuggestQty != views[j].SuggestQty {
				return views[i].SuggestQty > views[j].SuggestQty
			}
			return views[i].LastUpdateAt.After(views[j].LastUpdateAt)
		})

		c.JSON(200, gin.H{
			"tasks":  views,
			"count":  len(views),
			"status": status,
			"branch": branchNo,
			"group":  actualGroup,
			"full":   full,
		})
	}
}

// RestockTasksList GET /api/v1/restock/tasks?date=YYYY-MM-DD
//   新版陈列补货任务列表 (2026-08-30):
//   数据源: restock_display_suggest (建议量, 7/12/20:30 三次 tick 累加)
//          + restock_short_state    (短补锁定, ONCE)
//          + restock_need_purchase  (缺货后立即 upsert, 等完成才 close)
//
//   query:
//     date      = YYYY-MM-DD   (默认 today, 0 时区用本地时间)
//     branch_no = xxx           (默认 svc.Cfg.BranchNo)
//
//   返回: { tasks: [H5TaskItem...], count, date, branch }
//   H5TaskItem 字段(见 types.go):
//     item_no, item_name, branch_no, suggest_qty, inv_snapshot,
//     is_short, short_at, short_user, need_qty, need_status,
//     last_period, period_date, last_update_at
//
//   前端按钮逻辑:
//     is_short=false → [缺货] [已完成]
//     is_short=true  → [已完成] (防重复点缺货)
//   按钮 event_key 格式: "display-<branch>-<item>:short" / ":done"
func RestockTasksList(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		dateStr := strings.TrimSpace(c.Query("date"))
		var date time.Time
		if dateStr == "" {
			date = time.Now()
		} else {
			t, err := time.ParseInLocation("2006-01-02", dateStr, time.Local)
			if err != nil {
				c.JSON(400, gin.H{"error": "date 格式 YYYY-MM-DD, got: " + dateStr})
				return
			}
			date = t
		}

		branchNo := strings.TrimSpace(c.DefaultQuery("branch_no", svc.Cfg.BranchNo))

		tasks, err := svc.Store.ListH5Tasks(ctx, branchNo, date)
		if err != nil {
			log.Printf("[restock.tasks] ListH5Tasks: %v", err)
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}

		c.JSON(200, gin.H{
			"tasks":  tasks,
			"count":  len(tasks),
			"date":   date.Format("2006-01-02"),
			"branch": branchNo,
		})
	}
}

// RestockFeedback POST /api/v1/restock/feedback
//   新版陈列补货反馈 (2026-08-30), 统一 H5 + 企微 callback 入参
//
//   body:
//     {
//       "req_id":    "wecom-xxx"   // 企微 callback 才需要, H5 调可省
//       "chat_id":   "wrwxxx"     // 同上
//       "user_id":   "u_xxx",     // H5 调可省 (从 ctx auth_user_id 取)
//       "task_id":   "display-<branch>-<item>"  // 新版
//                  | "<旧 uuid>"                // 旧版兼容
//       "event_key": "display-<branch>-<item>:done"   // 新版
//                  | "display-<branch>-<item>:short"
//                  | "restock-<branch>-<item>:DONE"    // 旧版
//                  | "restock-<branch>-<item>:SHORT"
//                  | "done" | "short"                  // 兼容
//     }
//
//   权限 (按 kind 分别校验, 不能一个 perm 通杀):
//     kind=short → 需要 display:short     (cashier 无, 拒绝)
//     kind=done  → 需要 display:done      (所有有 restock:feedback 的都有 display:done)
//   fallback: 旧 event_key 走通用 restock:feedback
//
//   流程:
//     1) 解析 event_key → (kind, branch, itemNo)
//     2) 按 kind 校验 perm
//     3) 调 svc.OnButtonClick
//     4) 返回 200 + {ok, kind, is_short, need_purchase_id}
func RestockFeedback(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			ReqID    string `json:"req_id"`
			ChatID   string `json:"chat_id"`
			UserID   string `json:"user_id"`
			TaskID   string `json:"task_id"`
			EventKey string `json:"event_key"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
			return
		}
		if req.EventKey == "" {
			c.JSON(400, gin.H{"error": "event_key 必填"})
			return
		}

		// user_id 兜底
		uid := req.UserID
		if uid == "" {
			uid = userIDFromGin(c)
		}

		// 解析 event_key
		kind, branch, itemNo := parseDisplayButtonKey(req.EventKey)
		if kind == "" {
			c.JSON(400, gin.H{"error": "event_key 无法解析: " + req.EventKey})
			return
		}
		if branch == "" || itemNo == "" {
			branch, itemNo = parseDisplayTaskID(req.TaskID)
		}
		if branch == "" {
			branch = svc.Cfg.BranchNo
		}
		if branch == "" || itemNo == "" {
			c.JSON(400, gin.H{"error": "无法确定 branch/item, event_key=" + req.EventKey + " task_id=" + req.TaskID})
			return
		}

		// 权限: 按 kind 分别校验
		//   - new eventKey (display-...:short/done): 严格要求 display:short / display:done
		//   - legacy eventKey (DONE|SHORT, restock-...:DONE/SHORT): 用通用 restock:feedback
		isLegacyUpper := false
		if idx := strings.LastIndex(req.EventKey, ":"); idx > 0 {
			suf := req.EventKey[idx+1:]
			if suf == "DONE" || suf == "SHORT" {
				isLegacyUpper = true
			}
		}
		if strings.HasPrefix(req.EventKey, "DONE|") || strings.HasPrefix(req.EventKey, "SHORT|") {
			isLegacyUpper = true
		}
		required := "restock:feedback"
		if !isLegacyUpper {
			// 新版: 按 kind 取 perm (parseDisplayButtonKey 返小写 "short"/"done")
			if kind == "short" {
				required = "display:short"
			} else {
				required = "display:done"
			}
		}

		// 直接查 user role 检查 (绕过 RequirePerm, 因为它只支持单 perm)
		if !svc.hasPermForUser(c.Request.Context(), uid, required) {
			c.JSON(403, gin.H{
				"code":    "FORBIDDEN",
				"message": "需要 " + required + " 权限 (kind=" + kind + ")",
			})
			return
		}

		// 调 OnButtonClick
		svc.OnButtonClick(req.ReqID, req.ChatID, uid, req.TaskID, req.EventKey)

		// 查最新状态
		now := time.Now()
		isShort := false
		needPurchaseID := int64(0)
		ctx := c.Request.Context()
		if kind == FeedbackShort {
			if ss, _ := svc.Store.GetShortState(ctx, branch, itemNo); ss != nil {
				isShort = ss.IsShort
			}
		}
		if kind == FeedbackShort || kind == FeedbackDone {
			_ = svc.Store.pool.QueryRow(ctx,
				`SELECT id FROM restock_need_purchase WHERE branch_no=$1 AND item_no=$2 AND status IN ('pending','sent_to_supplier') ORDER BY id DESC LIMIT 1`,
				branch, itemNo).Scan(&needPurchaseID)
		}

		c.JSON(200, gin.H{
			"ok":               true,
			"task_id":          req.TaskID,
			"event_key":        req.EventKey,
			"kind":             kind,
			"branch_no":        branch,
			"item_no":          itemNo,
			"is_short":         isShort,
			"need_purchase_id": needPurchaseID,
			"tick_at":          now.Format(time.RFC3339),
		})
	}
}
//   body: {"type":"DONE"|"SHORT"}
//   替代企微按钮, H5 页面用
//   - DONE  → task.status='acked'
//   - SHORT → task.status='short' + 创建/更新 restock_need_purchase
func RestockH5Feedback(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		taskID := strings.TrimSpace(c.Param("task_id"))
		if taskID == "" {
			c.JSON(400, gin.H{"error": "task_id required"})
			return
		}
		var req struct {
			Type string `json:"type"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
			return
		}
		kind := strings.ToUpper(strings.TrimSpace(req.Type))
		if kind != FeedbackDone && kind != FeedbackShort {
			c.JSON(400, gin.H{"error": "type 必须 DONE 或 SHORT"})
			return
		}
		// 查 task
		task, err := svc.Store.GetTask(ctx, taskID)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		if task == nil {
			c.JSON(404, gin.H{"error": "task not found"})
			return
		}
		if task.Status == TaskStatusClosed {
			c.JSON(409, gin.H{"error": "task 已关闭, 不能反馈"})
			return
		}

		userID := userIDFromGin(c)

		// 1) 写 Feedback
		fb := &Feedback{
			TaskID:       taskID,
			FeedbackType: kind,
			FeedbackUser: userID,
			FeedbackTime: time.Now(),
		}
		if err := svc.Store.InsertFeedback(ctx, fb); err != nil {
			log.Printf("[restock.h5] insert feedback: %v", err)
		}

		// 2) 改 task 状态
		newStatus := TaskStatusAcked
		if kind == FeedbackShort {
			newStatus = TaskStatusShort
		}
		if err := svc.Store.UpdateStatus(ctx, taskID, newStatus); err != nil {
			c.JSON(500, gin.H{"error": "update status: " + err.Error()})
			return
		}

		// 3) SHORT → upsert need_purchase
		var npID int64
		if kind == FeedbackShort {
			np := &NeedPurchase{
				BranchNo:      task.BranchNo,
				ItemNo:        task.ItemNo,
				ItemName:      task.ItemName,
				SupplierName:  task.SupplierName,
				SuggestQty:    task.SuggestQty,
				TriggerKind:   TriggerShortFeedback,
				TriggerTaskID: taskID,
			}
			if err := svc.Store.UpsertNeedPurchase(ctx, np); err != nil {
				log.Printf("[restock.h5] upsert need_purchase: %v", err)
			} else {
				// 查 id (Upsert 没返回 id, 用一条简单查询)
				_ = svc.Store.pool.QueryRow(ctx,
					`SELECT id FROM restock_need_purchase WHERE branch_no=$1 AND item_no=$2 AND status='pending'`,
					task.BranchNo, task.ItemNo).Scan(&npID)
			}
		}

		log.Printf("[restock.h5] feedback: user=%s task=%s kind=%s status=%s",
			userID, taskID, kind, newStatus)
		c.JSON(200, gin.H{
			"ok":         true,
			"task_id":    taskID,
			"new_status": newStatus,
			"need_purchase_id": npID,
		})
	}
}

// RestockH5Categories GET /api/v1/restock/h5/categories?branch_no=xxx
//   分类树: 从 siss_sales_demo cube 拉 item_clsname
//   返回: { categories: [{name, count}], total }
func RestockH5Categories(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		branchNo := c.DefaultQuery("branch_no", svc.Cfg.BranchNo)
		if branchNo == "" {
			c.JSON(400, gin.H{"error": "branch_no required"})
			return
		}
		// 调 siss_sales_demo cube GROUP BY item_clsname
		//   - measure: count
		//   - dim: item_clsname
		//   - filter: branch_no = xxx
		rows, err := svc.Cube.agent.Execute("siss_sales_demo",
			[]string{"siss_sales_demo.count"},
			[]string{"siss_sales_demo.item_clsname"},
			[]map[string]any{
				{"member": "siss_sales_demo.branch_no", "operator": "equals", "values": []string{branchNo}},
			},
			nil, 2000)
		if err != nil {
			// cube 拉不到 → 降级用 supplier_name (来自 restock_task)
			//   fallback: 列出所有 open task 的 supplier_name
			rows2, e2 := loadSupplierCategoriesFallback(ctx, svc.Store, branchNo)
			if e2 != nil {
				c.JSON(500, gin.H{"error": "cube: " + err.Error() + "; fallback: " + e2.Error()})
				return
			}
			c.JSON(200, gin.H{
				"categories": rows2,
				"source":     "supplier_fallback",
				"branch":     branchNo,
			})
			return
		}
		type catRow struct {
			Name  string `json:"name"`
			Count int    `json:"count"`
		}
		out := make([]catRow, 0, len(rows))
		total := 0
		for _, r := range rows {
			name := asString(r, "siss_sales_demo.item_clsname")
			if name == "" {
				name = "(未分类)"
			}
			cnt := asInt(r, "siss_sales_demo.count")
			out = append(out, catRow{Name: name, Count: cnt})
			total += cnt
		}
		// 按 count desc
		sort.Slice(out, func(i, j int) bool {
			if out[i].Count != out[j].Count {
				return out[i].Count > out[j].Count
			}
			return out[i].Name < out[j].Name
		})
		c.JSON(200, gin.H{
			"categories": out,
			"total":      total,
			"source":     "siss_sales_demo",
			"branch":     branchNo,
		})
	}
}

// RestockH5ClsMap GET /api/v1/restock/h5/cls-map?item_nos=xxx,yyy
//   按 item_no 批量查 item_clsno / item_clsname / item_brand
//   走 siss_sales_demo cube + 单 item_no filter (小批量 < 30 个, 实际够用)
//   失败降级: 用 items cube 拿 clsno (无 clsname)
//   超时 3s
func RestockH5ClsMap(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		itemNosRaw := strings.TrimSpace(c.Query("item_nos"))
		if itemNosRaw == "" {
			c.JSON(400, gin.H{"error": "item_nos 必填 (逗号分隔,最多 50 个)"})
			return
		}
		parts := strings.Split(itemNosRaw, ",")
		if len(parts) > 50 {
			parts = parts[:50]
		}
		want := make(map[string]struct{}, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				want[p] = struct{}{}
			}
		}
		out := make(map[string]*itemClsItem, len(want))
		if svc.Cube == nil || svc.Cube.agent == nil {
			c.JSON(200, gin.H{"cls_map": out, "source": "no_cube"})
			return
		}

		// 用 siss_sales_demo cube IN filter (OR)
		// cube-agent-server 的 filter supports "in" / "equals" / etc.
		filters := []map[string]any{}
		if len(parts) == 1 {
			filters = append(filters, map[string]any{
				"member": "siss_sales_demo.item_no", "operator": "equals", "values": parts,
			})
		} else {
			values := make([]string, 0, len(parts))
			for _, p := range parts {
				values = append(values, p)
			}
			filters = append(filters, map[string]any{
				"member": "siss_sales_demo.item_no", "operator": "equals", "values": values,
			})
		}

		type result struct {
			rows []map[string]any
			err  error
		}
		resCh := make(chan result, 1)
		clsCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		go func() {
			rows, err := svc.Cube.agent.Execute("siss_sales_demo",
				[]string{"siss_sales_demo.count"},
				[]string{"siss_sales_demo.item_no", "siss_sales_demo.item_clsname", "siss_sales_demo.item_clsno", "siss_sales_demo.item_brand"},
				filters,
				nil, 5000)
			resCh <- result{rows, err}
		}()

		var rows []map[string]any
		select {
		case r := <-resCh:
			if r.err != nil {
				log.Printf("[restock.h5] cls-map cube err: %v", r.err)
				// 降级: 不带 item_no filter 查全表, 内存过滤
				rows2, e2 := svc.Cube.agent.Execute("siss_sales_demo",
					[]string{"siss_sales_demo.count"},
					[]string{"siss_sales_demo.item_no", "siss_sales_demo.item_clsname", "siss_sales_demo.item_clsno", "siss_sales_demo.item_brand"},
					nil, nil, 30000)
				if e2 != nil {
					c.JSON(200, gin.H{"cls_map": out, "source": "degrade_empty", "error": e2.Error()})
					return
				}
				for _, r2 := range rows2 {
					ino := asString(r2, "siss_sales_demo.item_no")
					if _, ok := want[ino]; !ok {
						continue
					}
					if _, exists := out[ino]; exists {
						continue
					}
					out[ino] = &itemClsItem{
						ClsNo:   asString(r2, "siss_sales_demo.item_clsno"),
						ClsName: asString(r2, "siss_sales_demo.item_clsname"),
						Brand:   asString(r2, "siss_sales_demo.item_brand"),
					}
				}
				c.JSON(200, gin.H{"cls_map": out, "source": "fallback_full_scan"})
				return
			}
			rows = r.rows
		case <-clsCtx.Done():
			c.JSON(200, gin.H{"cls_map": out, "source": "timeout"})
			return
		}

		for _, r := range rows {
			ino := asString(r, "siss_sales_demo.item_no")
			if _, ok := want[ino]; !ok {
				continue
			}
			if _, exists := out[ino]; exists {
				continue
			}
			out[ino] = &itemClsItem{
				ClsNo:   asString(r, "siss_sales_demo.item_clsno"),
				ClsName: asString(r, "siss_sales_demo.item_clsname"),
				Brand:   asString(r, "siss_sales_demo.item_brand"),
			}
		}
		// 没找到的也填个空 (前端知道没分类)
		for ino := range want {
			if _, ok := out[ino]; !ok {
				out[ino] = &itemClsItem{}
			}
		}
		c.JSON(200, gin.H{"cls_map": out, "source": "siss_sales_demo"})
	}
}

// RestockH5PurchasePlans GET /api/v1/restock/h5/purchase-plans
//   office 用: 看完整的采购计划
//   query:
//     supplier   (空 = 所有 supplier, 按 supplier 聚合返回)
//     branch_no  (默认 svc.Cfg.BranchNo, 空 = 不限门店)
//     status     (默认 pending)
//   返回: { plans, count, total_qty, suppliers: [{supplier, count, qty}] }
func RestockH5PurchasePlans(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		supplier := strings.TrimSpace(c.Query("supplier"))
		branchNo := strings.TrimSpace(c.DefaultQuery("branch_no", svc.Cfg.BranchNo))
		status := c.DefaultQuery("status", NeedStatusPending)

		// SQL: 按 supplier 可选, branch 可选
		q := `
			SELECT id, branch_no, item_no, COALESCE(item_name,''), COALESCE(barcode,''),
				COALESCE(supplier_name,''), suggest_qty, trigger_kind,
				COALESCE(trigger_task_id,''), status, created_at, updated_at, exported_at
			FROM restock_need_purchase
			WHERE status = $1`
		args := []any{status}
		if supplier != "" {
			args = append(args, supplier)
			q += fmt.Sprintf(" AND supplier_name = $%d", len(args))
		}
		if branchNo != "" {
			args = append(args, branchNo)
			q += fmt.Sprintf(" AND branch_no = $%d", len(args))
		}
		q += " ORDER BY created_at DESC LIMIT 500"
		rows, err := svc.Store.pool.Query(ctx, q, args...)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		plans := make([]*NeedPurchase, 0)
		bySup := map[string]struct {
			Count int
			Qty   int
		}{}
		totalQty := 0
		for rows.Next() {
			p := &NeedPurchase{}
			if err := rows.Scan(&p.ID, &p.BranchNo, &p.ItemNo, &p.ItemName, &p.Barcode,
				&p.SupplierName, &p.SuggestQty, &p.TriggerKind, &p.TriggerTaskID,
				&p.Status, &p.CreatedAt, &p.UpdatedAt, &p.ExportedAt); err != nil {
				c.JSON(500, gin.H{"error": err.Error()})
				return
			}
			plans = append(plans, p)
			s := bySup[p.SupplierName]
			s.Count++
			s.Qty += p.SuggestQty
			bySup[p.SupplierName] = s
			totalQty += p.SuggestQty
		}
		// supplier 列表 (按 qty desc)
		type supAgg struct {
			Supplier string `json:"supplier"`
			Count    int    `json:"count"`
			Qty      int    `json:"qty"`
		}
		sups := make([]supAgg, 0, len(bySup))
		for k, v := range bySup {
			sups = append(sups, supAgg{Supplier: k, Count: v.Count, Qty: v.Qty})
		}
		sort.Slice(sups, func(i, j int) bool {
			if sups[i].Qty != sups[j].Qty {
				return sups[i].Qty > sups[j].Qty
			}
			return sups[i].Supplier < sups[j].Supplier
		})
		c.JSON(200, gin.H{
			"supplier":   supplier,
			"branch_no":  branchNo,
			"status":     status,
			"plans":      plans,
			"count":      len(plans),
			"total_qty":  totalQty,
			"suppliers":  sups,
		})
	}
}

// ============== helpers ==============

func userIDFromGin(c *gin.Context) string {
	// auth middleware 设的 ctx key 是 "auth_user_id"
	if v, ok := c.Get("auth_user_id"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	// 兜底: X-User-Id header (开发/调试用)
	if uid := c.GetHeader("X-User-Id"); uid != "" {
		return uid
	}
	return ""
}

// loadTasksByStatus 按 status 拉 task (open 走 SQL 直接加 limit, 其它走通用 SQL)
func loadTasksByStatus(ctx context.Context, store *Store, branchNo, status string, limit int) ([]*Task, error) {
	if status == "open" || status == "" {
		// ListOpenTasks 不支持 limit, 这里直接 SQL 加 limit
		rows, err := store.pool.Query(ctx, `
			SELECT task_id, branch_no, item_no, item_name, COALESCE(supplier_name,''),
				current_stock, safety_stock, yesterday_sales, suggest_qty,
				COALESCE(reason,''), priority, status, first_push_at, last_push_at,
				last_update_at, closed_at, COALESCE(closed_reason,''), push_count
			FROM restock_task WHERE branch_no=$1 AND status='open'
			ORDER BY last_update_at DESC LIMIT $2
		`, branchNo, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []*Task
		for rows.Next() {
			t := &Task{}
			if err := rows.Scan(&t.TaskID, &t.BranchNo, &t.ItemNo, &t.ItemName, &t.SupplierName,
				&t.CurrentStock, &t.SafetyStock, &t.YesterdaySales, &t.SuggestQty,
				&t.Reason, &t.Priority, &t.Status, &t.FirstPushAt, &t.LastPushAt,
				&t.LastUpdateAt, &t.ClosedAt, &t.ClosedReason, &t.PushCount); err != nil {
				return nil, err
			}
			out = append(out, t)
		}
		return out, rows.Err()
	}
	if status == "all" {
		rows, err := store.pool.Query(ctx, `
			SELECT task_id, branch_no, item_no, item_name, COALESCE(supplier_name,''),
				current_stock, safety_stock, yesterday_sales, suggest_qty,
				COALESCE(reason,''), priority, status, first_push_at, last_push_at,
				last_update_at, closed_at, COALESCE(closed_reason,''), push_count
			FROM restock_task WHERE branch_no=$1
			ORDER BY last_update_at DESC LIMIT $2
		`, branchNo, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []*Task
		for rows.Next() {
			t := &Task{}
			if err := rows.Scan(&t.TaskID, &t.BranchNo, &t.ItemNo, &t.ItemName, &t.SupplierName,
				&t.CurrentStock, &t.SafetyStock, &t.YesterdaySales, &t.SuggestQty,
				&t.Reason, &t.Priority, &t.Status, &t.FirstPushAt, &t.LastPushAt,
				&t.LastUpdateAt, &t.ClosedAt, &t.ClosedReason, &t.PushCount); err != nil {
				return nil, err
			}
			out = append(out, t)
		}
		return out, rows.Err()
	}
	// 其它: acked / short / closed
	rows, err := store.pool.Query(ctx, `
		SELECT task_id, branch_no, item_no, item_name, COALESCE(supplier_name,''),
			current_stock, safety_stock, yesterday_sales, suggest_qty,
			COALESCE(reason,''), priority, status, first_push_at, last_push_at,
			last_update_at, closed_at, COALESCE(closed_reason,''), push_count
		FROM restock_task WHERE branch_no=$1 AND status=$2
		ORDER BY last_update_at DESC LIMIT $3
	`, branchNo, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t := &Task{}
		if err := rows.Scan(&t.TaskID, &t.BranchNo, &t.ItemNo, &t.ItemName, &t.SupplierName,
			&t.CurrentStock, &t.SafetyStock, &t.YesterdaySales, &t.SuggestQty,
			&t.Reason, &t.Priority, &t.Status, &t.FirstPushAt, &t.LastPushAt,
			&t.LastUpdateAt, &t.ClosedAt, &t.ClosedReason, &t.PushCount); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// itemClsItem 单 SKU 分类信息 (从 cube 拉)
type itemClsItem struct {
	Barcode string
	ClsNo   string
	ClsName string
	Brand   string
}

// loadItemClsMapForTasks 给一批 task 查分类
//   2026-08-30 优化: 用 items cube 拿 item_clsno (26485 行主表, 几乎瞬时)
//                  item_clsname 走 service.ClsNameOf 内存字典 (~100 行, 启动时拉一次缓存)
//   比 siss_sales_demo cube 的 IN filter 快 60x (3s → 50ms)
//   超时 1.5s, 失败降级 (分类名为空, 但 task 仍可用)
func loadItemClsMapForTasks(ctx context.Context, cube *CubeQuerier, svc *Service, branchNo string, tasks []*Task) (map[string]*itemClsItem, error) {
	out := make(map[string]*itemClsItem, len(tasks))
	if len(tasks) == 0 || cube == nil || cube.agent == nil {
		return out, nil
	}
	want := indexItemNos(tasks)
	itemNos := make([]string, 0, len(want))
	for ino := range want {
		itemNos = append(itemNos, ino)
	}
	// IN 列表太长 (>30) 走降级: 内存过滤
	// items cube 没 branch filter, 但本身只有 26485 行
	if len(itemNos) > 30 {
		itemNos = nil
	}

	type result struct {
		m   map[string]*itemClsItem
		err error
	}
	resCh := make(chan result, 1)
	clsCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()

	go func() {
		var filters []map[string]any
		if itemNos != nil {
			filters = []map[string]any{{
				"member": "items.item_no", "operator": "equals", "values": itemNos,
			}}
		}
		rows, err := cube.agent.Execute("items",
			[]string{"items.count"},
			[]string{"items.item_no", "items.item_clsno", "items.item_brand", "items.item_name"},
			filters,
			nil, 5000)
		if err != nil {
			resCh <- result{nil, err}
			return
		}
		m := make(map[string]*itemClsItem, len(rows))
		for _, r := range rows {
			ino := asString(r, "items.item_no")
			if ino == "" {
				continue
			}
			if _, ok := want[ino]; !ok {
				continue
			}
			if _, exists := m[ino]; exists {
				continue
			}
			clsno := asString(r, "items.item_clsno")
			clsname := ""
			if svc != nil {
				clsname = svc.ClsNameOf(clsno)
			}
			m[ino] = &itemClsItem{
				ClsNo:   clsno,
				ClsName: clsname,
				Brand:   asString(r, "items.item_brand"),
			}
		}
		resCh <- result{m, nil}
	}()

	select {
	case r := <-resCh:
		if r.err != nil {
			return out, r.err
		}
		return r.m, nil
	case <-clsCtx.Done():
		log.Printf("[restock.h5] loadItemClsMapForTasks timeout (degrade empty)")
		return out, nil
	}
}

func indexItemNos(tasks []*Task) map[string]struct{} {
	m := make(map[string]struct{}, len(tasks))
	for _, t := range tasks {
		m[t.ItemNo] = struct{}{}
	}
	return m
}

func loadSupplierCategoriesFallback(ctx context.Context, store *Store, branchNo string) ([]gin.H, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT COALESCE(supplier_name, '(未指定)') AS sup, COUNT(*) AS n
		FROM restock_task WHERE branch_no=$1 AND status='open'
		GROUP BY supplier_name
		ORDER BY n DESC
	`, branchNo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []gin.H
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out = append(out, gin.H{"name": name, "count": n})
	}
	return out, rows.Err()
}

// silence unused
var _ = fmt.Sprintf
