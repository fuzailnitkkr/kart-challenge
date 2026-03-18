package httpapi

import (
	"math"
	"sync"
	"time"
)

const (
	defaultRateLimitEntryTTL        = 15 * time.Minute
	defaultRateLimitCleanupInterval = time.Minute
)

// RateLimitConfig configures per-user API throttling behavior.
type RateLimitConfig struct {
	RequestsPerSecond float64
	Burst             int
	UserHeader        string
	EntryTTL          time.Duration
	CleanupInterval   time.Duration
}

// RateLimitOptions controls cleanup behavior of limiter state.
type RateLimitOptions struct {
	EntryTTL        time.Duration
	CleanupInterval time.Duration
}

// UserRateLimiter enforces per-user token-bucket limits.
type UserRateLimiter struct {
	limitPerSecond  float64
	burst           float64
	entryTTL        time.Duration
	cleanupInterval time.Duration

	mu          sync.Mutex
	buckets     map[string]*rateBucket
	lastCleanup time.Time
}

type rateBucket struct {
	tokens     float64
	lastRefill time.Time
	lastSeen   time.Time
}

// NewUserRateLimiter creates a limiter. Non-positive limits disable it.
func NewUserRateLimiter(requestsPerSecond float64, burst int, opts RateLimitOptions) *UserRateLimiter {
	if requestsPerSecond <= 0 || burst <= 0 || math.IsNaN(requestsPerSecond) || math.IsInf(requestsPerSecond, 0) {
		return nil
	}

	if opts.EntryTTL <= 0 {
		opts.EntryTTL = defaultRateLimitEntryTTL
	}
	if opts.CleanupInterval <= 0 {
		opts.CleanupInterval = defaultRateLimitCleanupInterval
	}

	return &UserRateLimiter{
		limitPerSecond:  requestsPerSecond,
		burst:           float64(burst),
		entryTTL:        opts.EntryTTL,
		cleanupInterval: opts.CleanupInterval,
		buckets:         make(map[string]*rateBucket, 256),
	}
}

// Allow reports whether a request from key should be served.
func (l *UserRateLimiter) Allow(key string) bool {
	if l == nil {
		return true
	}

	now := time.Now()
	if key == "" {
		key = "unknown"
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.cleanupLocked(now)

	bucket, ok := l.buckets[key]
	if !ok {
		bucket = &rateBucket{
			tokens:     l.burst,
			lastRefill: now,
			lastSeen:   now,
		}
		l.buckets[key] = bucket
	}

	elapsed := now.Sub(bucket.lastRefill).Seconds()
	if elapsed > 0 {
		bucket.tokens += elapsed * l.limitPerSecond
		if bucket.tokens > l.burst {
			bucket.tokens = l.burst
		}
		bucket.lastRefill = now
	}
	bucket.lastSeen = now

	if bucket.tokens < 1 {
		return false
	}

	bucket.tokens -= 1
	return true
}

// LimitPerSecond returns configured requests-per-second.
func (l *UserRateLimiter) LimitPerSecond() float64 {
	if l == nil {
		return 0
	}
	return l.limitPerSecond
}

// BurstCapacity returns configured burst size.
func (l *UserRateLimiter) BurstCapacity() int {
	if l == nil {
		return 0
	}
	return int(l.burst)
}

func (l *UserRateLimiter) cleanupLocked(now time.Time) {
	if !l.lastCleanup.IsZero() && now.Sub(l.lastCleanup) < l.cleanupInterval {
		return
	}

	for key, bucket := range l.buckets {
		if now.Sub(bucket.lastSeen) > l.entryTTL {
			delete(l.buckets, key)
		}
	}

	l.lastCleanup = now
}
