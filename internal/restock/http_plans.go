package restock

import (
	"context"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tinkler/collect-ai/internal/model"
)

// PurchasePlansList GET /api/v1/purchase-plans?supplier=xxx[&branch_no=xxx]
//   查 restock_need_purchase 表里某 supplier 的 pending 计划
//   用于企微 H5 采购收货单, 让员工看到"我今天要收的货"
//
//   2026-08-28 加入
func PurchasePlansList(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		supplier := strings.TrimSpace(c.Query("supplier"))
		if supplier == "" {
			c.JSON(400, gin.H{"error": "supplier 必填 (query ?supplier=xxx)"})
			return
		}
		branchNo := strings.TrimSpace(c.Query("branch_no"))
		if branchNo == "" && svc != nil && svc.Cfg != nil {
			// 默认用 service 配置的 branch_no
			branchNo = svc.Cfg.BranchNo
		}
		limit, _ := strconv.Atoi(c.Query("limit"))
		if limit <= 0 || limit > 1000 {
			limit = 200
		}
		plans, err := svc.Store.ListPendingNeedsBySupplier(c.Request.Context(), supplier, branchNo)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		if len(plans) > limit {
			plans = plans[:limit]
		}
		c.JSON(200, gin.H{
			"supplier":  supplier,
			"branch_no": branchNo,
			"plans":     plans,
			"count":     len(plans),
		})
	}
}

// AttachPlanQtyToRows 给 Session.Rows 注入 plan_qty / plan_item_no / plan_item_name
//   匹配规则: 优先 matched_barcode, 其次 raw_barcode, 都匹配不到就不附加
//   多个 plan 命中取 suggest_qty 最大(预计补货多的一次收齐)
//
//   2026-08-28 加入, 给 handler.GetSession 用
func (s *Service) AttachPlanQtyToRows(ctx context.Context, supplier string, rows []model.SkuRow) error {
	if s == nil || s.Store == nil || supplier == "" || len(rows) == 0 {
		return nil
	}
	plans, err := s.Store.ListPendingNeedsBySupplier(ctx, supplier, "")
	if err != nil {
		return err
	}
	if len(plans) == 0 {
		return nil
	}
	// 索引: barcode -> plan, item_no -> plan (后者兜底)
	byBarcode := make(map[string]*NeedPurchase, len(plans))
	byItemNo := make(map[string]*NeedPurchase, len(plans))
	for _, p := range plans {
		bc := strings.TrimSpace(p.Barcode)
		if bc != "" {
			// 多 plan 同 barcode 取 suggest_qty 最大
			if existing, ok := byBarcode[bc]; !ok || p.SuggestQty > existing.SuggestQty {
				byBarcode[bc] = p
			}
		}
		ino := strings.TrimSpace(p.ItemNo)
		if ino != "" {
			if existing, ok := byItemNo[ino]; !ok || p.SuggestQty > existing.SuggestQty {
				byItemNo[ino] = p
			}
		}
	}
	for i := range rows {
		if rows[i].IsDeleted {
			continue
		}
		// 优先 barcode 匹配
		key := strings.TrimSpace(rows[i].MatchedBarcode)
		if key == "" {
			key = strings.TrimSpace(rows[i].RawBarcode)
		}
		if key == "" {
			continue
		}
		var p *NeedPurchase
		if v, ok := byBarcode[key]; ok {
			p = v
		} else if v, ok := byItemNo[key]; ok {
			// 退而求其次用 item_no 匹配 (cube-agent 的 SKU 编码可能不一致)
			p = v
		}
		if p == nil {
			continue
		}
		rows[i].PlanItemNo = p.ItemNo
		rows[i].PlanItemName = p.ItemName
		rows[i].PlanBarcode = p.Barcode
		q := p.SuggestQty
		rows[i].PlanQty = &q
	}
	return nil
}
