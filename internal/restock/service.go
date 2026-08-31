package restock

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tinkler/collect-ai/internal/auth"
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

	// ClsDict 启动时一次性从 siss_sales_demo cube 拉的分类字典
	//   clsno -> clsname (~150 行, 内存缓存)
	//   用 sync.RWMutex 保护, 启动后只读
	clsDict   map[string]string
	clsDictMu sync.RWMutex

	// ItemClsMap 启动时一次性从 items cube 拉的 SKU→分类映射
	//   item_no -> clsno (~26485 行, 内存缓存)
	//   H5 task API 用它直接拿到 task 的 item_clsno, 无需每次查 cube
	itemClsMap   map[string]string
	itemClsMapMu sync.RWMutex

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func NewService(
	cfg *RestockConfig,
	pool *pgxpool.Pool,
	cube *CubeQuerier,
	llm *LlmPlanner,
	wecom *WeCom,
) *Service {
	return &Service{
		Cfg:         cfg,
		Store:       NewStore(pool),
		Cube:        cube,
		LLM:         llm,
		WeCom:       wecom,
		clsDict:     map[string]string{},
		itemClsMap:  map[string]string{},
	}
}

// LoadClsDict 启动时一次性加载分类字典
//   来源: siss_sales_demo cube GROUP BY item_clsno/item_clsname
func (s *Service) LoadClsDict(ctx context.Context) error {
	if s.Cube == nil || s.Cube.agent == nil {
		return nil
	}
	rows, err := s.Cube.agent.Execute("siss_sales_demo",
		[]string{"siss_sales_demo.count"},
		[]string{"siss_sales_demo.item_clsno", "siss_sales_demo.item_clsname"},
		nil, nil, 2000)
	if err != nil {
		return err
	}
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		no := asString(r, "siss_sales_demo.item_clsno")
		name := asString(r, "siss_sales_demo.item_clsname")
		if no == "" || name == "" {
			continue
		}
		m[no] = name
	}
	s.clsDictMu.Lock()
	s.clsDict = m
	s.clsDictMu.Unlock()
	log.Printf("[restock] ClsDict loaded: %d entries", len(m))
	return nil
}

// LoadItemClsMap 启动时一次性加载 item_no → item_clsno 映射
//   来源: items cube (~26485 行)
//   启动时调一次, 之后 H5 task 查 0 cube 调用
func (s *Service) LoadItemClsMap(ctx context.Context) error {
	if s.Cube == nil || s.Cube.agent == nil {
		return nil
	}
	rows, err := s.Cube.agent.Execute("items",
		[]string{"items.count"},
		[]string{"items.item_no", "items.item_clsno"},
		nil, nil, 30000)
	if err != nil {
		return err
	}
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		ino := asString(r, "items.item_no")
		cno := asString(r, "items.item_clsno")
		if ino == "" || cno == "" {
			continue
		}
		// 第一个出现的为准 (去重)
		if _, exists := m[ino]; !exists {
			m[ino] = cno
		}
	}
	s.itemClsMapMu.Lock()
	s.itemClsMap = m
	s.itemClsMapMu.Unlock()
	log.Printf("[restock] ItemClsMap loaded: %d entries", len(m))
	return nil
}

// ClsNameOf 给一个 item_clsno 查 clsname (带锁)
func (s *Service) ClsNameOf(clsno string) string {
	if clsno == "" {
		return ""
	}
	s.clsDictMu.RLock()
	defer s.clsDictMu.RUnlock()
	return s.clsDict[clsno]
}

// ItemClsNoOf 给一个 item_no 查 clsno (带锁)
func (s *Service) ItemClsNoOf(itemNo string) string {
	if itemNo == "" {
		return ""
	}
	s.itemClsMapMu.RLock()
	defer s.itemClsMapMu.RUnlock()
	return s.itemClsMap[itemNo]
}

// Start 启动 3 个陈列补货调度 goroutine(自实现,避免依赖 robfig/cron)
//   - eve  (07:00): DisplayRestockTick(period=eve),窗口 昨日 20:30 ~ 今 07:00
//   - morn (12:00): DisplayRestockTick(period=morn),窗口 今 07:00 ~ 12:00
//   - aft  (20:30): DisplayRestockTick(period=aft),窗口 今 12:00 ~ 20:30
//   替代旧 HourlyTick / AggregateTick / LlmPlanTick
func (s *Service) Start() error {
	s.stopCh = make(chan struct{})

	// 启动时 + 每 1 小时刷新一次分类字典
	initCtx, initCancel := context.WithTimeout(context.Background(), 60*time.Second)
	if err := s.LoadClsDict(initCtx); err != nil {
		log.Printf("[restock] LoadClsDict init failed: %v (分类名会显示空, 不阻断)", err)
	}
	if err := s.LoadItemClsMap(initCtx); err != nil {
		log.Printf("[restock] LoadItemClsMap init failed: %v (item_clsno 会空, 不阻断)", err)
	}
	initCancel()
	s.wg.Add(1)
	go s.loopClsDictRefresh()

	// 启动 3 个 cron 调度(07:00 / 12:00 / 20:30)
	mkTick := func(period string) func(context.Context) error {
		return func(ctx context.Context) error { return s.DisplayRestockTick(ctx, period) }
	}

	eveH, eveM, err := parseHHMM(s.Cfg.DisplayRestockCronEve)
	if err != nil {
		return fmt.Errorf("parse DisplayRestockCronEve: %w", err)
	}
	s.wg.Add(1)
	go s.loopDaily(eveH, eveM, mkTick(PeriodEve), "DisplayRestockTick-"+PeriodEve)

	mornH, mornM, err := parseHHMM(s.Cfg.DisplayRestockCronMorn)
	if err != nil {
		return fmt.Errorf("parse DisplayRestockCronMorn: %w", err)
	}
	s.wg.Add(1)
	go s.loopDaily(mornH, mornM, mkTick(PeriodMorn), "DisplayRestockTick-"+PeriodMorn)

	aftH, aftM, err := parseHHMM(s.Cfg.DisplayRestockCronAft)
	if err != nil {
		return fmt.Errorf("parse DisplayRestockCronAft: %w", err)
	}
	s.wg.Add(1)
	go s.loopDaily(aftH, aftM, mkTick(PeriodAft), "DisplayRestockTick-"+PeriodAft)

	log.Printf("[restock] schedulers started: eve=%02d:%02d morn=%02d:%02d aft=%02d:%02d",
		eveH, eveM, mornH, mornM, aftH, aftM)
	return nil
}

// loopClsDictRefresh 每小时刷一次分类字典 (防 siss_sales_demo cube 数据变化)
func (s *Service) loopClsDictRefresh() {
	defer s.wg.Done()
	t := time.NewTicker(1 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			if err := s.LoadClsDict(ctx); err != nil {
				log.Printf("[restock] LoadClsDict refresh: %v", err)
			}
			if err := s.LoadItemClsMap(ctx); err != nil {
				log.Printf("[restock] LoadItemClsMap refresh: %v", err)
			}
			cancel()
		}
	}
}

func (s *Service) Stop() {
	s.stopOnce.Do(func() {
		if s.stopCh != nil {
			close(s.stopCh)
		}
	})
	s.wg.Wait()
}

// ============== 陈列补货新版 (2026-08-30,替代 HourlyTick/AggregateTick/LlmPlanTick) ==============

// DisplayRestockTick 核心闭环(7:00 / 12:00 / 20:30 各自跑一次)
//
// 步骤:
//   1. 拉窗口销售 + 库存快照(带重试 5s/15s/45s 指数退避)
//   2. 遍历每个有销售的 item
//   2a. 读 prev_inv(本次 tick 前的 inv_snapshot)→ UpsertDisplaySuggest 累加
//   2b. 短补状态机
//       - is_short=TRUE  → 覆盖 need_purchase(持续累加),且 current_inv > prev_inv 解除 short(不 close need_purchase)
//       - is_short=FALSE → 推送到卖场群(节流 DisplayRestockMaxPush)
//   3. 写 tick_log(成功或失败,启动时扫描 error 告警)
func (s *Service) DisplayRestockTick(ctx context.Context, period string) error {
	branch := s.Cfg.BranchNo
	if branch == "" {
		return fmt.Errorf("RESTOCK_BRANCH_NO 未配置")
	}
	now := time.Now()
	windowFrom, windowTo := computeWindow(period, now)

	log.Printf("[restock] DisplayRestockTick start period=%s window=[%s, %s] branch=%s",
		period, windowFrom.Format("15:04"), windowTo.Format("15:04"), branch)

	// 1. 拉窗口销售(带重试)
	retryMax := s.Cfg.DisplayRestockRetryMax
	if retryMax < 1 {
		retryMax = 3
	}
	var windowSales map[string]*WindowSaleRow
	var err error
	for attempt := 1; attempt <= retryMax; attempt++ {
		windowSales, err = s.Cube.SalesInWindow(ctx, branch, windowFrom, windowTo)
		if err == nil {
			break
		}
		if attempt == retryMax {
			s.recordTickLog(ctx, branch, period, windowFrom, windowTo, TickStatusError, err.Error(), 0)
			log.Printf("[restock] SalesInWindow final fail (attempt %d): %v", attempt, err)
			return err
		}
		backoff := time.Duration(attempt*attempt) * 5 * time.Second // 5s, 20s, 45s
		log.Printf("[restock] SalesInWindow attempt %d failed: %v, retry in %v", attempt, err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}

	// 2. 遍历每个有销售的 item
	pushCount := 0
	maxPush := s.Cfg.DisplayRestockMaxPush
	if maxPush < 1 {
		maxPush = 30
	}
	// 2026-08-31: 加诊断 log — 区分 "items 拉到了" 和 "实际写入"
	//   items=45 但 DB 没数据 → 多半是 ws.SaleQty 都 <= 0 被过滤掉
	skipZeroQtyCount := 0
	writeCount := 0
	saleQtySum := 0.0

	for itemNo, ws := range windowSales {
		// 2026-08-31: 业务过滤 — < 0.5 件忽略(0.1, 0.2 件太碎,不值得补货)
		//   0.5 件及以上 Round 取整: 0.6→1, 1.3→1, 0.9→1, 1.5→2
		//   0 件 / 退货 (-N) 也不补
		if ws.SaleQty < 0.5 {
			skipZeroQtyCount++
			continue
		}
		// 取整:四舍五入
		effQty := int(math.Round(ws.SaleQty))
		if effQty < 1 {
			effQty = 1
		}
		saleQtySum += ws.SaleQty

		// 2a. 读 prev_inv(本次 tick 前)→ UpsertDisplaySuggest 累加
		prev, _ := s.Store.GetDisplaySuggest(ctx, branch, itemNo, now)
		prevInv := 0
		if prev != nil {
			prevInv = prev.InvSnapshot
		}

		if err := s.Store.UpsertDisplaySuggest(ctx, &DisplaySuggest{
			BranchNo:    branch,
			ItemNo:      itemNo,
			PeriodDate:  now,
			InvSnapshot: ws.InvSnapshot,
			LastPeriod:  period,
		}, effQty); err != nil {
			log.Printf("[restock] UpsertDisplaySuggest %s: %v", itemNo, err)
			continue
		}
		writeCount++

		// 2b. 短补状态机
		ss, _ := s.Store.GetShortState(ctx, branch, itemNo)
		isShort := ss != nil && ss.IsShort

		if isShort {
			// 2b-1. 覆盖 need_purchase(用当前 display_suggest.suggest_qty,持续累加)
			dsp, _ := s.Store.GetDisplaySuggest(ctx, branch, itemNo, now)
			curQty := 0
			if dsp != nil {
				curQty = dsp.SuggestQty
			}
			if curQty > 0 {
				if err := s.Store.UpsertNeedPurchaseFromDisplay(ctx, &NeedPurchase{
					BranchNo:     branch,
					ItemNo:       itemNo,
					ItemName:     ws.ItemName,
					Barcode:      ws.Barcode,
					SupplierName: ws.SupplierName,
					SuggestQty:   curQty,
					TriggerKind:  TriggerDisplayShort,
				}); err != nil {
					log.Printf("[restock] UpsertNeedPurchase %s: %v", itemNo, err)
				}
			}

			// 2b-2. 解除 short:current_inv > 上次 tick 时的 inv_snapshot
			// need_purchase 不 close(选项 A,等员工点完成时清 0)
			if ws.InvSnapshot > prevInv {
				if err := s.Store.ClearShortState(ctx, branch, itemNo); err != nil {
					log.Printf("[restock] ClearShortState %s: %v", itemNo, err)
				} else {
					log.Printf("[restock] short cleared: %s inv %d→%d (purchase kept pending)",
						itemNo, prevInv, ws.InvSnapshot)
				}
			}
			// 短补中不推 floor(已经走采购流程,推了反而干扰员工)
		} else {
			// 2b-3. 未短补,推送到卖场群(节流)
			if pushCount >= maxPush {
				continue
			}
			sku := &SkuSnapshot{
				BranchNo:     branch,
				ItemNo:       itemNo,
				ItemName:     ws.ItemName,
				Barcode:      ws.Barcode,
				SupplierName: ws.SupplierName,
				Stock:        ws.InvSnapshot,
			}
			card := RenderFloorCard(sku, int(math.Round(ws.SaleQty)), "display-"+branch+"-"+itemNo)
			if err := s.WeCom.SendAppChat(ctx, "floor", card); err != nil {
				log.Printf("[restock] push floor %s: %v", itemNo, err)
			} else {
				pushCount++
			}
		}
	}

	// 3. tick_log
	s.recordTickLog(ctx, branch, period, windowFrom, windowTo, TickStatusOK, "", len(windowSales))
	log.Printf("[restock] DisplayRestockTick done period=%s items=%d skipped_zero_qty=%d written=%d total_sale_qty=%.2f pushed=%d",
		period, len(windowSales), skipZeroQtyCount, writeCount, saleQtySum, pushCount)
	return nil
}

// computeWindow 根据 period 计算本次 tick 的销售窗口
//   eve  (07:00 tick): 昨日 20:30 ~ 今 07:00 (10.5h,跨天)
//   morn (12:00 tick): 今 07:00 ~ 12:00 (5h)
//   aft  (20:30 tick): 今 12:00 ~ 20:30 (8.5h)
//   manual(手动重跑): 最近 1h 兜底
func computeWindow(period string, now time.Time) (from, to time.Time) {
	loc := now.Location()
	switch period {
	case PeriodEve:
		to = time.Date(now.Year(), now.Month(), now.Day(), 7, 0, 0, 0, loc)
		from = to.Add(-10*time.Hour - 30*time.Minute)
	case PeriodMorn:
		from = time.Date(now.Year(), now.Month(), now.Day(), 7, 0, 0, 0, loc)
		to = time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, loc)
	case PeriodAft:
		from = time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, loc)
		to = time.Date(now.Year(), now.Month(), now.Day(), 20, 30, 0, 0, loc)
	default: // manual: 兜底 1h 窗口
		to = now
		from = now.Add(-1 * time.Hour)
	}
	return
}

// recordTickLog 记一次 tick 结果(成功 / 失败都记)
func (s *Service) recordTickLog(ctx context.Context, branch, period string, from, to time.Time, status, errMsg string, itemsCount int) {
	err := s.Store.RecordTickLog(ctx, &TickLog{
		BranchNo:   branch,
		Period:     period,
		TickAt:     time.Now(),
		WindowFrom: from,
		WindowTo:   to,
		Status:     status,
		ErrorMsg:   errMsg,
		ItemsCount: itemsCount,
	})
	if err != nil {
		log.Printf("[restock] RecordTickLog failed: %v", err)
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
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			if err := fn(ctx); err != nil {
				log.Printf("[restock] %s: %v", name, err)
			}
			cancel()
		}
	}
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
		if err := s.WeCom.SendAppChat(ctx, "floor", card); err != nil {
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
	return s.WeCom.SendAppChat(ctx, "office", card)
}

func (s *Service) pushOffice(ctx context.Context, event string, sku *SkuSnapshot, qty int, taskID string) {
	card := RenderOfficeCard(event, sku, qty, taskID)
	if err := s.WeCom.SendAppChat(ctx, "office", card); err != nil {
		log.Printf("[restock] push office %s: %v", event, err)
	}
}

// OnButtonClick 企微按钮点击(2026-08-30 新版,陈列补货建议)
//
// 解析 eventKey:
//   - "short"  → 员工点缺货(ONCE 锁定,已 short 的静默 ACK)
//   - "done"   → 员工点完成(清 0 suggest_qty + 解除 short + close need_purchase)
//
// 参数:
//   - reqID:   原 callback event 的 req_id,用于 aibot_respond_update_msg 帧(5 秒内)
//   - chatID:  群 ID
//   - userID:  员工企微 user_id
//   - taskID:  旧版兼容字段,新版从 eventKey 解析(格式 "display-<branch>-<item>")
//   - eventKey:"short" / "done" / "display-<branch>-<item>:short" / "display-<branch>-<item>:done"
func (s *Service) OnButtonClick(reqID, chatID, userID, taskID, eventKey string) {
	kind, branch, itemNo := parseDisplayButtonKey(eventKey)
	if kind == "" {
		log.Printf("[restock] OnButtonClick: unparsed eventKey=%q taskID=%q, treat as done", eventKey, taskID)
		kind = FeedbackDone
	}
	if branch == "" || itemNo == "" {
		// 退化:从 taskID 解析("display-<branch>-<item>" 格式)
		branch, itemNo = parseDisplayTaskID(taskID)
	}
	if branch == "" {
		branch = s.Cfg.BranchNo
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	now := time.Now()

	// 2026-08-30 修: parseDisplayButtonKey 返小写 ("short"/"done"), 跟 FeedbackDone/FeedbackShort 大写常量不匹配
	//   用 .ToUpper() 兼容 (旧 eventKey DONE|SHORT 也是大写, 都过)
	switch strings.ToUpper(kind) {
	case FeedbackShort:
		s.handleShortClick(ctx, reqID, chatID, userID, branch, itemNo, now)
	case FeedbackDone:
		s.handleDoneClick(ctx, reqID, chatID, userID, branch, itemNo, now)
	default:
		log.Printf("[restock] OnButtonClick: unknown kind=%q (eventKey=%q), ignore", kind, eventKey)
	}
}

// handleShortClick 员工点缺货
//   - 幂等检查:已 short 的静默 ACK
//   - suggest_qty=0 的无需标(没销售 = 没缺货需求)
//   - 写 short_state(is_short=TRUE)
//   - 立即 upsert need_purchase(suggest_qty = current display_suggest.suggest_qty)
func (s *Service) handleShortClick(ctx context.Context, reqID, chatID, userID, branch, itemNo string, now time.Time) {
	// 1) 幂等检查
	ss, _ := s.Store.GetShortState(ctx, branch, itemNo)
	if ss != nil && ss.IsShort {
		log.Printf("[restock] short already set: branch=%s item=%s (silent ack)", branch, itemNo)
		s.ackCard(reqID, "已标记缺货")
		return
	}

	// 2) 拿当前 display_suggest.suggest_qty(没有销售 = 无需标)
	dsp, _ := s.Store.GetDisplaySuggest(ctx, branch, itemNo, now)
	curQty := 0
	if dsp != nil {
		curQty = dsp.SuggestQty
	}
	if curQty == 0 {
		log.Printf("[restock] short clicked but suggest_qty=0: branch=%s item=%s (silent ack)", branch, itemNo)
		s.ackCard(reqID, "暂无销售,无需标记缺货")
		return
	}

	// 3) 写 short_state
	if err := s.Store.SetShortState(ctx, branch, itemNo, userID, true); err != nil {
		log.Printf("[restock] SetShortState %s: %v", itemNo, err)
		s.ackCard(reqID, "标记失败")
		return
	}

	// 4) 立即 upsert need_purchase(避免空窗期)
	_ = s.Store.UpsertNeedPurchaseFromDisplay(ctx, &NeedPurchase{
		BranchNo:     branch,
		ItemNo:       itemNo,
		ItemName:     ifStr(dsp != nil, dsp.ItemName, ""),
		SuggestQty:   curQty,
		TriggerKind:  TriggerDisplayShort,
	})

	// 5) 写 Feedback(审计)
	_ = s.Store.InsertFeedback(ctx, &Feedback{
		TaskID:       "display-" + branch + "-" + itemNo,
		FeedbackType: FeedbackShort,
		FeedbackUser: userID,
		FeedbackTime: now,
	})

	log.Printf("[restock] short set: branch=%s item=%s qty=%d user=%s", branch, itemNo, curQty, userID)
	s.ackCard(reqID, "已标记缺货")
}

// handleDoneClick 员工点完成
//   - 清 0 display_suggest.suggest_qty(当日)
//   - 解除 short_state(允许下一轮短补)
//   - close need_purchase(pending → cancelled, suggest_qty=0)
func (s *Service) handleDoneClick(ctx context.Context, reqID, chatID, userID, branch, itemNo string, now time.Time) {
	// 1) 清 display_suggest.suggest_qty
	if err := s.Store.ClearDisplaySuggestQty(ctx, branch, itemNo, now); err != nil {
		log.Printf("[restock] ClearDisplaySuggestQty %s: %v", itemNo, err)
	}

	// 2) 解除 short_state
	if err := s.Store.ClearShortState(ctx, branch, itemNo); err != nil {
		log.Printf("[restock] ClearShortState %s: %v", itemNo, err)
	}

	// 3) close need_purchase(pending → cancelled, suggest_qty=0;sent_to_supplier 不动)
	if err := s.Store.ClearNeedPurchase(ctx, branch, itemNo); err != nil {
		log.Printf("[restock] ClearNeedPurchase %s: %v", itemNo, err)
	}

	// 4) 写 Feedback(审计)
	_ = s.Store.InsertFeedback(ctx, &Feedback{
		TaskID:       "display-" + branch + "-" + itemNo,
		FeedbackType: FeedbackDone,
		FeedbackUser: userID,
		FeedbackTime: now,
	})

	log.Printf("[restock] done click: branch=%s item=%s user=%s (purchase cancelled)", branch, itemNo, userID)
	s.ackCard(reqID, "已补货")
}

// ackCard in-place 更新原卡片为"已确认"无按钮版(必须在 5 秒内)
//   用 RenderFloorCardAfterConfirm 复用旧版的渲染逻辑
func (s *Service) ackCard(reqID, msg string) {
	if reqID == "" {
		log.Printf("[restock] ack card skipped: no req_id")
		return
	}
	go func() {
		// 简化:直接发一个 ack 文本卡(5s 内必须到)
		ackCard := []byte(`{"msgtype":"text","text":{"content":"` + msg + `"}}`)
		updCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		if err := s.WeCom.SendUpdateCard(updCtx, reqID, ackCard); err != nil {
			log.Printf("[restock] ack card failed: %v (req_id=%s msg=%s)", err, reqID, msg)
		}
	}()
}

// parseDisplayButtonKey 解析新版 eventKey (2026-08-30 兼容旧版)
//   输入:
//     新版 (陈列补货):
//       "short" / "done"
//       "display-<branch>-<item>:short" / ":done"
//     旧版 (兼容, 不删):
//       "restock-<branch>-<item>:DONE" / ":SHORT"
//       "DONE|<uuid>" / "SHORT|<uuid>"  (旧卡片 button_list 格式)
//   输出: (kind, branch, itemNo) — kind 已统一小写
func parseDisplayButtonKey(eventKey string) (kind, branch, itemNo string) {
	if eventKey == "" {
		return "", "", ""
	}
	// 1) 裸 short / done
	low := strings.ToLower(strings.TrimSpace(eventKey))
	if low == "short" || low == "done" {
		return low, "", ""
	}
	// 2) 旧版 "DONE|<uuid>" / "SHORT|<uuid>" — 拿不到 branch/item, 留给 OnButtonClick 用 taskID 退化
	if strings.HasPrefix(eventKey, "DONE|") || strings.HasPrefix(eventKey, "SHORT|") ||
		strings.HasPrefix(eventKey, "done|") || strings.HasPrefix(eventKey, "short|") {
		idx := strings.Index(eventKey, "|")
		if idx < 0 {
			return "", "", ""
		}
		return strings.ToLower(eventKey[:idx]), "", ""
	}
	// 3) "<prefix>:<kind>" — 通用 (display- / restock- 都适用)
	idx := strings.LastIndex(eventKey, ":")
	if idx < 0 {
		return "", "", ""
	}
	prefix := eventKey[:idx]
	suffix := strings.ToLower(eventKey[idx+1:])
	if suffix != "short" && suffix != "done" {
		return "", "", ""
	}
	// 3a) 新版 display-<branch>-<item>
	prefix = strings.TrimPrefix(prefix, "display-")
	// 3b) 旧版 restock-<branch>-<item>
	prefix = strings.TrimPrefix(prefix, "restock-")
	if prefix == eventKey[:idx] {
		// 既不是 display- 也不是 restock- 前缀, 不识别
		return "", "", ""
	}
	// 最后一个 '-' 分割 branch 和 item
	sep := strings.LastIndex(prefix, "-")
	if sep < 0 {
		return suffix, prefix, ""
	}
	return suffix, prefix[:sep], prefix[sep+1:]
}

// parseDisplayTaskID 从 taskID "display-<branch>-<item>" 解析 (branch, item)
func parseDisplayTaskID(taskID string) (branch, itemNo string) {
	prefix := strings.TrimPrefix(taskID, "display-")
	sep := strings.LastIndex(prefix, "-")
	if sep < 0 {
		return "", ""
	}
	return prefix[:sep], prefix[sep+1:]
}

// hasPermForUser 查 user 的主 role 是否含指定 perm
//   2026-08-30: 加的 helper, 给 RestockFeedback 按 kind 拆 perm 用
//   走主 role 即可 (auth.HasPerm 一致), 不并集 user_roles
func (s *Service) hasPermForUser(ctx context.Context, userID, perm string) bool {
	if userID == "" {
		return false
	}
	var role string
	if err := s.Store.pool.QueryRow(ctx, `SELECT COALESCE(role,'') FROM users WHERE id=$1`, userID).Scan(&role); err != nil {
		return false
	}
	if role == "" {
		return false
	}
	// 调 auth.HasPerm (从 users.role 查)
	return auth.HasPerm(role, perm)
}

// ifStr 三元 helper(b == true 返 a,否则 "")
func ifStr(b bool, a, _ string) string {
	if b {
		return a
	}
	return ""
}

// ============== HTTP Handlers (供 router 调用) ==============

// RestockTasksList 已在 http_h5.go 中重写 (2026-08-30 新版陈列补货数据源)

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

// RestockManualTick POST /api/v1/restock/cron/tick?period=morn
//   手动触发一次 DisplayRestockTick(调试用,默认 period="manual" 拉最近 1h 窗口)
func RestockManualTick(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		period := c.DefaultQuery("period", "manual")
		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
		defer cancel()
		if err := svc.DisplayRestockTick(ctx, period); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "period": period, "ts": time.Now().Unix()})
	}
}

// RestockLlmPlanNow 旧版 LLM 手动重跑,新版陈列补货已不再用 LLM,保留路由占位
func RestockLlmPlanNow(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(410, gin.H{"error": "陈列补货新版不再用 LLM,改用 RestockManualTick?period=morn"})
	}
}

// RestockListChats GET /api/v1/restock/wecom/chats
//   列出所有已发现/已绑定的 chat_id
func RestockListChats(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		chats := svc.WeCom.ListChats()
		connected := svc.WeCom.IsConnected()
		c.JSON(200, gin.H{
			"chats":     chats,
			"count":     len(chats),
			"connected": connected,
		})
	}
}

// RestockBindChat POST /api/v1/restock/wecom/chats/bind
//   body: {"chat_id":"wrXXX", "role":"floor"|"office"|""}
//   把 chat_id 绑定到 floor/office 角色,服务重启后从 WECOM_BIND_FILE 恢复
func RestockBindChat(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			ChatID string `json:"chat_id"`
			Role   string `json:"role"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
			return
		}
		if req.ChatID == "" {
			c.JSON(400, gin.H{"error": "chat_id 必填"})
			return
		}
		if err := svc.WeCom.BindChat(req.ChatID, req.Role); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "chat_id": req.ChatID, "role": req.Role})
	}
}

// RestockTestChat POST /api/v1/restock/wecom/chats/test
//   body: {"chat_id":"wrXXX" or "role":"floor", "text":"测试消息"}
//   主动发一条测试消息(验证绑定是否对、机器人能否推到群)
func RestockTestChat(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			ChatID string `json:"chat_id"`
			Role   string `json:"role"`
			Text   string `json:"text"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
			return
		}
		target := req.ChatID
		if target == "" {
			target = req.Role
		}
		if target == "" {
			c.JSON(400, gin.H{"error": "chat_id 或 role 至少填一个"})
			return
		}
		text := req.Text
		if text == "" {
			text = "🛒 商超 AI 机器人测试消息 - " + time.Now().Format("15:04:05")
		}

		card := map[string]any{
			"msgtype": "markdown",
			"markdown": map[string]any{
				"content": text,
			},
		}
		body, _ := json.Marshal(card)

		ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
		defer cancel()
		if err := svc.WeCom.SendAppChat(ctx, target, body); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "sent_to": target})
	}
}

// RestockBulkBindChat POST /api/v1/restock/wecom/chats/bulk-bind
//   body: {"bindings":[{"chat_id":"wrXXX","role":"floor"},...]}
//   一次性批量绑定(适合从 .env 导入或脚本部署)
func RestockBulkBindChat(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Bindings []struct {
				ChatID string `json:"chat_id"`
				Role   string `json:"role"`
			} `json:"bindings"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "bad json: " + err.Error()})
			return
		}
		if len(req.Bindings) == 0 {
			c.JSON(400, gin.H{"error": "bindings 数组不能为空"})
			return
		}
		var ok, fail int
		var errs []string
		for _, b := range req.Bindings {
			if b.ChatID == "" {
				continue
			}
			if err := svc.WeCom.BindChat(b.ChatID, b.Role); err != nil {
				fail++
				errs = append(errs, b.ChatID+": "+err.Error())
			} else {
				ok++
			}
		}
		c.JSON(200, gin.H{
			"ok":      ok,
			"fail":    fail,
			"errors":  errs,
		})
	}
}
