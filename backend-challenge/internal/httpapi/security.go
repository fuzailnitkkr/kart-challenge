package httpapi

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strconv"
	"strings"
)

const defaultRateLimitUserHeader = "X-User-ID"

// AuthAndRateLimitConfig configures API auth and anti-abuse controls.
type AuthAndRateLimitConfig struct {
	APIKey        string
	UserHeader    string
	RateLimiter   *UserRateLimiter
	LimitPerSec   float64
	BurstCapacity int
}

func withAuthAndRateLimit(next http.Handler, cfg AuthAndRateLimitConfig) http.Handler {
	expectedAPIKey := strings.TrimSpace(cfg.APIKey)
	userHeader := strings.TrimSpace(cfg.UserHeader)
	if userHeader == "" {
		userHeader = defaultRateLimitUserHeader
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if expectedAPIKey != "" {
			provided := strings.TrimSpace(r.Header.Get("api_key"))
			if provided == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if !matchAPIKey(provided, expectedAPIKey) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
		}

		if cfg.RateLimiter != nil {
			if cfg.LimitPerSec > 0 {
				w.Header().Set("X-RateLimit-Limit", strconv.FormatFloat(cfg.LimitPerSec, 'f', -1, 64))
			}
			if cfg.BurstCapacity > 0 {
				w.Header().Set("X-RateLimit-Burst", strconv.Itoa(cfg.BurstCapacity))
			}

			userKey := requestUserKey(r, userHeader)
			if !cfg.RateLimiter.Allow(userKey) {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

func requestUserKey(r *http.Request, userHeader string) string {
	if userHeader != "" {
		if userID := strings.TrimSpace(r.Header.Get(userHeader)); userID != "" {
			return "user:" + userID
		}
	}

	remoteAddr := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil && host != "" {
		return "ip:" + host
	}
	if remoteAddr != "" {
		return "ip:" + remoteAddr
	}

	return "ip:unknown"
}

func matchAPIKey(provided, expected string) bool {
	if len(provided) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}
