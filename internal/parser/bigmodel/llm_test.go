package bigmodel

import (
	"errors"
	"strings"
	"testing"
)

// 2026-09-04 线上事故: GLM-4V max_tokens=1024 触顶,响应被截断
// (finish_reason=length),ParseLlmJson 应该能:
//   1. 检测到截断(没闭合 ])
//   2. 尝试截到最后一个 } 之前挽救部分 rows
//   3. 挽救失败时返回 ErrTruncated 让 caller 知道

func TestParseLlmJson_Normal(t *testing.T) {
	// 正常 JSON 响应: 完整 fence + 完整 JSON
	msg := "```json\n{\"rows\": [{\"barcode\": \"6901234567890\", \"name\": \"可口可乐\", \"qty\": 6, \"type\": \"data\"}]}\n```"
	rows, err := ParseLlmJson(msg)
	if err != nil {
		t.Fatalf("正常 JSON 不应失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows 数量 = %d, want 1", len(rows))
	}
	if rows[0].Barcode != "6901234567890" {
		t.Errorf("barcode = %q, want 6901234567890", rows[0].Barcode)
	}
	if rows[0].Qty == nil || *rows[0].Qty != 6 {
		t.Errorf("qty = %v, want 6", rows[0].Qty)
	}
}

func TestParseLlmJson_Truncated_RecoverPart(t *testing.T) {
	// 截断响应: 2 个 row, 第二个 row 中途被截断
	// 期望: 挽救第一个完整 row, 返回 1 条
	truncated := `{"rows": [{"barcode": "6901234567890", "name": "可口可乐", "qty": 6, "type": "data"}, {"barcode": "6909876543210", "name": "雪碧`
	// 没有闭合 ] 和 } - 模拟 max_tokens 触顶

	rows, err := ParseLlmJson(truncated)
	if err != nil {
		t.Fatalf("截断应能挽救至少 1 行, 不应失败: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("截断应只恢复 1 个完整 row, got %d", len(rows))
	}
	if len(rows) > 0 && rows[0].Barcode != "6901234567890" {
		t.Errorf("恢复的 row 应该是第一个完整 row, got barcode=%q", rows[0].Barcode)
	}
}

func TestParseLlmJson_Truncated_ReturnErrTruncated(t *testing.T) {
	// 完全无法挽救的截断: 第一个 { 也不完整
	// (实际场景: VLM 在第一个 { 中途就被截断, 完全没产出任何完整 row)
	truncated := `{"rows": [{"barcode": "6901234567890", "na`

	_, err := ParseLlmJson(truncated)
	if err == nil {
		t.Fatal("无法挽救时应返回 error")
	}
	if !errors.Is(err, ErrTruncated) {
		t.Errorf("应返回 ErrTruncated, got %v", err)
	}
	if !strings.Contains(err.Error(), "max_tokens") {
		t.Errorf("err 应说明原因 max_tokens, got %q", err.Error())
	}
}

func TestParseLlmJson_NotTruncated_NotJSON(t *testing.T) {
	// 看起来不是截断 (有闭合 ]), 但 JSON 完全乱
	// 应返回普通 "非 JSON" 错误, 不是 ErrTruncated
	garbage := "this is not json at all, but ends with }"
	_, err := ParseLlmJson(garbage)
	if err == nil {
		t.Fatal("乱码应失败")
	}
	if errors.Is(err, ErrTruncated) {
		t.Errorf("非截断场景不应返回 ErrTruncated, got %v", err)
	}
}

func TestParseLlmJson_SkipType(t *testing.T) {
	// type=skip 应被过滤
	msg := `{"rows": [
		{"barcode": "6901234567890", "name": "可口可乐", "qty": 6, "type": "data"},
		{"barcode": "6909876543210", "name": "雪碧", "qty": 3, "type": "skip"}
	]}`
	rows, err := ParseLlmJson(msg)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("应过滤 skip 行, rows = %d, want 1", len(rows))
	}
}

func TestParseLlmJson_HeaderFiltered(t *testing.T) {
	// "行号 条码 商品名称 数量 单价 金额" 这种 header 行应被过滤
	msg := `{"rows": [
		{"barcode": "", "name": "行号 条码 商品名称 数量 单价 金额", "qty": 0, "type": "header"},
		{"barcode": "6901234567890", "name": "可口可乐", "qty": 6, "type": "data"}
	]}`
	rows, err := ParseLlmJson(msg)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("header 应过滤, rows = %d, want 1", len(rows))
	}
	if rows[0].Barcode != "6901234567890" {
		t.Errorf("剩余应是有效行, got barcode=%q", rows[0].Barcode)
	}
}
