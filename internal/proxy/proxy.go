package proxy

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mison/firew2oai/internal/config"
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
	apiKey    string
	timeout   time.Duration
}

// New creates a new Proxy instance.
func New(transport *transport.FireworksTransport, apiKey string, timeout time.Duration) *Proxy {
	return &Proxy{
		transport: transport,
		apiKey:    apiKey,
		timeout:   timeout,
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
	_, _ = rand.Read(b)
	return fmt.Sprintf("chatcmpl-%x", b)
}

// ─── Route Handlers ───────────────────────────────────────────────────────

// handleRoot returns service info and available endpoints.
func (p *Proxy) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message":   "firew2oai - Fireworks to OpenAI API Proxy",
		"version":   "1.0.0",
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
		writeError(w, http.StatusMethodNotAllowed, "method not allowed, use POST")
		return
	}

	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: %v", err)
		return
	}

	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages array is required and must not be empty")
		return
	}

	if !config.ValidModel(req.Model) {
		writeError(w, http.StatusBadRequest, "model %q is not supported. Use /v1/models to list available models", req.Model)
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

	bodyBytes, _ := json.Marshal(fwReq)

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
		writeError(w, http.StatusBadGateway, "upstream error: %v", err)
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
	flush := func() {
		mu.Lock()
		defer mu.Unlock()
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
	writeSSE(w, roleChunk)
	flush()

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
			writeSSE(w, chunk)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flush()
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
					writeSSE(w, chunk)
					flush()
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
		writeSSE(w, chunk)
		flush()
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
		writeError(w, http.StatusBadGateway, "upstream error: %v", err)
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

	if err := scanner.Err(); err != nil {
		writeError(w, http.StatusBadGateway, "stream read error: %v", err)
		return
	}

	content := result.String()
	promptTokens := len(strings.Fields(string(body)))
	completionTokens := len(strings.Fields(content))

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
		Usage: Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	}

	writeJSON(w, http.StatusOK, resp)
}

// ─── JSON / SSE Helpers ──────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeSSE(w http.ResponseWriter, v interface{}) {
	data, _ := json.Marshal(v)
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func writeError(w http.ResponseWriter, status int, format string, args ...interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": fmt.Sprintf(format, args...),
			"type":    "invalid_request_error",
			"code":    status,
		},
	})
}

// ─── Middleware: Auth ─────────────────────────────────────────────────────

// AuthMiddleware validates the Bearer token.
// Returns a middleware function compatible with the chain() helper.
func AuthMiddleware(apiKey string) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if auth == "" {
				writeError(w, http.StatusUnauthorized, "missing Authorization header")
				return
			}
			if !strings.HasPrefix(auth, "Bearer ") {
				writeError(w, http.StatusUnauthorized, "invalid Authorization format, expected 'Bearer <key>'")
				return
			}
			token := auth[7:]
			if token != apiKey {
				writeError(w, http.StatusUnauthorized, "invalid API key")
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

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// ─── Middleware: CORS ─────────────────────────────────────────────────────

// CORSMiddleware adds permissive CORS headers for cross-origin API access.
func CORSMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
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
				writeError(w, http.StatusInternalServerError, "internal server error")
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
func NewMux(p *Proxy) http.Handler {
	mux := http.NewServeMux()

	// Public routes (no auth required)
	mux.HandleFunc("/", CORSMiddleware(RecoveryMiddleware(p.handleRoot)))
	mux.HandleFunc("/health", CORSMiddleware(RecoveryMiddleware(p.handleHealth)))

	// Protected routes (require auth)
	commonMW := chain(CORSMiddleware, RecoveryMiddleware, AuthMiddleware(p.apiKey), LoggingMiddleware)

	mux.HandleFunc("/v1/models", commonMW(p.handleModels))
	mux.HandleFunc("/v1/chat/completions", commonMW(p.handleChatCompletions))

	return mux
}

// Handler returns the main HTTP handler. Convenience wrapper for NewMux.
func Handler(p *Proxy) http.Handler {
	return NewMux(p)
}
