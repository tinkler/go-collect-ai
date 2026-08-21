package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/tinkler/collect-ai/internal/model"
)

// Client cube-agent-server /v1/load 客户端
type Client struct {
	baseURL string
	timeout time.Duration
	token   string // 可选
}

func NewClient(baseURL, token string, timeoutSec int) *Client {
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8088"
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		timeout: time.Duration(timeoutSec) * time.Second,
		token:   token,
	}
}

// Ping 健康检查
func (c *Client) Ping() error {
	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get(c.baseURL + "/livez")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// Query /v1/load 通用
type queryReq struct {
	Measures   []string         `json:"measures,omitempty"`
	Dimensions []string         `json:"dimensions,omitempty"`
	Filters    []map[string]any `json:"filters,omitempty"`
	TimeDimensions []any        `json:"timeDimensions,omitempty"`
	Limit      int              `json:"limit,omitempty"`
}

type loadResp struct {
	Data json.RawMessage `json:"data"`
}

// LoadSupplierSkus 多关键词并集 (用 ; 分隔)
func (c *Client) LoadSupplierSkus(supplierKeyword string, limit int) ([]model.SkuRecord, error) {
	keywords := splitKeywords(supplierKeyword)
	if len(keywords) == 0 {
		return nil, fmt.Errorf("供应商关键词不能为空")
	}

	seen := make(map[string]struct{})
	var merged []model.SkuRecord
	for _, kw := range keywords {
		q := queryReq{
			Measures:   []string{"products.qty"},
			Dimensions: []string{"products.barcode", "products.name", "products.main_supp_id", "products.main_supp_name", "products.src_sheet"},
			Filters: []map[string]any{
				{"member": "products.main_supp_name", "operator": "contains", "values": []string{kw}},
			},
			Limit: limit,
		}
		rows, err := c.load(q)
		if err != nil {
			return nil, err
		}
		for _, dict := range rows {
			r := model.SkuRecord{
				Barcode:      asString(dict, "products.barcode"),
				Name:         asString(dict, "products.name"),
				MainSuppId:   asString(dict, "products.main_supp_id"),
				MainSuppName: asString(dict, "products.main_supp_name"),
				SrcSheet:     asString(dict, "products.src_sheet"),
				StockQty:     asFloat(dict, "products.qty"),
			}
			if r.Barcode == "" && r.Name == "" {
				continue
			}
			var key string
			if r.Barcode != "" {
				key = "bc:" + r.Barcode
			} else {
				key = "sn:" + r.MainSuppName + "|" + r.Name
			}
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				merged = append(merged, r)
			}
		}
	}
	return merged, nil
}

// GetDistinctSuppliers 拉所有 distinct 供应商
func (c *Client) GetDistinctSuppliers(scanLimit int) ([]string, error) {
	q := queryReq{
		Measures:   []string{"products.qty"},
		Dimensions: []string{"products.main_supp_name"},
		Limit:      scanLimit,
	}
	rows, err := c.load(q)
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{})
	for _, d := range rows {
		s := asString(d, "products.main_supp_name")
		s = strings.TrimSpace(s)
		if s != "" {
			set[s] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// GetSuppliersByBrand 按品牌反查
func (c *Client) GetSuppliersByBrand(brand string, scanLimit int) ([]map[string]any, error) {
	if brand == "" {
		return nil, fmt.Errorf("品牌不能为空")
	}
	q := queryReq{
		Measures:   []string{"products.qty"},
		Dimensions: []string{"products.main_supp_id", "products.main_supp_name"},
		Filters: []map[string]any{
			{"member": "products.name", "operator": "contains", "values": []string{brand}},
		},
		Limit: scanLimit,
	}
	return c.load(q)
}

func (c *Client) load(q queryReq) ([]map[string]any, error) {
	bs, _ := json.Marshal(q)
	req, _ := http.NewRequest("POST", c.baseURL+"/v1/load", bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	httpClient := &http.Client{Timeout: c.timeout}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	bodyBs, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("agent /v1/load HTTP %d: %s", resp.StatusCode, truncate(string(bodyBs), 400))
	}
	var lr loadResp
	if err := json.Unmarshal(bodyBs, &lr); err != nil {
		return nil, fmt.Errorf("agent 响应解析: %w", err)
	}
	// data 可能是 {Columns, Rows} 或 直接 []
	var raw any
	if err := json.Unmarshal(lr.Data, &raw); err != nil {
		return nil, fmt.Errorf("agent data 解析: %w", err)
	}
	switch v := raw.(type) {
	case map[string]any:
		// {Columns, Rows}
		if rows, ok := v["Rows"].([]any); ok {
			return toDictList(rows), nil
		}
		return nil, nil
	case []any:
		return toDictList(v), nil
	}
	return nil, nil
}

func toDictList(arr []any) []map[string]any {
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func asString(d map[string]any, key string) string {
	if v, ok := d[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprintf("%v", v)
	}
	return ""
}

func asFloat(d map[string]any, key string) *float64 {
	v, ok := d[key]
	if !ok || v == nil {
		return nil
	}
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case float32:
		f = float64(x)
	case int:
		f = float64(x)
	case int64:
		f = float64(x)
	case string:
		_, err := fmt.Sscanf(x, "%f", &f)
		if err != nil {
			return nil
		}
	default:
		_, err := fmt.Sscanf(fmt.Sprintf("%v", v), "%f", &f)
		if err != nil {
			return nil
		}
	}
	return &f
}

func splitKeywords(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ';' || r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t'
	})
	seen := make(map[string]struct{})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		k := strings.ToLower(p)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, p)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
