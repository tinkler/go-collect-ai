package matcher

import (
	"strings"
	"testing"

	"github.com/tinkler/collect-ai/internal/model"
)

// ============================================================
// W3.1: L3 段位单测 — barcode 后 4 位 + 名称 Jaccard ≥ 0.6
// 场景: OCR 漏掉条码前缀(打印不全/裁切/手写),只识别到后几位
// ============================================================

// helper: 构造 mock SKU 库
func skus(records ...model.SkuRecord) []model.SkuRecord { return records }

func TestL3_Hit_BarcodeSuffix4_NameSimilar(t *testing.T) {
	// SKU 库: 1 个商品,完整 barcode 13 位,名称"可口可乐330ml"(无空格)
	// OCR 漏掉前 9 位,只识别到后 4 位"7890",名称识别为"可口可乐 330ml"(含空格)
	//   → L2 name exact 失败("可口可乐 330ml" != "可口可乐330ml")
	//   → L3 后缀匹配 + Jaccard("可口可乐 330ml" vs "可口可乐330ml" removeSpaces 完全等 → 1.0)→ 命中
	m := New(skus(
		model.SkuRecord{Barcode: "6901234567890", Name: "可口可乐330ml", MainSuppId: "S1", MainSuppName: "汇一"},
	), 0)

	got := m.Match(model.ParsedOcrRow{
		Barcode: "7890", // OCR 只识别到后 4 位
		Name:    "可口可乐 330ml",
	}, 1)

	if got.Status != "修正(条码后缀+名称模糊)" {
		t.Errorf("status = %q, want 修正(条码后缀+名称模糊)", got.Status)
	}
	if got.MatchedBarcode != "6901234567890" {
		t.Errorf("matched_barcode = %q, want 6901234567890", got.MatchedBarcode)
	}
	if got.IsNew {
		t.Errorf("IsNew = true, want false (L3 应命中)")
	}
}

func TestL3_Miss_BelowThreshold(t *testing.T) {
	// SKU: "可口可乐 330ml"
	// OCR: barcode 后 4 位 + 名称完全不一样("雪碧柠檬味 500ml")
	m := New(skus(
		model.SkuRecord{Barcode: "6901234567890", Name: "可口可乐 330ml"},
	), 0)

	got := m.Match(model.ParsedOcrRow{
		Barcode: "0123",
		Name:    "雪碧柠檬味 500ml",
	}, 1)

	// L3 失败,后续 L4 (Levenshtein) 也失败,L5 (substring) 也失败
	// 期望: IsNew=true, 落到兜底
	if !got.IsNew {
		t.Errorf("L3 不命中 + 后续也不命中,IsNew = false, want true. got status=%q", got.Status)
	}
	if got.MatchedName != "雪碧柠檬味 500ml" {
		t.Errorf("未命中时应保留 OCR 原文, got matched_name=%q", got.MatchedName)
	}
}

func TestL3_TooShortBarcode_Skip(t *testing.T) {
	// barcode < 4 位 → L3 跳过
	m := New(skus(
		model.SkuRecord{Barcode: "6901234567890", Name: "可口可乐"},
	), 0)

	got := m.Match(model.ParsedOcrRow{
		Barcode: "12",  // 太短
		Name:    "可口可乐",
	}, 1)

	// L1 失败 (bc 不全等) / L2 失败 (无) / L3 跳过 (太短)
	// L4 (Levenshtein) 命中 (fuzzy=0,但 removeSpaces 后"可口可乐"=="可口可乐")
	if got.Status != "修正(名称)" {
		t.Errorf("L3 跳过时,L4 应兜底命中, got status=%q", got.Status)
	}
}

func TestL3_NoBarcode_Skip(t *testing.T) {
	// OCR 没识别到 barcode → 跳过 L3
	m := New(skus(
		model.SkuRecord{Barcode: "6901234567890", Name: "可口可乐 330ml"},
	), 0)

	got := m.Match(model.ParsedOcrRow{
		Barcode: "",
		Name:    "可口可乐 330ml",
	}, 1)

	// L1 跳过 (无 bc) / L2 命中 (name 完全一致)
	if got.Status != "OK" && got.Status != "修正(名称)" {
		t.Errorf("name 完全一致应 L1/L2 命中, got status=%q", got.Status)
	}
	if got.IsNew {
		t.Errorf("name 完全匹配不应 IsNew")
	}
}

func TestL3_ThresholdBoundary(t *testing.T) {
	// 验证 0.6 是 Jaccard 阈值下限
	// "可口可乐" 2-grams: {可口,口可,可乐} (3)
	// "可口可乐 330ml" 2-grams: {可口,口可,可乐,乐 , 33,330,30,0m,ml} (let me recount)
	// 实际: "可口可乐 330ml" runes = [可 口 可 乐   3 3 0 m l]
	// 2-grams: 可口,口可, 可乐,乐 ,  3, 33,30,0m,ml → 9 个
	// 交集: 可口,口可,可乐 → 3
	// 并集: 3+9-3 = 9
	// Jaccard = 3/9 = 0.333 < 0.6 → L3 不命中

	// 测"中度相似"刚好在阈值上下
	// 候选 SKU name "可口可乐水" (5 runes: 可口口可乐乐水) → 4 grams
	// OCR name "可口可乐" → 3 grams
	// 交集: 可口,口可,可乐 = 3
	// 并集: 3+4-3 = 4
	// Jaccard = 3/4 = 0.75 > 0.6 → L3 命中
	m := New(skus(
		model.SkuRecord{Barcode: "6901234567890", Name: "可口可乐水"},
	), 0)

	got := m.Match(model.ParsedOcrRow{
		Barcode: "7890",
		Name:    "可口可乐",
	}, 1)

	if got.Status != "修正(条码后缀+名称模糊)" {
		t.Errorf("Jaccard=0.75 应命中 L3, got status=%q", got.Status)
	}
}

func TestL3_PriorOverLevenshtein(t *testing.T) {
	// L3 命中应先于 L4 (Levenshtein)
	// 准备 1 个 SKU,barcode 匹配 + name 模糊
	m := New(skus(
		model.SkuRecord{Barcode: "6901234567890", Name: "可口可乐"},
	), 5) // fuzzy=5 允许较多编辑距离

	_ = m.Match(model.ParsedOcrRow{
		Barcode: "7890",
		Name:    "可口可乐", // name 完全一致,应 L2 命中
	}, 1)

	// 实际 L2 (name exact) 会先命中,不是 L3
	// 这里测: L2 失败时,L3 命中,不会落到 L4
	m2 := New(skus(
		model.SkuRecord{Barcode: "6901234567890", Name: "可口可乐水"},
	), 5)

	got2 := m2.Match(model.ParsedOcrRow{
		Barcode: "7890",
		Name:    "可口可乐", // L2 失败, L3 应命中 (Jaccard 高)
	}, 1)

	if got2.Status != "修正(条码后缀+名称模糊)" {
		t.Errorf("L2 失败时 L3 应优先, got status=%q", got2.Status)
	}
}

func TestL3_MultipleCandidatesPickBest(t *testing.T) {
	// 多个 SKU 共用后 4 位,选 Jaccard 最高的
	m := New(skus(
		model.SkuRecord{Barcode: "1111000000001", Name: "完全不一样的东西"},
		model.SkuRecord{Barcode: "2222000000001", Name: "可口可乐"},
		model.SkuRecord{Barcode: "3333000000001", Name: "可口可乐水"},
	), 0)

	got := m.Match(model.ParsedOcrRow{
		Barcode: "0001", // 3 个 SKU 都有这个后缀
		Name:    "可口可乐",
	}, 1)

	// "可口可乐" vs "可口可乐" → Jaccard 1.0 (最高,直接命中)
	// "可口可乐" vs "可口可乐水" → Jaccard 0.75
	// "可口可乐" vs "完全不一样的东西" → 0
	if got.MatchedBarcode != "2222000000001" {
		t.Errorf("应选 '可口可乐' 完美匹配, got matched_barcode=%q (status=%q)",
			got.MatchedBarcode, got.Status)
	}
}

func TestL3_AllSegments_VerifyOrder(t *testing.T) {
	// 验证 L1-L6 段位顺序(回归测试)
	// 每个 subtest 用独立 matcher + 独立 SKU 库,避免 L2 name exact 干扰
	cases := []struct {
		name       string
		skus       []model.SkuRecord
		fuzzy      int
		ocr        model.ParsedOcrRow
		wantStatus string
		wantBarcode string
	}{
		{
			name: "L1_barcode_exact",
			skus: []model.SkuRecord{
				{Barcode: "A", Name: "任意名"},
			},
			fuzzy: 1,
			ocr:        model.ParsedOcrRow{Barcode: "A", Name: "其他名称"},
			wantStatus: "OK",
			wantBarcode: "A",
		},
		{
			name: "L2_name_exact",
			skus: []model.SkuRecord{
				{Barcode: "X", Name: "可口可乐"},
			},
			fuzzy: 1,
			ocr:        model.ParsedOcrRow{Barcode: "ZZ", Name: "可口可乐"},
			wantStatus: "修正(名称)",
			wantBarcode: "X",
		},
		{
			name: "L3_barcode_suffix4",
			// SKU barcode "XX0123" 后 4 位 = "0123",OCR 也只识别到 "0123"
			// OCR name "可口可乐水" 跟 SKU name "可口可乐" 不等(避免 L2)
			// 但 Jaccard 高(0.75)触发 L3
			skus: []model.SkuRecord{
				{Barcode: "XX0123", Name: "可口可乐"},
			},
			fuzzy: 1,
			ocr:        model.ParsedOcrRow{Barcode: "0123", Name: "可口可乐水"},
			wantStatus: "修正(条码后缀+名称模糊)",
			wantBarcode: "XX0123",
		},
		{
			name: "L4_levenshtein",
			// 库内只有 1 个 SKU "可口可乐汤",OCR name "可口可乐糖" 距离 1
			// 注意:必须避免 L5 substring 命中 - 但 "可口可乐" 是 "可口可乐糖" 的子串,
			//   所以 sku 库要避开"可口可乐"。这里用 "果粒橙" + "果粒橙汤" 避免 substring 干扰
			skus: []model.SkuRecord{
				{Barcode: "D", Name: "果粒橙汤"},
			},
			fuzzy: 1,
			ocr:        model.ParsedOcrRow{Barcode: "ZZ", Name: "果粒橙糖"},
			wantStatus: "修正(模糊)",
			wantBarcode: "D",
		},
		{
			name: "L5_substring",
			// 长度差 ≤ 3 + 任一端 ≥ 4 字符 + contains 关系
			skus: []model.SkuRecord{
				{Barcode: "E", Name: "其他可乐330ml"},
			},
			fuzzy: 1,
			ocr:        model.ParsedOcrRow{Barcode: "ZZ", Name: "可乐330ml"},
			wantStatus: "修正(子串)",
			wantBarcode: "E",
		},
		{
			name: "L6_new_sku",
			skus: []model.SkuRecord{
				{Barcode: "F", Name: "完全没关系的商品"},
			},
			fuzzy: 1,
			ocr:        model.ParsedOcrRow{Barcode: "Q", Name: "完全没见过的商品"},
			wantStatus: "新SKU",
			wantBarcode: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(tc.skus, tc.fuzzy)
			got := m.Match(tc.ocr, 1)
			t.Logf("[debug %s] ocr=%+v matched=%q status=%q isNew=%v",
				tc.name, tc.ocr, got.MatchedBarcode, got.Status, got.IsNew)
			if got.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q (matched=%q, name=%q)",
					got.Status, tc.wantStatus, got.MatchedBarcode, got.MatchedName)
			}
			if tc.wantBarcode != "" && got.MatchedBarcode != tc.wantBarcode {
				t.Errorf("matched_barcode = %q, want %q", got.MatchedBarcode, tc.wantBarcode)
			}
		})
	}
}

func TestL3_BigramEdgeCases(t *testing.T) {
	// 1-字符 name → bigrams 返回 nil → L3 自动跳过
	m := New(skus(
		model.SkuRecord{Barcode: "1234567890123", Name: "可"},
	), 0)

	got := m.Match(model.ParsedOcrRow{
		Barcode: "0123",
		Name:    "可",
	}, 1)

	// L3: name 1 字符 → bigrams nil → 跳过
	// L2: "可" 完全等 → 命中
	if !strings.Contains(got.Status, "OK") && !strings.Contains(got.Status, "名称") {
		t.Errorf("1-字符 name 应 L2 命中, got status=%q", got.Status)
	}
}

func TestL3_EmptyNameNameGrams(t *testing.T) {
	// OCR name 空 → L3 跳过 (name 空不可能算 Jaccard)
	m := New(skus(
		model.SkuRecord{Barcode: "1234567890123", Name: "可口可乐"},
	), 0)

	got := m.Match(model.ParsedOcrRow{
		Barcode: "0123",
		Name:    "", // OCR 没识别到名称
	}, 1)

	// L3 跳过,L4 跳过(无 name),L5 跳过
	// 应该: IsNew=true
	if !got.IsNew {
		t.Errorf("无 name 时应 IsNew, got status=%q isNew=%v", got.Status, got.IsNew)
	}
}
