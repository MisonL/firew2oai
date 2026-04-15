package proxy

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mison/firew2oai/internal/config"
	"github.com/mison/firew2oai/internal/tokenauth"
	"github.com/mison/firew2oai/internal/transport"
)

const (
	upstreamURL = "https://chat.fireworks.ai/chat/single"

	// thinkingSeparator is the emoji that Fireworks thinking models emit
	// between the thinking block and the actual response.
	thinkingSeparator = "\U0001f4af" // 💯
)

// ─── SSE Event Types ──────────────────────────────────────────────────────

// SSEEvent represents a parsed Fireworks SSE event.
type SSEEvent struct {
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
}

// ─── OpenAI Request / Response Types ──────────────────────────────────────

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatCompletionRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Stream      bool          `json:"stream,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	// Custom extension: show thinking process for thinking models
	ShowThinking *bool `json:"show_thinking,omitempty"`
}

type ChatCompletionChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ChatCompletionResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []ChatCompletionChoice `json:"choices"`
	Usage   Usage                  `json:"usage"`
}

type StreamDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type StreamChoice struct {
	Index        int         `json:"index"`
	Delta        StreamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type StreamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
}

type ModelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type ModelListResponse struct {
	Object string        `json:"object"`
	Data   []ModelObject `json:"data"`
}

// ─── Fireworks Request Format ─────────────────────────────────────────────

type FireworksMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type FireworksRequest struct {
	Messages           []FireworksMessage `json:"messages"`
	ModelKey           string             `json:"model_key"`
	ConversationID     string             `json:"conversation_id"`
	FunctionDefinitions []interface{}      `json:"function_definitions"`
}

// ─── Proxy ────────────────────────────────────────────────────────────────

// Proxy handles OpenAI-to-Fireworks protocol conversion.
type Proxy struct {
	transport *transport.FireworksTransport
	timeout   time.Duration
	version   string
}

// New creates a new Proxy instance.
func New(transport *transport.FireworksTransport, timeout time.Duration, version string) *Proxy {
	return &Proxy{
		transport: transport,
		timeout:   timeout,
		version:   version,
	}
}

// ─── Core Logic ───────────────────────────────────────────────────────────

// messagesToPrompt converts OpenAI multi-turn messages into a single prompt
// string since Fireworks chat/single processes a flat message list.
//
// We preserve the conversational structure by prepending role labels
// so the model can still understand context.
func messagesToPrompt(messages []ChatMessage) string {
	var parts []string
	for _, msg := range messages {
		switch msg.Role {
		case "system":
			parts = append(parts, "System: "+msg.Content)
		case "user":
			parts = append(parts, "User: "+msg.Content)
		case "assistant":
			parts = append(parts, "Assistant: "+msg.Content)
		default:
			parts = append(parts, msg.Content)
		}
	}
	return strings.Join(parts, "\n")
}

// generateRequestID creates an OpenAI-style chatcmpl- request ID.
func generateRequestID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// Extremely unlikely with crypto/rand, but log and use timestamp fallback
		slog.Error("crypto/rand.Read failed, using timestamp fallback", "error", err)
		b = []byte(fmt.Sprintf("%d", time.Now().UnixNano()))
	}
	return fmt.Sprintf("chatcmpl-%x", b)
}

// ─── Route Handlers ───────────────────────────────────────────────────────

// handleRoot returns service info and available endpoints.
func (p *Proxy) handleRoot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message":   "firew2oai - Fireworks to OpenAI API Proxy",
		"version":   p.version,
		"endpoints": map[string]string{"models": "GET /v1/models", "chat": "POST /v1/chat/completions", "health": "GET /health"},
	})
}

// handleHealth returns a simple health check response.
func (p *Proxy) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
	})
}

// handleModels returns the list of available models in OpenAI format.
func (p *Proxy) handleModels(w http.ResponseWriter, r *http.Request) {
	models := make([]ModelObject, len(config.AvailableModels))
	now := time.Now().Unix()
	for i, m := range config.AvailableModels {
		models[i] = ModelObject{
			ID:      m,
			Object:  "model",
			Created: now,
			OwnedBy: "fireworks-ai",
		}
	}

	writeJSON(w, http.StatusOK, ModelListResponse{
		Object: "list",
		Data:   models,
	})
}

// handleChatCompletions handles both streaming and non-streaming chat requests.
func (p *Proxy) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	// Only accept POST
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "method not allowed, use POST")
		return
	}

	var req ChatCompletionRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20) // 4MB max
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_body", "invalid request body: %v", err)
		return
	}

	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "empty_messages", "messages array is required and must not be empty")
		return
	}

	if !config.ValidModel(req.Model) {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model_not_found", "model %q is not supported. Use /v1/models to list available models", req.Model)
		return
	}

	requestID := generateRequestID()
	showThinking := req.ShowThinking != nil && *req.ShowThinking
	prompt := messagesToPrompt(req.Messages)

	slog.Info("chat completion request",
		"request_id", requestID,
		"model", req.Model,
		"stream", req.Stream,
		"messages", len(req.Messages),
		"thinking", showThinking,
	)

	// Build Fireworks request body
	fwReq := FireworksRequest{
		Messages: []FireworksMessage{
			{Role: "user", Content: prompt},
		},
		ModelKey:           req.Model,
		ConversationID:     fmt.Sprintf("session_%d_%d", time.Now().UnixMilli(), time.Now().UnixNano()%10000),
		FunctionDefinitions: []interface{}{},
	}

	bodyBytes, err := json.Marshal(fwReq)
	if err != nil {
		slog.Error("failed to marshal fireworks request", "error", err)
		writeError(w, http.StatusInternalServerError, "server_error", "marshal_failed", "failed to build upstream request")
		return
	}

	if req.Stream {
		p.handleStream(w, r, requestID, req.Model, bodyBytes, showThinking)
	} else {
		p.handleNonStream(w, r, requestID, req.Model, bodyBytes, showThinking)
	}
}

// ─── Streaming Handler ────────────────────────────────────────────────────

// handleStream converts Fireworks SSE events to OpenAI streaming format.
func (p *Proxy) handleStream(w http.ResponseWriter, r *http.Request, requestID, model string, body []byte, showThinking bool) {
	ctx := r.Context()

	reader, err := p.transport.StreamPost(ctx, upstreamURL, bytes.NewReader(body))
	if err != nil {
		slog.Error("upstream stream error", "request_id", requestID, "error", err)
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream_failed", "upstream error: %v", err)
		return
	}
	defer reader.Close()

	// SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, canFlush := w.(http.Flusher)
	isThinking := config.IsThinkingModel(model)
	inThinking := isThinking // thinking models start in thinking phase

	var mu sync.Mutex
	writeAndFlush := func(data []byte) {
		mu.Lock()
		defer mu.Unlock()
		w.Write(data)
		if canFlush {
			flusher.Flush()
		}
	}

	// Send initial role chunk (OpenAI spec: first chunk contains the role)
	roleChunk := StreamChunk{
		ID:      requestID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []StreamChoice{
			{Index: 0, Delta: StreamDelta{Role: "assistant"}, FinishReason: nil},
		},
	}
	writeAndFlush(sseChunk(roleChunk))

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		jsonStr := strings.TrimSpace(line[5:])
		if jsonStr == "" {
			continue
		}

		var evt SSEEvent
		if err := json.Unmarshal([]byte(jsonStr), &evt); err != nil {
			slog.Debug("failed to parse SSE event", "raw", jsonStr, "error", err)
			continue
		}

		if evt.Type == "done" {
			stop := "stop"
			chunk := StreamChunk{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   model,
				Choices: []StreamChoice{
					{Index: 0, Delta: StreamDelta{}, FinishReason: &stop},
				},
			}
			writeAndFlush(sseChunk(chunk))
			writeAndFlush([]byte("data: [DONE]\n\n"))
			slog.Debug("stream completed", "request_id", requestID)
			break
		}

		content := evt.Content
		if content == "" {
			continue
		}

		// Handle thinking separator 💯
		if content == thinkingSeparator {
			if isThinking {
				inThinking = false
				if showThinking {
					chunk := StreamChunk{
						ID:      requestID,
						Object:  "chat.completion.chunk",
						Created: time.Now().Unix(),
						Model:   model,
						Choices: []StreamChoice{
							{Index: 0, Delta: StreamDelta{Content: "\n\n--- Answer ---\n\n"}, FinishReason: nil},
						},
					}
					writeAndFlush(sseChunk(chunk))
				}
			}
			continue
		}

		// Skip thinking content if not showing
		if isThinking && inThinking && !showThinking {
			continue
		}

		chunk := StreamChunk{
			ID:      requestID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   model,
			Choices: []StreamChoice{
				{Index: 0, Delta: StreamDelta{Content: content}, FinishReason: nil},
			},
		}
		writeAndFlush(sseChunk(chunk))
	}

	if err := scanner.Err(); err != nil {
		slog.Error("stream read error", "request_id", requestID, "error", err)
	}
}

// ─── Non-Streaming Handler ────────────────────────────────────────────────

// handleNonStream collects the full response from Fireworks and returns it
// in OpenAI non-streaming format.
func (p *Proxy) handleNonStream(w http.ResponseWriter, r *http.Request, requestID, model string, body []byte, showThinking bool) {
	ctx := r.Context()

	reader, err := p.transport.StreamPost(ctx, upstreamURL, bytes.NewReader(body))
	if err != nil {
		slog.Error("upstream non-stream error", "request_id", requestID, "error", err)
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream_failed", "upstream error: %v", err)
		return
	}
	defer reader.Close()

	var result strings.Builder
	isThinking := config.IsThinkingModel(model)
	inThinking := isThinking

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		jsonStr := strings.TrimSpace(line[5:])
		if jsonStr == "" {
			continue
		}

		var evt SSEEvent
		if err := json.Unmarshal([]byte(jsonStr), &evt); err != nil {
			continue
		}

		if evt.Type == "done" {
			break
		}

		content := evt.Content
		if content == "" {
			continue
		}

		if content == thinkingSeparator {
			if isThinking {
				inThinking = false
				if showThinking {
					result.WriteString("\n\n--- Answer ---\n\n")
				}
			}
			continue
		}

		if isThinking && inThinking && !showThinking {
			continue
		}

		result.WriteString(content)
	}

	// On scanner error, return whatever content we've accumulated so far
	// rather than discarding it. This prevents data loss on partial reads.
	if err := scanner.Err(); err != nil {
		slog.Error("stream read error (returning partial result)", "request_id", requestID, "error", err)
	}

	content := result.String()

	resp := ChatCompletionResponse{
		ID:      requestID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []ChatCompletionChoice{
			{
				Index:        0,
				Message:      ChatMessage{Role: "assistant", Content: content},
				FinishReason: "stop",
			},
		},
		// Fireworks does not return token usage; return zeros to avoid misleading clients.
		Usage: Usage{},
	}

	writeJSON(w, http.StatusOK, resp)
}

// ─── JSON / SSE Helpers ──────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		slog.Error("json.Marshal failed", "error", err)
		http.Error(w, `{"error":{"message":"internal JSON error","type":"server_error","code":"marshal_error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(data)
	w.Write([]byte("\n"))
}

func sseChunk(v interface{}) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		slog.Error("json.Marshal failed for SSE chunk", "error", err)
		return []byte("data: {}\n\n")
	}
	return []byte(fmt.Sprintf("data: %s\n\n", data))
}

func writeError(w http.ResponseWriter, status int, errType string, errCode string, format string, args ...interface{}) {
	data, err := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{
			"message": fmt.Sprintf(format, args...),
			"type":    errType,
			"code":    errCode,
		},
	})
	if err != nil {
		slog.Error("json.Marshal failed for error response", "error", err)
		http.Error(w, `{"error":{"message":"internal error","type":"server_error","code":"internal_error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(data)
	w.Write([]byte("\n"))
}

// ─── Middleware: Auth ─────────────────────────────────────────────────────

// AuthMiddleware validates the Bearer token using constant-time comparison.
// Returns a middleware function compatible with the chain() helper.
//
// Deprecated: Use tokenauth.Manager.Middleware() instead for multi-key support
// with per-key quota and rate limiting. This is kept for backward compatibility.
func AuthMiddleware(apiKey string) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if auth == "" {
				writeError(w, http.StatusUnauthorized, "authentication_error", "missing_api_key", "missing Authorization header")
				return
			}
			if !strings.HasPrefix(auth, "Bearer ") {
				writeError(w, http.StatusUnauthorized, "authentication_error", "invalid_auth_format", "invalid Authorization format, expected 'Bearer <key>'")
				return
			}
			token := auth[7:]
			if subtle.ConstantTimeCompare([]byte(token), []byte(apiKey)) != 1 {
				writeError(w, http.StatusUnauthorized, "authentication_error", "invalid_api_key", "invalid API key")
				return
			}
			next(w, r)
		}
	}
}

// ─── Middleware: Logging ─────────────────────────────────────────────────

// LoggingMiddleware logs incoming requests with structured output.
func LoggingMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next(rw, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.statusCode,
			"duration", time.Since(start).String(),
			"remote", r.RemoteAddr,
		)
	}
}

// responseWriter wraps http.ResponseWriter to capture status codes while
// preserving Flusher and Hijacker interfaces for SSE streaming.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Unwrap returns the underlying ResponseWriter.
// This enables middleware chaining that checks for optional interfaces (Flusher, Hijacker).
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// ─── Middleware: CORS ─────────────────────────────────────────────────────

// CORSMiddleware adds CORS headers for cross-origin API access.
// origins is a comma-separated list; "*" allows all origins.
func CORSMiddleware(origins string) func(http.HandlerFunc) http.HandlerFunc {
	allowedSet := parseOrigins(origins)
	isWildcard := origins == "*" || len(allowedSet) == 0

	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			if isWildcard {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if origin != "" && allowedSet[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
			}

			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "86400")

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next(w, r)
		}
	}
}

func parseOrigins(origins string) map[string]bool {
	m := make(map[string]bool)
	for _, o := range strings.Split(origins, ",") {
		o = strings.TrimSpace(o)
		if o != "" {
			m[o] = true
		}
	}
	return m
}

// ─── Middleware: Recovery ─────────────────────────────────────────────────

// RecoveryMiddleware catches panics and returns 500 instead of crashing.
func RecoveryMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic recovered",
					"path", r.URL.Path,
					"error", fmt.Sprintf("%v", rec),
				)
				writeError(w, http.StatusInternalServerError, "server_error", "internal_error", "internal server error")
			}
		}()
		next(w, r)
	}
}

// ─── Middleware Chain ─────────────────────────────────────────────────────

// chain applies middlewares from outermost to innermost.
func chain(handlers ...func(http.HandlerFunc) http.HandlerFunc) func(http.HandlerFunc) http.HandlerFunc {
	return func(final http.HandlerFunc) http.HandlerFunc {
		for i := len(handlers) - 1; i >= 0; i-- {
			final = handlers[i](final)
		}
		return final
	}
}

// ─── Router ───────────────────────────────────────────────────────────────

// NewMux creates the HTTP handler with all routes registered.
// If tm is non-nil, its middleware handles auth + quota + rate limiting.
// If tm is nil, the legacy single-key AuthMiddleware is used with apiKey.
func NewMux(p *Proxy, corsOrigins string, apiKey string, tm *tokenauth.Manager) http.Handler {
	mux := http.NewServeMux()

	// Public routes (no auth required)
	mux.HandleFunc("/", CORSMiddleware(corsOrigins)(RecoveryMiddleware(p.handleRoot)))
	mux.HandleFunc("/health", CORSMiddleware(corsOrigins)(RecoveryMiddleware(p.handleHealth)))

	// Protected routes (require auth)
	var commonMW func(http.HandlerFunc) http.HandlerFunc
	if tm != nil && tm.TokenCount() > 0 {
		// Use tokenauth for multi-key auth + quota + rate limiting
		commonMW = chain(CORSMiddleware(corsOrigins), RecoveryMiddleware, tm.Middleware(), LoggingMiddleware)
	} else {
		// Legacy fallback: single key auth
		commonMW = chain(CORSMiddleware(corsOrigins), RecoveryMiddleware, AuthMiddleware(apiKey), LoggingMiddleware)
	}

	mux.HandleFunc("/v1/models", commonMW(p.handleModels))
	mux.HandleFunc("/v1/chat/completions", commonMW(p.handleChatCompletions))

	return mux
}
