// In-memory fixed-window rate limiting matching @fastify/rate-limit usage:
// a global per-IP limit plus per-route overrides keyed by ip:rulePath.
//
// The backend is a single Node instance (see AGENTS.md), so in-memory state is the
// correct parity choice rather than a shared store.
package middleware

import (
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/config"
)

type rateBucket struct {
	count   int
	resetAt time.Time
}

type fixedWindowLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rateBucket
}

func newFixedWindowLimiter() *fixedWindowLimiter {
	limiter := &fixedWindowLimiter{buckets: map[string]*rateBucket{}}
	go limiter.reap()
	return limiter
}

func (l *fixedWindowLimiter) reap() {
	ticker := time.NewTicker(time.Minute)
	for range ticker.C {
		now := time.Now()
		l.mu.Lock()
		for key, bucket := range l.buckets {
			if now.After(bucket.resetAt) {
				delete(l.buckets, key)
			}
		}
		l.mu.Unlock()
	}
}

// allow records a hit and reports whether it is within the limit.
func (l *fixedWindowLimiter) allow(key string, max int, window time.Duration) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	bucket, ok := l.buckets[key]
	if !ok || now.After(bucket.resetAt) {
		l.buckets[key] = &rateBucket{count: 1, resetAt: now.Add(window)}
		return true
	}
	bucket.count++
	return bucket.count <= max
}

// RateLimit applies the global limit and per-route overrides.
func RateLimit() gin.HandlerFunc {
	limiter := newFixedWindowLimiter()

	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if !strings.HasPrefix(path, "/api/") || config.IsWhitelisted(path) {
			c.Next()
			return
		}

		ip := ClientIP(c)
		max := config.GlobalRateLimit.Max
		window := config.ParseTimeWindow(config.GlobalRateLimit.TimeWindow)
		key := ip

		if rule := config.FindRateLimitRule(path, c.Request.Method); rule != nil {
			max = rule.Max
			window = config.ParseTimeWindow(rule.TimeWindow)
			key = ip + ":" + rule.Path
		}

		if !limiter.allow(key, max, window) {
			c.AbortWithStatusJSON(429, gin.H{"error": "Too many requests, please try again later"})
			return
		}
		c.Next()
	}
}
