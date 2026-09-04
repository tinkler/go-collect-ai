// Package glmocr 智谱 GLM「同步文件解析 prime-sync」客户端 (2026-09-04 双引擎改造)
//
//	来源: 从 tin-nova/pkg/glmocr 裁剪而来 (collect-ai 不跨仓库依赖 tin-nova),
//	只保留采购订单解析实际用到的 FileParserSync (引擎1: 印刷体/表格 OCR)。
//	引擎2 (DeepSeek 视觉模型) 走 trpc-agent-go, 不在本包。
//
//	端点: POST https://open.bigmodel.cn/api/paas/v4/files/parser/sync
//	  - multipart: file + tool_type=prime-sync + file_type
//	  - 返回: content (纯文本) + parsing_result_url (Markdown+布局 JSON, 24h 有效)
//	  - 印刷体/表格/复杂版面首选; 手写体不要用这个端点
package glmocr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://open.bigmodel.cn/api"
	defaultTimeout = 120 * time.Second // 解析表格/版面可能需要较长时间

	parserSyncPath = "/paas/v4/files/parser/sync"
)

// Client 智谱同步文件解析客户端
type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

// New 创建客户端
//
//	apiKey: 智谱 BigModel key (跟 chat/completions 同一个 key, 复用 BIGMODEL_API_KEY)
//	timeoutSec: http.Client 硬上限, <=0 用默认 120s
func New(apiKey string, timeoutSec int) *Client {
	timeout := defaultTimeout
	if timeoutSec > 0 {
		timeout = time.Duration(timeoutSec) * time.Second
	}
	return &Client{
		apiKey:     apiKey,
		baseURL:    defaultBaseURL,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// FileParserSync 文件解析同步接口 (⭐印刷体/表格/通用首选)
//
//	req.FileData: 图片 bytes
//	req.FileName: 用于推断 file_type (jpg/png/...)
//	req.ToolType: 留空 = ToolTypePrimeSync (高精度印刷体)
//	req.FileType: 留空按 FileName 扩展名推断
func (c *Client) FileParserSync(ctx context.Context, req *FileParserSyncRequest) (*FileParserSyncResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("glm parser: request is nil")
	}
	if len(req.FileData) == 0 {
		return nil, fmt.Errorf("glm parser: file data is empty")
	}
	if req.FileName == "" {
		return nil, fmt.Errorf("glm parser: FileName is required (for inferring file_type)")
	}
	if req.ToolType == "" {
		req.ToolType = ToolTypePrimeSync
	}
	ft := req.FileType
	if ft == "" {
		ft = inferParserFileType(req.FileName)
	}
	if ft == "" {
		return nil, fmt.Errorf("glm parser: 无法根据文件名 %q 推断 file_type, 请显式设置 req.FileType", req.FileName)
	}

	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	part, err := w.CreateFormFile("file", req.FileName)
	if err != nil {
		return nil, fmt.Errorf("glm parser: build multipart: %w", err)
	}
	if _, err := part.Write(req.FileData); err != nil {
		return nil, fmt.Errorf("glm parser: build multipart: %w", err)
	}
	if err := w.WriteField("tool_type", string(req.ToolType)); err != nil {
		return nil, fmt.Errorf("glm parser: build multipart: %w", err)
	}
	if err := w.WriteField("file_type", string(ft)); err != nil {
		return nil, fmt.Errorf("glm parser: build multipart: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("glm parser: close multipart: %w", err)
	}

	fullURL := c.baseURL + parserSyncPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("glm parser: create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", w.FormDataContentType())
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("glm parser: send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("glm parser: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var errResp OCRError
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Code != "" {
			return nil, &errResp
		}
		return nil, fmt.Errorf("glm parser: HTTP %d: %s", resp.StatusCode, truncateBody(respBody))
	}

	// 同步解析接口顶层一般是 {task_id, status, content}; 也可能包在 data{} 内
	var flat FileParserSyncResponse
	if uerr := json.Unmarshal(respBody, &flat); uerr == nil && (flat.TaskID != "" || flat.Status != "" || flat.Content != "" || flat.ParsingResultURL != "") {
		return &flat, nil
	}
	var wrapper map[string]json.RawMessage
	if werr := json.Unmarshal(respBody, &wrapper); werr == nil {
		for _, cand := range []string{"data", "result", "payload"} {
			if raw, ok := wrapper[cand]; ok {
				if err := json.Unmarshal(raw, &flat); err == nil && (flat.TaskID != "" || flat.Status != "" || flat.Content != "" || flat.ParsingResultURL != "") {
					return &flat, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("glm parser: 无法识别响应结构, 原始响应: %s", truncateBody(respBody))
}

// inferParserFileType 按文件名后缀推断 ParserFileType (全大写)
func inferParserFileType(fileName string) ParserFileType {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(fileName), "."))
	switch ext {
	case "pdf":
		return FilePDF
	case "png":
		return FilePNG
	case "jpg":
		return FileJPG
	case "jpeg":
		return FileJPEG
	case "bmp":
		return FileBMP
	case "webp":
		return FileWEBP
	}
	return ""
}

// truncateBody 截断过长响应体 (避免 log 爆炸)
func truncateBody(b []byte) string {
	const max = 1024
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + fmt.Sprintf("...(truncated, total=%d bytes)", len(b))
}

// itoa 供 response 状态展示用 (避免多余 import)
var _ = strconv.Itoa
