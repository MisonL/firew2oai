package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mison/firew2oai/internal/transport"
)

func newTestProxy() *Proxy {
	return New(transport.New(30 * time.Second), "test-key", 30*time.Second, "test")
}

func TestHandleRoot(t *testing.T) {
	p := newTestProxy()
	mux := NewMux(p, "*")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if body["message"] == nil {
		t.Error("missing message field")
	}
	if body["version"] != "test" {
		t.Errorf("version = %v, want test", body["version"])
	}
}

func TestHandleHealth(t *testing.T) {
	p := newTestProxy()
	mux := NewMux(p, "*")
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want ok", body["status"])
	}
}

func TestHandleModels_NoAuth(t *testing.T) {
	p := newTestProxy()
	mux := NewMux(p, "*")
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleModels_WithAuth(t *testing.T) {
	p := newTestProxy()
	mux := NewMux(p, "*")
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp ModelListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if resp.Object != "list" {
		t.Errorf("object = %q, want list", resp.Object)
	}
	if len(resp.Data) == 0 {
		t.Error("expected non-empty model list")
	}
}

func TestHandleModels_WrongKey(t *testing.T) {
	p := newTestProxy()
	mux := NewMux(p, "*")
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleModels_InvalidAuthFormat(t *testing.T) {
	p := newTestProxy()
	mux := NewMux(p, "*")
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Basic dGVzdA==")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleChatCompletions_MethodNotAllowed(t *testing.T) {
	p := newTestProxy()
	mux := NewMux(p, "*")
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestHandleChatCompletions_EmptyBody(t *testing.T) {
	p := newTestProxy()
	mux := NewMux(p, "*")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleChatCompletions_EmptyMessages(t *testing.T) {
	p := newTestProxy()
	mux := NewMux(p, "*")
	body := `{"model":"deepseek-v3p2","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleChatCompletions_InvalidModel(t *testing.T) {
	p := newTestProxy()
	mux := NewMux(p, "*")
	body := `{"model":"nonexistent","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestMessagesToPrompt(t *testing.T) {
	tests := []struct {
		name  string
		msgs  []ChatMessage
		want  string
	}{
		{
			name:  "single user",
			msgs:  []ChatMessage{{Role: "user", Content: "hello"}},
			want:  "User: hello",
		},
		{
			name:  "system + user",
			msgs:  []ChatMessage{{Role: "system", Content: "be helpful"}, {Role: "user", Content: "hi"}},
			want:  "System: be helpful\nUser: hi",
		},
		{
			name:  "multi-turn",
			msgs:  []ChatMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}, {Role: "user", Content: "bye"}},
			want:  "User: hi\nAssistant: hello\nUser: bye",
		},
		{
			name:  "unknown role",
			msgs:  []ChatMessage{{Role: "tool", Content: "data"}},
			want:  "data",
		},
		{
			name:  "empty",
			msgs:  []ChatMessage{},
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := messagesToPrompt(tt.msgs)
			if got != tt.want {
				t.Errorf("messagesToPrompt() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCORSMiddleware_Wildcard(t *testing.T) {
	p := newTestProxy()
	mux := NewMux(p, "*")
	req := httptest.NewRequest(http.MethodOptions, "/v1/models", nil)
	req.Header.Set("Origin", "https://evil.com")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("CORS origin = %q, want *", got)
	}
}

func TestCORSMiddleware_SpecificOrigin(t *testing.T) {
	p := newTestProxy()
	mux := NewMux(p, "https://example.com,https://trusted.com")
	req := httptest.NewRequest(http.MethodOptions, "/v1/models", nil)
	req.Header.Set("Origin", "https://trusted.com")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://trusted.com" {
		t.Errorf("CORS origin = %q, want https://trusted.com", got)
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want Origin", got)
	}
}

func TestCORSMiddleware_RejectedOrigin(t *testing.T) {
	p := newTestProxy()
	mux := NewMux(p, "https://example.com")
	req := httptest.NewRequest(http.MethodOptions, "/v1/models", nil)
	req.Header.Set("Origin", "https://evil.com")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("CORS origin for rejected origin = %q, want empty", got)
	}
}

func TestRecoveryMiddleware(t *testing.T) {
	// We can't easily trigger a panic through the mux, but we can test the middleware directly
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("unexpected panic: %v", r)
		}
	}()
	// Test that the recovery middleware catches panics
	handler := RecoveryMiddleware(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	errObj, ok := body["error"].(map[string]interface{})
	if !ok {
		t.Fatal("missing error object")
	}
	if errObj["type"] != "server_error" {
		t.Errorf("error.type = %v, want server_error", errObj["type"])
	}
}

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusBadRequest, "test_type", "test_code", "test message %s", "arg")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	errObj, ok := body["error"].(map[string]interface{})
	if !ok {
		t.Fatal("missing error object")
	}
	if errObj["message"] != "test message arg" {
		t.Errorf("error.message = %v", errObj["message"])
	}
	if errObj["type"] != "test_type" {
		t.Errorf("error.type = %v", errObj["type"])
	}
	if errObj["code"] != "test_code" {
		t.Errorf("error.code = %v", errObj["code"])
	}
}

func TestGenerateRequestID(t *testing.T) {
	id1 := generateRequestID()
	id2 := generateRequestID()
	if id1 == id2 {
		t.Error("two request IDs should not be equal")
	}
	if !strings.HasPrefix(id1, "chatcmpl-") {
		t.Errorf("request ID = %q, want chatcmpl- prefix", id1)
	}
}
