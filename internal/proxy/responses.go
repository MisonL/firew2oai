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
	Tools              json.RawMessage `json:"tools,omitempty"`
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
	ID        string            `json:"id"`
	Object    string            `json:"object"`
	CreatedAt int64             `json:"created_at"`
	Status    string            `json:"status"`
	Model     string            `json:"model"`
	Output    []json.RawMessage `json:"output,omitempty"`
}

type ResponseLifecycleEvent struct {
	Type     string            `json:"type"`
	Response ResponsesResponse `json:"response"`
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
	Type        string          `json:"type"`
	ResponseID  string          `json:"response_id"`
	OutputIndex int             `json:"output_index"`
	Item        json.RawMessage `json:"item"`
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
	Type        string          `json:"type"`
	ResponseID  string          `json:"response_id"`
	OutputIndex int             `json:"output_index"`
	Item        json.RawMessage `json:"item"`
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

func mustMarshalRawJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return json.RawMessage(data)
}

func buildResponsesMessageItem(messageID, text string) json.RawMessage {
	return mustMarshalRawJSON(buildResponsesMessage(messageID, text))
}

func buildResponsesOutput(messageID, text string) []json.RawMessage {
	return []json.RawMessage{buildResponsesMessageItem(messageID, text)}
}

func newResponsesResponse(responseID, messageID, model string, createdAt int64, status, text string) ResponsesResponse {
	return newResponsesResponseWithOutput(responseID, model, createdAt, status, buildResponsesOutput(messageID, text))
}

func newResponsesResponseWithOutput(responseID, model string, createdAt int64, status string, output []json.RawMessage) ResponsesResponse {
	resp := ResponsesResponse{
		ID:        responseID,
		Object:    "response",
		CreatedAt: createdAt,
		Status:    status,
		Model:     model,
	}
	if len(output) > 0 {
		resp.Output = output
	}
	return resp
}

func newResponseLifecycleEvent(eventType string, response ResponsesResponse) ResponseLifecycleEvent {
	return ResponseLifecycleEvent{
		Type:     eventType,
		Response: response,
	}
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

func splitCurrentTurnMessages(current []ChatMessage) ([]ChatMessage, string) {
	lastUser := -1
	for i := len(current) - 1; i >= 0; i-- {
		if current[i].Role == "user" && strings.TrimSpace(current[i].Content) != "" {
			lastUser = i
			break
		}
	}
	if lastUser < 0 {
		return cloneMessages(current), ""
	}

	context := make([]ChatMessage, 0, len(current)-1)
	for i, msg := range current {
		if i == lastUser {
			continue
		}
		context = append(context, msg)
	}
	return context, current[lastUser].Content
}

func extractInputMessages(v any) []ChatMessage {
	switch item := v.(type) {
	case string:
		return []ChatMessage{{Role: "user", Content: item}}
	case map[string]any:
		if messages := extractToolInputMessages(item); len(messages) > 0 {
			return messages
		}
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

func extractToolInputMessages(item map[string]any) []ChatMessage {
	typ, _ := item["type"].(string)
	callID, _ := item["call_id"].(string)

	switch typ {
	case "function_call", "custom_tool_call":
		name, _ := item["name"].(string)
		payload := ""
		if typ == "function_call" {
			if args, ok := item["arguments"].(string); ok {
				payload = args
			}
		} else if input, ok := item["input"].(string); ok {
			payload = input
		}
		return []ChatMessage{{
			Role:    "assistant",
			Content: formatToolCallSummary(name, callID, payload),
		}}
	case "function_call_output", "custom_tool_call_output":
		text, success := extractToolOutputText(item["output"])
		content := formatToolOutputSummary(callID, success, text)
		if content == "" {
			return nil
		}
		return []ChatMessage{{Role: "user", Content: content}}
	default:
		return nil
	}
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

func extractToolOutputText(v any) (string, *bool) {
	switch value := v.(type) {
	case string:
		return value, nil
	case map[string]any:
		var text string
		if content, ok := value["content"].(string); ok {
			text = content
		}
		if text == "" {
			if items, ok := value["content_items"].([]any); ok {
				text = extractTextParts(items)
			}
		}
		var success *bool
		if raw, ok := value["success"].(bool); ok {
			flag := raw
			success = &flag
		}
		return text, success
	default:
		return "", nil
	}
}

func formatToolCallSummary(name, callID, payload string) string {
	var builder strings.Builder
	builder.WriteString("Assistant requested tool")
	if name != "" {
		builder.WriteString(": ")
		builder.WriteString(name)
	}
	if callID != "" {
		builder.WriteString(" (call_id=")
		builder.WriteString(callID)
		builder.WriteByte(')')
	}
	if strings.TrimSpace(payload) != "" {
		builder.WriteString("\nTool payload:\n")
		builder.WriteString(payload)
	}
	return builder.String()
}

func formatToolOutputSummary(callID string, success *bool, text string) string {
	text = strings.TrimSpace(text)
	if callID == "" && success == nil && text == "" {
		return ""
	}

	var builder strings.Builder
	builder.WriteString("Tool result")
	if callID != "" {
		builder.WriteString(" (call_id=")
		builder.WriteString(callID)
		builder.WriteByte(')')
	}
	if success != nil {
		builder.WriteString("\nSuccess: ")
		if *success {
			builder.WriteString("true")
		} else {
			builder.WriteString("false")
		}
	}
	if text != "" {
		builder.WriteString("\nOutput:\n")
		builder.WriteString(text)
	}
	return builder.String()
}

func buildResponsesPrompt(base []ChatMessage, instructions string, current []ChatMessage, tools json.RawMessage) string {
	contextMessages, currentTask := splitCurrentTurnMessages(current)
	toolInstructions := summarizeResponsesTools(tools)

	var builder strings.Builder
	builder.Grow(4096)
	builder.WriteString("You are serving an OpenAI Responses API request through a text-only upstream model.\n")
	builder.WriteString("Follow the base instructions and developer context, but do not reply to them directly.\n")
	builder.WriteString("Treat CURRENT_USER_TASK as the active task for this turn.\n")
	builder.WriteString("Execute the current task immediately. Do not say you are ready, waiting, or asking for a task.\n")
	builder.WriteString("If the current task is a simple text request, answer with the exact result and do not inspect the workspace.\n")
	if currentTask != "" {
		builder.WriteString("\n<CURRENT_USER_TASK>\n")
		builder.WriteString(currentTask)
		builder.WriteString("\n</CURRENT_USER_TASK>\n")
	}
	if toolInstructions != "" {
		builder.WriteString("When a tool is required, reply with exactly one JSON object and no markdown fences or extra prose.\n")
		builder.WriteString("Use only tool names listed in AVAILABLE_TOOLS. Never invent or rename a tool.\n")
		builder.WriteString("Use this format for structured tools:\n")
		builder.WriteString("{\"type\":\"function_call\",\"name\":\"<tool_name>\",\"arguments\":{...}}\n")
		builder.WriteString("Use this format for freeform tools:\n")
		builder.WriteString("{\"type\":\"custom_tool_call\",\"name\":\"<tool_name>\",\"input\":\"<raw input>\"}\n")
	}

	if instructions = strings.TrimSpace(instructions); instructions != "" {
		builder.WriteString("\n<BASE_INSTRUCTIONS>\n")
		builder.WriteString(instructions)
		builder.WriteString("\n</BASE_INSTRUCTIONS>\n")
	}
	if len(base) > 0 {
		builder.WriteString("\n<PREVIOUS_CONVERSATION>\n")
		builder.WriteString(messagesToPrompt(base))
		builder.WriteString("\n</PREVIOUS_CONVERSATION>\n")
	}
	if len(contextMessages) > 0 {
		builder.WriteString("\n<CURRENT_TURN_CONTEXT>\n")
		builder.WriteString(messagesToPrompt(contextMessages))
		builder.WriteString("\n</CURRENT_TURN_CONTEXT>\n")
	}
	if toolInstructions != "" {
		builder.WriteString("\n<AVAILABLE_TOOLS>\n")
		builder.WriteString(toolInstructions)
		builder.WriteString("\n</AVAILABLE_TOOLS>\n")
	}
	return builder.String()
}

func summarizeResponsesTools(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("[]")) {
		return ""
	}

	var tools []map[string]any
	if err := json.Unmarshal(trimmed, &tools); err != nil {
		return ""
	}

	lines := make([]string, 0, len(tools))
	for _, tool := range tools {
		lines = append(lines, summarizeResponseTool(tool)...)
	}
	return strings.Join(lines, "\n")
}

func summarizeResponseTool(tool map[string]any) []string {
	toolType, _ := tool["type"].(string)
	switch toolType {
	case "namespace":
		namespaceName, _ := tool["name"].(string)
		rawTools, _ := tool["tools"].([]any)
		lines := make([]string, 0, len(rawTools))
		for _, rawTool := range rawTools {
			child, ok := rawTool.(map[string]any)
			if !ok {
				continue
			}
			name, _ := child["name"].(string)
			if namespaceName != "" && name != "" {
				child = cloneMap(child)
				child["name"] = namespaceName + "." + name
			}
			lines = append(lines, summarizeResponseTool(child)...)
		}
		return lines
	case "web_search":
		return []string{"- web_search: use for internet search when current information is required."}
	default:
		name, _ := tool["name"].(string)
		if name == "" {
			return nil
		}
		desc, _ := tool["description"].(string)
		desc = truncateString(strings.TrimSpace(desc), 180)
		params := summarizeToolParameters(tool["parameters"])
		line := "- " + name + " [" + toolType + "]"
		if desc != "" {
			line += ": " + desc
		}
		if params != "" {
			line += " Params: " + params
		}
		return []string{line}
	}
}

func summarizeToolParameters(v any) string {
	paramMap, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	props, ok := paramMap["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		return ""
	}

	keys := make([]string, 0, len(props))
	for key := range props {
		keys = append(keys, key)
	}
	if len(keys) > 8 {
		keys = keys[:8]
	}
	return strings.Join(keys, ", ")
}

func cloneMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

type parsedToolCall struct {
	item         json.RawMessage
	conversation ChatMessage
}

func parseToolCallOutput(text string) (*parsedToolCall, bool) {
	candidate := strings.TrimSpace(stripMarkdownCodeFence(text))
	if extracted, ok := extractJSONObject(candidate); ok {
		candidate = extracted
	}
	if candidate == "" || !strings.HasPrefix(candidate, "{") {
		return nil, false
	}

	var raw map[string]any
	if err := json.Unmarshal([]byte(candidate), &raw); err != nil {
		return nil, false
	}

	callType, _ := raw["type"].(string)
	name, _ := raw["name"].(string)
	name = normalizeToolName(name)
	if name == "" {
		return nil, false
	}

	callID := "call_" + strings.Replace(generateRequestID(), "chatcmpl-", "", 1)
	switch callType {
	case "function_call":
		args := raw["arguments"]
		argsText := "{}"
		switch value := args.(type) {
		case string:
			argsText = value
		case nil:
		default:
			data, err := json.Marshal(value)
			if err != nil {
				return nil, false
			}
			argsText = string(data)
		}
		item := mustMarshalRawJSON(map[string]any{
			"type":      "function_call",
			"name":      name,
			"arguments": argsText,
			"call_id":   callID,
		})
		return &parsedToolCall{
			item:         item,
			conversation: ChatMessage{Role: "assistant", Content: formatToolCallSummary(name, callID, argsText)},
		}, true
	case "custom_tool_call":
		input := ""
		if value, ok := raw["input"].(string); ok {
			input = value
		}
		item := mustMarshalRawJSON(map[string]any{
			"type":    "custom_tool_call",
			"name":    name,
			"input":   input,
			"call_id": callID,
		})
		return &parsedToolCall{
			item:         item,
			conversation: ChatMessage{Role: "assistant", Content: formatToolCallSummary(name, callID, input)},
		}, true
	default:
		return nil, false
	}
}

func stripMarkdownCodeFence(text string) string {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "```") {
		return text
	}

	lines := strings.Split(text, "\n")
	if len(lines) < 2 {
		return text
	}
	if strings.HasPrefix(strings.TrimSpace(lines[0]), "```") {
		lines = lines[1:]
	}
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "```" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

func extractJSONObject(text string) (string, bool) {
	start := strings.IndexByte(text, '{')
	if start < 0 {
		return "", false
	}

	depth := 0
	inString := false
	escaped := false
	end := -1
	for i := start; i < len(text); i++ {
		ch := text[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i
			}
		}
	}
	if end < start {
		return "", false
	}
	return text[start : end+1], true
}

func normalizeToolName(name string) string {
	switch name {
	case "run_terminal", "shell", "shell_command":
		return "exec_command"
	default:
		return name
	}
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
	promptMessages := append(cloneMessages(baseMessages), currentMessages...)
	prompt := buildResponsesPrompt(baseMessages, req.Instructions, currentMessages, req.Tools)
	bufferForToolCalls := len(strings.TrimSpace(string(req.Tools))) > 0

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
		"tools_present", bufferForToolCalls,
	)

	if req.Stream {
		p.handleResponsesStream(w, r, responseID, messageID, req.Model, bodyBytes, showThinking, append(cloneMessages(baseMessages), currentMessages...), bufferForToolCalls)
		return
	}
	p.handleResponsesNonStream(w, r, responseID, messageID, req.Model, bodyBytes, showThinking, append(cloneMessages(baseMessages), currentMessages...), bufferForToolCalls)
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

func (p *Proxy) handleResponsesStream(w http.ResponseWriter, r *http.Request, responseID, messageID, model string, body []byte, showThinking bool, conversationInput []ChatMessage, bufferForToolCalls bool) {
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
	created := newResponsesResponse(responseID, messageID, model, createdAt, "in_progress", "")
	if !writeAndFlushEvent("response.created", newResponseLifecycleEvent("response.created", created)) {
		return
	}
	if !bufferForToolCalls && !writeAndFlushEvent("response.output_item.added", ResponseOutputItemAddedEvent{
		Type:        "response.output_item.added",
		ResponseID:  responseID,
		OutputIndex: 0,
		Item:        buildResponsesMessageItem(messageID, ""),
	}) {
		return
	}
	if !bufferForToolCalls && !writeAndFlushEvent("response.content_part.added", ResponseContentPartAddedEvent{
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
			if bufferForToolCalls {
				if toolCall, ok := parseToolCallOutput(finalText); ok {
					completed := newResponsesResponseWithOutput(responseID, model, createdAt, "completed", []json.RawMessage{toolCall.item})
					if !writeAndFlushEvent("response.output_item.done", ResponseOutputItemDoneEvent{
						Type:        "response.output_item.done",
						ResponseID:  responseID,
						OutputIndex: 0,
						Item:        toolCall.item,
					}) {
						return false
					}
					if !writeAndFlushEvent("response.completed", newResponseLifecycleEvent("response.completed", completed)) {
						return false
					}
					p.responses.put(completed, conversationInput, append(cloneMessages(conversationInput), toolCall.conversation))
					return true
				}
				if !writeAndFlushEvent("response.output_item.done", ResponseOutputItemDoneEvent{
					Type:        "response.output_item.done",
					ResponseID:  responseID,
					OutputIndex: 0,
					Item:        buildResponsesMessageItem(messageID, finalText),
				}) {
					return false
				}
				completed := newResponsesResponse(responseID, messageID, model, createdAt, "completed", finalText)
				if !writeAndFlushEvent("response.completed", newResponseLifecycleEvent("response.completed", completed)) {
					return false
				}
				p.responses.put(completed, conversationInput, responseConversation(conversationInput, finalText))
				return true
			}
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
				Item:        buildResponsesMessageItem(messageID, finalText),
			}) {
				return false
			}
			completed := newResponsesResponse(responseID, messageID, model, createdAt, "completed", finalText)
			if !writeAndFlushEvent("response.completed", newResponseLifecycleEvent("response.completed", completed)) {
				return false
			}
			p.responses.put(completed, conversationInput, responseConversation(conversationInput, finalText))
			return true
		case "thinking_separator":
			fallthrough
		case "content":
			delta := evt.Content
			if evt.Type == "thinking_separator" {
				delta = "\n\n--- Answer ---\n\n"
			}
			result.WriteString(delta)
			if bufferForToolCalls {
				return true
			}
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
		if bufferForToolCalls {
			if toolCall, ok := parseToolCallOutput(finalText); ok {
				completed := newResponsesResponseWithOutput(responseID, model, createdAt, "completed", []json.RawMessage{toolCall.item})
				writeAndFlushEvent("response.output_item.done", ResponseOutputItemDoneEvent{
					Type:        "response.output_item.done",
					ResponseID:  responseID,
					OutputIndex: 0,
					Item:        toolCall.item,
				})
				writeAndFlushEvent("response.completed", newResponseLifecycleEvent("response.completed", completed))
				p.responses.put(completed, conversationInput, append(cloneMessages(conversationInput), toolCall.conversation))
				return
			}
			completed := newResponsesResponse(responseID, messageID, model, createdAt, "completed", finalText)
			writeAndFlushEvent("response.output_item.done", ResponseOutputItemDoneEvent{
				Type:        "response.output_item.done",
				ResponseID:  responseID,
				OutputIndex: 0,
				Item:        buildResponsesMessageItem(messageID, finalText),
			})
			writeAndFlushEvent("response.completed", newResponseLifecycleEvent("response.completed", completed))
			p.responses.put(completed, conversationInput, responseConversation(conversationInput, finalText))
			return
		}
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
			Item:        buildResponsesMessageItem(messageID, finalText),
		})
		completed := newResponsesResponse(responseID, messageID, model, createdAt, "completed", finalText)
		writeAndFlushEvent("response.completed", newResponseLifecycleEvent("response.completed", completed))
		p.responses.put(completed, conversationInput, responseConversation(conversationInput, finalText))
	}

	if scanErr != nil && !errors.Is(scanErr, context.Canceled) {
		slog.Error("responses stream read error", "response_id", responseID, "error", scanErr)
	}
}

func (p *Proxy) handleResponsesNonStream(w http.ResponseWriter, r *http.Request, responseID, messageID, model string, body []byte, showThinking bool, conversationInput []ChatMessage, bufferForToolCalls bool) {
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

	finalText := result.String()
	createdAt := time.Now().Unix()
	if bufferForToolCalls {
		if toolCall, ok := parseToolCallOutput(finalText); ok {
			completed := newResponsesResponseWithOutput(responseID, model, createdAt, "completed", []json.RawMessage{toolCall.item})
			p.responses.put(completed, conversationInput, append(cloneMessages(conversationInput), toolCall.conversation))
			writeJSON(w, http.StatusOK, completed)
			return
		}
	}

	completed := newResponsesResponse(responseID, messageID, model, createdAt, "completed", finalText)
	p.responses.put(completed, conversationInput, responseConversation(conversationInput, finalText))
	writeJSON(w, http.StatusOK, completed)
}
