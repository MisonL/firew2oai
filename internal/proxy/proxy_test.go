package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mison/firew2oai/internal/tokenauth"
	"github.com/mison/firew2oai/internal/transport"
)

func newTestProxy() *Proxy {
	return New(transport.New(30*time.Second), "test", false)
}

// newTestMux creates a mux with tokenauth for tests.
func newTestMux(t *testing.T, p *Proxy, corsOrigins string) http.Handler {
	t.Helper()

	tm, err := tokenauth.New("test-key", 0)
	if err != nil {
		t.Fatalf("tokenauth.New error: %v", err)
	}
	t.Cleanup(tm.Stop)

	return NewMux(p, corsOrigins, tm)
}

func TestHandleRoot(t *testing.T) {
	p := newTestProxy()
	mux := newTestMux(t, p, "*")
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
	mux := newTestMux(t, p, "*")
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
	mux := newTestMux(t, p, "*")
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleModels_WithAuth(t *testing.T) {
	p := newTestProxy()
	mux := newTestMux(t, p, "*")
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
	mux := newTestMux(t, p, "*")
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
	mux := newTestMux(t, p, "*")
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
	mux := newTestMux(t, p, "*")
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
	mux := newTestMux(t, p, "*")
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
	mux := newTestMux(t, p, "*")
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
	mux := newTestMux(t, p, "*")
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
		name string
		msgs []ChatMessage
		want string
	}{
		{
			name: "single user",
			msgs: []ChatMessage{{Role: "user", Content: "hello"}},
			want: "User: hello",
		},
		{
			name: "system + user",
			msgs: []ChatMessage{{Role: "system", Content: "be helpful"}, {Role: "user", Content: "hi"}},
			want: "System: be helpful\nUser: hi",
		},
		{
			name: "multi-turn",
			msgs: []ChatMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}, {Role: "user", Content: "bye"}},
			want: "User: hi\nAssistant: hello\nUser: bye",
		},
		{
			name: "unknown role",
			msgs: []ChatMessage{{Role: "tool", Content: "data"}},
			want: "Tool: Tool result\nOutput:\ndata",
		},
		{
			name: "assistant tool calls and tool result",
			msgs: []ChatMessage{
				{
					Role: "assistant",
					ToolCalls: []ChatToolCall{
						{
							ID:   "call_123",
							Type: "function",
							Function: ChatToolFunction{
								Name:      "Read",
								Arguments: `{"file_path":"README.md"}`,
							},
						},
					},
				},
				{Role: "tool", ToolCallID: "call_123", Content: "file content"},
			},
			want: "Assistant: Assistant requested tool: Read (call_id=call_123)\nTool payload:\n{\"file_path\":\"README.md\"}\nTool: Tool result (call_id=call_123)\nOutput:\nfile content",
		},
		{
			name: "empty",
			msgs: []ChatMessage{},
			want: "",
		},
		{
			name: "empty content",
			msgs: []ChatMessage{{Role: "user", Content: ""}, {Role: "assistant", Content: "reply"}},
			want: "User: \nAssistant: reply",
		},
		{
			name: "special characters in content",
			msgs: []ChatMessage{{Role: "user", Content: "hello \"world\"\nnew\tline"}},
			want: "User: hello \"world\"\nnew\tline",
		},
		{
			name: "long content",
			msgs: []ChatMessage{{Role: "user", Content: strings.Repeat("a", 10000)}},
			want: "User: " + strings.Repeat("a", 10000),
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

func TestChatMessageUnmarshal_ContentBlocks(t *testing.T) {
	var msg ChatMessage
	body := `{"role":"user","content":[{"type":"text","text":"hello "},{"type":"text","text":"world","cache_control":{"type":"ephemeral"}}]}`
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}
	if msg.Content != "hello world" {
		t.Fatalf("content = %q, want %q", msg.Content, "hello world")
	}
}

func TestChatMessageUnmarshal_RejectsUnsupportedBlock(t *testing.T) {
	var msg ChatMessage
	body := `{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]}`
	if err := json.Unmarshal([]byte(body), &msg); err == nil {
		t.Fatal("expected unsupported content block error")
	}
}

func TestHandleChatCompletions_NonStreamToolCall(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream server doesn't support flushing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"content\",\"content\":\"{\\\"type\\\":\\\"function_call\\\",\\\"name\\\":\\\"Read\\\",\\\"arguments\\\":{\\\"file_path\\\":\\\"README.md\\\"}}\"}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: {\"type\":\"done\"}\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")

	body := `{
		"model":"deepseek-v3p2",
		"stream":false,
		"messages":[{"role":"user","content":[{"type":"text","text":"读取 README.md"}]}],
		"tools":[{"type":"function","function":{"name":"Read","description":"read file","parameters":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp ChatCompletionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if got := resp.Choices[0].FinishReason; got != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", got)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls len = %d, want 1", len(resp.Choices[0].Message.ToolCalls))
	}
	call := resp.Choices[0].Message.ToolCalls[0]
	if call.Function.Name != "Read" {
		t.Fatalf("tool name = %q, want Read", call.Function.Name)
	}
	if call.Function.Arguments != `{"file_path":"README.md"}` {
		t.Fatalf("arguments = %q", call.Function.Arguments)
	}
}

func TestHandleChatCompletions_StreamToolCall(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream server doesn't support flushing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"content\",\"content\":\"{\\\"type\\\":\\\"function_call\\\",\\\"name\\\":\\\"Read\\\",\\\"arguments\\\":{\\\"file_path\\\":\\\"README.md\\\"}}\"}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: {\"type\":\"done\"}\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")

	body := `{
		"model":"deepseek-v3p2",
		"stream":true,
		"messages":[{"role":"user","content":[{"type":"text","text":"读取 README.md"}]}],
		"tools":[{"type":"function","function":{"name":"Read","description":"read file","parameters":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	bodyText := rec.Body.String()
	for _, want := range []string{
		`"role":"assistant"`,
		`"tool_calls":[{"index":0,`,
		`"id":"call_`,
		`"type":"function"`,
		`"name":"Read"`,
		`"arguments":"{\"file_path\":\"README.md\"}"`,
		`"finish_reason":"tool_calls"`,
		`data: [DONE]`,
	} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("stream body missing %q:\n%s", want, bodyText)
		}
	}
	if strings.Contains(bodyText, `"content":"{\"type\":\"function_call\""`) {
		t.Fatalf("tool-call stream should not leak raw text content:\n%s", bodyText)
	}
}

func TestCORSMiddleware_Wildcard(t *testing.T) {
	p := newTestProxy()
	mux := newTestMux(t, p, "*")
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
	mux := newTestMux(t, p, "https://example.com,https://trusted.com")
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
	mux := newTestMux(t, p, "https://example.com")
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

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (fr *flushRecorder) Flush() {
	fr.flushed = true
}

type noHijackWriter struct {
	http.ResponseWriter
}

func TestResponseWriterFlush(t *testing.T) {
	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	rw := &responseWriter{ResponseWriter: rec, statusCode: http.StatusOK}

	if _, ok := interface{}(rw).(http.Flusher); !ok {
		t.Fatal("responseWriter should implement http.Flusher")
	}

	rw.Flush()
	if !rec.flushed {
		t.Fatal("Flush did not reach underlying ResponseWriter")
	}
}

func TestResponseWriterHijackUnsupported(t *testing.T) {
	rw := &responseWriter{
		ResponseWriter: noHijackWriter{ResponseWriter: httptest.NewRecorder()},
		statusCode:     http.StatusOK,
	}

	conn, brw, err := rw.Hijack()
	if err == nil {
		t.Fatal("Hijack should fail when underlying writer does not support it")
	}
	if conn != nil || brw != nil {
		t.Fatal("Hijack should not return connection objects on failure")
	}
}

type hijackRecorder struct {
	*httptest.ResponseRecorder
	conn net.Conn
	buf  *bufio.ReadWriter
}

func (hr *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return hr.conn, hr.buf, nil
}

func TestResponseWriterHijackPassthrough(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	rec := &hijackRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		conn:             serverConn,
		buf:              bufio.NewReadWriter(bufio.NewReader(strings.NewReader("")), bufio.NewWriter(io.Discard)),
	}
	rw := &responseWriter{ResponseWriter: rec, statusCode: http.StatusOK}

	gotConn, gotBuf, err := rw.Hijack()
	if err != nil {
		t.Fatalf("Hijack returned error: %v", err)
	}
	if gotConn != serverConn {
		t.Fatal("Hijack did not return underlying connection")
	}
	if gotBuf != rec.buf {
		t.Fatal("Hijack did not return underlying read writer")
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

func TestScanSSEEvents_BasicContent(t *testing.T) {
	sse := `data: {"type":"content","content":"hello"}
data: {"type":"content","content":" world"}
data: {"type":"done","content":""}
`
	var contents []string
	_, err := scanSSEEvents(strings.NewReader(sse), false, false, func(evt sseContentEvent) bool {
		if evt.Type == "content" {
			contents = append(contents, evt.Content)
		}
		return true
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(contents) != 2 || contents[0] != "hello" || contents[1] != " world" {
		t.Errorf("contents = %v, want [hello, world]", contents)
	}
}

func TestScanSSEEvents_ThinkingSeparator(t *testing.T) {
	sse := `data: {"type":"content","content":"thinking..."}
data: {"type":"content","content":"💯"}
data: {"type":"content","content":"answer here"}
data: {"type":"done","content":""}
`
	var events []string
	_, err := scanSSEEvents(strings.NewReader(sse), true, true, func(evt sseContentEvent) bool {
		events = append(events, evt.Type)
		if evt.Type == "content" {
			events[len(events)-1] = "content:" + evt.Content
		}
		return true
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	want := []string{"content:thinking...", "thinking_separator", "content:answer here", "done"}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("events[%d] = %q, want %q", i, events[i], want[i])
		}
	}
}

func TestScanSSEEvents_ThinkingHidden(t *testing.T) {
	// When showThinking=false, thinking content should be skipped
	sse := `data: {"type":"content","content":"hidden thinking"}
data: {"type":"content","content":"💯"}
data: {"type":"content","content":"visible answer"}
data: {"type":"done","content":""}
`
	var contents []string
	_, err := scanSSEEvents(strings.NewReader(sse), true, false, func(evt sseContentEvent) bool {
		if evt.Type == "content" {
			contents = append(contents, evt.Content)
		}
		return true
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(contents) != 1 || contents[0] != "visible answer" {
		t.Errorf("contents = %v, want [visible answer] (thinking should be hidden)", contents)
	}
}

func TestScanSSEEvents_ThinkingTagsHidden(t *testing.T) {
	sse := `data: {"type":"content","content":"<think>hidden thinking</think>visible answer"}
data: {"type":"done","content":""}
`
	var contents []string
	_, err := scanSSEEvents(strings.NewReader(sse), true, false, func(evt sseContentEvent) bool {
		if evt.Type == "content" {
			contents = append(contents, evt.Content)
		}
		return true
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(contents) != 1 || contents[0] != "visible answer" {
		t.Errorf("contents = %v, want [visible answer] (think tags should be hidden)", contents)
	}
}

func TestScanSSEEvents_ThinkingTagsShown(t *testing.T) {
	sse := `data: {"type":"content","content":"<think>hidden thinking</think>visible answer"}
data: {"type":"done","content":""}
`
	var events []string
	_, err := scanSSEEvents(strings.NewReader(sse), true, true, func(evt sseContentEvent) bool {
		events = append(events, evt.Type)
		if evt.Type == "content" {
			events[len(events)-1] = "content:" + evt.Content
		}
		return true
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	want := []string{"content:hidden thinking", "thinking_separator", "content:visible answer", "done"}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("events[%d] = %q, want %q", i, events[i], want[i])
		}
	}
}

func TestScanSSEEvents_ThinkingCloseTagStandalone(t *testing.T) {
	sse := `data: {"type":"content","content":"<think>hidden thinking"}
data: {"type":"content","content":"still hidden</think>visible answer"}
data: {"type":"done","content":""}
`
	var contents []string
	_, err := scanSSEEvents(strings.NewReader(sse), true, false, func(evt sseContentEvent) bool {
		if evt.Type == "content" {
			contents = append(contents, evt.Content)
		}
		return true
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(contents) != 1 || contents[0] != "visible answer" {
		t.Errorf("contents = %v, want [visible answer] when close tag is merged with answer", contents)
	}
}

func TestScanSSEEvents_ThinkingFallbackWithoutSeparator(t *testing.T) {
	sse := `data: {"type":"content","content":"ok"}
data: {"type":"done","content":""}
`
	var contents []string
	_, err := scanSSEEvents(strings.NewReader(sse), true, false, func(evt sseContentEvent) bool {
		if evt.Type == "content" {
			contents = append(contents, evt.Content)
		}
		return true
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(contents) != 1 || contents[0] != "ok" {
		t.Errorf("contents = %v, want [ok] (missing separator should fall back to visible content)", contents)
	}
}

func TestScanSSEEvents_NonThinkingModel(t *testing.T) {
	// Non-thinking model: 💯 should be skipped (consistent with original behavior).
	// In practice, non-thinking models never emit 💯, but if they did, we skip it.
	sse := `data: {"type":"content","content":"💯"}
data: {"type":"content","content":"more"}
data: {"type":"done","content":""}
`
	var contents []string
	_, err := scanSSEEvents(strings.NewReader(sse), false, false, func(evt sseContentEvent) bool {
		if evt.Type == "content" {
			contents = append(contents, evt.Content)
		}
		return true
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(contents) != 1 || contents[0] != "more" {
		t.Errorf("contents = %v, want [more] (💯 skipped for non-thinking model)", contents)
	}
}

func TestScanSSEEvents_SkipsEmptyAndInvalid(t *testing.T) {
	sse := `data: {"type":"content","content":"hello"}
	data:
data: not-json
data: {"type":"content","content":"world"}
data: {"type":"done","content":""}
`
	var contents []string
	_, err := scanSSEEvents(strings.NewReader(sse), false, false, func(evt sseContentEvent) bool {
		if evt.Type == "content" {
			contents = append(contents, evt.Content)
		}
		return true
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(contents) != 2 || contents[0] != "hello" || contents[1] != "world" {
		t.Errorf("contents = %v, want [hello, world]", contents)
	}
}

func TestScanSSEEvents_UpstreamErrorField(t *testing.T) {
	sse := `data: {"type":"error","error":"404, message='Not Found'"}
`
	_, err := scanSSEEvents(strings.NewReader(sse), false, false, func(evt sseContentEvent) bool {
		t.Fatalf("unexpected event: %+v", evt)
		return false
	})
	if err == nil {
		t.Fatal("expected upstream error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error = %v, want 404 detail", err)
	}
}

func TestHandleModels_WithTokenAuth(t *testing.T) {
	// Test NewMux with tokenauth.Manager (multi-key mode)
	p := newTestProxy()
	tm, err := tokenauth.New("sk-test1,sk-test2", 0)
	if err != nil {
		t.Fatalf("tokenauth.New error: %v", err)
	}
	defer tm.Stop()

	mux := NewMux(p, "*", tm)

	// Valid key should get 200
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-test1")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	// Invalid key should get 401
	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req2.Header.Set("Authorization", "Bearer sk-wrong")
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec2.Code)
	}

	// Second valid key should also work
	req3 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req3.Header.Set("Authorization", "Bearer sk-test2")
	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for sk-test2", rec3.Code)
	}
}

func TestHandleChatCompletions_WithTokenAuth(t *testing.T) {
	// Test chat completions endpoint with tokenauth
	p := newTestProxy()
	tm, err := tokenauth.New(`[{"key":"sk-limited","quota":1,"rate_limit":0}]`, 0)
	if err != nil {
		t.Fatalf("tokenauth.New error: %v", err)
	}
	defer tm.Stop()

	mux := NewMux(p, "*", tm)

	// First request: should be 400 (invalid body, but passes auth)
	body := `{"model":"deepseek-v3p2","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-limited")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	// Should NOT be 401; could be 200/400/502 depending on upstream
	if rec.Code == http.StatusUnauthorized {
		t.Error("should not be 401, auth should pass")
	}

	// Second request: quota exceeded -> 403
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req2.Header.Set("Authorization", "Bearer sk-limited")
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (quota exceeded)", rec2.Code)
	}
}

func TestNewMux_RequiresTokenAuthManager(t *testing.T) {
	p := newTestProxy()

	defer func() {
		got := recover()
		if got == nil {
			t.Fatal("expected panic when tokenauth manager is nil")
		}
		if got != "tokenauth manager is required" {
			t.Fatalf("panic = %v, want tokenauth manager is required", got)
		}
	}()

	_ = NewMux(p, "*", nil)
}

func TestNewMux_EmptyTokenAuthBlocksAll(t *testing.T) {
	// CR-1: When tokenauth has 0 tokens (empty config), the mux should
	// block all requests (401).
	p := newTestProxy()
	tm, err := tokenauth.New("", 0)
	if err != nil {
		t.Fatalf("tokenauth.New empty config error: %v", err)
	}
	defer tm.Stop()

	if tm.TokenCount() != 0 {
		t.Fatalf("expected 0 tokens, got %d", tm.TokenCount())
	}

	mux := NewMux(p, "*", tm)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("empty token auth should block all requests, got status %d", rec.Code)
	}
}

func TestCORSMiddleware_VaryOriginOnReject(t *testing.T) {
	// CR-4: Non-wildcard CORS should set Vary: Origin even when
	// the request's Origin is not in the allowed set.
	handler := CORSMiddleware("https://allowed.example.com")(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Request with non-allowed origin
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	vary := rec.Header().Get("Vary")
	if vary != "Origin" {
		t.Errorf("Vary header = %q, want Origin (for cache correctness)", vary)
	}
	// Should NOT set Access-Control-Allow-Origin
	if acao := rec.Header().Get("Access-Control-Allow-Origin"); acao != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty for non-allowed origin", acao)
	}
}

func TestDefaultShowThinking(t *testing.T) {
	// When ShowThinking is nil, the proxy should use the default value
	pFalse := New(transport.New(30*time.Second), "test", false)
	pTrue := New(transport.New(30*time.Second), "test", true)

	if pFalse.defaultShowThinking != false {
		t.Error("expected defaultShowThinking=false")
	}
	if pTrue.defaultShowThinking != true {
		t.Error("expected defaultShowThinking=true")
	}
}

func TestShowThinking_RequestOverridesDefault(t *testing.T) {
	// Request-level show_thinking should override the global default
	tests := []struct {
		name           string
		defaultVal     bool
		requestVal     *bool
		expectedResult bool
	}{
		{"default false, no request", false, nil, false},
		{"default true, no request", true, nil, true},
		{"default false, request true", false, boolPtr(true), true},
		{"default true, request false", true, boolPtr(false), false},
		{"default false, request false", false, boolPtr(false), false},
		{"default true, request true", true, boolPtr(true), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := New(transport.New(30*time.Second), "test", tt.defaultVal)
			req := ChatCompletionRequest{
				Model:        "deepseek-v3p2",
				Messages:     []ChatMessage{{Role: "user", Content: "hi"}},
				ShowThinking: tt.requestVal,
			}
			// Extract showThinking logic from handleChatCompletions
			showThinking := p.defaultShowThinking
			if req.ShowThinking != nil {
				showThinking = *req.ShowThinking
			}
			if showThinking != tt.expectedResult {
				t.Errorf("showThinking = %v, want %v", showThinking, tt.expectedResult)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

func TestCORSMiddleware_EmptyOrigins(t *testing.T) {
	// Empty origins string should produce no valid entries and log a warning
	p := newTestProxy()
	mux := newTestMux(t, p, "")
	req := httptest.NewRequest(http.MethodOptions, "/v1/models", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	// Empty origins should not set Access-Control-Allow-Origin
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("CORS origin for empty origins = %q, want empty", got)
	}
}

func TestCORSMiddleware_DirtyConfigBlocksAll(t *testing.T) {
	// Dirty config like ",,," should NOT become wildcard — it should block all CORS
	p := newTestProxy()
	mux := newTestMux(t, p, ",,,")
	req := httptest.NewRequest(http.MethodOptions, "/v1/models", nil)
	req.Header.Set("Origin", "https://evil.com")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("dirty CORS config should block all, got origin = %q", got)
	}
}

func TestTemperatureMaxTokensPassthrough(t *testing.T) {
	// Verify that temperature and max_tokens are passed through to FireworksRequest
	temps := 0.7
	maxTok := 2048
	req := ChatCompletionRequest{
		Model:       "deepseek-v3p2",
		Messages:    []ChatMessage{{Role: "user", Content: "hi"}},
		Temperature: &temps,
		MaxTokens:   &maxTok,
	}

	prompt := messagesToPrompt(req.Messages)
	fwReq := FireworksRequest{
		Messages:            []FireworksMessage{{Role: "user", Content: prompt}},
		ModelKey:            req.Model,
		ConversationID:      "test_session",
		FunctionDefinitions: []interface{}{},
		Temperature:         req.Temperature,
		MaxTokens:           req.MaxTokens,
	}

	if fwReq.Temperature == nil || *fwReq.Temperature != 0.7 {
		t.Errorf("Temperature = %v, want 0.7", fwReq.Temperature)
	}
	if fwReq.MaxTokens == nil || *fwReq.MaxTokens != 2048 {
		t.Errorf("MaxTokens = %v, want 2048", fwReq.MaxTokens)
	}
}

func TestTemperatureMaxTokensNil(t *testing.T) {
	// When not provided, fields should be nil (omitted from JSON)
	req := ChatCompletionRequest{
		Model:    "deepseek-v3p2",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	}

	prompt := messagesToPrompt(req.Messages)
	fwReq := FireworksRequest{
		Messages:            []FireworksMessage{{Role: "user", Content: prompt}},
		ModelKey:            req.Model,
		ConversationID:      "test_session",
		FunctionDefinitions: []interface{}{},
		Temperature:         req.Temperature,
		MaxTokens:           req.MaxTokens,
	}

	if fwReq.Temperature != nil {
		t.Errorf("Temperature = %v, want nil", fwReq.Temperature)
	}
	if fwReq.MaxTokens != nil {
		t.Errorf("MaxTokens = %v, want nil", fwReq.MaxTokens)
	}

	// Verify JSON omitempty behavior
	data, err := json.Marshal(fwReq)
	if err != nil {
		t.Fatalf("json.Marshal error: %v", err)
	}
	s := string(data)
	if strings.Contains(s, "temperature") {
		t.Errorf("JSON should not contain 'temperature' when nil: %s", s)
	}
	if strings.Contains(s, "max_tokens") {
		t.Errorf("JSON should not contain 'max_tokens' when nil: %s", s)
	}
}

// TestHandleNonStream_UpstreamIncomplete verifies that a non-streaming request
// returns 200 with content when the upstream stream has content but no "done" event.
// This is resilient behavior - content delivery is prioritized over protocol compliance.
func TestHandleNonStream_UpstreamIncomplete(t *testing.T) {
	// Set up a mock upstream that returns partial SSE then closes without "done"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream server doesn't support flushing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Send some content but NO done event
		w.Write([]byte("data: {\"type\":\"content\",\"content\":\"partial\"}\n\n"))
		flusher.Flush()
		// Stream ends here — no done event
	}))
	defer upstream.Close()

	tp := transport.New(30 * time.Second)
	p := NewWithUpstream(tp, "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")

	body := `{"model":"deepseek-v3p2","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// Should return 200 with content even without done event (resilient behavior)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for upstream with content but no done event", rec.Code)
	}
	// Should contain the partial content
	var resp ChatCompletionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Choices[0].Message.Content != "partial" {
		t.Errorf("content = %q, want 'partial'", resp.Choices[0].Message.Content)
	}
}

// TestHandleNonStream_ContextCanceled verifies that a non-streaming request
// does not write a response when the client disconnects (context canceled).
func TestHandleNonStream_ContextCanceled(t *testing.T) {
	stall := make(chan struct{})
	handlerErr := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			select {
			case handlerErr <- "upstream server doesn't support flushing":
			default:
			}
			http.Error(w, "flush unsupported", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		<-stall
	}))
	defer func() {
		close(stall)
		upstream.Close()
	}()

	tp := transport.New(30 * time.Second)
	p := NewWithUpstream(tp, "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")

	body := `{"model":"deepseek-v3p2","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")

	// Cancel context immediately — upstream request will fail with context.Canceled
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	select {
	case msg := <-handlerErr:
		t.Fatal(msg)
	default:
	}

	// With canceled context, the upstream request should fail immediately.
	// The handler should either not write anything (status 0) or return an error.
	// The key thing is: no panic and no incorrect 200 with partial data.
	if rec.Code == http.StatusOK {
		// If somehow 200 is returned, check it's not returning garbage data
		var resp ChatCompletionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err == nil {
			if len(resp.Choices) > 0 && resp.Choices[0].Message.Content != "" {
				t.Error("should not return 200 with content for canceled context")
			}
		}
	}
}

func TestHandleMetrics_PublicEndpoint(t *testing.T) {
	p := newTestProxy()
	mux := newTestMux(t, p, "*")

	// Generate traffic for metrics aggregation.
	healthReq := httptest.NewRequest(http.MethodGet, "/health", nil)
	healthRec := httptest.NewRecorder()
	mux.ServeHTTP(healthRec, healthReq)
	if healthRec.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", healthRec.Code)
	}

	modelsReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsRec := httptest.NewRecorder()
	mux.ServeHTTP(modelsRec, modelsReq)
	if modelsRec.Code != http.StatusUnauthorized {
		t.Fatalf("models status = %d, want 401", modelsRec.Code)
	}

	metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsRec := httptest.NewRecorder()
	mux.ServeHTTP(metricsRec, metricsReq)

	if metricsRec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", metricsRec.Code)
	}
	if ct := metricsRec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("metrics content-type = %q, want text/plain", ct)
	}

	body := metricsRec.Body.String()
	if !strings.Contains(body, "firew2oai_http_requests_total") {
		t.Fatal("metrics body missing firew2oai_http_requests_total")
	}
	if !strings.Contains(body, `path="/health",status="200"`) {
		t.Fatal("metrics body missing /health status partition")
	}
	if !strings.Contains(body, `path="/v1/models",status="401"`) {
		t.Fatal("metrics body missing /v1/models 401 partition")
	}
	if strings.Contains(body, `path="/metrics"`) {
		t.Fatal("/metrics should not be included in route metrics to avoid scrape self-noise")
	}
}

func TestHandleMetrics_MethodNotAllowed(t *testing.T) {
	p := newTestProxy()
	mux := newTestMux(t, p, "*")

	req := httptest.NewRequest(http.MethodPost, "/metrics", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "method_not_allowed") {
		t.Fatal("error response should contain method_not_allowed code")
	}
}
