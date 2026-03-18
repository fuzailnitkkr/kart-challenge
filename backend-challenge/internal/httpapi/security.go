package httpapi

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strconv"
	"strings"
)

const (
	defaultRateLimitUserHeader = "X-User-ID"
	defaultDeviceIDHeader      = "X-Device-ID"
)

// AuthAndRateLimitConfig configures API auth and anti-abuse controls.
type AuthAndRateLimitConfig struct {
	APIKey        string
	UserHeader    string
	DeviceHeader  string
	RequireDevice bool
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
	deviceHeader := strings.TrimSpace(cfg.DeviceHeader)
	if deviceHeader == "" {
		deviceHeader = defaultDeviceIDHeader
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

		userID := requestUserID(r, userHeader)
		deviceID := requestDeviceID(r, deviceHeader)
		if cfg.RequireDevice && deviceID == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		clientIP := requestClientIP(r)
		if clientIP == "" {
			clientIP = "unknown"
		}

		if cfg.RateLimiter != nil {
			if cfg.LimitPerSec > 0 {
				w.Header().Set("X-RateLimit-Limit", strconv.FormatFloat(cfg.LimitPerSec, 'f', -1, 64))
			}
			if cfg.BurstCapacity > 0 {
				w.Header().Set("X-RateLimit-Burst", strconv.Itoa(cfg.BurstCapacity))
			}

			userKey := buildRateLimitKey(userID, deviceID, clientIP)
			if !cfg.RateLimiter.Allow(userKey) {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

func requestUserID(r *http.Request, userHeader string) string {
	if userHeader != "" {
		if userID := strings.TrimSpace(r.Header.Get(userHeader)); userID != "" {
			return userID
		}
	}
	return ""
}

func requestDeviceID(r *http.Request, deviceHeader string) string {
	if deviceHeader == "" {
		return ""
	}
	return strings.TrimSpace(r.Header.Get(deviceHeader))
}

func requestClientIP(r *http.Request) string {
	xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if xff != "" {
		parts := strings.Split(xff, ",")
		for _, part := range parts {
			ip := strings.TrimSpace(part)
			if ip == "" {
				continue
			}
			parsed := net.ParseIP(ip)
			if parsed != nil {
				return parsed.String()
			}
			return ip
		}
	}

	realIP := strings.TrimSpace(r.Header.Get("X-Real-IP"))
	if realIP != "" {
		if parsed := net.ParseIP(realIP); parsed != nil {
			return parsed.String()
		}
		return realIP
	}

	remoteAddr := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil && host != "" {
		if parsed := net.ParseIP(host); parsed != nil {
			return parsed.String()
		}
		return host
	}

	if parsed := net.ParseIP(remoteAddr); parsed != nil {
		return parsed.String()
	}
	return remoteAddr
}

func buildRateLimitKey(userID, deviceID, ip string) string {
	parts := make([]string, 0, 3)
	if userID != "" {
		parts = append(parts, "user:"+userID)
	}
	if deviceID != "" {
		parts = append(parts, "device:"+deviceID)
	}
	if ip != "" {
		parts = append(parts, "ip:"+ip)
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, "|")
}

func matchAPIKey(provided, expected string) bool {
	if len(provided) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}
