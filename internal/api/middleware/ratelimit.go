package middleware

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/sync/semaphore"
)

// SemaphoreLimiter 并发限流 (semaphore-based, 适合阻塞型任务)
type SemaphoreLimiter struct {
	sem        *semaphore.Weighted
	max        int
	active     atomic.Int64
	totalWait  atomic.Int64
	totalBlock atomic.Int64
}

// NewSemaphoreLimiter 创建限流器, max=最大并发
func NewSemaphoreLimiter(max int) *SemaphoreLimiter {
	if max <= 0 {
		max = 4
	}
	return &SemaphoreLimiter{
		sem: semaphore.NewWeighted(int64(max)),
		max: max,
	}
}

// Acquire 阻塞获取, 等待超时 (默认 30s)
func (s *SemaphoreLimiter) Acquire(ctx context.Context) error {
	s.totalWait.Add(1)
	err := s.sem.Acquire(ctx, 1)
	if err == nil {
		s.active.Add(1)
	}
	return err
}

// Release 释放
func (s *SemaphoreLimiter) Release() {
	s.active.Add(-1)
	s.sem.Release(1)
}

// Stats 当前状态
func (s *SemaphoreLimiter) Stats() (active int, max int, totalWait int64, totalBlock int64) {
	return int(s.active.Load()), s.max, s.totalWait.Load(), s.totalBlock.Load()
}

// Middleware 返回 gin 中间件: 阻塞获取 semaphore, 完成后释放
// 等待超时返回 503
func (s *SemaphoreLimiter) Middleware(timeout time.Duration) gin.HandlerFunc {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		if err := s.Acquire(ctx); err != nil {
			s.totalBlock.Add(1)
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"error":      "server busy, please retry",
				"max":        s.max,
				"active":     int(s.active.Load()),
				"waited_for": timeout.String(),
			})
			return
		}
		defer s.Release()
		c.Next()
	}
}
