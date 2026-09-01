// Package purchasealert 季节判定分类器 (W3.5)
//
// 三层实现:
//   1. KeywordSeasonClassifier  关键词快速匹配 (冰品→夏/暖手宝→冬/...) — 来自 OffseasonRule
//   2. BigModelSeasonClassifier LLM 语义判定 (deepseek / glm)  — 慢但准
//   3. CachingSeasonClassifier   内存 LRU 6h 缓存包装           — 避免每次都调 LLM
//
// 失败降级: LLM 不可用 / 网络错误 / JSON 解析失败 → 返回 ("neutral", nil)
//   Service 看到 neutral 不触发 alert,等同关键词模式的保守行为
package purchasealert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tinkler/collect-ai/internal/parser/bigmodel"
)

// Season 季节判定结果
type Season string

const (
	SeasonInSeason  Season = "in_season"  // 应季
	SeasonOffSeason Season = "off_season" // 反季 → 触发 alert
	SeasonNeutral   Season = "neutral"    // 中性 / 不可判定 → 不触发
	SeasonUnknown   Season = "unknown"    // 错误 / 超时 (内部)
)

// SeasonClassifier 季节判定接口
type SeasonClassifier interface {
	Classify(ctx context.Context, itemName string) Season
}

// ============================================================
// 1. 关键词分类器 (快速路径, W3.2 关键词表 + 季节组合)
// ============================================================

// KeywordSeasonClassifier 关键词 + 当前月份的简单判定
//   跟 W3.2 OffseasonRule 复用 seasonWords 表
//   返回 in_season / off_season / neutral
type KeywordSeasonClassifier struct {
	now func() time.Time
}

func NewKeywordSeasonClassifier(now func() time.Time) *KeywordSeasonClassifier {
	if now == nil {
		now = time.Now
	}
	return &KeywordSeasonClassifier{now: now}
}

func (k *KeywordSeasonClassifier) Classify(_ context.Context, itemName string) Season {
	cur := currentSeason(k.now())
	for word, seasons := range seasonWords {
		if !strings.Contains(itemName, word) {
			continue
		}
		for _, s := range seasons {
			if s == cur {
				return SeasonInSeason
			}
		}
		return SeasonOffSeason
	}
	return SeasonNeutral
}

// ============================================================
// 2. LLM 分类器 (慢但准)
// ============================================================

// BigModelSeasonClassifier 用大模型判定应季
//   默认 model=glm-4-flash (便宜 + 智谱已有, 不引入新依赖)
//   prompt 简短, 单次 ~150 tokens
type BigModelSeasonClassifier struct {
	llm   *bigmodel.LlmClient
	model string
	now   func() time.Time
}

func NewBigModelSeasonClassifier(llm *bigmodel.LlmClient, model string, now func() time.Time) *BigModelSeasonClassifier {
	if model == "" {
		model = "glm-4-flash"
	}
	if now == nil {
		now = time.Now
	}
	return &BigModelSeasonClassifier{llm: llm, model: model, now: now}
}

const seasonSystemPrompt = `你是商超采购助理。给定商品名称 + 当前月份/季节,判断该商品是"应季"、"反季"、还是"中性/不可判定"。

严格按 JSON 输出,不要其他文字:
{"season": "in_season" | "off_season" | "neutral", "reason": "一句话理由(≤30字)"}

判定原则:
- 时令强相关(冰品/电热/羽绒服/凉席/西瓜): 看当前季节是否匹配
- 通用商品(瓶装水/纸巾/文具): 中性
- 不确定: neutral
`

func (b *BigModelSeasonClassifier) Classify(ctx context.Context, itemName string) Season {
	if b.llm == nil {
		return SeasonNeutral
	}
	if strings.TrimSpace(itemName) == "" {
		return SeasonNeutral
	}
	cur := currentSeason(b.now())
	userPrompt := fmt.Sprintf("商品: %s\n当前季节: %s\n\n请按 JSON 格式回答:", itemName, cur)

	content, err := b.llm.ChatCompletion(seasonSystemPrompt, userPrompt, b.model)
	if err != nil {
		return SeasonUnknown // 内部 unknown, 包装层会降级为 neutral
	}

	var resp struct {
		Season string `json:"season"`
		Reason string `json:"reason"`
	}
	// 提取 JSON (LLM 可能夹杂文字)
	jsonStart := strings.Index(content, "{")
	jsonEnd := strings.LastIndex(content, "}")
	if jsonStart < 0 || jsonEnd < 0 || jsonEnd <= jsonStart {
		return SeasonUnknown
	}
	if err := json.Unmarshal([]byte(content[jsonStart:jsonEnd+1]), &resp); err != nil {
		return SeasonUnknown
	}
	switch Season(strings.TrimSpace(resp.Season)) {
	case SeasonInSeason, SeasonOffSeason, SeasonNeutral:
		return Season(resp.Season)
	default:
		return SeasonUnknown
	}
}

// ============================================================
// 3. 缓存包装
// ============================================================

// cachedEntry 缓存条目
type cachedEntry struct {
	season   Season
	cachedAt time.Time
}

// CachingSeasonClassifier 装饰器: 先查缓存, 未命中才调底层 classifier
//   默认 TTL=6h (跟方案一致), LRU 1000 条
//   失败降级: 底层返回 SeasonUnknown / error → 不缓存, 返回 neutral
type CachingSeasonClassifier struct {
	inner SeasonClassifier
	ttl   time.Duration
	max   int
	now   func() time.Time

	mu      sync.RWMutex
	entries map[string]cachedEntry
	order   []string // FIFO for LRU eviction
}

func NewCachingSeasonClassifier(inner SeasonClassifier, ttl time.Duration, max int, now func() time.Time) *CachingSeasonClassifier {
	if ttl <= 0 {
		ttl = 6 * time.Hour
	}
	if max <= 0 {
		max = 1000
	}
	if now == nil {
		now = time.Now
	}
	return &CachingSeasonClassifier{
		inner:   inner,
		ttl:     ttl,
		max:     max,
		now:     now,
		entries: make(map[string]cachedEntry, max),
	}
}

func (c *CachingSeasonClassifier) Classify(ctx context.Context, itemName string) Season {
	key := strings.TrimSpace(itemName)
	if key == "" {
		return SeasonNeutral
	}

	// 1) 查缓存
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if ok {
		if c.now().Sub(entry.cachedAt) < c.ttl {
			return entry.season
		}
		// 过期, 删除
		c.mu.Lock()
		delete(c.entries, key)
		c.removeFromOrder(key)
		c.mu.Unlock()
	}

	// 2) 未命中, 调底层
	season := c.inner.Classify(ctx, key)
	if season == SeasonUnknown {
		// 失败降级: 不缓存, 返回 neutral
		return SeasonNeutral
	}

	// 3) 写缓存
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; !exists {
		c.entries[key] = cachedEntry{season: season, cachedAt: c.now()}
		c.order = append(c.order, key)
		// LRU 淘汰
		for len(c.order) > c.max {
			old := c.order[0]
			c.order = c.order[1:]
			delete(c.entries, old)
		}
	} else {
		// 已存在 (race), 刷新
		c.entries[key] = cachedEntry{season: season, cachedAt: c.now()}
	}
	return season
}

// removeFromOrder 从 order 切片移除 key (O(n), 接受)
func (c *CachingSeasonClassifier) removeFromOrder(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

// CachingStats 缓存统计 (用于监控 / 调试)
type CachingStats struct {
	Size    int
	Max     int
	TTL     time.Duration
	Hits    int64
	Misses  int64
	Evicted int64
}

func (c *CachingSeasonClassifier) Stats() CachingStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return CachingStats{Size: len(c.entries), Max: c.max, TTL: c.ttl}
}

// ============================================================
// 4. 链式组合: 关键词 → LLM(带缓存)
// ============================================================

// ChainedSeasonClassifier 先用关键词, 未命中调 LLM(带缓存)
//   关键词快速 + LLM 兜底语义
//   LLM 失败降级为 neutral
type ChainedSeasonClassifier struct {
	fast SeasonClassifier
	slow SeasonClassifier
}

func NewChainedSeasonClassifier(fast, slow SeasonClassifier) *ChainedSeasonClassifier {
	return &ChainedSeasonClassifier{fast: fast, slow: slow}
}

func (c *ChainedSeasonClassifier) Classify(ctx context.Context, itemName string) Season {
	// 1) 快速路径
	season := c.fast.Classify(ctx, itemName)
	if season != SeasonNeutral {
		return season
	}
	// 2) 慢路径 (LLM + 缓存)
	//    slow 内部已把 unknown 降级为 neutral (除 Chained 单独使用场景外)
	//    但作为最后兜底, 这里再降一次确保不漏
	season = c.slow.Classify(ctx, itemName)
	if season == SeasonUnknown {
		return SeasonNeutral
	}
	return season
}

// ErrSeasonClassifierUnavailable LLM 不可用
var ErrSeasonClassifierUnavailable = errors.New("season classifier unavailable")
