package tokenauth

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TokenConfig defines the limits for a single API key.
type TokenConfig struct {
	Key       string `json:"key"`
	Quota     int    `json:"quota"`      // max total requests (0 = unlimited)
	RateLimit int    `json:"rate_limit"` // max requests per minute (0 = use global default)
}

// Manager handles multi-key authentication, quota tracking, and per-key rate limiting.
type Manager struct {
	mu     sync.RWMutex
	tokens map[string]*tokenState
	window time.Duration
	stopCh chan struct{}
	once   sync.Once // ensures Stop() is idempotent
}

type tokenState struct {
	cfg   TokenConfig
	used  int64 // total requests used
	limit *rateLimiter
}

// rateLimiter is a simple token bucket for per-key rate limiting.
// Cleanup is managed externally (by Manager) to avoid goroutine-per-limiter overhead.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    int
	window  time.Duration
}

type bucket struct {
	tokens   int
	lastTime time.Time
}

// newRateLimiter creates a rate limiter with the given rate (requests per window).
// rate 0 means disabled.
// Note: does NOT start a cleanup goroutine; use Manager.cleanupLoop() instead.
func newRateLimiter(rate int, window time.Duration) *rateLimiter {
	if rate <= 0 {
		return nil
	}
	return &rateLimiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		window:  window,
	}
}

func (rl *rateLimiter) Stop() {
	if rl == nil {
		return
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.buckets = nil // release memory
}

func (rl *rateLimiter) allow(key string) (allowed bool, remaining int, resetTime int64) {
	if rl == nil {
		return true, 0, 0
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{tokens: rl.rate, lastTime: time.Now()}
		rl.buckets[key] = b
	}

	elapsed := time.Since(b.lastTime)
	refill := int(elapsed / rl.window)
	if refill > 0 {
		b.tokens += refill * rl.rate
		if b.tokens > rl.rate {
			b.tokens = rl.rate
		}
		b.lastTime = b.lastTime.Add(time.Duration(refill) * rl.window)
	}

	if b.tokens > 0 {
		b.tokens--
		return true, b.tokens, b.lastTime.Add(rl.window).Unix()
	}

	return false, 0, b.lastTime.Add(rl.window).Unix()
}

// cleanup removes expired buckets from the rate limiter.
// Called periodically by the Manager's shared cleanup goroutine.
func (rl *rateLimiter) cleanup(now time.Time, maxAge time.Duration) {
	if rl == nil {
		return
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for k, b := range rl.buckets {
		if now.Sub(b.lastTime) > maxAge {
			delete(rl.buckets, k)
		}
	}
}

// New creates a token auth manager from a configuration string.
//
// Format (simple, comma-separated):
//
//	"sk-key1,sk-key2,sk-key3"
//
// Format (advanced, JSON file path):
//
//	"/path/to/tokens.json"
//
// JSON format:
//
//	[
//	  {"key": "sk-key1", "quota": 1000, "rate_limit": 60},
//	  {"key": "sk-key2", "quota": 0, "rate_limit": 0}
//	]
//
// - quota: max total requests (0 = unlimited)
// - rate_limit: max requests per minute (0 = use global default or disabled)
//
// globalRateLimit: used when per-key rate_limit is 0 and globalRateLimit > 0.
func New(configStr string, globalRateLimit int) (*Manager, error) {
	m := &Manager{
		tokens: make(map[string]*tokenState),
		window: time.Minute,
		stopCh: make(chan struct{}),
	}

	var tokenConfigs []TokenConfig

	if configStr == "" {
		go m.cleanupLoop()
		return m, nil
	}

	// Try loading as JSON file: check if the string refers to an existing file.
	// Use os.Stat to detect files regardless of path prefix (handles "tokens.json", "./tokens.json", etc.)
	if _, statErr := os.Stat(configStr); statErr == nil {
		f, err := os.Open(configStr)
		if err == nil {
			defer func() {
				_ = f.Close()
			}()
			data, err := io.ReadAll(f)
			if err != nil {
				return nil, fmt.Errorf("read token config file: %w", err)
			}
			if err := json.Unmarshal(data, &tokenConfigs); err != nil {
				return nil, fmt.Errorf("parse token config file: %w", err)
			}
			slog.Info("loaded token config from file", "path", configStr, "tokens", len(tokenConfigs))
		} else {
			return nil, fmt.Errorf("token config file exists but cannot be opened: %w", err)
		}
	} else if strings.HasPrefix(configStr, "/") || strings.HasPrefix(configStr, "./") || strings.HasPrefix(configStr, "../") {
		// Looks like a path but doesn't exist — report as error
		return nil, fmt.Errorf("token config looks like a file path but file does not exist: %s", configStr)
	} else if strings.HasPrefix(configStr, "[") {
		// Inline JSON array
		if err := json.Unmarshal([]byte(configStr), &tokenConfigs); err != nil {
			return nil, fmt.Errorf("parse inline token config JSON: %w", err)
		}
	} else if strings.HasPrefix(configStr, "{") {
		// Single JSON object
		var tc TokenConfig
		if err := json.Unmarshal([]byte(configStr), &tc); err != nil {
			return nil, fmt.Errorf("parse inline token config JSON: %w", err)
		}
		tokenConfigs = []TokenConfig{tc}
	} else {
		// Simple comma-separated keys
		for _, k := range strings.Split(configStr, ",") {
			k = strings.TrimSpace(k)
			if k != "" {
				tokenConfigs = append(tokenConfigs, TokenConfig{Key: k})
			}
		}
	}

	if len(tokenConfigs) == 0 {
		return nil, fmt.Errorf("no valid tokens found in configuration")
	}

	for _, tc := range tokenConfigs {
		if tc.Key == "" {
			return nil, fmt.Errorf("token config contains empty key")
		}
		// Reject negative values: treat as configuration errors rather than
		// silently disabling the limit (which a typo could cause).
		if tc.Quota < 0 {
			return nil, fmt.Errorf("token %q has negative quota %d; use 0 for unlimited", tc.Key, tc.Quota)
		}
		if tc.RateLimit < 0 {
			return nil, fmt.Errorf("token %q has negative rate_limit %d; use 0 for default/global", tc.Key, tc.RateLimit)
		}
		// Determine per-key rate limit: use per-key value, or fall back to global
		perKeyRate := tc.RateLimit
		if perKeyRate <= 0 && globalRateLimit > 0 {
			perKeyRate = globalRateLimit
		}
		m.tokens[tc.Key] = &tokenState{
			cfg:   tc,
			limit: newRateLimiter(perKeyRate, time.Minute),
		}
	}

	go m.cleanupLoop()
	return m, nil
}

// Stop cleans up all rate limiters and stops the shared cleanup goroutine.
// Safe to call multiple times (idempotent via sync.Once).
func (m *Manager) Stop() {
	m.once.Do(func() {
		close(m.stopCh)
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, ts := range m.tokens {
			ts.limit.Stop()
		}
	})
}

// cleanupLoop runs a single shared goroutine that cleans up expired buckets
// across all per-key rate limiters.
func (m *Manager) cleanupLoop() {
	ticker := time.NewTicker(m.window * 2)
	defer ticker.Stop()
	maxAge := m.window * 2
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.mu.RLock()
			now := time.Now()
			for _, ts := range m.tokens {
				ts.limit.cleanup(now, maxAge)
			}
			m.mu.RUnlock()
		}
	}
}

// Authenticate validates the token and returns (authenticated, reason).
func (m *Manager) Authenticate(token string) (bool, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	_, ok := m.tokens[token]
	if !ok {
		return false, "invalid_api_key"
	}

	return true, ""
}

// CheckQuota checks if the key has remaining quota.
func (m *Manager) CheckQuota(token string) (ok bool, remaining int, limit int) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ts, ok := m.tokens[token]
	if !ok {
		return false, 0, 0
	}

	quota := ts.cfg.Quota
	if quota <= 0 {
		return true, -1, 0 // unlimited
	}

	remaining = int(quota) - int(ts.used)
	return remaining > 0, remaining, quota
}

// RecordUsage increments the usage counter for a key.
func (m *Manager) RecordUsage(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ts, ok := m.tokens[token]; ok {
		ts.used++
	}
}

// CheckRateLimit checks the per-key rate limit.
func (m *Manager) CheckRateLimit(token string) (allowed bool, remaining int, limit int, resetTime int64) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ts, ok := m.tokens[token]
	if !ok {
		return true, 0, 0, 0
	}

	if ts.limit != nil {
		allowed, remaining, resetTime := ts.limit.allow(token)
		return allowed, remaining, ts.limit.rate, resetTime
	}

	return true, 0, 0, 0
}

// TokenCount returns the number of configured tokens.
func (m *Manager) TokenCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.tokens)
}

// authResult holds the combined result of Authenticate + CheckQuota + CheckRateLimit
// obtained in a single lock acquisition.
type authResult struct {
	authenticated bool
	authReason    string

	quotaExceeded  bool
	quotaRemaining int
	quotaLimit     int

	rateLimited   bool
	rateRemaining int
	rateLimit     int
	rateResetTime int64
}

// checkAll performs Authenticate + CheckQuota + CheckRateLimit in a single
// read-lock acquisition, then upgrades to a write-lock only for RecordUsage.
// This reduces per-request lock operations from 4 (RLock+RLock+RLock+Lock)
// to 2 (RLock+Lock), cutting lock contention by ~50%.
func (m *Manager) checkAll(token string) authResult {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ts, ok := m.tokens[token]
	if !ok {
		return authResult{authenticated: false, authReason: "invalid_api_key"}
	}

	result := authResult{authenticated: true}

	// Check quota
	if quota := ts.cfg.Quota; quota > 0 {
		remaining := int(quota) - int(ts.used)
		if remaining <= 0 {
			result.quotaExceeded = true
			result.quotaLimit = int(quota)
			return result
		}
		result.quotaRemaining = remaining
		result.quotaLimit = int(quota)
	}

	// Check rate limit
	if ts.limit != nil {
		allowed, remaining, resetTime := ts.limit.allow(token)
		if !allowed {
			result.rateLimited = true
			result.rateRemaining = 0
			result.rateLimit = ts.limit.rate
			result.rateResetTime = resetTime
			return result
		}
		result.rateRemaining = remaining
		result.rateLimit = ts.limit.rate
		result.rateResetTime = resetTime
	}

	return result
}

// Middleware returns an HTTP middleware that handles auth + quota + rate limiting.
func (m *Manager) Middleware() func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			// Extract Bearer token
			auth := r.Header.Get("Authorization")
			if auth == "" {
				writeAuthError(w, "missing_api_key", "missing Authorization header")
				return
			}
			if !strings.HasPrefix(auth, "Bearer ") {
				writeAuthError(w, "invalid_auth_format", "invalid Authorization format, expected 'Bearer <key>'")
				return
			}
			token := auth[7:]

			// Step 1-3: Combined auth + quota + rate limit check (single RLock)
			result := m.checkAll(token)

			if !result.authenticated {
				writeAuthError(w, result.authReason, "invalid API key")
				return
			}

			if result.quotaExceeded {
				w.Header().Set("X-Quota-Limit", strconv.Itoa(result.quotaLimit))
				w.Header().Set("X-Quota-Remaining", "0")
				writeLimitError(w, "quota_exceeded",
					fmt.Sprintf("quota exceeded: %d/%d requests used", result.quotaLimit, result.quotaLimit))
				return
			} else if result.quotaRemaining >= 0 {
				w.Header().Set("X-Quota-Limit", strconv.Itoa(result.quotaLimit))
				w.Header().Set("X-Quota-Remaining", strconv.Itoa(result.quotaRemaining-1))
			}

			if result.rateLimited {
				w.Header().Set("X-RateLimit-Limit", strconv.Itoa(result.rateLimit))
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", strconv.Itoa(int(result.rateResetTime)))
				retryAfter := int(result.rateResetTime - time.Now().Unix())
				if retryAfter < 1 {
					retryAfter = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				writeLimitError(w, "rate_limit_exceeded", "rate limit exceeded, please slow down")
				return
			} else if result.rateLimit > 0 {
				w.Header().Set("X-RateLimit-Limit", strconv.Itoa(result.rateLimit))
				w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(result.rateRemaining))
				w.Header().Set("X-RateLimit-Reset", strconv.Itoa(int(result.rateResetTime)))
			}

			// Step 4: All checks passed — record usage (single write-lock)
			m.RecordUsage(token)
			next(w, r)
		}
	}
}

// authErrorResponse is a structured error response to prevent JSON injection.
type authErrorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func writeAuthError(w http.ResponseWriter, code, message string) {
	resp := authErrorResponse{}
	resp.Error.Message = message
	resp.Error.Type = "authentication_error"
	resp.Error.Code = code

	data, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, `{"error":{"message":"internal error","type":"server_error","code":"internal_error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n"))
}

func writeLimitError(w http.ResponseWriter, code, message string) {
	status := http.StatusForbidden
	errType := "quota_error"
	if code == "rate_limit_exceeded" {
		status = http.StatusTooManyRequests
		errType = "rate_limit_error"
	}

	resp := authErrorResponse{}
	resp.Error.Message = message
	resp.Error.Type = errType
	resp.Error.Code = code

	data, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, `{"error":{"message":"internal error","type":"server_error","code":"internal_error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n"))
}
