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

// OcrClient 智谱 BigModel OCR (hand_write)
type OcrClient struct {
	apiKey    string
	baseURL   string
	model     string // hand_write / layout_parsing
	timeout   time.Duration
	language  string
	prob      string
}

func NewOcrClient(apiKey, baseURL, modelName string, timeoutSec int) *OcrClient {
	if baseURL == "" {
		baseURL = "https://open.bigmodel.cn/api/paas/v4"
	}
	if modelName == "" {
		modelName = "hand_write"
	}
	return &OcrClient{
		apiKey:   apiKey,
		baseURL:  baseURL,
		model:    modelName,
		timeout:  time.Duration(timeoutSec) * time.Second,
		language: "CHN_ENG",
		prob:     "true",
	}
}

// RecognizeFile 上传文件 → OCR 原始 words_result (未分行)
func (c *OcrClient) RecognizeFile(filePath string) ([]model.OcrWordBlock, error) {
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
	_ = w.WriteField("tool_type", c.model)
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
func (c *OcrClient) RecognizeBytes(name string, data []byte) ([]model.OcrWordBlock, error) {
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, err := w.CreateFormFile("file", name)
	if err != nil {
		return nil, err
	}
	if _, err = part.Write(data); err != nil {
		return nil, err
	}
	_ = w.WriteField("tool_type", c.model)
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
