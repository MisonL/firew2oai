package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mison/firew2oai/internal/config"
)

// ResponsesRequest is a minimal OpenAI Responses API subset.
type ResponsesRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Instructions       string          `json:"instructions,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	MaxOutputTokens    *int            `json:"max_output_tokens,omitempty"`
	ShowThinking       *bool           `json:"show_thinking,omitempty"`
}

type ResponseOutputText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ResponseOutputMessage struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Type    string               `json:"type"`
	Role    string               `json:"role"`
	Content []ResponseOutputText `json:"content"`
}

type ResponsesResponse struct {
	ID        string                  `json:"id"`
	Object    string                  `json:"object"`
	CreatedAt int64                   `json:"created_at"`
	Status    string                  `json:"status"`
	Model     string                  `json:"model"`
	Output    []ResponseOutputMessage `json:"output"`
}

type ResponseOutputTextDeltaEvent struct {
	Type         string `json:"type"`
	ResponseID   string `json:"response_id"`
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Delta        string `json:"delta"`
}

type ResponseOutputTextDoneEvent struct {
	Type         string `json:"type"`
	ResponseID   string `json:"response_id"`
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Text         string `json:"text"`
}

type ResponseOutputItemAddedEvent struct {
	Type        string                `json:"type"`
	ResponseID  string                `json:"response_id"`
	OutputIndex int                   `json:"output_index"`
	Item        ResponseOutputMessage `json:"item"`
}

type ResponseContentPartAddedEvent struct {
	Type         string             `json:"type"`
	ResponseID   string             `json:"response_id"`
	ItemID       string             `json:"item_id"`
	OutputIndex  int                `json:"output_index"`
	ContentIndex int                `json:"content_index"`
	Part         ResponseOutputText `json:"part"`
}

type ResponseOutputItemDoneEvent struct {
	Type        string                `json:"type"`
	ResponseID  string                `json:"response_id"`
	OutputIndex int                   `json:"output_index"`
	Item        ResponseOutputMessage `json:"item"`
}

type ResponseInputTextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ResponseInputItem struct {
	ID      string                  `json:"id"`
	Type    string                  `json:"type"`
	Role    string                  `json:"role"`
	Content []ResponseInputTextPart `json:"content"`
}

type ResponseInputItemList struct {
	Object  string              `json:"object"`
	Data    []ResponseInputItem `json:"data"`
	FirstID string              `json:"first_id,omitempty"`
	LastID  string              `json:"last_id,omitempty"`
	HasMore bool                `json:"has_more"`
}

func generateResponsesID() string {
	return strings.Replace(generateRequestID(), "chatcmpl-", "resp_", 1)
}

func generateResponseMessageID() string {
	return strings.Replace(generateRequestID(), "chatcmpl-", "msg_", 1)
}

func buildFireworksRequestBody(model, prompt string, temperature *float64, maxTokens *int) ([]byte, error) {
	fwReq := FireworksRequest{
		Messages: []FireworksMessage{
			{Role: "user", Content: prompt},
		},
		ModelKey:            model,
		ConversationID:      fmt.Sprintf("session_%d_%d", time.Now().UnixMilli(), time.Now().UnixNano()%10000),
		FunctionDefinitions: []interface{}{},
		Temperature:         temperature,
		MaxTokens:           maxTokens,
	}
	return json.Marshal(fwReq)
}

func resolveShowThinking(defaultShowThinking bool, override *bool) bool {
	showThinking := defaultShowThinking
	if override != nil {
		showThinking = *override
	}
	return showThinking
}

func buildResponsesMessage(messageID, text string) ResponseOutputMessage {
	return ResponseOutputMessage{
		ID:     messageID,
		Object: "message",
		Type:   "message",
		Role:   "assistant",
		Content: []ResponseOutputText{
			{Type: "output_text", Text: text},
		},
	}
}

func buildResponsesOutput(messageID, text string) []ResponseOutputMessage {
	return []ResponseOutputMessage{buildResponsesMessage(messageID, text)}
}

func newResponsesResponse(responseID, messageID, model string, createdAt int64, status, text string) ResponsesResponse {
	resp := ResponsesResponse{
		ID:        responseID,
		Object:    "response",
		CreatedAt: createdAt,
		Status:    status,
		Model:     model,
	}
	if status == "completed" {
		resp.Output = buildResponsesOutput(messageID, text)
	}
	return resp
}

func writeSSEEvent(w io.Writer, event string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}

	buf := getPooledJSONBuffer()
	defer putPooledJSONBuffer(buf)

	if event != "" {
		buf.WriteString("event: ")
		buf.WriteString(event)
		buf.WriteByte('\n')
	}
	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")

	_, err = w.Write(buf.Bytes())
	return err
}

func responseInputToMessages(input json.RawMessage) ([]ChatMessage, error) {
	messages := make([]ChatMessage, 0, 4)

	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 {
		return nil, errors.New("input is required")
	}

	var raw any
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}

	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, errors.New("input is required")
		}
		messages = append(messages, ChatMessage{Role: "user", Content: v})
	case []any:
		for _, item := range v {
			messages = append(messages, extractInputMessages(item)...)
		}
	default:
		return nil, errors.New("input must be a string or array")
	}

	nonSystemCount := 0
	filtered := messages[:0]
	for _, msg := range messages {
		if strings.TrimSpace(msg.Content) == "" {
			continue
		}
		filtered = append(filtered, msg)
		if msg.Role != "system" {
			nonSystemCount++
		}
	}
	if nonSystemCount == 0 {
		return nil, errors.New("input must contain at least one text item")
	}
	return filtered, nil
}

func responsesPromptMessages(base []ChatMessage, instructions string, current []ChatMessage) []ChatMessage {
	total := len(base) + len(current)
	if strings.TrimSpace(instructions) != "" {
		total++
	}
	messages := make([]ChatMessage, 0, total)
	if instructions = strings.TrimSpace(instructions); instructions != "" {
		messages = append(messages, ChatMessage{Role: "system", Content: instructions})
	}
	messages = append(messages, base...)
	messages = append(messages, current...)
	return messages
}

func extractInputMessages(v any) []ChatMessage {
	switch item := v.(type) {
	case string:
		return []ChatMessage{{Role: "user", Content: item}}
	case map[string]any:
		if text, ok := extractDirectInputText(item); ok {
			return []ChatMessage{{Role: "user", Content: text}}
		}

		role := "user"
		if s, ok := item["role"].(string); ok && strings.TrimSpace(s) != "" {
			role = s
		}

		switch content := item["content"].(type) {
		case string:
			return []ChatMessage{{Role: role, Content: content}}
		case []any:
			text := extractTextParts(content)
			if text != "" {
				return []ChatMessage{{Role: role, Content: text}}
			}
		}
	}
	return nil
}

func extractDirectInputText(item map[string]any) (string, bool) {
	text, ok := item["text"].(string)
	if !ok || strings.TrimSpace(text) == "" {
		return "", false
	}
	typ, _ := item["type"].(string)
	if typ == "" || strings.Contains(typ, "text") {
		return text, true
	}
	return "", false
}

func extractTextParts(parts []any) string {
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		m, ok := part.(map[string]any)
		if !ok {
			continue
		}
		text, ok := m["text"].(string)
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}
		typ, _ := m["type"].(string)
		if typ == "" || strings.Contains(typ, "text") {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "")
}

func buildResponseInputItemList(messages []ChatMessage) ResponseInputItemList {
	items := make([]ResponseInputItem, 0, len(messages))
	for i, msg := range messages {
		items = append(items, ResponseInputItem{
			ID:   fmt.Sprintf("item_%d", i+1),
			Type: "message",
			Role: msg.Role,
			Content: []ResponseInputTextPart{
				{Type: "input_text", Text: msg.Content},
			},
		})
	}

	list := ResponseInputItemList{
		Object:  "list",
		Data:    items,
		HasMore: false,
	}
	if len(items) > 0 {
		list.FirstID = items[0].ID
		list.LastID = items[len(items)-1].ID
	}
	return list
}

// handleResponses exposes a minimal OpenAI Responses-compatible endpoint.
func (p *Proxy) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "method not allowed, use POST")
		return
	}

	var req ResponsesRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Debug("invalid responses request body", "error", err)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_body", "invalid request body")
		return
	}

	if !config.ValidModel(req.Model) {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model_not_found", "model %q is not supported. Use /v1/models to list available models", req.Model)
		return
	}

	currentMessages, err := responseInputToMessages(req.Input)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_input", "%s", err.Error())
		return
	}

	baseMessages := []ChatMessage(nil)
	if req.PreviousResponseID != "" {
		entry, ok := p.responses.get(req.PreviousResponseID)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "previous_response_not_found", "previous_response_id %q was not found", req.PreviousResponseID)
			return
		}
		baseMessages = entry.conversation
	}

	responseID := generateResponsesID()
	messageID := generateResponseMessageID()
	showThinking := resolveShowThinking(p.defaultShowThinking, req.ShowThinking)
	promptMessages := responsesPromptMessages(baseMessages, req.Instructions, currentMessages)
	prompt := messagesToPrompt(promptMessages)

	bodyBytes, err := buildFireworksRequestBody(req.Model, prompt, req.Temperature, req.MaxOutputTokens)
	if err != nil {
		slog.Error("failed to marshal fireworks request for responses", "error", err)
		writeError(w, http.StatusInternalServerError, "server_error", "marshal_failed", "failed to build upstream request")
		return
	}

	slog.Info("responses request",
		"response_id", responseID,
		"model", req.Model,
		"stream", req.Stream,
		"messages", len(promptMessages),
		"previous_response_id", req.PreviousResponseID,
		"thinking", showThinking,
	)

	if req.Stream {
		p.handleResponsesStream(w, r, responseID, messageID, req.Model, bodyBytes, showThinking, append(cloneMessages(baseMessages), currentMessages...))
		return
	}
	p.handleResponsesNonStream(w, r, responseID, messageID, req.Model, bodyBytes, showThinking, append(cloneMessages(baseMessages), currentMessages...))
}

func (p *Proxy) handleResponseByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "method not allowed, use GET")
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/v1/responses/")
	path = strings.Trim(path, "/")
	if path == "" {
		writeError(w, http.StatusNotFound, "invalid_request_error", "response_not_found", "response not found")
		return
	}

	if strings.HasSuffix(path, "/input_items") {
		responseID := strings.TrimSuffix(path, "/input_items")
		responseID = strings.TrimSuffix(responseID, "/")
		entry, ok := p.responses.get(responseID)
		if !ok {
			writeError(w, http.StatusNotFound, "invalid_request_error", "response_not_found", "response %q was not found", responseID)
			return
		}
		writeJSON(w, http.StatusOK, buildResponseInputItemList(entry.inputItems))
		return
	}

	entry, ok := p.responses.get(path)
	if !ok {
		writeError(w, http.StatusNotFound, "invalid_request_error", "response_not_found", "response %q was not found", path)
		return
	}
	writeJSON(w, http.StatusOK, entry.response)
}

func (p *Proxy) handleResponsesStream(w http.ResponseWriter, r *http.Request, responseID, messageID, model string, body []byte, showThinking bool, conversationInput []ChatMessage) {
	ctx := r.Context()

	// Extract Authorization token from client request to forward to Fireworks
	authToken := ""
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		authToken = auth[7:]
	}

	reader, err := p.transport.StreamPost(ctx, p.upstreamURL, bytes.NewReader(body), authToken)
	if err != nil {
		slog.Error("upstream responses stream error", "response_id", responseID, "error", err)
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream_failed", "upstream error")
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, canFlush := w.(http.Flusher)
	var clientGone bool
	writeAndFlushEvent := func(event string, payload any) bool {
		if clientGone {
			return false
		}
		if err := writeSSEEvent(w, event, payload); err != nil {
			clientGone = true
			slog.Debug("client disconnected, stopping responses stream", "response_id", responseID, "error", err)
			return false
		}
		if canFlush {
			flusher.Flush()
		}
		return true
	}

	createdAt := time.Now().Unix()
	if !writeAndFlushEvent("response.created", newResponsesResponse(responseID, messageID, model, createdAt, "in_progress", "")) {
		return
	}
	if !writeAndFlushEvent("response.output_item.added", ResponseOutputItemAddedEvent{
		Type:        "response.output_item.added",
		ResponseID:  responseID,
		OutputIndex: 0,
		Item:        buildResponsesMessage(messageID, ""),
	}) {
		return
	}
	if !writeAndFlushEvent("response.content_part.added", ResponseContentPartAddedEvent{
		Type:         "response.content_part.added",
		ResponseID:   responseID,
		ItemID:       messageID,
		OutputIndex:  0,
		ContentIndex: 0,
		Part:         ResponseOutputText{Type: "output_text", Text: ""},
	}) {
		return
	}

	isThinking := config.IsThinkingModel(model)
	var result strings.Builder
	doneReceived := false
	contentEmitted, scanErr := scanSSEEvents(reader, isThinking, showThinking, func(evt sseContentEvent) bool {
		switch evt.Type {
		case "done":
			doneReceived = true
			finalText := result.String()
			if !writeAndFlushEvent("response.output_text.done", ResponseOutputTextDoneEvent{
				Type:         "response.output_text.done",
				ResponseID:   responseID,
				ItemID:       messageID,
				OutputIndex:  0,
				ContentIndex: 0,
				Text:         finalText,
			}) {
				return false
			}
			if !writeAndFlushEvent("response.output_item.done", ResponseOutputItemDoneEvent{
				Type:        "response.output_item.done",
				ResponseID:  responseID,
				OutputIndex: 0,
				Item:        buildResponsesMessage(messageID, finalText),
			}) {
				return false
			}
			if !writeAndFlushEvent("response.completed", newResponsesResponse(responseID, messageID, model, createdAt, "completed", finalText)) {
				return false
			}
			p.responses.put(newResponsesResponse(responseID, messageID, model, createdAt, "completed", finalText), conversationInput, responseConversation(conversationInput, finalText))
			return true
		case "thinking_separator":
			fallthrough
		case "content":
			delta := evt.Content
			if evt.Type == "thinking_separator" {
				delta = "\n\n--- Answer ---\n\n"
			}
			result.WriteString(delta)
			return writeAndFlushEvent("response.output_text.delta", ResponseOutputTextDeltaEvent{
				Type:         "response.output_text.delta",
				ResponseID:   responseID,
				ItemID:       messageID,
				OutputIndex:  0,
				ContentIndex: 0,
				Delta:        delta,
			})
		}
		return true
	})

	// If no done event but content was emitted, send completion events
	if !doneReceived && (contentEmitted || result.Len() > 0) {
		slog.Debug("responses stream ended without done event but content available", "response_id", responseID, "result_len", result.Len())
		finalText := result.String()
		writeAndFlushEvent("response.output_text.done", ResponseOutputTextDoneEvent{
			Type:         "response.output_text.done",
			ResponseID:   responseID,
			ItemID:       messageID,
			OutputIndex:  0,
			ContentIndex: 0,
			Text:         finalText,
		})
		writeAndFlushEvent("response.output_item.done", ResponseOutputItemDoneEvent{
			Type:        "response.output_item.done",
			ResponseID:  responseID,
			OutputIndex: 0,
			Item:        buildResponsesMessage(messageID, finalText),
		})
		completed := newResponsesResponse(responseID, messageID, model, createdAt, "completed", finalText)
		writeAndFlushEvent("response.completed", completed)
		p.responses.put(completed, conversationInput, responseConversation(conversationInput, finalText))
	}

	if scanErr != nil && !errors.Is(scanErr, context.Canceled) {
		slog.Error("responses stream read error", "response_id", responseID, "error", scanErr)
	}
}

func (p *Proxy) handleResponsesNonStream(w http.ResponseWriter, r *http.Request, responseID, messageID, model string, body []byte, showThinking bool, conversationInput []ChatMessage) {
	ctx, cancel := context.WithTimeout(r.Context(), p.transport.Timeout())
	defer cancel()

	// Extract Authorization token from client request to forward to Fireworks
	authToken := ""
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		authToken = auth[7:]
	}

	reader, err := p.transport.StreamPost(ctx, p.upstreamURL, bytes.NewReader(body), authToken)
	if err != nil {
		slog.Error("upstream responses non-stream error", "response_id", responseID, "error", err)
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream_failed", "upstream error")
		return
	}
	defer reader.Close()

	var result strings.Builder
	isThinking := config.IsThinkingModel(model)
	doneReceived := false

	contentEmitted, scanErr := scanSSEEvents(reader, isThinking, showThinking, func(evt sseContentEvent) bool {
		switch evt.Type {
		case "done":
			doneReceived = true
		case "thinking_separator":
			result.WriteString("\n\n--- Answer ---\n\n")
		case "content":
			result.WriteString(evt.Content)
		}
		return true
	})

	if scanErr != nil {
		if errors.Is(scanErr, context.Canceled) {
			slog.Debug("responses non-stream client disconnected", "response_id", responseID)
			return
		}
		// If we have content, return it even if there was a scanner error
		if !contentEmitted && result.Len() == 0 {
			slog.Error("responses stream read error (upstream incomplete)", "response_id", responseID, "error", scanErr)
			writeError(w, http.StatusBadGateway, "upstream_error", "upstream_incomplete", "%s", scanErr.Error())
			return
		}
		slog.Warn("responses stream read error but content available, returning partial response", "response_id", responseID, "error", scanErr)
	}
	if !doneReceived && !contentEmitted && result.Len() == 0 {
		slog.Error("responses stream ended without done event and no content", "response_id", responseID)
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream_incomplete", "upstream response ended without a completion signal")
		return
	}
	if !doneReceived {
		slog.Debug("responses stream ended without done event but content available, treating as success", "response_id", responseID, "result_len", result.Len())
	}

	completed := newResponsesResponse(responseID, messageID, model, time.Now().Unix(), "completed", result.String())
	p.responses.put(completed, conversationInput, responseConversation(conversationInput, result.String()))
	writeJSON(w, http.StatusOK, completed)
}
