package tokenauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// helper to create a Manager for tests
func newTestManager(t *testing.T, configStr string, globalRate int) *Manager {
	t.Helper()
	m, err := New(configStr, globalRate)
	if err != nil {
		t.Fatalf("New(%q, %d) error: %v", configStr, globalRate, err)
	}
	return m
}

func TestNew_SimpleKeys(t *testing.T) {
	m := newTestManager(t, "sk-key1,sk-key2,sk-key3", 0)
	defer m.Stop()

	if m.TokenCount() != 3 {
		t.Errorf("TokenCount = %d, want 3", m.TokenCount())
	}

	if ok, _ := m.Authenticate("sk-key1"); !ok {
		t.Error("Authenticate(sk-key1) = false, want true")
	}
	if ok, _ := m.Authenticate("sk-unknown"); ok {
		t.Error("Authenticate(sk-unknown) = true, want false")
	}
}

func TestNew_EmptyConfig(t *testing.T) {
	m, err := New("", 0)
	if err != nil {
		t.Fatalf("New empty error: %v", err)
	}
	if m.TokenCount() != 0 {
		t.Errorf("TokenCount = %d, want 0", m.TokenCount())
	}
}

func TestNew_InvalidConfig(t *testing.T) {
	_, err := New(`[{"key": "", "quota": 0}]`, 0)
	if err == nil {
		t.Error("expected error for empty key")
	}
}

func TestNew_TrimSpaces(t *testing.T) {
	m := newTestManager(t, "  sk-a  ,  sk-b  ", 0)
	defer m.Stop()
	if m.TokenCount() != 2 {
		t.Errorf("TokenCount = %d, want 2", m.TokenCount())
	}
}

func TestNew_JSONFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "tokens.json")
	data := []TokenConfig{
		{Key: "sk-admin", Quota: 10000, RateLimit: 60},
		{Key: "sk-user1", Quota: 100, RateLimit: 10},
		{Key: "sk-unlimited", Quota: 0, RateLimit: 0},
	}
	b, _ := json.Marshal(data)
	os.WriteFile(f, b, 0644)

	m, err := New(f, 30)
	if err != nil {
		t.Fatalf("New file error: %v", err)
	}
	defer m.Stop()

	if m.TokenCount() != 3 {
		t.Errorf("TokenCount = %d, want 3", m.TokenCount())
	}

	// sk-admin: per-key rate limit 60, quota 10000
	if ok, rem, _ := m.CheckQuota("sk-admin"); !ok || rem != 10000 {
		t.Errorf("sk-admin quota: ok=%v, rem=%d", ok, rem)
	}

	// sk-user1: per-key rate limit 10, quota 100
	if ok, rem, _ := m.CheckQuota("sk-user1"); !ok || rem != 100 {
		t.Errorf("sk-user1 quota: ok=%v, rem=%d", ok, rem)
	}

	// sk-unlimited: quota 0 = unlimited, rate_limit 0 = use global (30)
	if ok, rem, _ := m.CheckQuota("sk-unlimited"); !ok || rem != -1 {
		t.Errorf("sk-unlimited quota: ok=%v, rem=%d", ok, rem)
	}
}

func TestNew_InlineJSON(t *testing.T) {
	m := newTestManager(t, `[{"key":"sk-a","quota":50,"rate_limit":10}]`, 60)
	defer m.Stop()

	if m.TokenCount() != 1 {
		t.Errorf("TokenCount = %d, want 1", m.TokenCount())
	}
	if ok, rem, _ := m.CheckQuota("sk-a"); !ok || rem != 50 {
		t.Errorf("sk-a quota: ok=%v, rem=%d", ok, rem)
	}
}

func TestNew_InlineJSON_SingleObject(t *testing.T) {
	m := newTestManager(t, `{"key":"sk-single","quota":200}`, 0)
	defer m.Stop()

	if m.TokenCount() != 1 {
		t.Errorf("TokenCount = %d, want 1", m.TokenCount())
	}
}

func TestQuota_Unlimited(t *testing.T) {
	m := newTestManager(t, "sk-key", 0)
	defer m.Stop()

	ok, rem, limit := m.CheckQuota("sk-key")
	if !ok || rem != -1 || limit != 0 {
		t.Errorf("unlimited quota: ok=%v, rem=%d, limit=%d", ok, rem, limit)
	}
}

func TestQuota_Limited(t *testing.T) {
	m := newTestManager(t, `[{"key":"sk-limited","quota":3}]`, 0)
	defer m.Stop()

	// Use all quota
	for i := 0; i < 3; i++ {
		m.RecordUsage("sk-limited")
	}

	ok, rem, limit := m.CheckQuota("sk-limited")
	if ok || rem != 0 || limit != 3 {
		t.Errorf("exhausted quota: ok=%v, rem=%d, limit=%d", ok, rem, limit)
	}
}

func TestQuota_NonexistentKey(t *testing.T) {
	m := newTestManager(t, "sk-key", 0)
	defer m.Stop()

	ok, _, _ := m.CheckQuota("sk-unknown")
	if ok {
		t.Error("CheckQuota(unknown) = true, want false")
	}
}

func TestRateLimit_PerKey(t *testing.T) {
	m := newTestManager(t, `[{"key":"sk-fast","rate_limit":2}]`, 0)
	defer m.Stop()

	// First request: allowed
	allowed, remaining, _, _ := m.CheckRateLimit("sk-fast")
	if !allowed {
		t.Error("request 1: not allowed, want allowed")
	}
	if remaining != 1 {
		t.Errorf("request 1: remaining=%d, want 1", remaining)
	}

	// Second request: allowed
	allowed, remaining, _, _ = m.CheckRateLimit("sk-fast")
	if !allowed || remaining != 0 {
		t.Errorf("request 2: allowed=%v, remaining=%d", allowed, remaining)
	}

	// Third request: denied
	allowed, _, _, _ = m.CheckRateLimit("sk-fast")
	if allowed {
		t.Error("request 3: allowed, want denied")
	}
}

func TestRateLimit_GlobalFallback(t *testing.T) {
	// per-key rate_limit=0, global=5
	m := newTestManager(t, `[{"key":"sk-global","rate_limit":0}]`, 5)
	defer m.Stop()

	for i := 0; i < 5; i++ {
		allowed, _, _, _ := m.CheckRateLimit("sk-global")
		if !allowed {
			t.Errorf("request %d: not allowed", i+1)
		}
	}

	allowed, _, _, _ := m.CheckRateLimit("sk-global")
	if allowed {
		t.Error("request 6: allowed, want denied")
	}
}

func TestRateLimit_Disabled(t *testing.T) {
	m := newTestManager(t, "sk-key", 0)
	defer m.Stop()

	for i := 0; i < 100; i++ {
		allowed, _, limit, _ := m.CheckRateLimit("sk-key")
		if !allowed {
			t.Errorf("request %d: not allowed", i+1)
		}
		if limit != 0 {
			t.Errorf("limit = %d, want 0 (disabled)", limit)
		}
	}
}

func TestRecordUsage(t *testing.T) {
	m := newTestManager(t, "sk-key", 0)
	defer m.Stop()

	m.RecordUsage("sk-key")
	m.RecordUsage("sk-key")

	ok, rem, _ := m.CheckQuota("sk-key")
	if !ok || rem != -1 {
		t.Errorf("after 2 uses: ok=%v, rem=%d (unlimited)", ok, rem)
	}
}

func TestMiddleware_MissingAuth(t *testing.T) {
	m := newTestManager(t, "sk-key", 0)
	defer m.Stop()

	handler := m.Middleware()(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestMiddleware_InvalidKey(t *testing.T) {
	m := newTestManager(t, "sk-key", 0)
	defer m.Stop()

	handler := m.Middleware()(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-wrong")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestMiddleware_ValidKey(t *testing.T) {
	m := newTestManager(t, "sk-key", 0)
	defer m.Stop()

	handler := m.Middleware()(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-key")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestMiddleware_QuotaExceeded(t *testing.T) {
	m := newTestManager(t, `[{"key":"sk-tiny","quota":1}]`, 0)
	defer m.Stop()

	handler := m.Middleware()(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// First request: OK
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-tiny")
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("request 1: status = %d, want 200", rec.Code)
	}

	// Second request: quota exceeded
	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req2.Header.Set("Authorization", "Bearer sk-tiny")
	rec2 := httptest.NewRecorder()
	handler(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Errorf("request 2: status = %d, want 403", rec2.Code)
	}
}

func TestMiddleware_RateLimitExceeded(t *testing.T) {
	m := newTestManager(t, `[{"key":"sk-fast","rate_limit":1}]`, 0)
	defer m.Stop()

	handler := m.Middleware()(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// First request: OK
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-fast")
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("request 1: status = %d, want 200", rec.Code)
	}

	// Second request: rate limited
	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req2.Header.Set("Authorization", "Bearer sk-fast")
	rec2 := httptest.NewRecorder()
	handler(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("request 2: status = %d, want 429", rec2.Code)
	}
}

func TestMiddleware_RateLimitHeaders(t *testing.T) {
	m := newTestManager(t, `[{"key":"sk-hdr","rate_limit":5}]`, 0)
	defer m.Stop()

	handler := m.Middleware()(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-hdr")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if h := rec.Header().Get("X-RateLimit-Limit"); h != "5" {
		t.Errorf("X-RateLimit-Limit = %q, want 5", h)
	}
	if h := rec.Header().Get("X-RateLimit-Remaining"); h != "4" {
		t.Errorf("X-RateLimit-Remaining = %q, want 4", h)
	}
}

func TestMiddleware_QuotaHeaders(t *testing.T) {
	m := newTestManager(t, `[{"key":"sk-qh","quota":100}]`, 0)
	defer m.Stop()

	handler := m.Middleware()(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-qh")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if h := rec.Header().Get("X-Quota-Limit"); h != "100" {
		t.Errorf("X-Quota-Limit = %q, want 100", h)
	}
	if h := rec.Header().Get("X-Quota-Remaining"); h != "100" {
		t.Errorf("X-Quota-Remaining = %q, want 100 (quota checked before record)", h)
	}
}
