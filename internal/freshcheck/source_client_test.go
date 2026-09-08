package freshcheck

// ============================================================
// freshcheck SourceClient 单元测试 (W1.6)
// 覆盖:
//   - Aggregate 调 /admin/source-direct + 解析响应
//   - 错误响应处理
//   - PingSource 连通性
// 不需要 PG / 真实 cube-agent-server
// ============================================================

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ============== Test: Aggregate OK ==============

func TestSourceClient_Aggregate_OK(t *testing.T) {
	// mock server 模拟 cube-agent-server /admin/source-direct
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/source-direct" {
			t.Errorf("path = %s, want /admin/source-direct", r.URL.Path)
		}
		if r.Method != "POST" {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		// 返 24h 销售总额 12345.67, 100 行
		w.Write([]byte(`{
			"datasource": "hbpos",
			"table": "t_rm_saleflow",
			"op": "sum",
			"column": "sale_money",
			"value": 12345.67,
			"rows_counted": 100,
			"duration_ms": 50,
			"query_sql": "SELECT SUM(sale_money) AS value, COUNT(*) FROM dbo.t_rm_saleflow WHERE ..."
		}`))
	}))
	defer srv.Close()

	c := NewSourceClient(srv.URL)
	resp, err := c.Aggregate(context.Background(), "t_rm_saleflow", "sum", "sale_money",
		"2026-09-08 00:00:00", "2026-09-08 23:59:59")
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if resp.Value != 12345.67 {
		t.Errorf("Value = %f, want 12345.67", resp.Value)
	}
	if resp.RowsCounted != 100 {
		t.Errorf("RowsCounted = %d, want 100", resp.RowsCounted)
	}
	if resp.QuerySQL == "" {
		t.Error("QuerySQL 应返调试信息")
	}
}

// ============== Test: Aggregate 4xx/5xx 错误 ==============

func TestSourceClient_Aggregate_4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"table 不在白名单"}`))
	}))
	defer srv.Close()

	c := NewSourceClient(srv.URL)
	_, err := c.Aggregate(context.Background(), "INVALID_TABLE", "sum", "sale_money",
		"2026-09-08 00:00:00", "2026-09-08 23:59:59")
	if err == nil {
		t.Error("4xx 应返 error")
	}
	if err != nil && (err.Error() == "" || !contains(err.Error(), "400")) {
		t.Errorf("error 应含 status 400, got %v", err)
	}
}

func TestSourceClient_Aggregate_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"query failed: timeout"}`))
	}))
	defer srv.Close()

	c := NewSourceClient(srv.URL)
	_, err := c.Aggregate(context.Background(), "t_rm_saleflow", "sum", "sale_money",
		"2026-09-08 00:00:00", "2026-09-08 23:59:59")
	if err == nil {
		t.Error("5xx 应返 error")
	}
}

// ============== Test: PingSource ==============

func TestSourceClient_PingSource_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/datasources" {
			t.Errorf("path = %s, want /admin/datasources", r.URL.Path)
		}
		w.Write([]byte(`{"datasources":[{"name":"hbpos","driver":"mssql"}]}`))
	}))
	defer srv.Close()

	c := NewSourceClient(srv.URL)
	if err := c.PingSource(context.Background()); err != nil {
		t.Errorf("PingSource: %v", err)
	}
}

func TestSourceClient_PingSource_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()

	c := NewSourceClient(srv.URL)
	if err := c.PingSource(context.Background()); err == nil {
		t.Error("503 应返 error")
	}
}

// ============== Test: HTTP timeout ==============

func TestSourceClient_Aggregate_Timeout(t *testing.T) {
	// mock server 慢响应, 客户端 timeout 触发
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond) // 慢响应
		w.Write([]byte(`{"value":0}`))
	}))
	defer srv.Close()

	c := &SourceClient{
		BaseURL: srv.URL,
		HTTP:    &http.Client{Timeout: 10 * time.Millisecond}, // 极短
	}
	_, err := c.Aggregate(context.Background(), "t_rm_saleflow", "sum", "sale_money",
		"2026-09-08 00:00:00", "2026-09-08 23:59:59")
	if err == nil {
		t.Error("timeout 应返 error")
	}
}

// ============== helpers ==============

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
