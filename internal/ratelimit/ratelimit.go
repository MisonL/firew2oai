package ratelimit

import (
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Limiter implements a per-IP token bucket rate limiter.
// It returns standard X-RateLimit-* headers and 429 when exceeded.
type Limiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	rate     int           // tokens per window
	window   time.Duration // window duration
	cleanup  time.Duration
	stopCh   chan struct{}
	stopped  bool
}

type bucket struct {
	tokens   int
	lastTime time.Time
}

// New creates a new rate limiter.
// rate: max requests per window per IP.
// window: the sliding window duration.
func New(rate int, window time.Duration) *Limiter {
	rl := &Limiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		window:  window,
		cleanup: window * 2,
		stopCh:  make(chan struct{}),
	}
	go rl.cleanupLoop()
	return rl
}

// Stop gracefully stops the cleanup goroutine.
func (rl *Limiter) Stop() {
	rl.mu.Lock()
	if rl.stopped {
		rl.mu.Unlock()
		return
	}
	rl.stopped = true
	rl.mu.Unlock()
	close(rl.stopCh)
}

// Middleware returns an HTTP middleware that enforces rate limits per client IP.
func (rl *Limiter) Middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := extractIP(r)

		rl.mu.Lock()
		b, ok := rl.buckets[ip]
		if !ok {
			b = &bucket{tokens: rl.rate, lastTime: time.Now()}
			rl.buckets[ip] = b
		}

		// Refill tokens based on elapsed time
		elapsed := time.Since(b.lastTime)
		refill := int(elapsed / rl.window)
		if refill > 0 {
			b.tokens += refill * rl.rate
			if b.tokens > rl.rate {
				b.tokens = rl.rate
			}
			b.lastTime = b.lastTime.Add(time.Duration(refill) * rl.window)
		}

		remaining := 0
		if b.tokens > 0 {
			b.tokens--
			remaining = b.tokens
		} else {
			// No tokens available — rate limit exceeded
			resetTime := b.lastTime.Add(rl.window).Unix()
			rl.mu.Unlock()

			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(rl.rate))
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.Itoa(int(resetTime)))
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Retry-After", strconv.Itoa(int(rl.window.Seconds())))
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"rate limit exceeded","type":"rate_limit_error","code":"rate_limit_exceeded"}}`))
			return
		}
		resetTime := b.lastTime.Add(rl.window).Unix()

		rl.mu.Unlock()

		// Set rate limit headers
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(rl.rate))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
		w.Header().Set("X-RateLimit-Reset", strconv.Itoa(int(resetTime)))

		next(w, r)
	}
}

func (rl *Limiter) cleanupLoop() {
	ticker := time.NewTicker(rl.cleanup)
	defer ticker.Stop()
	for {
		select {
		case <-rl.stopCh:
			return
		case <-ticker.C:
			rl.mu.Lock()
			now := time.Now()
			for ip, b := range rl.buckets {
				if now.Sub(b.lastTime) > rl.cleanup {
					delete(rl.buckets, ip)
				}
			}
			rl.mu.Unlock()
		}
	}
}

// extractIP extracts the client IP from the request.
// Priority: X-Real-IP > X-Forwarded-For (first IP) > RemoteAddr.
//
// SECURITY NOTE: X-Forwarded-For and X-Real-IP are set by reverse proxies.
// If firew2oai is deployed without a trusted reverse proxy, these headers
// can be spoofed by clients to bypass rate limiting.
// Always deploy behind a trusted proxy (nginx, Caddy, etc.) that overwrites these headers.
func extractIP(r *http.Request) string {
	// Prefer X-Real-IP (set by nginx/Caddy to the real client IP)
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		// Validate it looks like an IP to prevent header injection
		if net.ParseIP(xri) != nil {
			return xri
		}
		slog.Warn("invalid X-Real-IP header, falling back to RemoteAddr", "value", xri)
	}

	// Fallback to RemoteAddr (strip port)
	addr := r.RemoteAddr
	// Try splitting on last colon to handle IPv6:port
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
