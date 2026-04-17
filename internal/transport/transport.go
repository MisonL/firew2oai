package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
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
	client  *http.Client
	timeout time.Duration // overall request timeout (for non-streaming scenarios)
}

// Timeout returns the configured overall request timeout.
func (t *FireworksTransport) Timeout() time.Duration {
	return t.timeout
}

// New creates a new FireworksTransport with Chrome-mimicking TLS config and timeouts.
func New(timeout time.Duration) *FireworksTransport {
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
		timeout: timeout,
	}
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
		return nil, fmt.Errorf("send request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}

	return resp.Body, nil
}
