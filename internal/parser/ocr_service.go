package parser

import (
	"sort"
	"strconv"
	"strings"

	"github.com/tinkler/collect-ai/internal/model"
)

// ParseOcrResponse: words_result[] → 按 top 分行 → 拆合并行
func ParseOcrResponse(blocks []model.OcrWordBlock) []model.OcrLine {
	// 1) 按 top 升序
	sort.SliceStable(blocks, func(i, j int) bool {
		if blocks[i].Top == blocks[j].Top {
			return blocks[i].Left < blocks[j].Left
		}
		return blocks[i].Top < blocks[j].Top
	})

	// 2) 自适应行高阈值
	var lines []model.OcrLine
	var currentTop, currentHeight, threshold int
	var current *model.OcrLine
	for _, b := range blocks {
		words := strings.TrimSpace(b.Words)
		if words == "" {
			continue
		}
		if current == nil {
			current = &model.OcrLine{Top: b.Top, Blocks: []model.OcrWordBlock{b}}
			currentTop = b.Top
			currentHeight = maxInt(1, b.Height)
			threshold = maxInt(8, currentHeight/2)
			continue
		}
		if absInt(b.Top-currentTop) <= threshold {
			current.Blocks = append(current.Blocks, b)
			currentTop = (currentTop + b.Top) / 2
			currentHeight = maxInt(currentHeight, b.Height)
			threshold = maxInt(8, currentHeight/2)
		} else {
			// sort by left
			sort.SliceStable(current.Blocks, func(i, j int) bool {
				return current.Blocks[i].Left < current.Blocks[j].Left
			})
			lines = append(lines, *current)
			current = &model.OcrLine{Top: b.Top, Blocks: []model.OcrWordBlock{b}}
			currentTop = b.Top
			currentHeight = maxInt(1, b.Height)
			threshold = maxInt(8, currentHeight/2)
		}
	}
	if current != nil {
		sort.SliceStable(current.Blocks, func(i, j int) bool {
			return current.Blocks[i].Left < current.Blocks[j].Left
		})
		lines = append(lines, *current)
	}

	// 3) 拆合并行
	return SplitMergedLines(lines)
}

// SplitMergedLines:
//   A 层: 多 block 行, 含 2+ 13 位 barcode block → 按 barcode 切
//   B 层: 单 block 行, 文本内 2+ 13 位数字 → 按数字切
func SplitMergedLines(lines []model.OcrLine) []model.OcrLine {
	var result []model.OcrLine
	for _, line := range lines {
		blocks := line.Blocks

		// A 层
		var barcodeIdxs []int
		for i, b := range blocks {
			w := clean(b.Words)
			if looksLikeBarcode(w) {
				barcodeIdxs = append(barcodeIdxs, i)
			}
		}
		if len(barcodeIdxs) > 1 {
			for k := 0; k < len(barcodeIdxs); k++ {
				start := barcodeIdxs[k]
				end := lineEnd(barcodeIdxs, k, len(blocks))
				if start > 0 {
					prev := clean(blocks[start-1].Words)
					if isShortRowNo(prev) {
						start = start - 1
					}
				}
				seg := append([]model.OcrWordBlock(nil), blocks[start:end]...)
				result = append(result, model.OcrLine{Top: line.Top, Blocks: seg})
			}
			continue
		}

		// B 层
		if len(blocks) == 1 {
			text := clean(blocks[0].Words)
			bcPos := findBarcodePositions(text)
			if len(bcPos) > 1 {
				for k := 0; k < len(bcPos); k++ {
					start := bcPos[k]
					end := lineEndPos(bcPos, k, len(text))
					segText := strings.TrimSpace(text[start:end])
					if segText == "" {
						continue
					}
					result = append(result, model.OcrLine{
						Top: line.Top,
						Blocks: []model.OcrWordBlock{{
							Words:   segText,
							Left:    blocks[0].Left + start,
							Top:     line.Top,
							Width:   end - start,
							Height:  blocks[0].Height,
							Average: blocks[0].Average,
						}},
					})
				}
				continue
			}
		}

		result = append(result, line)
	}
	return result
}

func lineEnd(idxs []int, k, total int) int {
	if k+1 < len(idxs) {
		return idxs[k+1]
	}
	return total
}

func lineEndPos(idxs []int, k, total int) int {
	if k+1 < len(idxs) {
		return idxs[k+1]
	}
	return total
}

// isShortRowNo: 1-3 位纯数字 (行号) 且不是 barcode
func isShortRowNo(s string) bool {
	if s == "" || len(s) > 3 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return !looksLikeBarcode(s)
}

// findBarcodePositions: text 内所有 13 位纯数字的起始位置
func findBarcodePositions(text string) []int {
	var positions []int
	i := 0
	for i < len(text) {
		runStart := -1
		for i < len(text) && text[i] >= '0' && text[i] <= '9' {
			if runStart < 0 {
				runStart = i
			}
			i++
		}
		if runStart >= 0 {
			runLen := i - runStart
			if runLen == 13 {
				positions = append(positions, runStart)
			}
			runStart = -1
		} else {
			i++
		}
	}
	return positions
}

func looksLikeBarcode(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 6 || len(s) > 14 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func clean(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "　", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}

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

// ParseQty 启发式: 行内最右最大数字 (排除规格)
func ParseQty(line model.OcrLine) *int {
	if len(line.Blocks) == 0 {
		return nil
	}
	if len(line.Blocks) == 1 {
		t := clean(line.Blocks[0].Words)
		// 找最大数字
		digits := extractNumbers(t)
		if len(digits) == 0 {
			return nil
		}
		best := digits[0]
		for _, d := range digits {
			if d > best {
				best = d
			}
		}
		return &best
	}
	// 多 block: 找最右 + 全数字 + <= 4 字符
	for i := len(line.Blocks) - 1; i >= 0; i-- {
		t := clean(line.Blocks[i].Words)
		if t == "" {
			continue
		}
		if len(t) > 4 {
			continue
		}
		if v, err := strconv.Atoi(t); err == nil {
			return &v
		}
	}
	// 兜底: 全部数字合并后提取
	all := ""
	for _, b := range line.Blocks {
		all += clean(b.Words) + " "
	}
	digits := extractNumbers(all)
	if len(digits) == 0 {
		return nil
	}
	best := digits[0]
	for _, d := range digits {
		if d > best {
			best = d
		}
	}
	return &best
}

func extractNumbers(s string) []int {
	var out []int
	i := 0
	for i < len(s) {
		if s[i] >= '0' && s[i] <= '9' {
			start := i
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}
			// 排除 13+ 位 barcode
			numStr := s[start:i]
			if len(numStr) < 6 {
				if v, err := strconv.Atoi(numStr); err == nil {
					out = append(out, v)
				}
			}
		} else {
			i++
		}
	}
	return out
}
