package bigmodel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tinkler/collect-ai/internal/model"
)

const ocrEndpoint = "https://open.bigmodel.cn/api/paas/v4/files/ocr"

// OcrClient 智谱 BigModel OCR
//   - model (tool_type) 是每次调用传的, 不存在 client 上, 便于 per-template 切换
//   - 合法值: "hand_write" (手写) / "layout_parsing" (印刷)
//   - 空字符串时 client 自动回退到 "hand_write"
type OcrClient struct {
	apiKey   string
	baseURL  string
	timeout  time.Duration
	language string
	prob     string
}

func NewOcrClient(apiKey, baseURL string, timeoutSec int) *OcrClient {
	if baseURL == "" {
		baseURL = "https://open.bigmodel.cn/api/paas/v4"
	}
	return &OcrClient{
		apiKey:   apiKey,
		baseURL:  baseURL,
		timeout:  time.Duration(timeoutSec) * time.Second,
		language: "CHN_ENG",
		prob:     "true",
	}
}

// resolveOcrModel 空值回退到 hand_write
func resolveOcrModel(model string) string {
	if model == "" {
		return "hand_write"
	}
	return model
}

// RecognizeFile 上传文件 → OCR 原始 words_result (未分行)
//   toolType: "hand_write" / "layout_parsing" / "" (回退 hand_write)
func (c *OcrClient) RecognizeFile(filePath, toolType string) ([]model.OcrWordBlock, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, err := w.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return nil, err
	}
	if _, err = io.Copy(part, f); err != nil {
		return nil, err
	}
	_ = w.WriteField("tool_type", resolveOcrModel(toolType))
	_ = w.WriteField("language_type", c.language)
	_ = w.WriteField("probability", c.prob)
	if err = w.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", c.baseURL+"/files/ocr", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", w.FormDataContentType())

	httpClient := &http.Client{Timeout: c.timeout}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bs, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OCR HTTP %d: %s", resp.StatusCode, truncate(string(bs), 400))
	}

	var parsed struct {
		Status      string                `json:"status"`
		Message     string                `json:"message,omitempty"`
		WordsResult []model.OcrWordBlock `json:"words_result"`
	}
	if err = json.Unmarshal(bs, &parsed); err != nil {
		return nil, fmt.Errorf("parse ocr response: %w; body=%s", err, truncate(string(bs), 200))
	}
	if parsed.Status != "succeeded" {
		return nil, fmt.Errorf("OCR 未成功: %s", parsed.Message)
	}
	return parsed.WordsResult, nil
}

// RecognizeBytes 收 bytes (png/jpg) → OCR words_result
//   toolType: "hand_write" / "layout_parsing" / "" (回退 hand_write)
func (c *OcrClient) RecognizeBytes(name string, data []byte, toolType string) ([]model.OcrWordBlock, error) {
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, err := w.CreateFormFile("file", name)
	if err != nil {
		return nil, err
	}
	if _, err = part.Write(data); err != nil {
		return nil, err
	}
	_ = w.WriteField("tool_type", resolveOcrModel(toolType))
	_ = w.WriteField("language_type", c.language)
	_ = w.WriteField("probability", c.prob)
	if err = w.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", c.baseURL+"/files/ocr", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", w.FormDataContentType())

	httpClient := &http.Client{Timeout: c.timeout}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bs, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OCR HTTP %d: %s", resp.StatusCode, truncate(string(bs), 400))
	}

	var parsed struct {
		Status      string                `json:"status"`
		Message     string                `json:"message,omitempty"`
		WordsResult []model.OcrWordBlock `json:"words_result"`
	}
	if err = json.Unmarshal(bs, &parsed); err != nil {
		return nil, fmt.Errorf("parse ocr response: %w; body=%s", err, truncate(string(bs), 200))
	}
	if parsed.Status != "succeeded" {
		return nil, fmt.Errorf("OCR 未成功: %s", parsed.Message)
	}
	return parsed.WordsResult, nil
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
