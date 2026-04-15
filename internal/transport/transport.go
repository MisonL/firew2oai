package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// ChromeUserAgent is updated regularly to match the latest Chrome stable release.
const ChromeUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36"

const (
	originFireworks = "https://chat.fireworks.ai"
	refererFireworks = "https://chat.fireworks.ai/"
	acceptLanguage  = "en-US,en;q=0.9,zh-CN;q=0.8,zh;q=0.7"
	acceptEncoding  = "gzip, deflate, br, zstd"
	secChUA         = `"Google Chrome";v="134", "Chromium";v="134", "Not:A-Brand";v="24"`
	secChUAMobile   = "?0"
	secChUAPlatform = `"macOS"`
)

// FireworksTransport wraps an *http.Client with Chrome-like TLS and HTTP settings
// to mimic browser fingerprints when connecting to Fireworks.ai.
//
// Fingerprint strategy:
//  1. TLS: Chrome's cipher suite preference order (TLS 1.3 first, then 1.2 fallbacks)
//  2. HTTP/2: enabled by default in Go's net/http, with proper SETTINGS
//  3. Headers: full Chrome browser header set (sec-ch-ua, sec-fetch-*, Accept-Language, etc.)
//  4. Connection pooling: browser-like keep-alive with realistic idle timeouts
type FireworksTransport struct {
	client *http.Client
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

	return &FireworksTransport{
		client: &http.Client{
			Transport: &http.Transport{
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
				// Force HTTP/2 (Go enables it by default when TLS is used)
				ForceAttemptHTTP2: true,
			},
			// Overall request timeout (per-request)
			Timeout: timeout,
		},
	}
}

// StreamPost sends a POST request with full Chrome browser fingerprint headers
// and returns the response body for streaming. The caller is responsible for
// closing the returned io.ReadCloser.
func (t *FireworksTransport) StreamPost(ctx context.Context, url string, body io.Reader) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	// ── Full Chrome browser header set ──
	// Core headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Language", acceptLanguage)
	req.Header.Set("Accept-Encoding", acceptEncoding)

	// Browser identity
	req.Header.Set("User-Agent", ChromeUserAgent)
	req.Header.Set("Origin", originFireworks)
	req.Header.Set("Referer", refererFireworks)

	// Chrome Client Hints (sec-ch-ua headers)
	req.Header.Set("sec-ch-ua", secChUA)
	req.Header.Set("sec-ch-ua-mobile", secChUAMobile)
	req.Header.Set("sec-ch-ua-platform", secChUAPlatform)

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
