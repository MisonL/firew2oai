package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLimiter_AllowsUnderLimit(t *testing.T) {
	rl := New(5, time.Minute)
	defer rl.Stop()
	count := 0
	handler := rl.Middleware(func(w http.ResponseWriter, r *http.Request) {
		count++
		w.WriteHeader(http.StatusOK)
	})

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "1.2.3.4:1234"
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i+1, rec.Code)
		}
	}
	if count != 5 {
		t.Errorf("count = %d, want 5", count)
	}
}

func TestLimiter_BlocksOverLimit(t *testing.T) {
	rl := New(3, time.Minute)
	defer rl.Stop()
	handler := rl.Middleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Send 4 requests
	for i := 0; i < 4; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "1.2.3.4:1234"
		rec := httptest.NewRecorder()
		handler(rec, req)
		if i < 3 && rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i+1, rec.Code)
		}
		if i == 3 && rec.Code != http.StatusTooManyRequests {
			t.Fatalf("request 4: expected 429, got %d", rec.Code)
		}
	}
}

func TestLimiter_DifferentIPs(t *testing.T) {
	rl := New(1, time.Minute)
	defer rl.Stop()
	handler := rl.Middleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// IP A
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "1.1.1.1:1234"
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("IP A first: got %d, want 200", rec.Code)
	}

	// IP A again (blocked)
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "1.1.1.1:1234"
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("IP A second: got %d, want 429", rec.Code)
	}

	// IP B (allowed, separate bucket)
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "2.2.2.2:5678"
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("IP B first: got %d, want 200", rec.Code)
	}
}

func TestLimiter_RateLimitHeaders(t *testing.T) {
	rl := New(10, time.Minute)
	defer rl.Stop()
	handler := rl.Middleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "1.2.3.4:1234"
	rec := httptest.NewRecorder()
	handler(rec, req)

	if got := rec.Header().Get("X-RateLimit-Limit"); got != "10" {
		t.Errorf("X-RateLimit-Limit = %q, want 10", got)
	}
	if got := rec.Header().Get("X-RateLimit-Remaining"); got != "9" {
		t.Errorf("X-RateLimit-Remaining = %q, want 9", got)
	}
	// X-RateLimit-Reset should be a valid timestamp string
	if got := rec.Header().Get("X-RateLimit-Reset"); got == "" {
		t.Error("X-RateLimit-Reset is empty")
	}
}

func TestExtractIP_RemoteAddr(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.1.1:45678"
	if got := extractIP(req); got != "192.168.1.1" {
		t.Errorf("extractIP() = %q, want 192.168.1.1", got)
	}
}

func TestExtractIP_RemoteAddrNoPort(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.1.1"
	if got := extractIP(req); got != "192.168.1.1" {
		t.Errorf("extractIP() = %q, want 192.168.1.1", got)
	}
}

func TestExtractIP_RealIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Real-IP", "203.0.113.5")
	if got := extractIP(req); got != "203.0.113.5" {
		t.Errorf("extractIP() with X-Real-IP = %q, want 203.0.113.5", got)
	}
}

func TestExtractIP_XForwardedFor_Ignored(t *testing.T) {
	// X-Forwarded-For is intentionally NOT trusted (SSRF prevention).
	// Only X-Real-IP (set by trusted reverse proxy) is used.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.10, 10.0.0.2")
	if got := extractIP(req); got != "10.0.0.1" {
		t.Errorf("extractIP() = %q, want 10.0.0.1 (XFF should be ignored)", got)
	}
}

func TestExtractIP_RealIP_TakesPrecedence(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Real-IP", "203.0.113.5")
	req.Header.Set("X-Forwarded-For", "198.51.100.5")
	// X-Real-IP should take precedence over RemoteAddr
	if got := extractIP(req); got != "203.0.113.5" {
		t.Errorf("extractIP() = %q, want X-Real-IP (203.0.113.5)", got)
	}
}

func TestExtractIP_InvalidRealIP_Fallback(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Real-IP", "not-an-ip-address")
	// Should fall back to RemoteAddr since X-Real-IP is not a valid IP
	if got := extractIP(req); got != "10.0.0.1" {
		t.Errorf("extractIP() = %q, want 10.0.0.1 (fallback for invalid X-Real-IP)", got)
	}
}

// fastFormat was replaced by strconv.Itoa; test via extractIP output instead.
func TestExtractIP_IPV6(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "[::1]:45678"
	if got := extractIP(req); got != "::1" {
		t.Errorf("extractIP() = %q, want ::1", got)
	}
}

func TestStop(t *testing.T) {
	rl := New(10, time.Minute)
	rl.Stop() // should not panic or hang
	rl.Stop() // double stop should also be safe
}
