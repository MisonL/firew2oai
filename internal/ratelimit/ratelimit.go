package ratelimit

import (
	"net/http"
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
	}
	// Periodic cleanup of stale buckets
	go rl.cleanupLoop()
	return rl
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
		}
		resetTime := b.lastTime.Add(rl.window).Unix()

		rl.mu.Unlock()

		// Set rate limit headers
		w.Header().Set("X-RateLimit-Limit", intToStr(rl.rate))
		w.Header().Set("X-RateLimit-Remaining", intToStr(remaining))
		w.Header().Set("X-RateLimit-Reset", intToStr(int(resetTime)))

		if remaining == 0 {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Retry-After", intToStr(int(rl.window.Seconds())))
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"rate limit exceeded","type":"rate_limit_error","code":"rate_limit_exceeded"}}`))
			return
		}

		next(w, r)
	}
}

func (rl *Limiter) cleanupLoop() {
	ticker := time.NewTicker(rl.cleanup)
	defer ticker.Stop()
	for range ticker.C {
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

func extractIP(r *http.Request) string {
	// Check X-Forwarded-For first (for reverse proxy setups)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Take the first IP in the chain
		if idx := len(xff); idx > 0 {
			for i, c := range xff {
				if c == ',' {
					return xff[:i]
				}
				if i == idx-1 {
					return xff
				}
			}
		}
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}
	// Fallback to RemoteAddr (strip port)
	addr := r.RemoteAddr
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i]
		}
	}
	return addr
}

func intToStr(n int) string {
	return fastFormat(n)
}

// fastFormat avoids strconv import for this hot path.
func fastFormat(n int) string {
	if n == 0 {
		return "0"
	}
	if n < 0 {
		return "-" + fastFormat(-n)
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
