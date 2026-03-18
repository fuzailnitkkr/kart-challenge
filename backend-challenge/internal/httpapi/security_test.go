package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestClientIPPrefersForwardedFor(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/product", nil)
	req.Header.Set("X-Forwarded-For", "198.51.100.10, 10.0.0.1")
	req.Header.Set("X-Real-IP", "203.0.113.20")
	req.RemoteAddr = "192.0.2.33:4567"

	got := requestClientIP(req)
	if got != "198.51.100.10" {
		t.Fatalf("requestClientIP() = %q, want %q", got, "198.51.100.10")
	}
}

func TestRequestClientIPFallsBackToRemoteAddr(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/product", nil)
	req.RemoteAddr = "192.0.2.33:4567"

	got := requestClientIP(req)
	if got != "192.0.2.33" {
		t.Fatalf("requestClientIP() = %q, want %q", got, "192.0.2.33")
	}
}

func TestBuildRateLimitKeyIncludesDeviceAndIP(t *testing.T) {
	t.Parallel()

	keyA := buildRateLimitKey("user-1", "device-a", "203.0.113.1")
	keyB := buildRateLimitKey("user-1", "device-b", "203.0.113.1")

	if keyA == keyB {
		t.Fatalf("expected distinct keys per device; got %q and %q", keyA, keyB)
	}
}
