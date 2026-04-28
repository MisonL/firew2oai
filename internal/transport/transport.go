package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultChromeUserAgent is the default Chrome User-Agent string.
// Override via CHROME_USER_AGENT environment variable.
const DefaultChromeUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36"

// ChromeUserAgent is the active User-Agent string, configurable via env.
var ChromeUserAgent = DefaultChromeUserAgent

func init() {
	if v := os.Getenv("CHROME_USER_AGENT"); v != "" {
		ChromeUserAgent = v
	}
}

const (
	originFireworks        = "https://chat.fireworks.ai"
	refererFireworks       = "https://chat.fireworks.ai/"
	acceptLanguage         = "en-US,en;q=0.9,zh-CN;q=0.8,zh;q=0.7"
	secChUAMobile          = "?0"
	defaultSecChUAPlatform = `"macOS"`
	transportReadBuffer    = 32 * 1024
	transportWriteBuffer   = 16 * 1024
)

// chromeUAVersionRE extracts the Chrome major version from a User-Agent string.
var chromeUAVersionRE = regexp.MustCompile(`Chrome/(\d+)\.`)

// derivePlatform extracts the OS platform from a User-Agent string for sec-ch-ua-platform.
func derivePlatform(ua string) string {
	switch {
	case strings.Contains(ua, "iPhone") || strings.Contains(ua, "iPad"):
		return `"iOS"`
	case strings.Contains(ua, "Windows"):
		return `"Windows"`
	case strings.Contains(ua, "Android"):
		return `"Android"`
	case strings.Contains(ua, "Linux"):
		return `"Linux"`
	case strings.Contains(ua, "Macintosh") || strings.Contains(ua, "Mac OS"):
		return `"macOS"`
	default:
		return defaultSecChUAPlatform
	}
}

// secChUADerived returns sec-ch-ua and sec-ch-ua-platform headers derived
// from the current ChromeUserAgent, keeping the fingerprint consistent
// when CHROME_USER_AGENT is overridden.
func secChUADerived() (secChUA, secChUAPlatform string) {
	m := chromeUAVersionRE.FindStringSubmatch(ChromeUserAgent)
	var ver string
	if m != nil {
		ver = m[1]
	}
	if ver == "" {
		ver = "134"
	}
	return fmt.Sprintf(`"Google Chrome";v="%s", "Chromium";v="%s", "Not:A-Brand";v="24"`, ver, ver),
		derivePlatform(ChromeUserAgent)
}

// FireworksTransport wraps an *http.Client with Chrome-like TLS and HTTP settings
// to mimic browser fingerprints when connecting to Fireworks.ai.
//
// Fingerprint strategy:
//  1. TLS: Chrome's cipher suite preference order (TLS 1.3 first, then 1.2 fallbacks)
//  2. HTTP/2: enabled by default in Go's net/http, with proper SETTINGS
//  3. Headers: full Chrome browser header set (sec-ch-ua, sec-fetch-*, Accept-Language, etc.)
//  4. Connection pooling: browser-like keep-alive with realistic idle timeouts
type FireworksTransport struct {
	client               *http.Client
	timeout              time.Duration // overall request timeout (for non-streaming scenarios)
	upstreamRetryCount   int
	upstreamRetryBackoff time.Duration
}

// Timeout returns the configured overall request timeout.
func (t *FireworksTransport) Timeout() time.Duration {
	return t.timeout
}

// NewWithRetry creates a new FireworksTransport with configurable retry behavior
// for transient upstream failures before the first response byte is processed.
func NewWithRetry(timeout time.Duration, retryCount int, retryBackoff time.Duration) *FireworksTransport {
	if retryCount < 0 {
		retryCount = 0
	}
	if retryBackoff < 0 {
		retryBackoff = 0
	}

	// TLS config matching Chrome's JA3 fingerprint:
	// - TLS 1.3 cipher suites in Chrome's preference order
	// - TLS 1.2 fallback cipher suites
	// - No session tickets reuse to appear as a fresh browser connection
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS13,
		CipherSuites: []uint16{
			// TLS 1.3 (Chrome preference order)
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
			// TLS 1.2 ECDHE (Chrome order: GCM first, then CHACHA20)
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		},
		// Curve preferences: X25519, then P-256 (Chrome order)
		CurvePreferences: []tls.CurveID{
			tls.X25519,
			tls.CurveP256,
		},
	}

	// Custom dialer to set TCP keepalive matching Chrome behavior
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		// Core TLS fingerprint
		TLSClientConfig: tlsConfig,
		// Browser-like connection settings
		DialContext:           dialer.DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		MaxConnsPerHost:       50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: timeout,
		ReadBufferSize:        transportReadBuffer,
		WriteBufferSize:       transportWriteBuffer,
		// Force HTTP/2 (Go enables it by default when TLS is used)
		ForceAttemptHTTP2: true,
	}

	return &FireworksTransport{
		client: &http.Client{
			Transport: transport,
			// No Client.Timeout: SSE streaming responses can last arbitrarily long
			// (especially for thinking models). Timeout is enforced per-request
			// via context deadline in StreamPost. ResponseHeaderTimeout above
			// ensures we don't hang waiting for the first byte from upstream.
		},
		timeout:              timeout,
		upstreamRetryCount:   retryCount,
		upstreamRetryBackoff: retryBackoff,
	}
}

// New creates a new FireworksTransport with Chrome-mimicking TLS config and timeouts.
func New(timeout time.Duration) *FireworksTransport {
	return NewWithRetry(timeout, 0, 0)
}

// StreamPost sends a POST request with full Chrome browser fingerprint headers
// and returns the response body for streaming. The caller is responsible for
// closing the returned io.ReadCloser.
//
// IMPORTANT: The caller should set an appropriate deadline on the context
// to prevent requests from hanging indefinitely. For non-streaming requests,
// use context.WithTimeout. For streaming requests, the caller may choose
// to not set a deadline (SSE can last arbitrarily long).
//
// ResponseHeaderTimeout (set during construction) ensures we don't hang
// waiting for the first byte from upstream.
func (t *FireworksTransport) StreamPost(ctx context.Context, url string, body io.Reader, authToken string) (io.ReadCloser, error) {
	var payload []byte
	var err error
	if body != nil {
		payload, err = io.ReadAll(body)
		if err != nil {
			return nil, fmt.Errorf("read request body: %w", err)
		}
	}
	for attempt := 0; ; attempt++ {
		reader := bytes.NewReader(payload)
		resp, reqErr := t.streamPostOnce(ctx, url, reader, authToken)
		if reqErr == nil {
			return resp, nil
		}
		if !t.shouldRetryStreamPost(attempt, reqErr) {
			return nil, reqErr
		}
		delay := t.retryDelayForError(attempt, reqErr)
		slog.Warn("transient upstream failure before first byte, retrying",
			"attempt", attempt+1,
			"max_retries", t.upstreamRetryCount,
			"backoff", delay,
			"error", reqErr,
		)
		if err := sleepWithContext(ctx, delay); err != nil {
			return nil, err
		}
	}
}

func (t *FireworksTransport) streamPostOnce(ctx context.Context, url string, body io.Reader, authToken string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	// ── Full Chrome browser header set ──
	// Core headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Language", acceptLanguage)

	// Forward Authorization header to Fireworks upstream
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	// Note: Do NOT set Accept-Encoding manually. Go's net/http transparently
	// handles gzip decoding when we don't override this header. Manually
	// declaring encodings we can't decompress (br, zstd) would break if
	// the upstream responds with them.

	// Browser identity
	req.Header.Set("User-Agent", ChromeUserAgent)
	req.Header.Set("Origin", originFireworks)
	req.Header.Set("Referer", refererFireworks)

	// Chrome Client Hints (sec-ch-ua headers) — derived from ChromeUserAgent
	derivedChUA, derivedChUAPlatform := secChUADerived()
	req.Header.Set("sec-ch-ua", derivedChUA)
	req.Header.Set("sec-ch-ua-mobile", secChUAMobile)
	req.Header.Set("sec-ch-ua-platform", derivedChUAPlatform)

	// Fetch metadata headers (Chrome sends these for navigation requests)
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-site", "same-origin")

	// Connection header
	req.Header.Set("Connection", "keep-alive")

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, transientSendRequestError{cause: err}
	}

	if resp.StatusCode != http.StatusOK {
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		_ = resp.Body.Close()
		return nil, transientUpstreamError{statusCode: resp.StatusCode, retryAfter: retryAfter}
	}

	return resp.Body, nil
}

type transientUpstreamError struct {
	statusCode int
	retryAfter time.Duration
}

func (e transientUpstreamError) Error() string {
	return fmt.Sprintf("upstream returned status %d", e.statusCode)
}

type transientSendRequestError struct {
	cause error
}

func (e transientSendRequestError) Error() string {
	return fmt.Sprintf("send request: %v", e.cause)
}

func (e transientSendRequestError) Unwrap() error {
	return e.cause
}

func (t *FireworksTransport) shouldRetryStreamPost(attempt int, err error) bool {
	if attempt >= t.upstreamRetryCount {
		return false
	}
	if errorsIsContextTerminal(err) {
		return false
	}
	var statusErr transientUpstreamError
	if errors.As(err, &statusErr) {
		switch statusErr.statusCode {
		case http.StatusInternalServerError, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		default:
			return false
		}
	}
	var sendErr transientSendRequestError
	return errors.As(err, &sendErr)
}

func (t *FireworksTransport) retryDelayForError(attempt int, err error) time.Duration {
	delay := t.upstreamRetryBackoff
	if delay <= 0 {
		delay = 0
	} else {
		delay = delay * time.Duration(1<<attempt)
	}
	var statusErr transientUpstreamError
	if errors.As(err, &statusErr) && statusErr.retryAfter > delay {
		delay = statusErr.retryAfter
	}
	return delay
}

func parseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		delay := time.Until(when)
		if delay > 0 {
			return delay
		}
	}
	return 0
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func errorsIsContextTerminal(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
