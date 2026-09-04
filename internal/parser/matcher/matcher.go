package matcher

import (
	"math"
	"strings"

	"github.com/tinkler/collect-ai/internal/model"
)

// SkuMatcher 3 级匹配 (2026-09-04 用户重定义: 砍掉 L4/L5/L6 模糊段位)
//
//	2026-09-04 用户重定义: matcher 只做 3 级段位,不够 L1/L2/L3 的统一原封不动当新品。
//	status 字段: "L1 精确(条码)" / "L2 精确(名称)" / "L3 修正(条码后缀+名称模糊)" /
//	             "L3 修正(条码相似+名称相似)" / "新品"
//
// 1) L1: barcode exact (任意位数 6-14,全等)
// 2) L2: name exact (with/without space, case-insensitive)
// 3) L3 拆 2 路径:
//    a) 后 4 位 barcode + 名称 Jaccard >= 0.6   (OCR 漏掉前缀场景)
//    b) barcode 相似 + 名称相似: 13 位差异 <= 3, 短码差异 <= 1
// 不满足 L1/L2/L3 -> IsNew=true, 原封不动保留 OCR 输出 (barcode/name/qty 原值)
type SkuMatcher struct {
	skus             []model.SkuRecord
	fuzzyDist        int // 保留字段(向后兼容), 当前实现不再使用
	byBarcode        map[string]model.SkuRecord
	byName           map[string]model.SkuRecord
	byNameNoSp       map[string]model.SkuRecord
	byBarcodeSuffix4 map[string][]model.SkuRecord // L3a: barcode 后 4 位桶
}

const (
	// L3a 名称相似度阈值 (Jaccard 字符 2-gram)
	l3JaccardThreshold = 0.6
	// L3b barcode 字符差异阈值
	l3bLongCodeLen   = 9  // 9+ 位算"长码"
	l3bLongCodeDiff  = 3  // 长码允许 3 个字符差异
	l3bShortCodeLen  = 8  // <= 8 位算"短码"
	l3bShortCodeDiff = 1  // 短码允许 1 个字符差异
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
			// 2026-09-02 Phase A: cube 返的 barcode 带尾空格, Match 时 OCR 端会 TrimSpace,
			//   但索引时没 trim, 永远 miss. 索引 + 后 4 位桶都先 TrimSpace 一次.
			bc := strings.TrimSpace(s.Barcode)
			if bc != "" {
				if _, exists := m.byBarcode[bc]; !exists {
					m.byBarcode[bc] = s
				}
				// L3a: barcode 后 4 位入桶
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

// Match OCR 解析行 -> SkuRow
//
//	2026-09-04 用户重定义: 3 级段位 (L1/L2/L3), 不满足的原封不动当新品
//	  L1: barcode exact (任意位数 6-14 全等)
//	  L2: name exact (with/without space, case-insensitive)
//	  L3a: 后 4 位 barcode + 名称 Jaccard >= 0.6   (OCR 漏掉前缀)
//	  L3b: barcode 字符相似 + 名称相似 (13 位差异 <= 3, 短码差异 <= 1)
//	  新品: 不满足上面任何段位, 保留 OCR 原始 barcode/name/qty
func (m *SkuMatcher) Match(ocr model.ParsedOcrRow, seq int) model.SkuRow {
	row := model.SkuRow{
		Seq:        seq,
		RawBarcode: ocr.Barcode,
		RawName:    ocr.Name,
		RawQty:     ocr.QtyRaw,
		Qty:        ocr.Qty,
	}

	// 1) L1: barcode exact (任意位数, 全等)
	if ocr.Barcode != "" {
		bc := strings.TrimSpace(ocr.Barcode)
		if bc != "" {
			if hit, ok := m.byBarcode[bc]; ok {
				m.applyMatch(&row, hit, "L1 精确(条码)")
				return row
			}
		}
	}

	if ocr.Name != "" {
		n := strings.TrimSpace(ocr.Name)
		// 2) L2: name exact (with space)
		if n != "" {
			if hit, ok := m.byName[n]; ok {
				m.applyMatch(&row, hit, "L2 精确(名称)")
				return row
			}
		}
		// 2b) L2: name exact (without space) -- 大小写/全半角空格统一
		ns := removeSpaces(n)
		if ns != "" {
			if hit, ok := m.byNameNoSp[ns]; ok {
				m.applyMatch(&row, hit, "L2 精确(名称去空)")
				return row
			}
		}

		// 3a) L3a: 后 4 位 barcode + 名称 Jaccard >= 0.6
		//    场景: OCR 漏掉条码前缀(打印不清/裁切),只识别到后几位
		//    桶预计算 -> 同桶的 sku 与 OCR name 做相似度,取最高分且 >= 0.6
		if ocr.Barcode != "" {
			if hit := m.findByBarcodeSuffix4(ocr.Barcode, n); hit != nil {
				m.applyMatch(&row, *hit, "L3 修正(条码后缀+名称模糊)")
				return row
			}
		}

		// 3b) L3b: barcode 字符相似 + 名称相似
		//    用户定义: 13 位差异 <= 3, 短码(< 13 位) 差异 <= 1
		//    实现: 对每个 SKU barcode b, 与 OCR barcode a 按位比较
		//      - 长度相同: 位级 Hamming 距离
		//      - 长度差 1: 短码侧在头部/尾部补位, 取 min 距离
		//      - 长度差 > 1: 跳过
		//    名称 Jaccard >= 0.6 (跟 L3a 共用阈值)
		if ocr.Barcode != "" {
			if hit := m.findByBarcodeSimilar(ocr.Barcode, ns); hit != nil {
				if hit.Name != "" {
					if nameJaccard(bigrams(removeSpaces(hit.Name)), ns) >= l3JaccardThreshold {
						m.applyMatch(&row, *hit, "L3 修正(条码相似+名称相似)")
						return row
					}
				}
			}
		}
	}

	// 不满足 L1/L2/L3a/L3b -> 原封不动录入新品
	// 关键: IsNew=true, 但 matched_barcode/matched_name 用 OCR 原值 (跟之前留空不同)
	// 业务定义: "新品" 意味着这个商品还没在 SKU 库注册, 但本次识别结果保留
	// 等用户手工录入到 SKU 库后, 后续解析这个 barcode/name 就能走 L1/L2 命中
	row.IsNew = true
	row.Status = "新品"
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

// findByBarcodeSuffix4 L3a 段位
//
//	场景: OCR 漏掉条码前缀(打印不全/裁切/手写),只识别到后 N 位
//	算法:
//	  1) 取 ocr.Barcode 后 4 位 -> 在 byBarcodeSuffix4 桶里找候选
//	  2) 对每个候选,跟 ocr.Name 做 Jaccard 字符 2-gram 相似度
//	  3) 取最高分且 >= 0.6 的候选返回
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

// findByBarcodeSimilar L3b 段位 (2026-09-04 用户新定义)
//
//	场景: barcode 字符相近(用户描述: 13 位差 <= 3, 短码差 <= 1),
//	  名称也相似(由 caller 二次校验 Jaccard >= 0.6)
//
//	算法:
//	  1) 遍历所有 SKU barcode b
//	  2) 长度差 <= 1 (允许 1 位 prefix 差异, e.g. 漏掉前导 0)
//	  3) 字符差异数 (barcodeCharDiff):
//	     - 同长度: 按位比较, 差异字符数 = Hamming 距离
//	     - 长度差 1: 把长码各位置删 1 位, 看是否等于短码, 取 min 差异
//	  4) 阈值: 9+ 位算"长码" 差异 <= 3, <= 8 位算"短码" 差异 <= 1
//
//	返回: 第一个命中(差异数最小)的候选, 由 caller 做名称 Jaccard 二次校验
func (m *SkuMatcher) findByBarcodeSimilar(ocrBarcode, ocrNameNoSp string) *model.SkuRecord {
	if ocrBarcode == "" || len(ocrBarcode) < 6 {
		return nil
	}
	var best *model.SkuRecord
	bestDiff := math.MaxInt32

	for _, s := range m.skus {
		bc := strings.TrimSpace(s.Barcode)
		if bc == "" {
			continue
		}
		// 长度差 <= 1
		absLenDiff := absInt(len(bc) - len(ocrBarcode))
		if absLenDiff > 1 {
			continue
		}
		// 字符差异数
		diff := barcodeCharDiff(bc, ocrBarcode)
		if diff < 0 {
			continue
		}
		// 阈值: 长码 9+ 位 -> diff <= 3, 短码 <= 8 位 -> diff <= 1
		refLen := len(bc)
		if len(ocrBarcode) < refLen {
			refLen = len(ocrBarcode)
		}
		var allowed int
		if refLen >= l3bLongCodeLen {
			allowed = l3bLongCodeDiff
		} else {
			allowed = l3bShortCodeDiff
		}
		if diff > allowed {
			continue
		}
		// 取差异最小的
		if diff < bestDiff {
			bestDiff = diff
			best = &s
			if diff == 0 {
				break
			}
		}
	}
	return best
}

// barcodeCharDiff 计算两个 barcode 的字符差异数
//
//	同长度: Hamming 距离 (按位比较, 差异字符数)
//	长度差 1: 等价于"长码删 1 位 -> 短码"的 Hamming 距离
//	  实际: 遍历长码每个位置 i 删 1 位, 看是否等于短码, 取 min 差异
//	长度差 > 1: 返 -1 (调用方应跳过)
func barcodeCharDiff(a, b string) int {
	la, lb := len(a), len(b)
	if absInt(la-lb) > 1 {
		return -1
	}
	if la == lb {
		diff := 0
		for i := 0; i < la; i++ {
			if a[i] != b[i] {
				diff++
			}
		}
		return diff
	}
	// 长度差 1: 把长的当成"短码 + 1 位" 的所有可能, 取 min diff
	var short, long string
	if la < lb {
		short, long = a, b
	} else {
		short, long = b, a
	}
	best := math.MaxInt32
	ls := len(short)
	ll := len(long)
	for i := 0; i <= ll-ls; i++ {
		// 删 long[i] 这位, 比 short[0..ls]
		diff := 0
		for j := 0; j < ls; j++ {
			// long 在 short 索引 j 的位置: j < i 用 j, 否则用 j+1
			longIdx := j
			if j >= i {
				longIdx = j + 1
			}
			if short[j] != long[longIdx] {
				diff++
			}
		}
		if diff < best {
			best = diff
			if best == 0 {
				return 0
			}
		}
	}
	return best
}

// bigrams 字符 2-gram 集合(中文 OK,按 rune 切)
//
//	例: "可口可乐" -> {"可口","口可","可乐"}
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
