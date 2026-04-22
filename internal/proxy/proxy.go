package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	Error   string `json:"error,omitempty"`
}

// ─── OpenAI Request / Response Types ──────────────────────────────────────

type ChatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	Name       *string        `json:"name,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []ChatToolCall `json:"tool_calls,omitempty"`
}

type ChatToolCall struct {
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type"`
	Function ChatToolFunction `json:"function,omitempty"`
}

type ChatToolFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type ChatCompletionRequest struct {
	Model             string          `json:"model"`
	Messages          []ChatMessage   `json:"messages"`
	Stream            bool            `json:"stream,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	MaxTokens         *int            `json:"max_tokens,omitempty"`
	Tools             json.RawMessage `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
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
	Role      string         `json:"role,omitempty"`
	Content   string         `json:"content,omitempty"`
	ToolCalls []ChatToolCall `json:"tool_calls,omitempty"`
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
	Messages            []FireworksMessage `json:"messages"`
	ModelKey            string             `json:"model_key"`
	ConversationID      string             `json:"conversation_id"`
	FunctionDefinitions []interface{}      `json:"function_definitions"`
	Temperature         *float64           `json:"temperature,omitempty"`
	MaxTokens           *int               `json:"max_tokens,omitempty"`
}

// ─── Proxy ────────────────────────────────────────────────────────────────

// Proxy handles OpenAI-to-Fireworks protocol conversion.
type Proxy struct {
	transport           *transport.FireworksTransport
	version             string
	defaultShowThinking bool
	upstreamURL         string
	upstreamEmptyRetry  upstreamEmptyRetryPolicy
	webSearchClient     *http.Client
	webSearchEndpoint   string
	metrics             *metricsCollector
	responses           *responseStore
}

// New creates a new Proxy instance.
// defaultShowThinking controls whether thinking models show their thinking process
// when the request does not explicitly set show_thinking.
func New(transport *transport.FireworksTransport, version string, defaultShowThinking bool) *Proxy {
	return newProxy(transport, version, defaultShowThinking, upstreamURL, newDefaultUpstreamEmptyRetryPolicy())
}

// NewWithRetryPolicy creates a new Proxy with explicit empty-upstream retry behavior.
func NewWithRetryPolicy(transport *transport.FireworksTransport, version string, defaultShowThinking bool, emptyRetryCount int, emptyRetryBackoff time.Duration) *Proxy {
	return newProxy(transport, version, defaultShowThinking, upstreamURL, newUpstreamEmptyRetryPolicy(emptyRetryCount, emptyRetryBackoff))
}

// NewWithUpstream creates a Proxy with a custom upstream URL (for testing).
func NewWithUpstream(transport *transport.FireworksTransport, version string, defaultShowThinking bool, upstreamURL string) *Proxy {
	return newProxy(transport, version, defaultShowThinking, upstreamURL, newDefaultUpstreamEmptyRetryPolicy())
}

// NewWithUpstreamAndRetryPolicy creates a Proxy with a custom upstream URL and explicit retry behavior.
func NewWithUpstreamAndRetryPolicy(transport *transport.FireworksTransport, version string, defaultShowThinking bool, upstreamURL string, emptyRetryCount int, emptyRetryBackoff time.Duration) *Proxy {
	return newProxy(transport, version, defaultShowThinking, upstreamURL, newUpstreamEmptyRetryPolicy(emptyRetryCount, emptyRetryBackoff))
}

func newProxy(transport *transport.FireworksTransport, version string, defaultShowThinking bool, upstream string, retryPolicy upstreamEmptyRetryPolicy) *Proxy {
	return &Proxy{
		transport:           transport,
		version:             version,
		defaultShowThinking: defaultShowThinking,
		upstreamURL:         upstream,
		upstreamEmptyRetry:  retryPolicy,
		webSearchClient:     &http.Client{Timeout: transport.Timeout()},
		webSearchEndpoint:   defaultWebSearchEndpoint,
		metrics:             newMetricsCollector(time.Now),
		responses:           newResponseStore(defaultResponseStoreEntries),
	}
}

// ─── Core Logic ───────────────────────────────────────────────────────────

// generateRequestID creates an OpenAI-style chatcmpl- request ID.
func generateRequestID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// Extremely unlikely with crypto/rand, but log and use timestamp fallback
		slog.Error("crypto/rand.Read failed, using timestamp fallback", "error", err)
		return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("chatcmpl-%x", b)
}

// ─── Route Handlers ───────────────────────────────────────────────────────

// handleRoot returns service info and available endpoints.
func (p *Proxy) handleRoot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "firew2oai - Fireworks to OpenAI API Proxy",
		"version": p.version,
		"endpoints": map[string]string{
			"models":  "GET /v1/models",
			"chat":    "POST /v1/chat/completions",
			"health":  "GET /health",
			"metrics": "GET /metrics",
		},
	})
}

// handleHealth returns a simple health check response.
func (p *Proxy) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
	})
}

// handleMetrics returns Prometheus-style runtime and HTTP metrics.
func (p *Proxy) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "method not allowed, use GET")
		return
	}
	if p.metrics == nil {
		writeError(w, http.StatusInternalServerError, "server_error", "metrics_not_initialized", "metrics collector not initialized")
		return
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(p.metrics.Render())); err != nil {
		slog.Debug("failed to write metrics response", "error", err)
	}
}

// handleModels returns the list of available models in OpenAI format.
func (p *Proxy) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "method not allowed, use GET")
		return
	}

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
		slog.Debug("invalid request body", "error", err)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_body", "invalid request body")
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
	showThinking := p.defaultShowThinking
	if req.ShowThinking != nil {
		showThinking = *req.ShowThinking
	}
	allowParallelToolCalls := req.ParallelToolCalls == nil || *req.ParallelToolCalls
	maxToolCalls := 0
	if !allowParallelToolCalls {
		maxToolCalls = 1
	}
	normalizedTools := normalizeChatTools(req.Tools)
	normalizedToolChoice := normalizeChatToolChoice(req.ToolChoice)
	toolCatalog := buildResponseToolCatalog(normalizedTools)
	toolChoice := resolveToolChoice(normalizedToolChoice)
	if err := validateToolChoiceConfiguration(toolChoice, toolCatalog); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_tool_choice", "%s", err.Error())
		return
	}
	currentTask := latestUserTask(req.Messages)
	autoRequireTool := len(toolCatalog) > 0 && !toolChoice.DisableTools && !toolChoice.RequireTool && taskLikelyNeedsTools(currentTask)
	toolConstraints := toolProtocolConstraints{
		RequiredTool: toolChoice.RequiredTool,
		RequireTool:  toolChoice.RequireTool,
		MaxCalls:     maxToolCalls,
	}
	bufferForToolCalls := len(toolCatalog) > 0 && !toolChoice.DisableTools
	promptTools := toolsForPrompt(normalizedTools, toolChoice)
	prompt := messagesToPrompt(req.Messages)
	if len(promptTools) > 0 || len(normalizedToolChoice) > 0 {
		prompt = buildChatPrompt(req.Messages, promptTools, normalizedToolChoice, maxToolCalls)
	}

	slog.Info("chat completion request",
		"request_id", requestID,
		"model", req.Model,
		"stream", req.Stream,
		"messages", len(req.Messages),
		"thinking", showThinking,
		"tools_present", bufferForToolCalls,
		"required_tool", toolConstraints.RequiredTool,
		"require_tool", toolConstraints.RequireTool,
		"auto_require_tool", autoRequireTool,
		"max_tool_calls", toolConstraints.MaxCalls,
	)

	// Build Fireworks request body
	fwReq := FireworksRequest{
		Messages: []FireworksMessage{
			{Role: "user", Content: prompt},
		},
		ModelKey:            req.Model,
		ConversationID:      fmt.Sprintf("session_%d_%d", time.Now().UnixMilli(), time.Now().UnixNano()%10000),
		FunctionDefinitions: []interface{}{},
		Temperature:         req.Temperature,
		MaxTokens:           req.MaxTokens,
	}

	bodyBytes, err := json.Marshal(fwReq)
	if err != nil {
		slog.Error("failed to marshal fireworks request", "error", err)
		writeError(w, http.StatusInternalServerError, "server_error", "marshal_failed", "failed to build upstream request")
		return
	}

	if req.Stream {
		p.handleStream(w, r, requestID, req.Model, bodyBytes, showThinking, toolCatalog, toolConstraints, bufferForToolCalls)
	} else {
		p.handleNonStream(w, r, requestID, req.Model, bodyBytes, showThinking, toolCatalog, toolConstraints, bufferForToolCalls)
	}
}

// ─── Streaming Handler ────────────────────────────────────────────────────

// handleStream converts Fireworks SSE events to OpenAI streaming format.
func (p *Proxy) handleStream(w http.ResponseWriter, r *http.Request, requestID, model string, body []byte, showThinking bool, toolCatalog map[string]responseToolDescriptor, toolConstraints toolProtocolConstraints, bufferForToolCalls bool) {
	ctx := r.Context()

	// Extract Authorization token from client request to forward to Fireworks
	authToken := ""
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		authToken = auth[7:]
	}

	openReader := func() (io.ReadCloser, error) {
		return p.transport.StreamPost(ctx, p.upstreamURL, bytes.NewReader(body), authToken)
	}

	reader, err := openReader()
	if err != nil {
		slog.Error("upstream stream error", "request_id", requestID, "error", err)
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream_failed", "upstream error: %s", err.Error())
		return
	}

	// SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, canFlush := w.(http.Flusher)

	var clientGone bool
	writeAndFlushBytes := func(data []byte) bool {
		if clientGone {
			return false
		}
		if _, err := w.Write(data); err != nil {
			clientGone = true
			slog.Debug("client disconnected, stopping stream", "request_id", requestID, "error", err)
			return false
		}
		if canFlush {
			flusher.Flush()
		}
		return true
	}

	writeAndFlushChunk := func(chunk StreamChunk) bool {
		if clientGone {
			return false
		}
		if err := writeSSEChunk(w, chunk); err != nil {
			clientGone = true
			slog.Debug("client disconnected, stopping stream", "request_id", requestID, "error", err)
			return false
		}
		if canFlush {
			flusher.Flush()
		}
		return true
	}

	// Pre-allocate timestamp once — reused for all chunks in this stream.
	// Sub-second precision is not required by the OpenAI SSE spec.
	created := time.Now().Unix()

	// Send initial role chunk (OpenAI spec: first chunk contains the role)
	roleChunk := StreamChunk{
		ID:      requestID,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []StreamChoice{
			{Index: 0, Delta: StreamDelta{Role: "assistant"}, FinishReason: nil},
		},
	}
	if !writeAndFlushChunk(roleChunk) {
		return
	}

	isThinking := config.IsThinkingModel(model)
	var result strings.Builder
	doneReceived := false
	contentEmitted := false
	var scanErr error
	attempt := 0
	for {
		doneReceived = false
		contentEmitted, scanErr = scanSSEEvents(reader, isThinking, showThinking, func(evt sseContentEvent) bool {
			switch evt.Type {
			case "done":
				doneReceived = true
				return finalizeChatStream(writeAndFlushChunk, writeAndFlushBytes, requestID, model, created, result.String(), toolCatalog, toolConstraints, bufferForToolCalls)

			case "thinking_separator":
				if bufferForToolCalls {
					result.WriteString("\n\n--- Answer ---\n\n")
					return true
				}
				chunk := StreamChunk{
					ID:      requestID,
					Object:  "chat.completion.chunk",
					Created: created,
					Model:   model,
					Choices: []StreamChoice{
						{Index: 0, Delta: StreamDelta{Content: "\n\n--- Answer ---\n\n"}, FinishReason: nil},
					},
				}
				return writeAndFlushChunk(chunk)

			case "content":
				if bufferForToolCalls {
					result.WriteString(evt.Content)
					return true
				}
				chunk := StreamChunk{
					ID:      requestID,
					Object:  "chat.completion.chunk",
					Created: created,
					Model:   model,
					Choices: []StreamChoice{
						{Index: 0, Delta: StreamDelta{Content: evt.Content}, FinishReason: nil},
					},
				}
				return writeAndFlushChunk(chunk)
			}
			return true
		})
		_ = reader.Close()

		if p.upstreamEmptyRetry.shouldRetry(attempt, doneReceived, contentEmitted, result.Len(), scanErr, clientGone) {
			delay := p.upstreamEmptyRetry.delay(attempt)
			attempt++
			slog.Warn("chat upstream empty attempt, retrying",
				"request_id", requestID,
				"attempt", attempt,
				"max_retries", p.upstreamEmptyRetry.count,
				"backoff", delay,
				"error", scanErr,
			)
			if err := sleepWithContext(ctx, delay); err != nil {
				break
			}
			reader, err = openReader()
			if err != nil {
				scanErr = err
				break
			}
			continue
		}
		break
	}

	// If no done event but content was emitted, send completion markers
	if !doneReceived && contentEmitted {
		slog.Debug("stream ended without done event but content available, sending completion markers", "request_id", requestID)
		finalizeChatStream(writeAndFlushChunk, writeAndFlushBytes, requestID, model, created, result.String(), toolCatalog, toolConstraints, bufferForToolCalls)
	}

	// If stream ended without any payload, emit a terminal chunk instead of silently
	// leaving the client with only the initial assistant role event.
	if !doneReceived && !contentEmitted && result.Len() == 0 && !clientGone && !errors.Is(scanErr, context.Canceled) {
		finalText := "Codex adapter error: upstream response ended without a completion signal"
		if scanErr != nil {
			finalText = "Codex adapter error: upstream stream failed before content: " + scanErr.Error()
		}
		slog.Error("chat stream ended without completion payload", "request_id", requestID, "error", scanErr)
		writeTerminalChatStreamText(writeAndFlushChunk, writeAndFlushBytes, requestID, model, created, finalText)
		return
	}

	if scanErr != nil && !errors.Is(scanErr, context.Canceled) {
		slog.Error("stream read error", "request_id", requestID, "error", scanErr)
	}
}

func writeTerminalChatStreamText(
	writeChunk func(StreamChunk) bool,
	writeDone func([]byte) bool,
	requestID, model string,
	created int64,
	finalText string,
) bool {
	if strings.TrimSpace(finalText) != "" {
		chunk := StreamChunk{
			ID:      requestID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []StreamChoice{
				{Index: 0, Delta: StreamDelta{Content: finalText}, FinishReason: nil},
			},
		}
		if !writeChunk(chunk) {
			return false
		}
	}

	reason := "stop"
	stopChunk := StreamChunk{
		ID:      requestID,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []StreamChoice{
			{Index: 0, Delta: StreamDelta{}, FinishReason: &reason},
		},
	}
	if !writeChunk(stopChunk) {
		return false
	}
	return writeDone([]byte("data: [DONE]\n\n"))
}

func finalizeChatStream(
	writeChunk func(StreamChunk) bool,
	writeDone func([]byte) bool,
	requestID, model string,
	created int64,
	finalText string,
	toolCatalog map[string]responseToolDescriptor,
	toolConstraints toolProtocolConstraints,
	bufferForToolCalls bool,
) bool {
	if bufferForToolCalls {
		calls, visibleText, parseErr := parseChatToolCallOutput(finalText, toolCatalog, toolConstraints)
		slog.Info("tool protocol outcome",
			"api", "chat",
			"request_id", requestID,
			"mode", parseToolCallOutputsWithConstraints(finalText, toolCatalog, toolConstraints).mode,
			"tool_calls", len(calls),
			"required_tool", toolConstraints.RequiredTool,
			"require_tool", toolConstraints.RequireTool,
			"max_tool_calls", toolConstraints.MaxCalls,
			"error", toolProtocolErrorString(parseErr),
		)
		if len(calls) > 0 {
			for i := range calls {
				callIndex := i
				calls[i].Index = &callIndex
			}
			chunk := StreamChunk{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []StreamChoice{
					{
						Index:        0,
						Delta:        StreamDelta{ToolCalls: calls},
						FinishReason: nil,
					},
				},
			}
			if !writeChunk(chunk) {
				return false
			}
			reason := "tool_calls"
			stopChunk := StreamChunk{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []StreamChoice{
					{Index: 0, Delta: StreamDelta{}, FinishReason: &reason},
				},
			}
			if !writeChunk(stopChunk) {
				return false
			}
			return writeDone([]byte("data: [DONE]\n\n"))
		}
		if parseErr != nil {
			finalText = buildToolProtocolErrorMessage(parseErr, finalText)
		} else {
			finalText = visibleText
		}
	}

	if strings.TrimSpace(finalText) != "" {
		chunk := StreamChunk{
			ID:      requestID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []StreamChoice{
				{Index: 0, Delta: StreamDelta{Content: finalText}, FinishReason: nil},
			},
		}
		if !writeChunk(chunk) {
			return false
		}
	}

	reason := "stop"
	stopChunk := StreamChunk{
		ID:      requestID,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []StreamChoice{
			{Index: 0, Delta: StreamDelta{}, FinishReason: &reason},
		},
	}
	if !writeChunk(stopChunk) {
		return false
	}
	return writeDone([]byte("data: [DONE]\n\n"))
}

// ─── Non-Streaming Handler ────────────────────────────────────────────────

// handleNonStream collects the full response from Fireworks and returns it
// in OpenAI non-streaming format.
func (p *Proxy) handleNonStream(w http.ResponseWriter, r *http.Request, requestID, model string, body []byte, showThinking bool, toolCatalog map[string]responseToolDescriptor, toolConstraints toolProtocolConstraints, bufferForToolCalls bool) {
	ctx := r.Context()

	// For non-streaming requests, apply an overall timeout to prevent
	// the request from hanging indefinitely if upstream sends headers but
	// then stalls. The transport's ResponseHeaderTimeout only covers the
	// initial header wait; this covers the full request lifecycle.
	ctx, cancel := context.WithTimeout(ctx, p.transport.Timeout())
	defer cancel()

	// Extract Authorization token from client request to forward to Fireworks
	authToken := ""
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		authToken = auth[7:]
	}

	openReader := func() (io.ReadCloser, error) {
		return p.transport.StreamPost(ctx, p.upstreamURL, bytes.NewReader(body), authToken)
	}

	reader, err := openReader()
	if err != nil {
		slog.Error("upstream non-stream error", "request_id", requestID, "error", err)
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream_failed", "upstream error: %s", err.Error())
		return
	}

	var result strings.Builder
	isThinking := config.IsThinkingModel(model)
	doneReceived := false
	contentEmitted := false
	var scanErr error
	attempt := 0
	for {
		doneReceived = false
		contentEmitted, scanErr = scanSSEEvents(reader, isThinking, showThinking, func(evt sseContentEvent) bool {
			switch evt.Type {
			case "done":
				doneReceived = true
				// handled by scanSSEEvents breaking the loop
			case "thinking_separator":
				result.WriteString("\n\n--- Answer ---\n\n")
			case "content":
				result.WriteString(evt.Content)
			}
			return true
		})
		_ = reader.Close()

		if p.upstreamEmptyRetry.shouldRetry(attempt, doneReceived, contentEmitted, result.Len(), scanErr, false) {
			delay := p.upstreamEmptyRetry.delay(attempt)
			attempt++
			slog.Warn("chat non-stream empty attempt, retrying",
				"request_id", requestID,
				"attempt", attempt,
				"max_retries", p.upstreamEmptyRetry.count,
				"backoff", delay,
				"error", scanErr,
			)
			if err := sleepWithContext(ctx, delay); err != nil {
				break
			}
			reader, err = openReader()
			if err != nil {
				scanErr = err
				break
			}
			continue
		}
		break
	}

	slog.Debug("non-stream scan complete", "request_id", requestID, "scanErr", scanErr, "doneReceived", doneReceived, "contentEmitted", contentEmitted, "result_len", result.Len())

	// On scanner error, return 502 so the client
	// can distinguish "complete response" from "truncated/corrupted response".
	if scanErr != nil {
		if errors.Is(scanErr, context.Canceled) {
			// Client disconnected — don't write a response that nobody will read.
			slog.Debug("non-stream client disconnected", "request_id", requestID)
			return
		}
		// If we have content, return it even if there was a scanner error
		// (some models may not send a proper done event)
		if contentEmitted || result.Len() > 0 {
			slog.Warn("stream read error but content available, returning partial response", "request_id", requestID, "error", scanErr)
		} else {
			slog.Error("stream read error (upstream incomplete)", "request_id", requestID, "error", scanErr)
			writeError(w, http.StatusBadGateway, "upstream_error", "upstream_incomplete", "%s", scanErr.Error())
			return
		}
	}

	// If we didn't receive a done event but the stream ended cleanly
	// and we have content, treat it as a successful response.
	// Some models (e.g., kimi) may not send a done event.
	if !doneReceived && !contentEmitted && result.Len() == 0 {
		slog.Error("stream ended without done event and no content", "request_id", requestID)
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream_incomplete",
			"upstream response ended without a completion signal")
		return
	}

	if !doneReceived {
		slog.Debug("stream ended without done event but content available, treating as success", "request_id", requestID, "result_len", result.Len())
	}

	if bufferForToolCalls {
		assistantText := result.String()
		parseResult := parseToolCallOutputsWithConstraints(assistantText, toolCatalog, toolConstraints)
		calls, visibleText, parseErr := parseChatToolCallOutput(assistantText, toolCatalog, toolConstraints)
		slog.Info("tool protocol outcome",
			"api", "chat",
			"request_id", requestID,
			"mode", parseResult.mode,
			"tool_calls", len(parseResult.calls),
			"required_tool", toolConstraints.RequiredTool,
			"require_tool", toolConstraints.RequireTool,
			"max_tool_calls", toolConstraints.MaxCalls,
			"error", toolProtocolErrorString(parseErr),
		)
		if len(calls) > 0 {
			resp := ChatCompletionResponse{
				ID:      requestID,
				Object:  "chat.completion",
				Created: time.Now().Unix(),
				Model:   model,
				Choices: []ChatCompletionChoice{
					{
						Index: 0,
						Message: ChatMessage{
							Role:      "assistant",
							Content:   "",
							ToolCalls: calls,
						},
						FinishReason: "tool_calls",
					},
				},
				Usage: Usage{},
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}
		if parseErr != nil {
			result.Reset()
			result.WriteString(buildToolProtocolErrorMessage(parseErr, assistantText))
		} else {
			result.Reset()
			result.WriteString(visibleText)
		}
	}

	resp := ChatCompletionResponse{
		ID:      requestID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []ChatCompletionChoice{
			{
				Index:        0,
				Message:      ChatMessage{Role: "assistant", Content: result.String()},
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
	buf := getPooledJSONBuffer()
	defer putPooledJSONBuffer(buf)

	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		slog.Error("json.Marshal failed", "error", err)
		http.Error(w, `{"error":{"message":"internal JSON error","type":"server_error","code":"marshal_error"}}`, http.StatusInternalServerError)
		return
	}
	// json.Encoder.Encode appends a newline; trim it and add our own
	data := buf.Bytes()
	if len(data) > 0 && data[len(data)-1] == '\n' {
		data = data[:len(data)-1]
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(data)
	w.Write([]byte("\n"))
}

func writeError(w http.ResponseWriter, status int, errType string, errCode string, format string, args ...interface{}) {
	buf := getPooledJSONBuffer()
	defer putPooledJSONBuffer(buf)

	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": fmt.Sprintf(format, args...),
			"type":    errType,
			"code":    errCode,
		},
	}); err != nil {
		slog.Error("json.Marshal failed for error response", "error", err)
		http.Error(w, `{"error":{"message":"internal error","type":"server_error","code":"internal_error"}}`, http.StatusInternalServerError)
		return
	}
	data := buf.Bytes()
	if len(data) > 0 && data[len(data)-1] == '\n' {
		data = data[:len(data)-1]
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(data)
	w.Write([]byte("\n"))
}

type routeDurationMetrics struct {
	count      atomic.Int64
	durationNS atomic.Int64
}

type metricsCollector struct {
	now             func() time.Time
	startedAt       time.Time
	inFlight        atomic.Int64
	requestTotal    atomic.Int64
	durationTotalNS atomic.Int64
	durationCount   atomic.Int64

	statusCounters sync.Map // key: method\x1fpath\x1fstatus, value: *atomic.Int64
	routeDurations sync.Map // key: method\x1fpath, value: *routeDurationMetrics
}

func newMetricsCollector(now func() time.Time) *metricsCollector {
	if now == nil {
		now = time.Now
	}
	started := now()
	return &metricsCollector{
		now:       now,
		startedAt: started,
	}
}

func statusCounterKey(method, path string, status int) string {
	return method + "\x1f" + path + "\x1f" + strconv.Itoa(status)
}

func routeDurationKey(method, path string) string {
	return method + "\x1f" + path
}

func (m *metricsCollector) observe(method, path string, status int, duration time.Duration) {
	m.requestTotal.Add(1)
	m.durationTotalNS.Add(duration.Nanoseconds())
	m.durationCount.Add(1)

	sk := statusCounterKey(method, path, status)
	sv, _ := m.statusCounters.LoadOrStore(sk, &atomic.Int64{})
	sv.(*atomic.Int64).Add(1)

	rk := routeDurationKey(method, path)
	rv, _ := m.routeDurations.LoadOrStore(rk, &routeDurationMetrics{})
	rm := rv.(*routeDurationMetrics)
	rm.count.Add(1)
	rm.durationNS.Add(duration.Nanoseconds())
}

func escapeLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

func (m *metricsCollector) Render() string {
	now := m.now()
	uptime := int64(now.Sub(m.startedAt).Seconds())
	if uptime < 0 {
		uptime = 0
	}

	type statusRow struct {
		Method string
		Path   string
		Status string
		Count  int64
	}
	statusRows := make([]statusRow, 0, 32)
	m.statusCounters.Range(func(k, v any) bool {
		parts := strings.SplitN(k.(string), "\x1f", 3)
		if len(parts) != 3 {
			return true
		}
		statusRows = append(statusRows, statusRow{
			Method: parts[0],
			Path:   parts[1],
			Status: parts[2],
			Count:  v.(*atomic.Int64).Load(),
		})
		return true
	})
	sort.Slice(statusRows, func(i, j int) bool {
		if statusRows[i].Method != statusRows[j].Method {
			return statusRows[i].Method < statusRows[j].Method
		}
		if statusRows[i].Path != statusRows[j].Path {
			return statusRows[i].Path < statusRows[j].Path
		}
		return statusRows[i].Status < statusRows[j].Status
	})

	type durationRow struct {
		Method     string
		Path       string
		Count      int64
		DurationNS int64
	}
	durationRows := make([]durationRow, 0, 16)
	m.routeDurations.Range(func(k, v any) bool {
		parts := strings.SplitN(k.(string), "\x1f", 2)
		if len(parts) != 2 {
			return true
		}
		rm := v.(*routeDurationMetrics)
		durationRows = append(durationRows, durationRow{
			Method:     parts[0],
			Path:       parts[1],
			Count:      rm.count.Load(),
			DurationNS: rm.durationNS.Load(),
		})
		return true
	})
	sort.Slice(durationRows, func(i, j int) bool {
		if durationRows[i].Method != durationRows[j].Method {
			return durationRows[i].Method < durationRows[j].Method
		}
		return durationRows[i].Path < durationRows[j].Path
	})

	var b strings.Builder
	b.Grow(4096)

	b.WriteString("# HELP firew2oai_uptime_seconds Process uptime in seconds\n")
	b.WriteString("# TYPE firew2oai_uptime_seconds gauge\n")
	b.WriteString("firew2oai_uptime_seconds ")
	b.WriteString(strconv.FormatInt(uptime, 10))
	b.WriteByte('\n')

	b.WriteString("# HELP firew2oai_go_goroutines Number of live goroutines\n")
	b.WriteString("# TYPE firew2oai_go_goroutines gauge\n")
	b.WriteString("firew2oai_go_goroutines ")
	b.WriteString(strconv.Itoa(runtime.NumGoroutine()))
	b.WriteByte('\n')

	b.WriteString("# HELP firew2oai_http_requests_in_flight Number of in-flight HTTP requests\n")
	b.WriteString("# TYPE firew2oai_http_requests_in_flight gauge\n")
	b.WriteString("firew2oai_http_requests_in_flight ")
	b.WriteString(strconv.FormatInt(m.inFlight.Load(), 10))
	b.WriteByte('\n')

	b.WriteString("# HELP firew2oai_http_requests_total Total HTTP requests handled\n")
	b.WriteString("# TYPE firew2oai_http_requests_total counter\n")
	b.WriteString("firew2oai_http_requests_total ")
	b.WriteString(strconv.FormatInt(m.requestTotal.Load(), 10))
	b.WriteByte('\n')

	b.WriteString("# HELP firew2oai_http_request_duration_seconds_sum Total request duration in seconds\n")
	b.WriteString("# TYPE firew2oai_http_request_duration_seconds_sum counter\n")
	b.WriteString("firew2oai_http_request_duration_seconds_sum ")
	b.WriteString(strconv.FormatFloat(float64(m.durationTotalNS.Load())/float64(time.Second), 'f', 6, 64))
	b.WriteByte('\n')

	b.WriteString("# HELP firew2oai_http_request_duration_seconds_count Number of requests in duration summary\n")
	b.WriteString("# TYPE firew2oai_http_request_duration_seconds_count counter\n")
	b.WriteString("firew2oai_http_request_duration_seconds_count ")
	b.WriteString(strconv.FormatInt(m.durationCount.Load(), 10))
	b.WriteByte('\n')

	b.WriteString("# HELP firew2oai_http_requests_by_status_total Total requests partitioned by method/path/status\n")
	b.WriteString("# TYPE firew2oai_http_requests_by_status_total counter\n")
	for _, row := range statusRows {
		b.WriteString(`firew2oai_http_requests_by_status_total{method="`)
		b.WriteString(escapeLabelValue(row.Method))
		b.WriteString(`",path="`)
		b.WriteString(escapeLabelValue(row.Path))
		b.WriteString(`",status="`)
		b.WriteString(escapeLabelValue(row.Status))
		b.WriteString(`"} `)
		b.WriteString(strconv.FormatInt(row.Count, 10))
		b.WriteByte('\n')
	}

	b.WriteString("# HELP firew2oai_http_request_duration_by_route_seconds_sum Request duration sum by method/path\n")
	b.WriteString("# TYPE firew2oai_http_request_duration_by_route_seconds_sum counter\n")
	for _, row := range durationRows {
		b.WriteString(`firew2oai_http_request_duration_by_route_seconds_sum{method="`)
		b.WriteString(escapeLabelValue(row.Method))
		b.WriteString(`",path="`)
		b.WriteString(escapeLabelValue(row.Path))
		b.WriteString(`"} `)
		b.WriteString(strconv.FormatFloat(float64(row.DurationNS)/float64(time.Second), 'f', 6, 64))
		b.WriteByte('\n')
	}

	b.WriteString("# HELP firew2oai_http_request_duration_by_route_seconds_count Request count by method/path in duration summary\n")
	b.WriteString("# TYPE firew2oai_http_request_duration_by_route_seconds_count counter\n")
	for _, row := range durationRows {
		b.WriteString(`firew2oai_http_request_duration_by_route_seconds_count{method="`)
		b.WriteString(escapeLabelValue(row.Method))
		b.WriteString(`",path="`)
		b.WriteString(escapeLabelValue(row.Path))
		b.WriteString(`"} `)
		b.WriteString(strconv.FormatInt(row.Count, 10))
		b.WriteByte('\n')
	}

	return b.String()
}

// MetricsMiddleware records per-request status and latency metrics.
func MetricsMiddleware(mc *metricsCollector) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		if mc == nil {
			return next
		}
		return func(w http.ResponseWriter, r *http.Request) {
			start := mc.now()
			mc.inFlight.Add(1)
			rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
			defer func() {
				mc.inFlight.Add(-1)
				if r.URL.Path != "/metrics" {
					mc.observe(r.Method, r.URL.Path, rw.statusCode, mc.now().Sub(start))
				}
			}()
			next(rw, r)
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

		// Extract token fingerprint for debugging (SHA-256 hash, first 8 hex chars)
		tokenFP := ""
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token := auth[7:]
			if token != "" {
				h := sha256.Sum256([]byte(token))
				tokenFP = hex.EncodeToString(h[:])[:8]
			}
		}

		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.statusCode,
			"duration", time.Since(start).String(),
			"remote", r.RemoteAddr,
			"token", tokenFP,
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

func (rw *responseWriter) Flush() {
	if flusher, ok := rw.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
	}
	return hijacker.Hijack()
}

// Unwrap returns the underlying ResponseWriter.
// This enables middleware chaining that checks for optional interfaces (Flusher, Hijacker).
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// ─── Middleware: CORS ─────────────────────────────────────────────────────

// CORSMiddleware adds CORS headers for cross-origin API access.
// origins is a comma-separated list; "*" allows all origins.
// Invalid configurations (e.g. ",,,") that produce no valid entries will
// block all cross-origin requests rather than silently becoming wildcard.
func CORSMiddleware(origins string) func(http.HandlerFunc) http.HandlerFunc {
	origins = strings.TrimSpace(origins)
	isWildcard := origins == "*"
	isBlocked := false // invalid config: block all CORS
	allowedSet := parseOrigins(origins)
	if !isWildcard && len(allowedSet) == 0 && origins != "" {
		slog.Warn("CORS origins produced no valid entries, blocking all cross-origin requests", "raw", origins)
		isBlocked = true
	}

	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			if isBlocked {
				// Invalid CORS config: do not set any CORS headers.
				// Same-origin requests still work; cross-origin requests will be blocked by browser.
			} else if isWildcard {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if origin != "" {
				// Always set Vary: Origin for non-wildcard CORS so that caches
				// (CDN, browser) don't reuse a response for one origin on another.
				w.Header().Add("Vary", "Origin")
				if allowedSet[origin] {
					w.Header().Set("Access-Control-Allow-Origin", origin)
				}
			}

			// Access-Control-Allow-Methods/Headers/Max-Age are only meaningful
			// for preflight (OPTIONS) requests. Setting them on every response
			// wastes bandwidth and pollutes cache entries.
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				w.Header().Set("Access-Control-Max-Age", "86400")
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
// tm must be constructed by the caller so auth configuration errors fail fast at startup.
func NewMux(p *Proxy, corsOrigins string, tm *tokenauth.Manager) http.Handler {
	if tm == nil {
		panic("tokenauth manager is required")
	}

	mux := http.NewServeMux()

	mc := p.metrics
	publicMW := chain(CORSMiddleware(corsOrigins), MetricsMiddleware(mc), RecoveryMiddleware)

	// Public routes (no auth required)
	mux.HandleFunc("/", publicMW(p.handleRoot))
	mux.HandleFunc("/health", publicMW(p.handleHealth))
	mux.HandleFunc("/metrics", publicMW(p.handleMetrics))

	// Protected routes (require auth)
	commonMW := chain(CORSMiddleware(corsOrigins), MetricsMiddleware(mc), RecoveryMiddleware, tm.Middleware(), LoggingMiddleware)

	mux.HandleFunc("/v1/models", commonMW(p.handleModels))
	mux.HandleFunc("/v1/chat/completions", commonMW(p.handleChatCompletions))
	mux.HandleFunc("/v1/responses", commonMW(p.handleResponses))
	mux.HandleFunc("/v1/responses/", commonMW(p.handleResponseByID))

	return mux
}
