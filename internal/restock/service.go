package restock

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Service restock 业务编排
//
// 持有依赖 → 启动 cron → HourlyTick 跑核心循环 → LLM 批量 / 聚合 cron
type Service struct {
	Cfg    *RestockConfig
	Store  *Store
	Cube   *CubeQuerier
	LLM    *LlmPlanner
	WeCom  *WeCom

	stopCh chan struct{}
	wg     sync.WaitGroup
}

func NewService(
	cfg *RestockConfig,
	pool *pgxpool.Pool,
	cube *CubeQuerier,
	llm *LlmPlanner,
	wecom *WeCom,
) *Service {
	return &Service{
		Cfg:   cfg,
		Store: NewStore(pool),
		Cube:  cube,
		LLM:   llm,
		WeCom: wecom,
	}
}

// Start 启动 3 个调度 goroutine(自实现,避免依赖 robfig/cron)
//   - 每小时 HourlyTick (7-21 点内)
//   - 每天 21:30 AggregateTick
//   - 每天 1,7,13,19 点 LlmPlanTick
func (s *Service) Start() error {
	s.stopCh = make(chan struct{})

	// Hourly tick:检查每分钟一次,匹配 cfg.HourlyCron 解析出的 hours
	hours, err := parseHourlyHours(s.Cfg.HourlyCron)
	if err != nil {
		return fmt.Errorf("parse HourlyCron: %w", err)
	}
	s.wg.Add(1)
	go s.loopHourly(hours)

	// Aggregate:21:30 每日
	aggH, aggM, err := parseHHMM(s.Cfg.AggregateCron)
	if err != nil {
		return fmt.Errorf("parse AggregateCron: %w", err)
	}
	s.wg.Add(1)
	go s.loopDaily(aggH, aggM, s.AggregateTick, "AggregateTick")

	// LLM Plan:每天 1,7,13,19 点
	llmHours, err := parseLlmHours(s.Cfg.LlmPlanCron)
	if err != nil {
		return fmt.Errorf("parse LlmPlanCron: %w", err)
	}
	s.wg.Add(1)
	go s.loopDailyHours(llmHours, s.LlmPlanTick, "LlmPlanTick")

	log.Printf("[restock] schedulers started: hourly=%v aggregate=%02d:%02d llm=%v",
		hours, aggH, aggM, llmHours)
	return nil
}

func (s *Service) Stop() {
	if s.stopCh != nil {
		close(s.stopCh)
	}
	s.wg.Wait()
}

// loopHourly 每分钟检查一次,在目标小时内的 :00 整点触发
func (s *Service) loopHourly(hours []int) {
	defer s.wg.Done()
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case now := <-t.C:
			if now.Second() != 0 {
				continue // 整点对齐(已对齐 ticker,通常已为 0)
			}
			if !containsInt(hours, now.Hour()) {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			if err := s.HourlyTick(ctx); err != nil {
				log.Printf("[restock] HourlyTick: %v", err)
			}
			cancel()
		}
	}
}

// loopDaily 每天 HH:MM 触发一次
func (s *Service) loopDaily(hh, mm int, fn func(context.Context) error, name string) {
	defer s.wg.Done()
	for {
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, now.Location())
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		d := next.Sub(now)
		t := time.NewTimer(d)
		select {
		case <-s.stopCh:
			t.Stop()
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			if err := fn(ctx); err != nil {
				log.Printf("[restock] %s: %v", name, err)
			}
			cancel()
		}
	}
}

// loopDailyHours 每天多个指定小时整点触发
func (s *Service) loopDailyHours(hours []int, fn func(context.Context) error, name string) {
	defer s.wg.Done()
	lastDay := -1
	lastHour := -1
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case now := <-t.C:
			if now.Second() != 0 {
				continue
			}
			if !containsInt(hours, now.Hour()) {
				continue
			}
			if now.Day() == lastDay && now.Hour() == lastHour {
				continue
			}
			lastDay, lastHour = now.Day(), now.Hour()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			if err := fn(ctx); err != nil {
				log.Printf("[restock] %s: %v", name, err)
			}
			cancel()
		}
	}
}

// parseHourlyHours 解析 "0 7-21 * * *" 形式 → [7,8,...,21]
func parseHourlyHours(spec string) ([]int, error) {
	// 简化:只解析 "0 H-H * * *" 或 "0 h,h,h * * *"
	parts := strings.Fields(spec)
	if len(parts) < 2 {
		return nil, fmt.Errorf("bad spec: %s", spec)
	}
	hourPart := parts[1]
	if strings.Contains(hourPart, "-") {
		segs := strings.SplitN(hourPart, "-", 2)
		from, _ := strconv.Atoi(segs[0])
		to, _ := strconv.Atoi(segs[1])
		out := make([]int, 0, to-from+1)
		for i := from; i <= to; i++ {
			out = append(out, i)
		}
		return out, nil
	}
	var out []int
	for _, s := range strings.Split(hourPart, ",") {
		n, _ := strconv.Atoi(s)
		out = append(out, n)
	}
	return out, nil
}

// parseHHMM 解析 "0 30 21 * * *" → (21, 30)
func parseHHMM(spec string) (int, int, error) {
	parts := strings.Fields(spec)
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("bad spec: %s", spec)
	}
	mm, _ := strconv.Atoi(parts[0])
	hh, _ := strconv.Atoi(parts[1])
	return hh, mm, nil
}

// parseLlmHours 解析 "0 0 1,7,13,19 * * *" → [1,7,13,19]
func parseLlmHours(spec string) ([]int, error) {
	parts := strings.Fields(spec)
	if len(parts) < 3 {
		return nil, fmt.Errorf("bad spec: %s", spec)
	}
	hourPart := parts[2]
	var out []int
	for _, s := range strings.Split(hourPart, ",") {
		n, _ := strconv.Atoi(s)
		out = append(out, n)
	}
	return out, nil
}

func containsInt(arr []int, v int) bool {
	for _, x := range arr {
		if x == v {
			return true
		}
	}
	return false
}

// HourlyTick 核心循环(7-21 点每小时跑一次)
//
// 步骤:
//   1. 拉 3 个 cube 数据(sales / inventory / promo)
//   2. 拉所有 open 状态的 task
//   3. 对每个 SKU 跑 ShouldRestock
//   4. upsert task(open 唯一)
//   5. 必要时推送(卖场群首次/6h 后/状态变更)
//   6. 写 need_purchase(短补/低于安全库存/反馈 SHORT)
//   7. 检查库存增加 → close task
func (s *Service) HourlyTick(ctx context.Context) error {
	branch := s.Cfg.BranchNo
	if branch == "" {
		return fmt.Errorf("RESTOCK_BRANCH_NO 未配置")
	}
	now := time.Now()
	log.Printf("[restock] HourlyTick start branch=%s now=%s", branch, now.Format("15:04"))

	// 1. 拉 cube
	salesMap, err := s.Cube.SalesYesterday(ctx, branch)
	if err != nil {
		return fmt.Errorf("sales: %w", err)
	}
	invMap, err := s.Cube.InventoryCurrent(ctx, branch)
	if err != nil {
		return fmt.Errorf("inventory: %w", err)
	}
	promoMap, err := s.Cube.Promotion7d(ctx, branch)
	if err != nil {
		log.Printf("[restock] promotion cube: %v (降级为无促销)", err)
		promoMap = map[string]bool{}
	}

	// 2. 拉 open tasks
	openTasks, err := s.Store.ListOpenTasks(ctx, branch)
	if err != nil {
		return fmt.Errorf("list open tasks: %w", err)
	}
	openByItem := make(map[string]*Task, len(openTasks))
	for _, t := range openTasks {
		openByItem[t.ItemNo] = t
	}

	pushCount := 0
	for itemNo, sales := range salesMap {
		// 组装 sku
		sku := *sales
		sku.Stock = invMap[itemNo] // 缺库存的 SKU 视为 0
		sku.HasPromo7d = promoMap[itemNo]

		// R2/R2b 判定(查 restock_feedback 关联 + restock_sales_watch)
		since24h := now.Add(-24 * time.Hour)
		hasRecentShort, _ := s.Store.HasRecentFeedback(ctx, branch, itemNo, FeedbackShort, since24h)
		hasRecentDone, _ := s.Store.HasRecentFeedback(ctx, branch, itemNo, FeedbackDone, since24h)

		hasDoneWithSales := false
		hasDoneNoSales := false
		if hasRecentDone {
			// 24h 内有销售
			sales24h, _ := s.Store.CountSalesInWindow(ctx, branch, itemNo, since24h, now)
			if sales24h > 0 {
				hasDoneWithSales = true
			} else {
				hasDoneNoSales = true
			}
		}

		// 决策
		existing := openByItem[itemNo]
		need, qty, prio, reason := ShouldRestock(
			&sku, existing,
			hasDoneWithSales, hasDoneNoSales, hasRecentShort,
			s.Cfg, now,
		)

		// R4 不触发且没有 open task → 跳过
		if !need && existing == nil {
			continue
		}

		// 计算供应商 fill_rate 并调整 qty
		var fillRate float64 = 1.0
		if sku.SupplierName != "" {
			rel, _ := s.Store.GetSupplierReliability(ctx, sku.SupplierName, sku.ItemNo)
			if rel != nil {
				fillRate = rel.FillRate
			}
		}
		// 用 LLM planner 算(优先 LLM,降级规则 + supplier 调整)
		finalQty := s.LLM.Plan(ctx, &sku, qty, fillRate)
		if finalQty < 1 {
			finalQty = 1
		}

		// 检测库存增加 → close
		if existing != nil && sku.Stock > existing.CurrentStock+5 {
			_ = s.Store.CloseTask(ctx, existing.TaskID, "restocked")
			log.Printf("[restock] task closed (restocked): %s stock %d→%d",
				existing.TaskID, existing.CurrentStock, sku.Stock)
			// 通知办公室群
			s.pushOffice(ctx, "restocked", &sku, finalQty, existing.TaskID)
			continue
		}

		if !need {
			continue
		}

		// upsert task
		task := &Task{
			TaskID:         "restock-" + branch + "-" + itemNo,
			BranchNo:       branch,
			ItemNo:         itemNo,
			ItemName:       sku.ItemName,
			SupplierName:   sku.SupplierName,
			CurrentStock:   sku.Stock,
			SafetyStock:    int(float64(sku.YesterdaySales) * 1.5),
			YesterdaySales: sku.YesterdaySales,
			SuggestQty:     finalQty,
			Reason:         reason,
			Priority:       prio,
		}
		created, err := s.Store.UpsertTask(ctx, task)
		if err != nil {
			log.Printf("[restock] upsert task %s: %v", itemNo, err)
			continue
		}

		// R3: 反馈缺货 → need_purchase
		if hasRecentShort {
			_ = s.Store.UpsertNeedPurchase(ctx, &NeedPurchase{
				BranchNo: branch, ItemNo: itemNo, ItemName: sku.ItemName,
				Barcode: sku.Barcode, SupplierName: sku.SupplierName,
				SuggestQty: finalQty, TriggerKind: TriggerShortFeedback,
				TriggerTaskID: task.TaskID,
			})
		}

		// 触发推送决策
		//   - 首次 created → 推
		//   - 6h 后 → 推(shouldRestock 内部已判断 R1 抑制)
		//   - 优先级 P0 → 推
		shouldPush := created ||
			(existing != nil && existing.LastPushAt != nil && now.Sub(*existing.LastPushAt) > 6*time.Hour) ||
			prio == PriorityP0

		if !shouldPush {
			continue
		}

		// 节流:每 tick 最多推 N 条
		if pushCount >= s.Cfg.MaxPushPerTick {
			log.Printf("[restock] max push reached (%d), skip %s", pushCount, itemNo)
			continue
		}

		// 推卖场群
		card := RenderFloorCard(&sku, finalQty, task.TaskID)
		if err := s.WeCom.SendAppChat(ctx, s.Cfg.WeComFloorChatID, card); err != nil {
			log.Printf("[restock] push floor %s: %v", itemNo, err)
		} else {
			_ = s.Store.MarkPushed(ctx, task.TaskID)
			pushCount++
		}

		// P0 推办公室群
		if prio == PriorityP0 {
			s.pushOffice(ctx, "below_safety", &sku, finalQty, task.TaskID)
		}
	}

	// 静默升级扫描(对所有 open task)
	for _, t := range openTasks {
		newPrio, escalated := ShouldEscalate(t, s.Cfg, now)
		if escalated {
			_, _ = s.Store.pool.Exec(ctx, `UPDATE restock_task SET priority=$2 WHERE task_id=$1`, t.TaskID, newPrio)
			log.Printf("[restock] task %s escalated %s→%s", t.TaskID, t.Priority, newPrio)
			// 升级触发推送
			if pushCount < s.Cfg.MaxPushPerTick {
				sku := &SkuSnapshot{
					BranchNo: t.BranchNo, ItemNo: t.ItemNo, ItemName: t.ItemName,
					Stock: t.CurrentStock, YesterdaySales: t.YesterdaySales,
					SupplierName: t.SupplierName,
				}
				s.pushOffice(ctx, "escalation", sku, t.SuggestQty, t.TaskID)
				pushCount++
			}
		}
	}

	log.Printf("[restock] HourlyTick done pushed=%d", pushCount)
	return nil
}

// LlmPlanTick 批量 LLM 算补货量
func (s *Service) LlmPlanTick(ctx context.Context) error {
	branch := s.Cfg.BranchNo
	salesMap, err := s.Cube.SalesYesterday(ctx, branch)
	if err != nil {
		return err
	}
	invMap, err := s.Cube.InventoryCurrent(ctx, branch)
	if err != nil {
		return err
	}
	promoMap, _ := s.Cube.Promotion7d(ctx, branch)

	// 收集当日触发补货的 SKU(open task 的)
	openTasks, _ := s.Store.ListOpenTasks(ctx, branch)
	skus := make([]*SkuSnapshot, 0, len(openTasks))
	defaultQty := make(map[string]int, len(openTasks))
	for _, t := range openTasks {
		sales, ok := salesMap[t.ItemNo]
		if !ok {
			continue
		}
		sku := *sales
		sku.Stock = invMap[t.ItemNo]
		sku.HasPromo7d = promoMap[t.ItemNo]
		skus = append(skus, &sku)
		defaultQty[t.ItemNo] = t.SuggestQty
	}

	log.Printf("[restock] LlmPlanTick: %d skus to plan", len(skus))
	return s.LLM.PlanBatch(ctx, skus, defaultQty)
}

// AggregateTick 21:30 聚合:把 pending need_purchase 推送汇总到办公室群 + 标可导出
func (s *Service) AggregateTick(ctx context.Context) error {
	branch := s.Cfg.BranchNo
	needs, err := s.Store.ListPendingNeeds(ctx, branch)
	if err != nil {
		return err
	}
	if len(needs) == 0 {
		log.Printf("[restock] AggregateTick: no pending needs")
		return nil
	}

	// 按 supplier 分组(暂不分组,简单汇总推送一条通知)
	log.Printf("[restock] AggregateTick: %d pending needs", len(needs))
	card := RenderOfficeCard("aggregate", &SkuSnapshot{
		BranchNo: branch, ItemName: fmt.Sprintf("%d 项待采购", len(needs)),
	}, len(needs), "")
	return s.WeCom.SendAppChat(ctx, s.Cfg.WeComOfficeChatID, card)
}

func (s *Service) pushOffice(ctx context.Context, event string, sku *SkuSnapshot, qty int, taskID string) {
	card := RenderOfficeCard(event, sku, qty, taskID)
	if err := s.WeCom.SendAppChat(ctx, s.Cfg.WeComOfficeChatID, card); err != nil {
		log.Printf("[restock] push office %s: %v", event, err)
	}
}

// ============== HTTP Handlers (供 router 调用) ==============

// RestockTasksList GET /api/v1/restock/tasks?status=open&limit=50
func RestockTasksList(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		status := c.DefaultQuery("status", "open")
		limit, _ := strconv.Atoi(c.Query("limit"))
		if limit <= 0 || limit > 500 {
			limit = 50
		}

		var tasks []*Task
		var err error
		if status == "open" {
			tasks, err = svc.Store.ListOpenTasks(ctx, svc.Cfg.BranchNo)
		} else {
			// 其它状态:简化处理,直接拉所有(后续可加 ListByStatus)
			rows, qErr := svc.Store.pool.Query(ctx, `
				SELECT task_id, branch_no, item_no, item_name, COALESCE(supplier_name,''),
					current_stock, safety_stock, yesterday_sales, suggest_qty,
					COALESCE(reason,''), priority, status, first_push_at, last_push_at,
					last_update_at, closed_at, COALESCE(closed_reason,''), push_count
				FROM restock_task WHERE branch_no=$1 AND status=$2
				ORDER BY last_update_at DESC LIMIT $3
			`, svc.Cfg.BranchNo, status, limit)
			if qErr != nil {
				err = qErr
			} else {
				defer rows.Close()
				for rows.Next() {
					t := &Task{}
					if err2 := rows.Scan(&t.TaskID, &t.BranchNo, &t.ItemNo, &t.ItemName, &t.SupplierName,
						&t.CurrentStock, &t.SafetyStock, &t.YesterdaySales, &t.SuggestQty,
						&t.Reason, &t.Priority, &t.Status, &t.FirstPushAt, &t.LastPushAt,
						&t.LastUpdateAt, &t.ClosedAt, &t.ClosedReason, &t.PushCount); err2 != nil {
						err = err2
						break
					}
					tasks = append(tasks, t)
				}
				if err == nil {
					err = rows.Err()
				}
			}
		}
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		if len(tasks) > limit {
			tasks = tasks[:limit]
		}
		c.JSON(200, gin.H{"tasks": tasks, "count": len(tasks), "status": status})
	}
}

// RestockNeedPurchaseList GET /api/v1/restock/need-purchase
func RestockNeedPurchaseList(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		needs, err := svc.Store.ListPendingNeeds(ctx, svc.Cfg.BranchNo)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"needs": needs, "count": len(needs)})
	}
}

// RestockManualTick POST /api/v1/restock/cron/tick
//   手动触发一次 HourlyTick(调试用)
func RestockManualTick(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
		defer cancel()
		if err := svc.HourlyTick(ctx); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "ts": time.Now().Unix()})
	}
}

// RestockLlmPlanNow GET /api/v1/restock/llm/plan
//   手动触发一次 LLM 批量规划(调试用)
func RestockLlmPlanNow(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Minute)
		defer cancel()
		if err := svc.LlmPlanTick(ctx); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "ts": time.Now().Unix()})
	}
}
