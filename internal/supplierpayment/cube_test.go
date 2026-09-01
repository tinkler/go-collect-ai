package supplierpayment

import (
	"context"
	"errors"
	"testing"
)

// ============================================================
// NoopCubeQuerier
// ============================================================

func TestNoopCubeQuerier_Defaults(t *testing.T) {
	q := NewNoopCubeQuerier()
	ctx := context.Background()

	promo, err := q.PromoIntensitySupplier(ctx, "any-sup", 30)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if promo != 0.5 {
		t.Errorf("promo = %v, want 0.5", promo)
	}

	sell, err := q.SellthroughSupplier(ctx, "any-sup", 30)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if sell != 0.8 {
		t.Errorf("sellthrough = %v, want 0.8", sell)
	}
}

func TestNoopCubeQuerier_Custom(t *testing.T) {
	q := &NoopCubeQuerier{PromoDefault: 0.3, SellthroughDefault: 0.9}
	promo, _ := q.PromoIntensitySupplier(context.Background(), "x", 30)
	sell, _ := q.SellthroughSupplier(context.Background(), "x", 30)
	if promo != 0.3 || sell != 0.9 {
		t.Errorf("custom defaults 不生效: promo=%v sell=%v", promo, sell)
	}
}

// ============================================================
// RealCubeQuerier (mock CubeClient)
// ============================================================

type mockCubeClient struct {
	rows []map[string]any
	err  error
}

func (m *mockCubeClient) Execute(cube string, measures, dimensions []string, filters []map[string]any, segments []string, limit int) ([]map[string]any, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.rows, nil
}

func TestRealCubeQuerier_PromoIntensity(t *testing.T) {
	calls := 0
	q := NewRealCubeQuerier(mockFuncClient(func() ([]map[string]any, error) {
		calls++
		t.Logf("[mock] call #%d", calls)
		if calls == 1 {
			return []map[string]any{
				{"sales.supplier_name": "汇一", "sales.sale_money": 10000.0},
			}, nil
		}
		return []map[string]any{
			{"sales.supplier_name": "汇一", "sales.sale_money": 3000.0},
		}, nil
	}))
	ratio, err := q.PromoIntensitySupplier(context.Background(), "汇一", 30)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	t.Logf("ratio = %v", ratio)
	if ratio < 0.29 || ratio > 0.31 {
		t.Errorf("ratio = %v, want ~0.3", ratio)
	}
}

func TestRealCubeQuerier_PromoIntensity_NoData(t *testing.T) {
	q := NewRealCubeQuerier(mockFuncClient(func() ([]map[string]any, error) {
		return nil, errors.New("cube timeout")
	}))
	_, err := q.PromoIntensitySupplier(context.Background(), "x", 30)
	if err == nil {
		t.Error("cube 错误应传播")
	}
}

func TestRealCubeQuerier_Sellthrough(t *testing.T) {
	q := NewRealCubeQuerier(mockFuncClient(func() ([]map[string]any, error) {
		return []map[string]any{
			{"sales.supplier_name": "汇一", "sales.sale_qty": 80.0, "sales.stock_qty": 20.0},
		}, nil
	}))
	ratio, err := q.SellthroughSupplier(context.Background(), "汇一", 30)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ratio < 0.79 || ratio > 0.81 {
		t.Errorf("ratio = %v, want 0.8 (80 sold / 100 total)", ratio)
	}
}

// ============================================================
// helpers
// ============================================================

type mockFuncClient func() ([]map[string]any, error)

func (m mockFuncClient) Execute(string, []string, []string, []map[string]any, []string, int) ([]map[string]any, error) {
	return m()
}

// ============================================================
// W5 集成: Service.cubeWeightsFromCube
// ============================================================

func TestService_CubeWeights_Noop(t *testing.T) {
	svc := NewService(nil, "") // pool nil OK for this test (不调 SQL)
	svc.cube = NewNoopCubeQuerier()
	promo, sell := svc.cubeWeightsFromCube(context.Background(), "x")
	// Noop: promo=0.5 → 0.9+0.5*0.4=1.1; sell=0.8 → 0.7+0.8*0.5=1.1
	if promo < 1.09 || promo > 1.11 {
		t.Errorf("promo = %v, want 1.1", promo)
	}
	if sell < 1.09 || sell > 1.11 {
		t.Errorf("sell = %v, want 1.1", sell)
	}
}

func TestService_CubeWeights_NilCube_DegradeTo1(t *testing.T) {
	svc := NewService(nil, "")
	svc.cube = nil
	promo, sell := svc.cubeWeightsFromCube(context.Background(), "x")
	if promo != 1.0 || sell != 1.0 {
		t.Errorf("nil cube 应降级 (1.0, 1.0), got (%v, %v)", promo, sell)
	}
}

func TestService_CubeWeights_Clamping(t *testing.T) {
	// 高 promo (1.0) → 0.9+1.0*0.4=1.3 (clamp 1.3)
	// 高 sell (1.0) → 0.7+1.0*0.5=1.2 (clamp 1.2)
	svc := NewService(nil, "")
	svc.cube = &NoopCubeQuerier{PromoDefault: 1.0, SellthroughDefault: 1.0}
	promo, sell := svc.cubeWeightsFromCube(context.Background(), "x")
	if promo != 1.3 || sell != 1.2 {
		t.Errorf("clamp 异常: promo=%v sell=%v", promo, sell)
	}
}
