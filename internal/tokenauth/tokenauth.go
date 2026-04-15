package tokenauth

import (
	"crypto/subtle"
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
	Key        string `json:"key"`
	Quota      int    `json:"quota"`       // max total requests (0 = unlimited)
	RateLimit  int    `json:"rate_limit"`  // max requests per minute (0 = use global default)
}

// Manager handles multi-key authentication, quota tracking, and per-key rate limiting.
type Manager struct {
	mu      sync.RWMutex
	tokens  map[string]*tokenState
	limiter *rateLimiter
	window  time.Duration
}

type tokenState struct {
	cfg    TokenConfig
	used   int64 // total requests used
	limit  *rateLimiter
}

// rateLimiter is a simple token bucket for per-key rate limiting.
type rateLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*bucket
	rate       int
	window     time.Duration
	stopCh     chan struct{}
	stopped    bool
}

type bucket struct {
	tokens   int
	lastTime time.Time
}

// newRateLimiter creates a rate limiter with the given rate (requests per window).
// rate 0 means disabled.
func newRateLimiter(rate int, window time.Duration) *rateLimiter {
	if rate <= 0 {
		return nil
	}
	rl := &rateLimiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		window:  window,
		stopCh:  make(chan struct{}),
	}
	go rl.cleanupLoop()
	return rl
}

func (rl *rateLimiter) Stop() {
	if rl == nil {
		return
	}
	rl.mu.Lock()
	if rl.stopped {
		rl.mu.Unlock()
		return
	}
	rl.stopped = true
	rl.mu.Unlock()
	close(rl.stopCh)
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

func (rl *rateLimiter) cleanupLoop() {
	ticker := time.NewTicker(rl.window * 2)
	defer ticker.Stop()
	for {
		select {
		case <-rl.stopCh:
			return
		case <-ticker.C:
			rl.mu.Lock()
			now := time.Now()
			for k, b := range rl.buckets {
				if now.Sub(b.lastTime) > rl.window*2 {
					delete(rl.buckets, k)
				}
			}
			rl.mu.Unlock()
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
	}

	var tokenConfigs []TokenConfig

	if configStr == "" {
		return m, nil
	}

	// Try loading as JSON file first
	if strings.HasPrefix(configStr, "/") || strings.HasPrefix(configStr, "./") || strings.HasPrefix(configStr, "../") {
		f, err := os.Open(configStr)
		if err == nil {
			defer f.Close()
			data, err := io.ReadAll(f)
			if err != nil {
				return nil, fmt.Errorf("read token config file: %w", err)
			}
			if err := json.Unmarshal(data, &tokenConfigs); err != nil {
				return nil, fmt.Errorf("parse token config file: %w", err)
			}
			slog.Info("loaded token config from file", "path", configStr, "tokens", len(tokenConfigs))
		} else {
			return nil, fmt.Errorf("token config looks like a file path but cannot be opened: %w", err)
		}
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

	return m, nil
}

// Stop cleans up all rate limiter goroutines.
func (m *Manager) Stop() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, ts := range m.tokens {
		ts.limit.Stop()
	}
	if m.limiter != nil {
		m.limiter.Stop()
	}
}

// SetGlobalRateLimit sets a global rate limiter applied to all authenticated keys.
// This is used when no per-key rate_limit is specified.
func (m *Manager) SetGlobalRateLimit(rate int) {
	if rate <= 0 {
		return
	}
	m.limiter = newRateLimiter(rate, time.Minute)
}

// Authenticate validates the token and returns (authenticated, reason).
func (m *Manager) Authenticate(token string) (bool, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ts, ok := m.tokens[token]
	if !ok {
		return false, "invalid_api_key"
	}

	// Check constant-time
	if subtle.ConstantTimeCompare([]byte(token), []byte(ts.cfg.Key)) != 1 {
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

	// Check global rate limit
	if m.limiter != nil {
		allowed, remaining, resetTime := m.limiter.allow(token)
		return allowed, remaining, m.limiter.rate, resetTime
	}

	return true, 0, 0, 0
}

// TokenCount returns the number of configured tokens.
func (m *Manager) TokenCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.tokens)
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

			// Step 1: Authenticate
			if ok, reason := m.Authenticate(token); !ok {
				writeAuthError(w, reason, "invalid API key")
				return
			}

			// Step 2: Check quota
			if ok, remaining, limit := m.CheckQuota(token); !ok {
				w.Header().Set("X-Quota-Limit", strconv.Itoa(limit))
				w.Header().Set("X-Quota-Remaining", "0")
				writeLimitError(w, "quota_exceeded",
					fmt.Sprintf("quota exceeded: %d/%d requests used", limit, limit))
				return
			} else if remaining >= 0 {
				w.Header().Set("X-Quota-Limit", strconv.Itoa(limit))
				w.Header().Set("X-Quota-Remaining", strconv.Itoa(remaining))
			}

			// Step 3: Check rate limit
			if allowed, remaining, limit, resetTime := m.CheckRateLimit(token); !allowed {
				w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", strconv.Itoa(int(resetTime)))
				w.Header().Set("Retry-After", strconv.Itoa(60))
				writeLimitError(w, "rate_limit_exceeded", "rate limit exceeded, please slow down")
				return
			} else if limit > 0 {
				w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
				w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
				w.Header().Set("X-RateLimit-Reset", strconv.Itoa(int(resetTime)))
			}

			// Step 4: Record usage
			m.RecordUsage(token)

			next(w, r)
		}
	}
}

func writeAuthError(w http.ResponseWriter, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(fmt.Sprintf(
		`{"error":{"message":"%s","type":"authentication_error","code":"%s"}}`+"\n",
		message, code,
	)))
}

func writeLimitError(w http.ResponseWriter, code, message string) {
	status := http.StatusForbidden // 403 for quota
	if code == "rate_limit_exceeded" {
		status = http.StatusTooManyRequests // 429 for rate limit
	}
	errType := "quota_error"
	if code == "rate_limit_exceeded" {
		errType = "rate_limit_error"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write([]byte(fmt.Sprintf(
		`{"error":{"message":"%s","type":"%s","code":"%s"}}`+"\n",
		message, errType, code,
	)))
}
