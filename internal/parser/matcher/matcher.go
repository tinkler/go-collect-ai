package matcher

import (
	"math"
	"strings"

	"github.com/tinkler/collect-ai/internal/model"
)

// SkuMatcher 6 级级联匹配 (W3.1: 新增 L3 段位)
// 1) barcode exact
// 2) name exact (with/without space, case-insensitive)
// 3) barcode suffix(4) + name jaccard ≥ 0.6   (2026-09-01 W3.1: L3 段位,OCR 漏掉前缀场景)
// 4) name no-space + Levenshtein <= fuzzy
// 5) substring (收紧: 任一端 >= 4 字符, 长度差 <= 3, 取长度差最小)
// 6) 失败 → IsNew
type SkuMatcher struct {
	skus             []model.SkuRecord
	fuzzyDist        int
	byBarcode        map[string]model.SkuRecord
	byName           map[string]model.SkuRecord
	byNameNoSp       map[string]model.SkuRecord
	byBarcodeSuffix4 map[string][]model.SkuRecord // W3.1: barcode 后 4 位桶
}

const (
	// L3 名称相似度阈值 (Jaccard 字符 2-gram)
	l3JaccardThreshold = 0.6
)

func New(skus []model.SkuRecord, fuzzyDist int) *SkuMatcher {
	m := &SkuMatcher{
		skus:             skus,
		fuzzyDist:        maxInt(0, fuzzyDist),
		byBarcode:        make(map[string]model.SkuRecord, len(skus)),
		byName:           make(map[string]model.SkuRecord, len(skus)),
		byNameNoSp:       make(map[string]model.SkuRecord, len(skus)),
		byBarcodeSuffix4: make(map[string][]model.SkuRecord),
	}
	for _, s := range skus {
		if s.Barcode != "" {
			// 2026-09-02 Phase A: cube 返的 barcode 带尾空格 (e.g. "447600408           "),
			//   Match 时 OCR 端会 TrimSpace, 但索引时没 trim, 永远 miss.
			//   索引 + 后 4 位桶都先 TrimSpace 一次.
			bc := strings.TrimSpace(s.Barcode)
			if bc != "" {
				if _, exists := m.byBarcode[bc]; !exists {
					m.byBarcode[bc] = s
				}
				// W3.1 L3: barcode 后 4 位入桶
				if len(bc) >= 4 {
					suf := bc[len(bc)-4:]
					m.byBarcodeSuffix4[suf] = append(m.byBarcodeSuffix4[suf], s)
				}
			}
		}
		if s.Name != "" {
			n := strings.TrimSpace(s.Name)
			if n != "" {
				if _, exists := m.byName[n]; !exists {
					m.byName[n] = s
				}
				ns := removeSpaces(n)
				if ns != "" {
					if _, exists := m.byNameNoSp[ns]; !exists {
						m.byNameNoSp[ns] = s
					}
				}
			}
		}
	}
	return m
}

func (m *SkuMatcher) Count() int { return len(m.skus) }

// Match OCR 解析行 → SkuRow
func (m *SkuMatcher) Match(ocr model.ParsedOcrRow, seq int) model.SkuRow {
	row := model.SkuRow{
		Seq:        seq,
		RawBarcode: ocr.Barcode,
		RawName:    ocr.Name,
		RawQty:     ocr.QtyRaw,
		Qty:        ocr.Qty,
	}

	// 1) barcode exact
	if ocr.Barcode != "" {
		bc := strings.TrimSpace(ocr.Barcode)
		// 2026-09-04 修复: GLM-4V 偶发把 13 位 EAN-13 误读为 14 位(常见:多加前缀 0
		//   或重复最后一位),导致所有级联 miss → IsNew 误标。增加 14→13 fallback:
		//   - 去首 1 位: 适用于 "0" 前缀 (e.g. "06933269100486" → "6933269100486")
		//   - 去尾 1 位: 适用于重复最后一位 (e.g. "69331691004866" → "6933169100486")
		//   - 任一 fallback 命中即用,不再 fall through
		if hit, ok := m.byBarcode[bc]; ok {
			m.applyMatch(&row, hit, "OK")
			return row
		}
		if len(bc) == 14 {
			if len(bc) >= 2 {
				if hit, ok := m.byBarcode[bc[1:]]; ok { // 去首
					m.applyMatch(&row, hit, "OK(去首位修复14位)")
					return row
				}
				if hit, ok := m.byBarcode[bc[:len(bc)-1]]; ok { // 去尾
					m.applyMatch(&row, hit, "OK(去尾位修复14位)")
					return row
				}
			}
			// 都不命中 → fall through 到 name 匹配,14 位原值不写到 row
			ocr.Barcode = bc[:13] // 至少截成 13 位,避免污染后续 L3 桶查找
			row.RawBarcode = ocr.Barcode
		}
	}

	if ocr.Name != "" {
		n := strings.TrimSpace(ocr.Name)
		// 2) name exact
		if hit, ok := m.byName[n]; ok {
			m.applyMatch(&row, hit, "修正(名称)")
			return row
		}
		// 3) W3.1 L3: barcode 后 4 位 + 名称 Jaccard ≥ 0.6
		//    场景: OCR 漏掉条码前缀(打印不清/裁切),只识别到后几位
		//    桶预计算 → 同桶的 sku 与 OCR name 做相似度,取最高分且 ≥ 0.6
		if ocr.Barcode != "" {
			if hit := m.findByBarcodeSuffix4(ocr.Barcode, n); hit != nil {
				m.applyMatch(&row, *hit, "修正(条码后缀+名称模糊)")
				return row
			}
		}
		// 4) name no-space exact
		ns := removeSpaces(n)
		if ns != "" {
			if hit, ok := m.byNameNoSp[ns]; ok {
				m.applyMatch(&row, hit, "修正(名称)")
				return row
			}
		}
		// 5) fuzzy (Levenshtein)
		if m.fuzzyDist > 0 && ns != "" {
			if best := m.findFuzzy(ns, m.fuzzyDist); best != nil {
				m.applyMatch(&row, *best, "修正(模糊)")
				return row
			}
		}
		// 6) substring (收紧版)
		if ns != "" {
			if best := m.findSubstring(ns); best != nil {
				m.applyMatch(&row, *best, "修正(子串)")
				return row
			}
		}
	}

	row.IsNew = true
	row.Status = "新SKU"
	row.MatchedBarcode = ocr.Barcode
	row.MatchedName = ocr.Name
	return row
}

func (m *SkuMatcher) applyMatch(row *model.SkuRow, hit model.SkuRecord, status string) {
	row.MatchedBarcode = hit.Barcode
	row.MatchedName = hit.Name
	row.MatchedSupp = hit.MainSuppName
	row.MatchedSrc = hit.SrcSheet
	row.StockQty = hit.StockQty
	row.Status = status
	row.IsNew = false
}

func (m *SkuMatcher) findFuzzy(s string, maxDist int) *model.SkuRecord {
	var best *model.SkuRecord
	bestDist := math.MaxInt32
	slen := kLen(s) // W3.1 fix: 用 rune 数,非字节数(中文 4 字 = 12 字节会误剪枝)
	for k, v := range m.byNameNoSp {
		if absInt(kLen(k)-slen) > maxDist {
			continue
		}
		d := levenshtein(k, s, maxDist)
		if d < bestDist {
			bestDist = d
			best = &v
			if d == 0 {
				break
			}
		}
	}
	if bestDist <= maxDist {
		return best
	}
	return nil
}

func (m *SkuMatcher) findSubstring(s string) *model.SkuRecord {
	var best *model.SkuRecord
	bestDiff := math.MaxInt32
	slen := kLen(s) // W3.1 fix: rune 数,非字节数
	for k, v := range m.byNameNoSp {
		if kLen(k) < 4 || slen < 4 {
			continue
		}
		if absInt(kLen(k)-slen) > 3 {
			continue
		}
		if strings.Contains(s, k) || strings.Contains(k, s) {
			diff := absInt(kLen(k) - slen)
			if diff < bestDiff {
				bestDiff = diff
				best = &v
			}
		}
	}
	return best
}

// findByBarcodeSuffix4 W3.1 L3 段位
//
//	场景: OCR 漏掉条码前缀(打印不全/裁切/手写),只识别到后 N 位
//	算法:
//	  1) 取 ocr.Barcode 后 4 位 → 在 byBarcodeSuffix4 桶里找候选
//	  2) 对每个候选,跟 ocr.Name 做 Jaccard 字符 2-gram 相似度
//	  3) 取最高分且 ≥ 0.6 的候选返回
//	阈值可通过 l3JaccardThreshold 调整
func (m *SkuMatcher) findByBarcodeSuffix4(barcode, name string) *model.SkuRecord {
	if len(barcode) < 4 || name == "" {
		return nil
	}
	suf := barcode[len(barcode)-4:]
	candidates, ok := m.byBarcodeSuffix4[suf]
	if !ok || len(candidates) == 0 {
		return nil
	}
	ocrGrams := bigrams(removeSpaces(strings.TrimSpace(name)))
	if len(ocrGrams) == 0 {
		return nil
	}
	var best *model.SkuRecord
	bestScore := 0.0
	for i := range candidates {
		c := &candidates[i]
		if c.Name == "" {
			continue
		}
		score := nameJaccard(ocrGrams, removeSpaces(c.Name))
		if score > bestScore {
			bestScore = score
			best = c
		}
	}
	if bestScore >= l3JaccardThreshold {
		return best
	}
	return nil
}

// bigrams 字符 2-gram 集合(中文 OK,按 rune 切)
//
//	例: "可口可乐" → {"可口","口可","可乐"}
func bigrams(s string) map[string]struct{} {
	runes := []rune(s)
	if len(runes) < 2 {
		return nil
	}
	set := make(map[string]struct{}, len(runes))
	for i := 0; i < len(runes)-1; i++ {
		set[string(runes[i:i+2])] = struct{}{}
	}
	return set
}

// nameJaccard 计算 name 与 grams 的 Jaccard 相似度
func nameJaccard(grams map[string]struct{}, name string) float64 {
	candGrams := bigrams(name)
	if candGrams == nil {
		return 0
	}
	inter := 0
	for k := range grams {
		if _, ok := candGrams[k]; ok {
			inter++
		}
	}
	union := len(grams) + len(candGrams) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// ----- helpers -----

func removeSpaces(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, " ", ""), "　", "")
}

func kLen(s string) int { return len([]rune(s)) }

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Levenshtein 编辑距离 (中英混合 OK, 按 rune 计)
// maxDist 提前剪枝
func levenshtein(a, b string, maxDist int) int {
	ar := []rune(a)
	br := []rune(b)
	la, lb := len(ar), len(br)
	if absInt(la-lb) > maxDist {
		return maxDist + 1
	}
	// 用 2 行滚动
	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j <= lb; j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			v := min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			cur[j] = v
			if v < rowMin {
				rowMin = v
			}
		}
		if rowMin > maxDist {
			return maxDist + 1
		}
		prev, cur = cur, prev
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}
