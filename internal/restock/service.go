package restock

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tinkler/collect-ai/internal/model"
)

// asAnyString 业务字段值 → string (适配 map[string]any 业务响应)
//   2026-09-02 加,LoadItemDict 走 Gateway 业务字段名后,row 是 map[key]any
//   不是 cube 物理响应 (map[physicalRef]any)
func asAnyString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// Service restock 模块主服务 (2026-09-02 重构)
//
// 精简后只保留:
//   - DisplayRestockTick  3 次 cron 拉 cube → 写 display_suggest
//   - HandleFeedback      H5 反馈入口 (DONE/SHORT)
//
// 删掉:
//   - 企微双群推送 (SendAppChat / RenderFloorCard / RenderOfficeCard)
//   - LLM 批量调量 (LlmPlanner)
//   - 旧 ROP 触发 (HourlyTick)
//   - 21:30 汇总 (AggregateTick)
//   - 旧优先级计算 (computePriority / ShouldEscalate)
type Service struct {
	Cfg          *RestockConfig
	Store        *Store
	Cube         *CubeQuerier
	clsNoDict    map[string]string // item_no -> cls_no
	clsNameDict  map[string]string // cls_no -> cls_name
	itemUnitDict map[string]string // item_no -> unit_no
	itemLock     sync.RWMutex
}

// NewService 构造
func NewService(cfg *RestockConfig, pool *pgxpool.Pool, cube *CubeQuerier) *Service {
	return &Service{
		Cfg:          cfg,
		Store:        NewStore(pool),
		Cube:         cube,
		clsNoDict:    make(map[string]string),
		clsNameDict:  make(map[string]string),
		itemUnitDict: make(map[string]string),
	}
}

// ============== Start / Stop (cron 调度) ==============

// Start 启动 cron (3 次 tick)
func (s *Service) Start() error {
	if s.Cfg.BranchNo == "" {
		return fmt.Errorf("RESTOCK_BRANCH_NO 未配置")
	}
	ctx, cancel := context.WithCancel(context.Background())
	_ = cancel // 2026-09-02: Stop 暂不取消 (跟 main 进程同生命周期)

	eveH, eveM, err := parseHHMM(s.Cfg.CronEve)
	if err != nil {
		return fmt.Errorf("parse CronEve: %w", err)
	}
	mornH, mornM, err := parseHHMM(s.Cfg.CronMorn)
	if err != nil {
		return fmt.Errorf("parse CronMorn: %w", err)
	}
	aftH, aftM, err := parseHHMM(s.Cfg.CronAft)
	if err != nil {
		return fmt.Errorf("parse CronAft: %w", err)
	}

	go s.loopDaily(ctx, eveH, eveM, func(ctx context.Context) error {
		return s.DisplayRestockTick(ctx, PeriodEve)
	}, "DisplayRestockTick-"+PeriodEve)

	go s.loopDaily(ctx, mornH, mornM, func(ctx context.Context) error {
		return s.DisplayRestockTick(ctx, PeriodMorn)
	}, "DisplayRestockTick-"+PeriodMorn)

	go s.loopDaily(ctx, aftH, aftM, func(ctx context.Context) error {
		return s.DisplayRestockTick(ctx, PeriodAft)
	}, "DisplayRestockTick-"+PeriodAft)

	log.Printf("[restock] cron started: eve=%02d:%02d morn=%02d:%02d aft=%02d:%02d branch=%s",
		eveH, eveM, mornH, mornM, aftH, aftM, s.Cfg.BranchNo)
	return nil
}

// Stop 停止 cron
func (s *Service) Stop() {
	// 2026-09-02: cron goroutine 用 ctx 控制,这里只打 log
	// 真实生产: 用 sync.WaitGroup 等待退出
	log.Printf("[restock] service stopped")
}

// loopDaily 每日定时 HH:MM 跑 fn
func (s *Service) loopDaily(ctx context.Context, hour, minute int, fn func(context.Context) error, name string) {
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	wait := time.Until(next)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	for {
		if err := fn(ctx); err != nil {
			log.Printf("[restock] %s err: %v", name, err)
		}
		timer.Reset(24 * time.Hour)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

func parseHHMM(spec string) (int, int, error) {
	// spec 格式: "HH:MM" 或空
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0, 0, fmt.Errorf("empty spec")
	}
	var h, m int
	if _, err := fmt.Sscanf(spec, "%d:%d", &h, &m); err != nil {
		return 0, 0, err
	}
	return h, m, nil
}

// ============== DisplayRestockTick 核心 ==============

// DisplayRestockTick 核心闭环(07:00 / 12:00 / 20:30 各跑一次, 或手动)
//
// 步骤:
//   1. 拉窗口销售 (带重试)
//   2. 遍历每个有销售 item
//      - < 0.5 件忽略
//      - Round 后 ≥1 → 累加 display_suggest
//      - 短补状态机: is_short=TRUE → 覆盖 need_purchase + 检查解除
//   3. 写 tick_log
//
// 2026-09-02 重构: 删 floor 推群步骤, 不再 SendAppChat
func (s *Service) DisplayRestockTick(ctx context.Context, period string) error {
	branch := s.Cfg.BranchNo
	if branch == "" {
		return fmt.Errorf("RESTOCK_BRANCH_NO 未配置")
	}
	now := time.Now()
	windowFrom, windowTo := computeWindow(period, now)

	log.Printf("[restock] DisplayRestockTick start period=%s window=[%s, %s] branch=%s",
		period, windowFrom.Format("15:04"), windowTo.Format("15:04"), branch)

	// 1. 拉窗口销售 (带重试)
	retryMax := s.Cfg.RetryMax
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
	skipZeroQtyCount := 0
	writeCount := 0
	saleQtySum := 0.0

	for itemNo, ws := range windowSales {
		// < 0.5 件忽略
		if ws.SaleQty < 0.5 {
			skipZeroQtyCount++
			continue
		}
		// Round 取整
		effQty := int(math.Round(ws.SaleQty))
		if effQty < 1 {
			effQty = 1
		}
		saleQtySum += ws.SaleQty

		// 读 prev_inv (本 tick 前)
		prev, _ := s.Store.GetDisplaySuggest(ctx, branch, itemNo, now)
		prevInv := 0
		if prev != nil {
			prevInv = prev.InvSnapshot
		}

		// 累加 suggest_qty
		if err := s.Store.UpsertDisplaySuggest(ctx, &DisplaySuggest{
			BranchNo:    branch,
			ItemNo:      itemNo,
			ItemName:    ws.ItemName,
			PeriodDate:  now,
			InvSnapshot: ws.InvSnapshot,
			LastPeriod:  period,
		}, effQty); err != nil {
			log.Printf("[restock] UpsertDisplaySuggest %s: %v", itemNo, err)
			continue
		}
		writeCount++

		// 短补状态机
		ss, _ := s.Store.GetShortState(ctx, branch, itemNo)
		isShort := ss != nil && ss.IsShort

		if isShort {
			// 持续覆盖 need_purchase
			dsp, _ := s.Store.GetDisplaySuggest(ctx, branch, itemNo, now)
			curQty := 0
			if dsp != nil {
				curQty = dsp.SuggestQty
			}
			if curQty > 0 {
				if err := s.Store.UpsertNeedPurchase(ctx, &PurchasePlan{
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

			// 解除 short: current_inv > prev_inv
			// need_purchase 不 close (等员工点完成时清 0)
			if ws.InvSnapshot > prevInv {
				if err := s.Store.ClearShortState(ctx, branch, itemNo); err != nil {
					log.Printf("[restock] ClearShortState %s: %v", itemNo, err)
				} else {
					log.Printf("[restock] short cleared: %s inv %d→%d (purchase kept pending)",
						itemNo, prevInv, ws.InvSnapshot)
				}
			}
		}
		// 2026-09-02 重构: 删 floor 推群逻辑
	}

	// 3. tick_log
	s.recordTickLog(ctx, branch, period, windowFrom, windowTo, TickStatusOK, "", len(windowSales))
	log.Printf("[restock] DisplayRestockTick done period=%s items=%d skipped_zero_qty=%d written=%d total_sale_qty=%.2f",
		period, len(windowSales), skipZeroQtyCount, writeCount, saleQtySum)
	return nil
}

// computeWindow 根据 period 算销售窗口
//   eve  : 昨日 20:30 ~ 今 07:00 (10.5h, 跨天)
//   morn : 今 07:00 ~ 12:00 (5h)
//   aft  : 今 12:00 ~ 20:30 (8.5h)
//   manual: 最近 1h
func computeWindow(period string, now time.Time) (time.Time, time.Time) {
	loc := now.Location()
	switch period {
	case PeriodEve:
		// 昨日 20:30
		from := time.Date(now.Year(), now.Month(), now.Day()-1, 20, 30, 0, 0, loc)
		// 今 07:00
		to := time.Date(now.Year(), now.Month(), now.Day(), 7, 0, 0, 0, loc)
		return from, to
	case PeriodMorn:
		from := time.Date(now.Year(), now.Month(), now.Day(), 7, 0, 0, 0, loc)
		to := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, loc)
		return from, to
	case PeriodAft:
		from := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, loc)
		to := time.Date(now.Year(), now.Month(), now.Day(), 20, 30, 0, 0, loc)
		return from, to
	default: // manual
		from := now.Add(-1 * time.Hour)
		return from, now
	}
}

func (s *Service) recordTickLog(ctx context.Context, branch, period string, from, to time.Time, status, errMsg string, itemsCount int) {
	_ = s.Store.RecordTickLog(ctx, &TickLog{
		BranchNo:   branch,
		Period:     period,
		TickAt:     time.Now(),
		WindowFrom: from,
		WindowTo:   to,
		Status:     status,
		ErrorMsg:   errMsg,
		ItemsCount: itemsCount,
	})
}

// ============== HandleFeedback 员工反馈入口 ==============

// HandleFeedback H5 反馈入口 (DONE/SHORT)
//   2026-09-02 重构: 重命名自 OnButtonClick, 适配 H5 反馈 (不依赖企微 callback)
func (s *Service) HandleFeedback(ctx context.Context, branchNo, itemNo, userID, kind string) error {
	if branchNo == "" {
		branchNo = s.Cfg.BranchNo
	}
	if branchNo == "" || itemNo == "" {
		return fmt.Errorf("branch / item_no required")
	}

	now := time.Now()

	switch kind {
	case FeedbackShort:
		return s.handleShortClick(ctx, branchNo, itemNo, userID, now)
	case FeedbackDone:
		return s.handleDoneClick(ctx, branchNo, itemNo, userID, now)
	default:
		return fmt.Errorf("unknown kind: %s", kind)
	}
}

// handleShortClick 员工点缺货
//   - 幂等: 已 short 静默 ACK
//   - suggest_qty=0 的无需标
//   - 写 short_state + upsert need_purchase
func (s *Service) handleShortClick(ctx context.Context, branch, itemNo, userID string, now time.Time) error {
	// 1) 幂等检查
	ss, _ := s.Store.GetShortState(ctx, branch, itemNo)
	if ss != nil && ss.IsShort {
		log.Printf("[restock] short already set: branch=%s item=%s (silent ack)", branch, itemNo)
		return nil
	}

	// 2) 拿当前 suggest_qty (0 = 无销售, 无需标)
	curQty := 0
	// 跨天累加: ListActiveItems 已经把多日期 SUM 了, 但这里单条要算累计需要再聚合
	// 简化: GetDisplaySuggest 拿最新一行, 跨天累加场景建议加个 GetCurrentSuggestQty
	dsp, _ := s.Store.GetDisplaySuggest(ctx, branch, itemNo, now)
	if dsp != nil {
		curQty = dsp.SuggestQty
	}
	// 跨天累加: 拉所有日期行 SUM
	if dsp != nil {
		curQty = s.sumSuggestQtyAllDates(ctx, branch, itemNo)
	}

	if curQty == 0 {
		log.Printf("[restock] short clicked but suggest_qty=0: branch=%s item=%s (silent ack)", branch, itemNo)
		return nil
	}

	// 3) 写 short_state
	if err := s.Store.SetShortState(ctx, branch, itemNo, userID, true); err != nil {
		return fmt.Errorf("SetShortState: %w", err)
	}

	// 4) upsert need_purchase
	_ = s.Store.UpsertNeedPurchase(ctx, &PurchasePlan{
		BranchNo:    branch,
		ItemNo:      itemNo,
		SuggestQty:  curQty,
		TriggerKind: TriggerDisplayShort,
	})

	log.Printf("[restock] short set: branch=%s item=%s qty=%d user=%s", branch, itemNo, curQty, userID)
	return nil
}

// sumSuggestQtyAllDates 跨天累加 suggest_qty
func (s *Service) sumSuggestQtyAllDates(ctx context.Context, branch, itemNo string) int {
	var total int
	err := s.Store.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(suggest_qty), 0)
		FROM restock_display_suggest
		WHERE branch_no=$1 AND item_no=$2
	`, branch, itemNo).Scan(&total)
	if err != nil {
		log.Printf("[restock] sumSuggestQtyAllDates: %v", err)
		return 0
	}
	return total
}

// handleDoneClick 员工点完成
//   - 清 display_suggest.suggest_qty (跨天所有日期行)
//   - 解除 short_state
//   - close need_purchase (pending → cancelled)
func (s *Service) handleDoneClick(ctx context.Context, branch, itemNo, userID string, now time.Time) error {
	// 1) 清 display_suggest.suggest_qty (跨天所有)
	if _, err := s.Store.pool.Exec(ctx, `
		UPDATE restock_display_suggest
		SET suggest_qty = 0, last_update_at = NOW()
		WHERE branch_no=$1 AND item_no=$2
	`, branch, itemNo); err != nil {
		log.Printf("[restock] clear display_suggest %s: %v", itemNo, err)
	}

	// 2) 解除 short_state
	if err := s.Store.ClearShortState(ctx, branch, itemNo); err != nil {
		log.Printf("[restock] ClearShortState %s: %v", itemNo, err)
	}

	// 3) close need_purchase
	if err := s.Store.ClearNeedPurchase(ctx, branch, itemNo); err != nil {
		log.Printf("[restock] ClearNeedPurchase %s: %v", itemNo, err)
	}

	log.Printf("[restock] done click: branch=%s item=%s user=%s (purchase cancelled)", branch, itemNo, userID)
	return nil
}

// ============== AttachPlanQtyToRows 采购收货单反查建议量 ==============

// AttachPlanQtyToRows 采购收货单按 supplier 反查 restock_need_purchase
//   按 row.MatchedBarcode (或 raw_barcode) 匹配 plan_item_no / plan_qty
//   注: 2026-09-02 重构后,新版陈列补货的 plan 主要由"短补"触发,
//   但采购收货单仍然可以从 need_purchase 表读 plan_qty (兼容 W4 供应商结算)
func (s *Service) AttachPlanQtyToRows(ctx context.Context, supplier string, rows []model.SkuRow) error {
	if len(rows) == 0 {
		return nil
	}
	// 拉该 supplier 所有 pending plan
	plans, err := s.Store.ListPlansBySupplier(ctx, supplier, "")
	if err != nil {
		return err
	}
	// 建 plan 索引: barcode -> plan
	planByBarcode := make(map[string]*PurchasePlan, len(plans))
	for _, p := range plans {
		if p.Barcode != "" {
			planByBarcode[p.Barcode] = p
		}
	}
	// 给每行附加 plan 字段
	for i, r := range rows {
		barcode := r.MatchedBarcode
		if barcode == "" {
			barcode = r.RawBarcode
		}
		if p, ok := planByBarcode[barcode]; ok {
			rows[i].PlanItemNo = p.ItemNo
			rows[i].PlanItemName = p.ItemName
			rows[i].PlanBarcode = p.Barcode
			qty := p.SuggestQty
			rows[i].PlanQty = &qty
		}
	}
	return nil
}

// ============== ClsDict (item 分类字典, 启动时加载) ==============

// LoadItemDict 启动时从 products entity 加载 3 个字典
//   2026-09-02 重构: 改走 business.Gateway.Query (业务字段名 → products 实体 → cube)
//   原 hardcode "items" cube + 物理字段名,现改:
//     item_no  → products.barcode (hbpos)  或  products.barcode (erp,共字段)
//     clsno    → products.clsno
//     clsname  → products.clsname
//     unit     → products.unit
//   业务字段名由 Registry 翻译,跨数据源一致
func (s *Service) LoadItemDict(ctx context.Context) error {
	if s.Cube == nil || s.Cube.Gateway == nil {
		return nil
	}
	ds := s.Cube.Gateway.Client().GetDataSource()
	bizFields := []string{"barcode", "clsno", "clsname", "unit"}
	rows, err := s.Cube.Gateway.Query("products", ds, bizFields, nil, 30000)
	if err != nil {
		log.Printf("[restock] LoadItemDict: gateway query err: %v (前端 clsno/clsname/unit 走空)", err)
		return err
	}
	s.itemLock.Lock()
	defer s.itemLock.Unlock()
	for _, r := range rows {
		itemNo := asAnyString(r["barcode"])
		clsno := asAnyString(r["clsno"])
		clsname := asAnyString(r["clsname"])
		unit := asAnyString(r["unit"])
		if itemNo != "" {
			s.clsNoDict[itemNo] = clsno
			if unit != "" {
				s.itemUnitDict[itemNo] = unit
			}
		}
		if clsno != "" {
			s.clsNameDict[clsno] = clsname
		}
	}
	log.Printf("[restock] item dict loaded: %d item→clsno, %d clsno→clsname, %d item→unit",
		len(s.clsNoDict), len(s.clsNameDict), len(s.itemUnitDict))
	return nil
}

// ItemClsNoOf 查 item 的 clsno
func (s *Service) ItemClsNoOf(itemNo string) string {
	if itemNo == "" {
		return ""
	}
	s.itemLock.RLock()
	defer s.itemLock.RUnlock()
	return s.clsNoDict[itemNo]
}

// ClsNameOf 查 clsno 的 name
func (s *Service) ClsNameOf(clsno string) string {
	if clsno == "" {
		return ""
	}
	s.itemLock.RLock()
	defer s.itemLock.RUnlock()
	return s.clsNameDict[clsno]
}

// UnitOf 查 item 的 unit
func (s *Service) UnitOf(itemNo string) string {
	if itemNo == "" {
		return ""
	}
	s.itemLock.RLock()
	defer s.itemLock.RUnlock()
	return s.itemUnitDict[itemNo]
}
