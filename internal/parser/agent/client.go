package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Client cube-agent-server /v1/load 客户端
//
// 字段约定:使用 cube-agent-server 的物理 cube 字段名
//   业务字段名 → 物理字段名的转换由 collect-ai 业务层( internal/business)负责
//   此 client 只发物理 cube 名 + 物理字段名
//
// 数据源切换:由 cube-agent-server 端 cube 的 metadata.datasource 决定
//   同一 cube 名 (e.g. "products") 在 erp / hbpos 各有一个 plugin 定义
//   调用时直接发 cube 名,agent 找对应 plugin
//   (不再用 ?datasource= 参数,因为 cube 已经跟 datasource 绑定)
type Client struct {
	baseURL string
	timeout time.Duration
	token   string // 可选

	mu        sync.RWMutex
	datasource string // 当前数据源(er / hbpos),供业务层使用
}

// NewClient 构造 agent client
//   dataSource 初始数据源(空字符串 = erp,默认)
func NewClient(baseURL, token string, timeoutSec int, dataSource string) *Client {
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8088"
	}
	if dataSource == "" {
		dataSource = "erp"
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		timeout:    time.Duration(timeoutSec) * time.Second,
		token:      token,
		datasource: dataSource,
	}
}

// SetDataSource 切换数据源(线程安全,业务层会调)
func (c *Client) SetDataSource(ds string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ds == "" {
		ds = "erp"
	}
	c.datasource = ds
}

// GetDataSource 当前数据源
func (c *Client) GetDataSource() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.datasource
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

// Query /v1/load 通用(物理 cube + 物理字段)
type Query struct {
	Measures       []string         `json:"measures,omitempty"`
	Dimensions     []string         `json:"dimensions,omitempty"`
	Filters        []map[string]any `json:"filters,omitempty"`
	Segments       []string         `json:"segments,omitempty"`
	TimeDimensions []any            `json:"timeDimensions,omitempty"`
	Limit          int              `json:"limit,omitempty"`
}

type loadResp struct {
	Data json.RawMessage `json:"data"`
}

// Execute 执行任意 cube 查询,返回原始数据 map 列表
//   segments 引用 cube YAML 定义的预定义过滤段(如 ["sup_only"] 排除客户)
//   未在 cube.Segments 声明的 segment 名会被 cube-agent-server pass3 静默忽略(安全)
//
//   透传到 cube-agent-server 时,cube.js 风格要求 segments 字段带 cube 前缀
//   (e.g. "t_bd_item_info.sup_only"),本方法自动为裸名加当前 cube 前缀
//   调用方写 ["sup_only"] → 实际发 ["t_bd_item_info.sup_only"]
//   调用方已带点的 ["t_bd_item_info.sup_only"] 原样透传
func (c *Client) Execute(cube string, measures, dimensions []string, filters []map[string]any, segments []string, limit int) ([]map[string]any, error) {
	// 自动给裸 segment 名加 cube 前缀(cube-agent-server query parser 要求 cube.field 格式)
	prefixed := make([]string, len(segments))
	for i, s := range segments {
		if strings.Contains(s, ".") {
			prefixed[i] = s
		} else {
			prefixed[i] = cube + "." + s
		}
	}
	q := Query{
		Measures:   measures,
		Dimensions: dimensions,
		Filters:    filters,
		Segments:   prefixed,
		Limit:      limit,
	}
	return c.load(cube, q)
}

func (c *Client) load(cube string, q Query) ([]map[string]any, error) {
	bs, _ := json.Marshal(q)
	url := c.baseURL + "/v1/load"
	req, _ := http.NewRequest("POST", url, bytes.NewReader(bs))
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
	// data 可能是 {Columns, Rows} 或直接 []
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

// ============================================================
// 旧的便捷方法(保留,内部 handler 可选用,或者全部迁到 business 包)
// ============================================================

// LoadSupplierSkus 加载某供应商的所有 SKU(物理字段名,内部用)
// 业务层 (BusinessQueryExecutor) 会包装这个做业务名翻译
func (c *Client) LoadSupplierSkus(supplierKeyword string, limit int) ([]map[string]any, error) {
	keywords := splitKeywords(supplierKeyword)
	if len(keywords) == 0 {
		return nil, fmt.Errorf("供应商关键词不能为空")
	}

	seen := make(map[string]struct{})
	var merged []map[string]any
	for _, kw := range keywords {
		// 物理字段名(由调用方决定哪个 cube 用什么物理字段)
		// 简化:假设 erp 物理 cube = products
		// 实际业务层调用请用 business.Registry.ToPhysicalQuery 翻译
		q := Query{
			Measures: []string{"products.qty"},
			Dimensions: []string{
				"products.barcode",
				"products.name",
				"products.main_supp_id",
				"products.main_supp_name",
				"products.src_sheet",
			},
			Filters: []map[string]any{
				{"member": "products.main_supp_name", "operator": "contains", "values": []string{kw}},
			},
			Limit: limit,
		}
		rows, err := c.load("products", q)
		if err != nil {
			return nil, err
		}
		for _, dict := range rows {
			var key string
			bc := asString(dict, "products.barcode")
			name := asString(dict, "products.name")
			if bc != "" {
				key = "bc:" + bc
			} else {
				key = "sn:" + asString(dict, "products.main_supp_name") + "|" + name
			}
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				merged = append(merged, dict)
			}
		}
	}
	return merged, nil
}

// GetDistinctSuppliers 拉所有 distinct 供应商(物理字段名)
//   调用方传 cube 名 (e.g. "products" / "suppliers")
func (c *Client) GetDistinctSuppliers(cube string, nameField string, scanLimit int) ([]string, error) {
	// cube 必须有至少 1 个 measure(cube.js 要求),我们用 stock_qty / count / 类似
	// 简化:让 cube 选任一 measure
	q := Query{
		Dimensions: []string{nameField},
		Limit:      scanLimit,
	}
	rows, err := c.load(cube, q)
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{})
	for _, d := range rows {
		s := strings.TrimSpace(asString(d, nameField))
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
