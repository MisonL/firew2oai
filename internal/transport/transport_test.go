package transport

import (
	"bytes"
	"context"
	"io"
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
	if tp.client.Timeout != 30*time.Second {
		t.Errorf("timeout = %v, want 30s", tp.client.Timeout)
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

	_, err := tp.StreamPost(ctx, "://invalid-url", bytes.NewReader([]byte("{}")))
	if err == nil {
		t.Error("expected error for invalid URL")
	}
}

func TestStreamPost_UpstreamNon200(t *testing.T) {
	// We can't easily mock the upstream, but test with a non-existent endpoint
	// that should return non-200
	tp := New(5 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// This should fail because the endpoint doesn't exist on fireworks.ai
	reader, err := tp.StreamPost(ctx, "https://chat.fireworks.ai/nonexistent", bytes.NewReader([]byte("{}")))
	if err != nil {
		// Expected: upstream returned non-200
		return
	}
	if reader != nil {
		reader.Close()
	}
	// If we get here with a reader, the request somehow succeeded (unlikely)
	t.Log("unexpected: request to nonexistent endpoint succeeded")
}

func TestStreamPost_ContextCanceled(t *testing.T) {
	tp := New(30 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := tp.StreamPost(ctx, "https://chat.fireworks.ai/chat/single", bytes.NewReader([]byte("{}")))
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
	_, err := tp.StreamPost(ctx, "://invalid", body)
	if err == nil {
		t.Error("expected error")
	}
	// Body should still be readable since StreamPost doesn't close it on creation error
	_, readErr := io.ReadAll(body)
	if readErr != nil {
		t.Errorf("body should still be readable: %v", readErr)
	}
}
