package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	tp := New(30 * time.Second)
	if tp == nil {
		t.Fatal("New returned nil")
	}
	if tp.client == nil {
		t.Fatal("client is nil")
	}
	// Client.Timeout must be 0: SSE streaming responses can last arbitrarily long.
	// Timeout is enforced via context deadline, not Client.Timeout.
	if tp.client.Timeout != 0 {
		t.Errorf("Client.Timeout = %v, want 0 (disabled for SSE streaming)", tp.client.Timeout)
	}
	transport, ok := tp.client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("client transport is not *http.Transport")
	}
	if transport.ReadBufferSize != transportReadBuffer {
		t.Errorf("ReadBufferSize = %d, want %d", transport.ReadBufferSize, transportReadBuffer)
	}
	if transport.WriteBufferSize != transportWriteBuffer {
		t.Errorf("WriteBufferSize = %d, want %d", transport.WriteBufferSize, transportWriteBuffer)
	}
	// The timeout parameter should be used as ResponseHeaderTimeout, not Client.Timeout
	if transport.ResponseHeaderTimeout != 30*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want 30s", transport.ResponseHeaderTimeout)
	}
	if tp.upstreamRetryCount != 0 {
		t.Errorf("upstreamRetryCount = %d, want 0 for backward-compatible New()", tp.upstreamRetryCount)
	}
}

func TestNewWithRetry(t *testing.T) {
	tp := NewWithRetry(30*time.Second, 2, 250*time.Millisecond)
	if tp == nil {
		t.Fatal("NewWithRetry returned nil")
	}
	if tp.upstreamRetryCount != 2 {
		t.Fatalf("upstreamRetryCount = %d, want 2", tp.upstreamRetryCount)
	}
	if tp.upstreamRetryBackoff != 250*time.Millisecond {
		t.Fatalf("upstreamRetryBackoff = %v, want 250ms", tp.upstreamRetryBackoff)
	}
}

func TestChromeUserAgent(t *testing.T) {
	ua := ChromeUserAgent
	if ua == "" {
		t.Error("ChromeUserAgent is empty")
	}
	if len(ua) < 20 {
		t.Error("ChromeUserAgent seems too short")
	}
}

func TestStreamPost_InvalidURL(t *testing.T) {
	tp := New(5 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := tp.StreamPost(ctx, "://invalid-url", bytes.NewReader([]byte("{}")), "")
	if err == nil {
		t.Error("expected error for invalid URL")
	}
}

func TestStreamPost_UpstreamNon200(t *testing.T) {
	// Use httptest.Server to simulate an upstream that returns 404
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	tp := New(5 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	reader, err := tp.StreamPost(ctx, ts.URL, bytes.NewReader([]byte("{}")), "")
	if err == nil {
		t.Error("expected error for non-200 status")
		if reader != nil {
			reader.Close()
		}
	}
	// Verify error mentions the status code
	if err != nil && !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention 404, got: %v", err)
	}
}

func TestStreamPost_RetriesTransientStatusBeforeSuccess(t *testing.T) {
	var attempts int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	tp := NewWithRetry(5*time.Second, 1, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	reader, err := tp.StreamPost(ctx, ts.URL, bytes.NewReader([]byte("{}")), "")
	if err != nil {
		t.Fatalf("StreamPost error = %v, want nil", err)
	}
	defer reader.Close()

	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestStreamPost_RetriesInternalServerErrorBeforeSuccess(t *testing.T) {
	var attempts int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	tp := NewWithRetry(5*time.Second, 1, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	reader, err := tp.StreamPost(ctx, ts.URL, bytes.NewReader([]byte("{}")), "")
	if err != nil {
		t.Fatalf("StreamPost error = %v, want nil", err)
	}
	defer reader.Close()

	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestStreamPost_DoesNotRetryNonTransientStatus(t *testing.T) {
	var attempts int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	tp := NewWithRetry(5*time.Second, 3, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := tp.StreamPost(ctx, ts.URL, bytes.NewReader([]byte("{}")), "")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestStreamPost_RetriesSendRequestErrorOnce(t *testing.T) {
	var attempts int
	stall := make(chan struct{})
	defer close(stall)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("server does not support hijacking")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatalf("hijack failed: %v", err)
			}
			_ = conn.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	tp := NewWithRetry(5*time.Second, 1, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	reader, err := tp.StreamPost(ctx, ts.URL, bytes.NewReader([]byte("{}")), "")
	if err != nil {
		t.Fatalf("StreamPost error = %v, want nil", err)
	}
	defer reader.Close()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestStreamPost_ContextCanceledDuringRetryBackoffReturnsContextError(t *testing.T) {
	var attempts int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()

	tp := NewWithRetry(5*time.Second, 1, 200*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := tp.StreamPost(ctx, ts.URL, bytes.NewReader([]byte("{}")), "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 before cancellation interrupts retry", attempts)
	}
}

func TestStreamPost_ContextCanceled(t *testing.T) {
	// Use httptest.Server instead of relying on external network
	stall := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This handler will never respond, simulating a slow upstream
		<-stall
	}))
	defer func() {
		close(stall)
		ts.Close()
	}()

	tp := New(30 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := tp.StreamPost(ctx, ts.URL, bytes.NewReader([]byte("{}")), "")
	if err == nil {
		t.Error("expected error for canceled context")
	}
}

// t.TempDeadline doesn't exist; context.WithTimeout is the right approach.
// This test verifies that StreamPost closes the reader on error.
func TestStreamPost_BodyClosedOnError(t *testing.T) {
	tp := New(5 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	body := bytes.NewReader([]byte("{}"))
	_, err := tp.StreamPost(ctx, "://invalid", body, "")
	if err == nil {
		t.Error("expected error")
	}
	remaining, readErr := io.ReadAll(body)
	if readErr != nil {
		t.Errorf("body read after error failed: %v", readErr)
	}
	if len(remaining) != 0 {
		t.Errorf("remaining bytes = %q, want empty because request body is buffered for retry support", string(remaining))
	}
}

func TestSecChUADerived_Default(t *testing.T) {
	// Reset to default UA
	original := ChromeUserAgent
	ChromeUserAgent = original
	defer func() { ChromeUserAgent = original }()

	chUA, chUAPlatform := secChUADerived()
	if chUA == "" {
		t.Error("secChUADerived returned empty sec-ch-ua")
	}
	// Default UA is Chrome/134, should contain 134
	if !strings.Contains(chUA, `"134"`) {
		t.Errorf("sec-ch-ua should contain version 134, got: %s", chUA)
	}
	if !strings.Contains(chUA, `"Google Chrome"`) {
		t.Errorf("sec-ch-ua should contain 'Google Chrome', got: %s", chUA)
	}
	if !strings.Contains(chUA, `"Chromium"`) {
		t.Errorf("sec-ch-ua should contain 'Chromium', got: %s", chUA)
	}
	// Default platform is macOS
	if chUAPlatform != `"macOS"` {
		t.Errorf("sec-ch-ua-platform = %s, want \"macOS\"", chUAPlatform)
	}
}

func TestSecChUADerived_CustomVersion(t *testing.T) {
	original := ChromeUserAgent
	ChromeUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36"
	defer func() { ChromeUserAgent = original }()

	chUA, chUAPlatform := secChUADerived()
	if !strings.Contains(chUA, `"135"`) {
		t.Errorf("sec-ch-ua should contain version 135, got: %s", chUA)
	}
	// Should NOT contain 134 from the override
	if strings.Contains(chUA, `"134"`) {
		t.Errorf("sec-ch-ua should not contain old version 134, got: %s", chUA)
	}
	if chUAPlatform != `"Windows"` {
		t.Errorf("sec-ch-ua-platform = %s, want \"Windows\"", chUAPlatform)
	}
}

func TestSecChUADerived_NoVersion(t *testing.T) {
	original := ChromeUserAgent
	ChromeUserAgent = "SomeRandomBot/1.0"
	defer func() { ChromeUserAgent = original }()

	chUA, chUAPlatform := secChUADerived()
	// Should fall back to 134
	if !strings.Contains(chUA, `"134"`) {
		t.Errorf("sec-ch-ua should fall back to 134, got: %s", chUA)
	}
	if chUAPlatform != `"macOS"` {
		t.Errorf("sec-ch-ua-platform = %s, want \"macOS\" (default)", chUAPlatform)
	}
}

func TestDerivePlatform(t *testing.T) {
	tests := []struct {
		ua   string
		want string
	}{
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/134.0.0.0", `"Windows"`},
		{"Mozilla/5.0 (X11; Linux x86_64) Chrome/134.0.0.0", `"Linux"`},
		{"Mozilla/5.0 (Linux; Android 14) Chrome/134.0.0.0 Mobile", `"Android"`},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Chrome/134.0.0.0", `"macOS"`},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Chrome/134.0.0.0", `"iOS"`},
		{"Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) Chrome/134.0.0.0", `"iOS"`},
		{"UnknownBot/1.0", `"macOS"`}, // default fallback
	}
	for _, tt := range tests {
		got := derivePlatform(tt.ua)
		if got != tt.want {
			t.Errorf("derivePlatform(%q) = %s, want %s", tt.ua, got, tt.want)
		}
	}
}

// TestTimeout_ReturnsConfiguredTimeout verifies that Timeout() returns
// the value passed to New().
func TestTimeout_ReturnsConfiguredTimeout(t *testing.T) {
	tp := New(42 * time.Second)
	if got := tp.Timeout(); got != 42*time.Second {
		t.Errorf("Timeout() = %v, want 42s", got)
	}
}
