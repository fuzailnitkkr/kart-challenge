package httpapi

import (
	"log"
	"net/http"
	"strings"
	"time"
)

// RequestLoggingConfig configures request logging metadata extraction.
type RequestLoggingConfig struct {
	UserHeader   string
	DeviceHeader string
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *loggingResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

func withRequestLogging(next http.Handler, cfg RequestLoggingConfig) http.Handler {
	userHeader := strings.TrimSpace(cfg.UserHeader)
	if userHeader == "" {
		userHeader = defaultRateLimitUserHeader
	}
	deviceHeader := strings.TrimSpace(cfg.DeviceHeader)
	if deviceHeader == "" {
		deviceHeader = defaultDeviceIDHeader
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &loggingResponseWriter{ResponseWriter: w}

		next.ServeHTTP(rw, r)

		status := rw.status
		if status == 0 {
			status = http.StatusOK
		}

		userID := requestUserID(r, userHeader)
		if userID == "" {
			userID = "-"
		}

		deviceID := requestDeviceID(r, deviceHeader)
		if deviceID == "" {
			deviceID = "-"
		}

		clientIP := requestClientIP(r)
		if clientIP == "" {
			clientIP = "unknown"
		}

		log.Printf(
			"request method=%s path=%s status=%d bytes=%d duration_ms=%d ip=%s device_id=%s user_id=%s",
			r.Method,
			r.URL.Path,
			status,
			rw.bytes,
			time.Since(start).Milliseconds(),
			clientIP,
			deviceID,
			userID,
		)
	})
}
