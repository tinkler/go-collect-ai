package matcher

import (
	"math"
	"strings"

	"github.com/tinkler/collect-ai/internal/model"
)

// SkuMatcher 5 级级联匹配
// 1) barcode exact
// 2) name exact (with/without space, case-insensitive)
// 3) name no-space + Levenshtein <= fuzzy
// 4) substring (收紧: 任一端 >= 4 字符, 长度差 <= 3, 取长度差最小)
// 5) 失败 → IsNew
type SkuMatcher struct {
	skus         []model.SkuRecord
	fuzzyDist    int
	byBarcode    map[string]model.SkuRecord
	byName       map[string]model.SkuRecord
	byNameNoSp   map[string]model.SkuRecord
}

func New(skus []model.SkuRecord, fuzzyDist int) *SkuMatcher {
	m := &SkuMatcher{
		skus:       skus,
		fuzzyDist:  maxInt(0, fuzzyDist),
		byBarcode:  make(map[string]model.SkuRecord, len(skus)),
		byName:     make(map[string]model.SkuRecord, len(skus)),
		byNameNoSp: make(map[string]model.SkuRecord, len(skus)),
	}
	for _, s := range skus {
		if s.Barcode != "" {
			if _, exists := m.byBarcode[s.Barcode]; !exists {
				m.byBarcode[s.Barcode] = s
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
		if hit, ok := m.byBarcode[strings.TrimSpace(ocr.Barcode)]; ok {
			m.applyMatch(&row, hit, "OK")
			return row
		}
	}

	if ocr.Name != "" {
		n := strings.TrimSpace(ocr.Name)
		// 2) name exact
		if hit, ok := m.byName[n]; ok {
			m.applyMatch(&row, hit, "修正(名称)")
			return row
		}
		// 3) name no-space exact
		ns := removeSpaces(n)
		if ns != "" {
			if hit, ok := m.byNameNoSp[ns]; ok {
				m.applyMatch(&row, hit, "修正(名称)")
				return row
			}
		}
		// 4) fuzzy (Levenshtein)
		if m.fuzzyDist > 0 && ns != "" {
			if best := m.findFuzzy(ns, m.fuzzyDist); best != nil {
				m.applyMatch(&row, *best, "修正(模糊)")
				return row
			}
		}
		// 5) substring (收紧版)
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
	for k, v := range m.byNameNoSp {
		if absInt(kLen(k)-len(s)) > maxDist {
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
	for k, v := range m.byNameNoSp {
		if kLen(k) < 4 || len(s) < 4 {
			continue
		}
		if absInt(kLen(k)-len(s)) > 3 {
			continue
		}
		if strings.Contains(s, k) || strings.Contains(k, s) {
			diff := absInt(kLen(k) - len(s))
			if diff < bestDiff {
				bestDiff = diff
				best = &v
			}
		}
	}
	return best
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
