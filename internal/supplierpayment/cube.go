// Package supplierpayment cube 数据源 (W5)
//
// 目标: 用 cube 真实销售/促销数据替换 W4.2 的 1.0 占位系数
//   - promo_weight  ← v_prom_saleflow 的 sale_money 占比 (近 N 天)
//   - sellthrough   ← siss_saleflow 的 售罄率 (sale_qty / stock) (近 N 天)
//
// 当前 (W5 占位实现):
//   - NoopCubeQuerier 返回固定值 (0.5 / 0.8), 让 cron 跑通
//   - 真实接入需在 main.go 注入 RealCubeQuerier (使用 internal/parser/agent.Client.Execute)
//
// 2026-09-02 重构:
//   - 删本地 CubeClient interface,统一复用 business.CubeClient (Gateway.RawQuery 包装)
//   - RealCubeQuerier 持 business.CubeClient,不直接 import parser/agent
//
// 接口设计原则:
//   - 抽象成 CubeQuerier interface, 业务代码不强依赖 collect-ai agent.Client
//   - NoopQuerier 保证 devMode 友好 (无 LLM/无 cube 也能跑)
package supplierpayment

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/tinkler/collect-ai/internal/business"
)

// CubeQuerier cube 数据源抽象
type CubeQuerier interface {
	// PromoIntensitySupplier 近 N 天某 supplier 促销销售占比 (0..1)
	//   算法: v_prom_saleflow WHERE supplier=? AND date>=now-N 的 sale_money / 所有 sale_money
	//   比例越高 → supplier 越依赖促销 → 越应优先结算
	PromoIntensitySupplier(ctx context.Context, supplier string, days int) (float64, error)
	// SellthroughSupplier 近 N 天某 supplier 售罄率 (0..1)
	//   算法: siss_saleflow WHERE supplier=? AND date>=now-N 的 sum(sale_qty) / sum(sale_qty + stock_qty)
	//   比例越高 → 动销越好 → 越应多结算备货
	SellthroughSupplier(ctx context.Context, supplier string, days int) (float64, error)
}

// ============================================================
// NoopCubeQuerier 占位 (W5 默认, devMode 友好)
// ============================================================

// NoopCubeQuerier 返回固定系数, 业务照常跑, 系数没接真数据
type NoopCubeQuerier struct {
	// 默认 promo = 0.5, sellthrough = 0.8 (中性偏正向, 避免过度调整)
	PromoDefault      float64
	SellthroughDefault float64
}

func NewNoopCubeQuerier() *NoopCubeQuerier {
	return &NoopCubeQuerier{
		PromoDefault:      0.5,
		SellthroughDefault: 0.8,
	}
}

func (n *NoopCubeQuerier) PromoIntensitySupplier(_ context.Context, _ string, _ int) (float64, error) {
	return n.PromoDefault, nil
}

func (n *NoopCubeQuerier) SellthroughSupplier(_ context.Context, _ string, _ int) (float64, error) {
	return n.SellthroughDefault, nil
}

// ============================================================
// RealCubeQuerier 真实实现 (W5 后续, 用 collect-ai 现有 agent.Client)
//
//   字段说明: cube 名 + measure/dim 名取决于 cube-agent-server 的 YAML 定义
//   W1 阶段已经使用过:
//     - sales: cube='siss_saleflow', measure='sales.sale_money' (或 'sales.sale_qty')
//     - promo: cube='v_prom_saleflow', measure='sales.sale_money'
//   实际 measure/dim 名请参考 docs/agent-purchase-plan.md §6 + cube-agent-server/plugins/
// ============================================================

// RealCubeQuerier 真实 cube 客户端包装
//   2026-09-02 重构: 改持 business.CubeClient interface(原 CubeClient interface 已删,统一复用 business.CubeClient)
//   实参可以是 *agent.Client 或 Gateway.RawQuery 包装
//   不再 import internal/parser/agent
type RealCubeQuerier struct {
	client business.CubeClient
	// 实际 cube 名 + measure/dim 在这里集中管理
	salesCube        string
	promoCube        string
	supplierDim      string
	saleMoneyMeasure string
	saleQtyMeasure   string
	stockQtyMeasure  string
	now              func() time.Time
}

func NewRealCubeQuerier(client business.CubeClient) *RealCubeQuerier {
	return &RealCubeQuerier{
		client:           client,
		salesCube:        "siss_saleflow",
		promoCube:        "v_prom_saleflow",
		supplierDim:      "sales.supplier_name",
		saleMoneyMeasure: "sales.sale_money",
		saleQtyMeasure:   "sales.sale_qty",
		stockQtyMeasure:  "sales.stock_qty",
		now:              time.Now,
	}
}

func (r *RealCubeQuerier) PromoIntensitySupplier(ctx context.Context, supplier string, days int) (float64, error) {
	if r.client == nil {
		return 0, fmt.Errorf("cube client nil")
	}
	cutoff := r.now().AddDate(0, 0, -days)
	// 全部 sale_money (分子分母都查)
	allRows, err := r.client.Execute(r.salesCube, []string{r.saleMoneyMeasure}, []string{r.supplierDim}, []map[string]any{
		{"member": "sales.oper_date", "operator": "afterOrOn", "values": []string{cutoff.Format("2006-01-02")}},
	}, nil, days*100)
	if err != nil {
		return 0, err
	}
	// 过滤该 supplier
	var supplierTotal float64
	for _, row := range allRows {
		if name, _ := row[r.supplierDim].(string); name == supplier {
			if v, ok := row[r.saleMoneyMeasure].(float64); ok {
				supplierTotal = v
			}
		}
	}
	// 促销 sale_money
	promoRows, err := r.client.Execute(r.promoCube, []string{r.saleMoneyMeasure}, []string{r.supplierDim}, []map[string]any{
		{"member": "sales.oper_date", "operator": "afterOrOn", "values": []string{cutoff.Format("2006-01-02")}},
	}, nil, days*100)
	if err != nil {
		return 0, err
	}
	var supplierPromo float64
	for _, row := range promoRows {
		if name, _ := row[r.supplierDim].(string); name == supplier {
			if v, ok := row[r.saleMoneyMeasure].(float64); ok {
				supplierPromo = v
			}
		}
	}
	if supplierTotal <= 0 {
		return 0, nil
	}
	ratio := supplierPromo / supplierTotal
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	return ratio, nil
}

func (r *RealCubeQuerier) SellthroughSupplier(ctx context.Context, supplier string, days int) (float64, error) {
	if r.client == nil {
		return 0, fmt.Errorf("cube client nil")
	}
	cutoff := r.now().AddDate(0, 0, -days)
	rows, err := r.client.Execute(r.salesCube, []string{r.saleQtyMeasure, r.stockQtyMeasure}, []string{r.supplierDim}, []map[string]any{
		{"member": "sales.oper_date", "operator": "afterOrOn", "values": []string{cutoff.Format("2006-01-02")}},
	}, nil, days*100)
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		name, _ := row[r.supplierDim].(string)
		if name != supplier {
			continue
		}
		sold, _ := row[r.saleQtyMeasure].(float64)
		stock, _ := row[r.stockQtyMeasure].(float64)
		total := sold + stock
		if total <= 0 {
			return 0, nil
		}
		ratio := sold / total
		if ratio < 0 {
			ratio = 0
		}
		if ratio > 1 {
			ratio = 1
		}
		return ratio, nil
	}
	return 0, nil
}

// ============================================================
// W4 集成: 把 CubeQuerier 注入 supplierpayment.Service
// ============================================================

// SetCubeQuerier 注入 (单测可用 Noop)
func (s *Service) SetCubeQuerier(q CubeQuerier) {
	s.cube = q
}

// cubeWeightsFromCube 用 cube 数据算 promo/sellthrough 系数
//   输入 cube 0..1, 输出 W4 算法系数 (0.9~1.3 / 0.7~1.2)
func (s *Service) cubeWeightsFromCube(ctx context.Context, supplier string) (promoW, sellthroughW float64) {
	if s.cube == nil {
		return 1.0, 1.0
	}
	promoRatio, err := s.cube.PromoIntensitySupplier(ctx, supplier, 30)
	if err != nil {
		log.Printf("[supplierpayment] cube PromoIntensity err: %v (用 1.0 占位)", err)
		promoRatio = 0.5
	}
	sellRatio, err := s.cube.SellthroughSupplier(ctx, supplier, 30)
	if err != nil {
		log.Printf("[supplierpayment] cube Sellthrough err: %v (用 0.5 占位)", err)
		sellRatio = 0.5
	}
	// promo: 0.9 + ratio × 0.4 → 0.9 ~ 1.3
	promoW = 0.9 + promoRatio*0.4
	if promoW < 0.9 {
		promoW = 0.9
	}
	if promoW > 1.3 {
		promoW = 1.3
	}
	// sellthrough: 0.7 + ratio × 0.5 → 0.7 ~ 1.2
	sellthroughW = 0.7 + sellRatio*0.5
	if sellthroughW < 0.7 {
		sellthroughW = 0.7
	}
	if sellthroughW > 1.2 {
		sellthroughW = 1.2
	}
	return
}
