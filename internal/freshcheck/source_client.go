package freshcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// SourceClient 调 cube-agent-server /admin/source-direct 端点
//
// 用途: collect-ai freshcheck C7 同步校验 (跟思迅源库对账)
//   强制走 cube-agent-server 代理 (AGENTS.md §12.1: collect-ai 不直连思迅)
//
// 不直连 SQL Server, 走 HTTP
type SourceClient struct {
	BaseURL string
	HTTP    *http.Client
}

// NewSourceClient 构造
//   baseURL: cube-agent-server 地址 (e.g. http://127.0.0.1:8088)
func NewSourceClient(baseURL string) *SourceClient {
	return &SourceClient{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// SourceDirectRequest 跟 cube-agent-server handler 端的 SourceDirectRequest 字段一致
type SourceDirectRequest struct {
	Datasource string `json:"datasource"`
	Table      string `json:"table"`
	Op         string `json:"op"`
	Column     string `json:"column"`
	Since      string `json:"since"`
	Until      string `json:"until"`
}

// SourceDirectResponse 跟 cube-agent-server handler 端的 SourceDirectResponse 字段一致
type SourceDirectResponse struct {
	Datasource  string  `json:"datasource"`
	Table       string  `json:"table"`
	Op          string  `json:"op"`
	Column      string  `json:"column"`
	Value       float64 `json:"value"`
	RowsCounted int     `json:"rows_counted"`
	DurationMs  int64   `json:"duration_ms"`
	QuerySQL    string  `json:"query_sql"`
}

// Aggregate 调 /admin/source-direct 拿 aggregate 数值
//   返回: value (sum/avg/count/max/min 结果)
//   例: Aggregate(ctx, "t_rm_saleflow", "sum", "sale_money", "2026-09-08 00:00:00", "2026-09-08 23:59:59") -> 12345.67
func (c *SourceClient) Aggregate(ctx context.Context, table, op, column, since, until string) (*SourceDirectResponse, error) {
	req := SourceDirectRequest{
		Datasource: "hbpos", // 默认思迅
		Table:      table,
		Op:         op,
		Column:     column,
		Since:      since,
		Until:      until,
	}
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/admin/source-direct", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http call: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
	}

	var out SourceDirectResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w (body=%s)", err, string(respBody))
	}
	return &out, nil
}

// PingSource 简单连通性测试 (拿 datasource config 列表)
func (c *SourceClient) PingSource(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/admin/datasources", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("cube-agent-server datasources endpoint status %d", resp.StatusCode)
	}
	return nil
}
