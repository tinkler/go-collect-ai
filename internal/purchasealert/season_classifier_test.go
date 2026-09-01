package purchasealert

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ============================================================
// 测试 fixture
// ============================================================

// stubClassifier 固定返回值的 mock
type stubClassifier struct {
	season Season
	calls  int64
}

func (s *stubClassifier) Classify(_ context.Context, _ string) Season {
	atomic.AddInt64(&s.calls, 1)
	return s.season
}

// erroringClassifier 永远返 unknown
type erroringClassifier struct {
	calls int64
}

func (e *erroringClassifier) Classify(_ context.Context, _ string) Season {
	atomic.AddInt64(&e.calls, 1)
	return SeasonUnknown
}

// ============================================================
// KeywordSeasonClassifier
// ============================================================

func TestKeywordSeasonClassifier_KnownInSeason(t *testing.T) {
	// 7 月 = 夏
	c := NewKeywordSeasonClassifier(func() time.Time {
		return time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	})
	got := c.Classify(context.Background(), "可口可乐冰品大全")
	if got != SeasonInSeason {
		t.Errorf("夏季冰品应 in_season, got %s", got)
	}
}

func TestKeywordSeasonClassifier_KnownOffSeason(t *testing.T) {
	// 9 月 = 秋, 冰品反季
	c := NewKeywordSeasonClassifier(func() time.Time {
		return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	})
	got := c.Classify(context.Background(), "可口可乐冰品大全")
	if got != SeasonOffSeason {
		t.Errorf("秋季冰品应 off_season, got %s", got)
	}
}

func TestKeywordSeasonClassifier_Unknown(t *testing.T) {
	c := NewKeywordSeasonClassifier(func() time.Time {
		return time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	})
	got := c.Classify(context.Background(), "可口可乐 330ml") // 无关键词
	if got != SeasonNeutral {
		t.Errorf("无关键词应 neutral, got %s", got)
	}
}

func TestKeywordSeasonClassifier_EmptyName(t *testing.T) {
	c := NewKeywordSeasonClassifier(nil)
	if got := c.Classify(context.Background(), ""); got != SeasonNeutral {
		t.Errorf("空 name 应 neutral, got %s", got)
	}
}

// ============================================================
// CachingSeasonClassifier
// ============================================================

func TestCaching_Hit(t *testing.T) {
	inner := &stubClassifier{season: SeasonInSeason}
	c := NewCachingSeasonClassifier(inner, 6*time.Hour, 100, nil)

	got1 := c.Classify(context.Background(), "可口可乐")
	got2 := c.Classify(context.Background(), "可口可乐")

	if got1 != SeasonInSeason || got2 != SeasonInSeason {
		t.Errorf("cache hit 应都 in_season, got %s / %s", got1, got2)
	}
	if inner.calls != 1 {
		t.Errorf("第二次应走缓存, 底层调用 1 次, got %d", inner.calls)
	}
}

func TestCaching_TTLExpire(t *testing.T) {
	inner := &stubClassifier{season: SeasonInSeason}
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	current := now
	c := NewCachingSeasonClassifier(inner, 1*time.Hour, 100, func() time.Time { return current })

	c.Classify(context.Background(), "X")
	current = now.Add(2 * time.Hour) // 过期
	c.Classify(context.Background(), "X")

	if inner.calls != 2 {
		t.Errorf("过期应重新调底层, got %d calls", inner.calls)
	}
}

func TestCaching_UnknownNotCached(t *testing.T) {
	inner := &erroringClassifier{}
	c := NewCachingSeasonClassifier(inner, 6*time.Hour, 100, nil)

	got1 := c.Classify(context.Background(), "X")
	got2 := c.Classify(context.Background(), "X")

	// 两次都应降级为 neutral, 但底层调 2 次 (因为 unknown 不缓存)
	if got1 != SeasonNeutral || got2 != SeasonNeutral {
		t.Errorf("unknown 应降级 neutral, got %s / %s", got1, got2)
	}
	if inner.calls != 2 {
		t.Errorf("unknown 不应缓存, 底层应调 2 次, got %d", inner.calls)
	}
}

func TestCaching_LRUEviction(t *testing.T) {
	inner := &stubClassifier{season: SeasonInSeason}
	c := NewCachingSeasonClassifier(inner, 6*time.Hour, 3, nil)

	c.Classify(context.Background(), "A")
	c.Classify(context.Background(), "B")
	c.Classify(context.Background(), "C")
	c.Classify(context.Background(), "D") // 触发淘汰 A

	stats := c.Stats()
	if stats.Size != 3 {
		t.Errorf("size = %d, want 3 (max)", stats.Size)
	}
}

func TestCaching_Stats(t *testing.T) {
	c := NewCachingSeasonClassifier(&stubClassifier{season: SeasonInSeason}, time.Hour, 10, nil)
	c.Classify(context.Background(), "X")
	c.Classify(context.Background(), "X")

	stats := c.Stats()
	if stats.Max != 10 {
		t.Errorf("stats.Max = %d, want 10", stats.Max)
	}
}

// ============================================================
// ChainedSeasonClassifier
// ============================================================

func TestChained_FastHits(t *testing.T) {
	fast := &stubClassifier{season: SeasonInSeason}
	slow := &stubClassifier{season: SeasonOffSeason} // 不应被调
	c := NewChainedSeasonClassifier(fast, slow)
	got := c.Classify(context.Background(), "X")
	if got != SeasonInSeason {
		t.Errorf("fast 命中应 in_season, got %s", got)
	}
	if slow.calls != 0 {
		t.Errorf("fast 命中不应调 slow, got %d", slow.calls)
	}
}

func TestChained_FastMiss_GoesSlow(t *testing.T) {
	fast := &stubClassifier{season: SeasonNeutral}
	slow := &stubClassifier{season: SeasonOffSeason}
	c := NewChainedSeasonClassifier(fast, slow)
	got := c.Classify(context.Background(), "X")
	if got != SeasonOffSeason {
		t.Errorf("fast neutral 应 fallback slow, got %s", got)
	}
	if slow.calls != 1 {
		t.Errorf("slow 应调 1 次, got %d", slow.calls)
	}
}

func TestChained_SlowError_DegradeToNeutral(t *testing.T) {
	fast := &stubClassifier{season: SeasonNeutral}
	slow := &erroringClassifier{}
	c := NewChainedSeasonClassifier(fast, slow)
	got := c.Classify(context.Background(), "X")
	if got != SeasonNeutral {
		t.Errorf("slow 失败应降级 neutral, got %s", got)
	}
}

// ============================================================
// 错误类型导出测试
// ============================================================

func TestErrSeasonClassifierUnavailable(t *testing.T) {
	if ErrSeasonClassifierUnavailable == nil {
		t.Fatal("error 不应 nil")
	}
	if !strings.Contains(ErrSeasonClassifierUnavailable.Error(), "unavailable") {
		t.Errorf("err msg 缺 'unavailable': %v", ErrSeasonClassifierUnavailable)
	}
	_ = errors.Unwrap
}
