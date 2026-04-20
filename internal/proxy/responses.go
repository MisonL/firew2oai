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
	"unicode/utf8"

	"github.com/mison/firew2oai/internal/config"
)

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
	messages, _, err := responseInputToMessagesAndItems(input)
	return messages, err
}

func responseInputToMessagesAndItems(input json.RawMessage) ([]ChatMessage, []json.RawMessage, error) {
	messages := make([]ChatMessage, 0, 4)
	items := make([]json.RawMessage, 0, 4)

	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 {
		return nil, nil, errors.New("input is required")
	}

	var raw any
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return nil, nil, fmt.Errorf("parse input: %w", err)
	}

	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, nil, errors.New("input is required")
		}
		messages = append(messages, ChatMessage{Role: "user", Content: v})
		items = append(items, buildInputMessageItem("user", v))
	case []any:
		for _, item := range v {
			extracted := extractInputMessages(item)
			if len(extracted) == 0 {
				continue
			}
			messages = append(messages, extracted...)
			if rawItem, ok := normalizeRawResponseInputItem(item); ok {
				items = append(items, rawItem)
			}
		}
	default:
		return nil, nil, errors.New("input must be a string or array")
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
		return nil, nil, errors.New("input must contain at least one text item")
	}
	return filtered, items, nil
}

func buildInputMessageItem(role, text string) json.RawMessage {
	return mustMarshalRawJSON(map[string]any{
		"type": "message",
		"role": role,
		"content": []map[string]string{
			{"type": "input_text", "text": text},
		},
	})
}

func normalizeRawResponseInputItem(item any) (json.RawMessage, bool) {
	switch value := item.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return nil, false
		}
		return buildInputMessageItem("user", value), true
	case map[string]any:
		data, err := json.Marshal(value)
		if err != nil {
			return nil, false
		}
		return json.RawMessage(data), true
	default:
		return nil, false
	}
}

func rawItemsToMessages(items []json.RawMessage) []ChatMessage {
	messages := make([]ChatMessage, 0, len(items))
	for _, raw := range items {
		var item any
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		messages = append(messages, extractInputMessages(item)...)
	}
	filtered := messages[:0]
	for _, msg := range messages {
		if strings.TrimSpace(msg.Content) != "" {
			filtered = append(filtered, msg)
		}
	}
	return filtered
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
	case []any:
		return extractTextParts(value), nil
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

type responsesPromptOptions struct {
	CompactForFinalize  bool
	SuppressMetaContext bool
}

func buildResponsesPrompt(base []ChatMessage, instructions string, current []ChatMessage, tools json.RawMessage, maxToolCalls int, options responsesPromptOptions) string {
	contextMessages, fallbackTask := splitCurrentTurnMessages(current)
	combined := append(cloneMessages(base), current...)
	currentTask := stableActionableUserTask(combined)
	if currentTask == "" {
		currentTask = fallbackTask
	} else if strings.TrimSpace(fallbackTask) != "" && strings.TrimSpace(fallbackTask) != strings.TrimSpace(currentTask) {
		// Keep tool-output user messages in turn context while retaining the original actionable task.
		contextMessages = append(contextMessages, ChatMessage{Role: "user", Content: fallbackTask})
	}
	toolInstructions := summarizeResponsesTools(tools)
	toolCatalog := buildResponseToolCatalog(tools)
	requiresToolLoop := toolInstructions != "" && taskLikelyNeedsTools(currentTask)
	baseMessages := cloneMessages(base)
	if options.SuppressMetaContext || options.CompactForFinalize {
		baseMessages = filterPromptMetaMessages(baseMessages)
		contextMessages = filterPromptMetaMessages(contextMessages)
	}

	var builder strings.Builder
	builder.Grow(4096)
	builder.WriteString("You are serving an OpenAI Responses API request through a text-only upstream model.\n")
	builder.WriteString("Follow the base instructions and developer context, but do not reply to them directly.\n")
	builder.WriteString("Treat CURRENT_USER_TASK as the active task for this turn.\n")
	builder.WriteString("Only CURRENT_USER_TASK is the target output. Never summarize repository guidelines or instruction blocks as the final answer.\n")
	builder.WriteString("Execute the current task immediately. Do not say you are ready, waiting, or asking for a task.\n")
	builder.WriteString("Never ask for additional task context when CURRENT_USER_TASK is already present.\n")
	builder.WriteString("Do not return repository overviews unless CURRENT_USER_TASK explicitly asks for an overview.\n")
	builder.WriteString("If CURRENT_USER_TASK defines an exact output format, follow it exactly and output nothing extra.\n")
	builder.WriteString("If the current task is a simple text request, answer with the exact result and do not inspect the workspace.\n")
	builder.WriteString("If CURRENT_USER_TASK names specific files or commands, call tools for those targets first and avoid unrelated exploration.\n")
	if options.CompactForFinalize {
		builder.WriteString("Finalize stage reached. Ignore handoff, checkpoint, and readiness chatter from prior turns.\n")
		builder.WriteString("Use CURRENT_USER_TASK and EXECUTION_EVIDENCE to produce the final answer.\n")
		builder.WriteString("Do not acknowledge session state or ask what task to work on.\n")
		if formatBlock := buildFinalizeOutputFormatBlock(currentTask); formatBlock != "" {
			builder.WriteString(formatBlock)
		}
	}
	if requiresToolLoop {
		builder.WriteString("CURRENT_USER_TASK requires real workspace execution. Emit tool calls before any final answer text.\n")
		builder.WriteString("Do not stop after read-only inspection when the task still requires edits or tests.\n")
	}
	if gate := buildTaskCompletionGate(currentTask); gate != "" {
		builder.WriteString(gate)
	}
	if currentTask != "" {
		builder.WriteString("\n<CURRENT_USER_TASK>\n")
		builder.WriteString(currentTask)
		builder.WriteString("\n</CURRENT_USER_TASK>\n")
	}
	if toolInstructions != "" {
		appendToolProtocolInstructionsForCatalog(&builder, true, maxToolCalls, toolCatalog)
	}

	if instructions = strings.TrimSpace(instructions); instructions != "" {
		builder.WriteString("\n<BASE_INSTRUCTIONS>\n")
		builder.WriteString(instructions)
		builder.WriteString("\n</BASE_INSTRUCTIONS>\n")
	}
	if !options.CompactForFinalize && len(baseMessages) > 0 {
		builder.WriteString("\n<PREVIOUS_CONVERSATION>\n")
		builder.WriteString(messagesToPrompt(baseMessages))
		builder.WriteString("\n</PREVIOUS_CONVERSATION>\n")
	}
	if !options.CompactForFinalize && len(contextMessages) > 0 {
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

func buildFinalizeOutputFormatBlock(task string) string {
	labels := dedupePreserveOrder(extractRequiredOutputLabels(task))
	if len(labels) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n<FINAL_OUTPUT_FORMAT>\n")
	b.WriteString("Output exactly these labels in this order, one line per label, with no preface, no markdown, and no extra lines:\n")
	for _, label := range labels {
		b.WriteString(label)
		b.WriteString(": <value>\n")
	}
	b.WriteString("</FINAL_OUTPUT_FORMAT>\n")
	return b.String()
}

func filterPromptMetaMessages(messages []ChatMessage) []ChatMessage {
	if len(messages) == 0 {
		return nil
	}
	filtered := make([]ChatMessage, 0, len(messages))
	for _, msg := range messages {
		if isPromptMetaNoiseMessage(msg) {
			continue
		}
		filtered = append(filtered, msg)
	}
	return filtered
}

func isPromptMetaNoiseMessage(msg ChatMessage) bool {
	text := strings.TrimSpace(msg.Content)
	if text == "" {
		return false
	}
	if isToolResultSummaryMessage(text) || strings.Contains(text, "Assistant requested tool") {
		return false
	}

	lower := strings.ToLower(text)
	metaMarkers := []string{
		"context handoff",
		"handoff summary",
		"checkpoint handoff",
		"checkpoint compaction",
		"compaction request",
		"fresh session",
		"no prior work in progress",
		"no active task context",
		"previous model",
		"ready to assist",
		"ready to help",
		"provide the specific task",
		"what would you like me to work on",
		"what would you like me to work on?",
		"what task would you like",
		"ongoing work",
		"reviewed the handoff",
		"received the context handoff",
		"received the context checkpoint handoff",
		"交接",
		"检查点",
		"准备好协助",
		"提供具体任务",
		"你想让我做什么",
	}
	for _, marker := range metaMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
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

type responseToolDescriptor struct {
	Name       string
	Type       string
	Structured bool
}

type parsedToolCallResult struct {
	call           *parsedToolCall
	candidateFound bool
	err            error
}

func parseToolCallOutput(text string, allowedTools map[string]responseToolDescriptor, requiredTool string) parsedToolCallResult {
	batch := parseToolCallOutputs(text, allowedTools, requiredTool)
	result := parsedToolCallResult{
		candidateFound: batch.candidateFound,
		err:            batch.err,
	}
	if len(batch.calls) > 1 {
		result.candidateFound = true
		result.err = errors.New("multiple tool calls require parseToolCallOutputs")
		return result
	}
	if len(batch.calls) == 1 {
		result.call = &batch.calls[0]
	}
	return result
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
	trimmed := strings.TrimSpace(name)
	switch strings.ToLower(trimmed) {
	case "run_terminal", "run_terminal_cmd", "run_command", "shell", "shell_command", "bash", "terminal", "terminal_exec", "read_file", "readfile", "cat", "list_files", "listfiles", "execute_command":
		return "exec_command"
	default:
		return trimmed
	}
}

func buildResponseInputItemList(items []json.RawMessage) ResponseInputItemList {
	list := ResponseInputItemList{
		Object:  "list",
		Data:    items,
		HasMore: false,
	}
	if len(items) > 0 {
		list.FirstID = rawItemID(items[0])
		list.LastID = rawItemID(items[len(items)-1])
	}
	return list
}

func rawItemID(item json.RawMessage) string {
	if len(item) == 0 {
		return ""
	}
	var decoded map[string]any
	if err := json.Unmarshal(item, &decoded); err != nil {
		return ""
	}
	if id, _ := decoded["id"].(string); id != "" {
		return id
	}
	if callID, _ := decoded["call_id"].(string); callID != "" {
		return callID
	}
	return ""
}

func buildHistoryItems(baseHistory, requestItems, outputItems []json.RawMessage) []json.RawMessage {
	history := make([]json.RawMessage, 0, len(baseHistory)+len(requestItems)+len(outputItems))
	history = append(history, cloneRawItems(baseHistory)...)
	history = append(history, cloneRawItems(requestItems)...)
	history = append(history, cloneRawItems(outputItems)...)
	return history
}

func shouldRetryEmptyResponsesAttempt(doneReceived, contentEmitted bool, resultLen int, scanErr error, attempt int, clientGone bool) bool {
	if attempt > 0 {
		return false
	}
	if clientGone || errors.Is(scanErr, context.Canceled) {
		return false
	}
	return !doneReceived && !contentEmitted && resultLen == 0
}

func estimateResponseUsage(inputItems, outputItems []json.RawMessage) *ResponseUsage {
	inputTokens := estimateMessagesTokenCount(rawItemsToMessages(inputItems))
	outputTokens := estimateMessagesTokenCount(rawItemsToMessages(outputItems))
	return &ResponseUsage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
	}
}

func estimateMessagesTokenCount(messages []ChatMessage) int {
	total := 0
	for _, msg := range messages {
		total += estimateTokenCount(msg.Content)
	}
	return total
}

func estimateTokenCount(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}

	estimate := len(strings.Fields(text))
	charEstimate := utf8.RuneCountInString(text) / 4
	if utf8.RuneCountInString(text)%4 != 0 {
		charEstimate++
	}
	if charEstimate > estimate {
		estimate = charEstimate
	}
	if estimate < 1 {
		return 1
	}
	return estimate
}

func newCompletedResponse(responseID, messageID, model string, createdAt int64, outputItems, inputItems []json.RawMessage) ResponsesResponse {
	resp := newResponsesResponseWithOutput(responseID, model, createdAt, "completed", outputItems)
	resp.Usage = estimateResponseUsage(inputItems, outputItems)
	if len(resp.Output) == 0 && messageID != "" {
		resp.Output = buildResponsesOutput(messageID, "")
	}
	return resp
}

// handleResponses exposes a minimal OpenAI Responses-compatible endpoint.
func buildResponseToolCatalog(raw json.RawMessage) map[string]responseToolDescriptor {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("[]")) {
		return nil
	}

	var tools []map[string]any
	if err := json.Unmarshal(trimmed, &tools); err != nil {
		return nil
	}

	catalog := make(map[string]responseToolDescriptor)
	var walk func(prefix string, tool map[string]any)
	walk = func(prefix string, tool map[string]any) {
		toolType, _ := tool["type"].(string)
		if toolType == "namespace" {
			namespaceName := prefix
			if name, _ := tool["name"].(string); name != "" {
				namespaceName = name
			}
			rawChildren, _ := tool["tools"].([]any)
			for _, child := range rawChildren {
				childMap, ok := child.(map[string]any)
				if !ok {
					continue
				}
				childCopy := cloneMap(childMap)
				if name, _ := childCopy["name"].(string); namespaceName != "" && name != "" {
					childCopy["name"] = namespaceName + "." + name
				}
				walk(namespaceName, childCopy)
			}
			return
		}

		name, _ := tool["name"].(string)
		if name == "" {
			return
		}
		catalog[name] = responseToolDescriptor{
			Name:       name,
			Type:       toolType,
			Structured: toolType != "custom",
		}
	}

	for _, tool := range tools {
		walk("", tool)
	}
	return catalog
}

type resolvedToolChoice struct {
	RequiredTool string
	DisableTools bool
	RequireTool  bool
}

func validateToolChoiceConfiguration(toolChoice resolvedToolChoice, toolCatalog map[string]responseToolDescriptor) error {
	if !toolChoice.RequireTool {
		return nil
	}
	if len(toolCatalog) == 0 {
		if toolChoice.RequiredTool != "" {
			return fmt.Errorf("tool_choice requires declared tool %q, but tools array is empty", toolChoice.RequiredTool)
		}
		return errors.New("tool_choice requires at least one declared tool in tools")
	}
	if toolChoice.RequiredTool != "" {
		if _, ok := toolCatalog[toolChoice.RequiredTool]; !ok {
			return fmt.Errorf("tool_choice requires declared tool %q, but it is missing from tools", toolChoice.RequiredTool)
		}
	}
	return nil
}

func toolsForPrompt(raw json.RawMessage, toolChoice resolvedToolChoice) json.RawMessage {
	if toolChoice.DisableTools {
		return nil
	}
	return raw
}

func resolveToolChoice(toolChoice json.RawMessage) resolvedToolChoice {
	trimmed := bytes.TrimSpace(toolChoice)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return resolvedToolChoice{}
	}
	if bytes.Equal(trimmed, []byte(`"none"`)) {
		return resolvedToolChoice{DisableTools: true}
	}

	var decoded any
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return resolvedToolChoice{}
	}
	switch value := decoded.(type) {
	case string:
		if value == "none" {
			return resolvedToolChoice{DisableTools: true}
		}
		if value == "required" {
			return resolvedToolChoice{RequireTool: true}
		}
		return resolvedToolChoice{}
	case map[string]any:
		name, _ := value["name"].(string)
		if name == "" {
			return resolvedToolChoice{}
		}
		return resolvedToolChoice{RequiredTool: name, RequireTool: true}
	default:
		return resolvedToolChoice{}
	}
}

func shouldDisableToolsForExecutionFinalize(policy executionPolicy, toolChoice resolvedToolChoice) bool {
	if !policy.Enabled || policy.Stage != "finalize" {
		return false
	}
	if toolChoice.DisableTools {
		return true
	}
	if toolChoice.RequireTool || toolChoice.RequiredTool != "" {
		return false
	}
	return true
}

func buildToolChoiceInstructions(toolChoice json.RawMessage) string {
	trimmed := bytes.TrimSpace(toolChoice)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if bytes.Equal(trimmed, []byte(`"none"`)) {
		return "Do not call any tools. Answer with plain text only."
	}

	var decoded any
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return ""
	}
	switch value := decoded.(type) {
	case string:
		switch value {
		case "none":
			return "Do not call any tools. Answer with plain text only."
		case "required":
			return "You must end your reply with an AI_ACTIONS block whose mode is tool."
		default:
			return ""
		}
	case map[string]any:
		name, _ := value["name"].(string)
		if name == "" {
			return ""
		}
		return fmt.Sprintf("You must end your reply with an AI_ACTIONS block whose mode is tool, and every call name must be %q.", normalizeToolName(name))
	default:
		return ""
	}
}

func buildPendingWriteMutationHint(policy executionPolicy, toolCatalog map[string]responseToolDescriptor) string {
	if !policy.PendingWrite || len(toolCatalog) == 0 {
		return ""
	}

	mutationTools := availableMutationToolNames(toolCatalog)
	var b strings.Builder
	b.WriteString("\n<WRITE_MUTATION_HINT>\n")
	b.WriteString("This task still requires a real file mutation before final output.\n")
	if len(policy.MissingFiles) > 0 {
		b.WriteString("Missing target files that should be created now:\n")
		for _, filePath := range policy.MissingFiles {
			b.WriteString("- ")
			b.WriteString(filePath)
			b.WriteByte('\n')
		}
	}
	if len(policy.EmptyFiles) > 0 {
		b.WriteString("These target files already exist but are still empty. Repeating scaffold-only commands is not enough:\n")
		for _, filePath := range policy.EmptyFiles {
			b.WriteString("- ")
			b.WriteString(filePath)
			b.WriteByte('\n')
		}
	}
	if len(policy.RepeatedScaffold) > 0 {
		b.WriteString("Repeated scaffold-only commands were already observed for these files. Do not run mkdir/touch again:\n")
		for _, filePath := range policy.RepeatedScaffold {
			b.WriteString("- ")
			b.WriteString(filePath)
			b.WriteByte('\n')
		}
	}
	if len(mutationTools) > 0 {
		b.WriteString("Available mutation tools:\n")
		for _, name := range mutationTools {
			b.WriteString("- ")
			b.WriteString(name)
			b.WriteByte('\n')
		}
		b.WriteString("Do not call sed/cat/read_file/list_files again for already inspected or missing targets. Emit exactly one mutation tool call now.\n")
	} else if _, ok := toolCatalog["exec_command"]; ok {
		if policy.AllRequiredFilesSeen {
			b.WriteString("All required files have already been inspected.\n")
		}
		b.WriteString("No dedicated file mutation tool is declared. Use exec_command with a shell write/edit command now.\n")
		b.WriteString("Valid exec_command patterns include cat > file <<'EOF', python - <<'PY', perl -0pi -e, or printf ... > file.\n")
		b.WriteString("Invalid now: pwd, ls, cat, sed -n, head, tail, rg, grep, or any other read-only command.\n")
		b.WriteString("Emit exactly one exec_command call whose cmd mutates the target file now.\n")
	} else {
		return ""
	}
	b.WriteString("</WRITE_MUTATION_HINT>\n")
	return b.String()
}

func availableMutationToolNames(toolCatalog map[string]responseToolDescriptor) []string {
	if len(toolCatalog) == 0 {
		return nil
	}

	preferred := []string{
		"apply_patch",
		"write_file",
		"edit_file",
		"replace_in_file",
		"append_file",
		"create_file",
	}
	names := make([]string, 0, len(preferred))
	seen := make(map[string]struct{}, len(preferred))
	for _, name := range preferred {
		if _, ok := toolCatalog[name]; !ok {
			continue
		}
		names = append(names, name)
		seen[name] = struct{}{}
	}
	for name := range toolCatalog {
		if _, ok := seen[name]; ok {
			continue
		}
		if !isMutationToolName(name) {
			continue
		}
		names = append(names, name)
	}
	return names
}

func preferredPendingWriteTool(preferredMutationTools []string) string {
	if len(preferredMutationTools) == 0 {
		return ""
	}

	for _, candidate := range []string{
		"write_file",
		"edit_file",
		"replace_in_file",
		"append_file",
		"create_file",
		"apply_patch",
	} {
		for _, name := range preferredMutationTools {
			if name == candidate {
				return candidate
			}
		}
	}
	return preferredMutationTools[0]
}

func summarizeTaskForLog(task string) string {
	normalized := strings.Join(strings.Fields(strings.TrimSpace(task)), " ")
	if normalized == "" {
		return ""
	}
	return truncateString(normalized, 260)
}

func compactCommandsForLog(commands []string) []string {
	if len(commands) == 0 {
		return nil
	}
	compacted := make([]string, 0, len(commands))
	for _, command := range commands {
		normalized := strings.Join(strings.Fields(strings.TrimSpace(command)), " ")
		if normalized == "" {
			continue
		}
		compacted = append(compacted, truncateString(normalized, 160))
	}
	if len(compacted) == 0 {
		return nil
	}
	return compacted
}

func writeResponsesMessageAdded(writeAndFlushEvent func(string, any) bool, responseID, messageID string) bool {
	if !writeAndFlushEvent("response.output_item.added", ResponseOutputItemAddedEvent{
		Type:        "response.output_item.added",
		ResponseID:  responseID,
		OutputIndex: 0,
		Item:        buildResponsesMessageItem(messageID, ""),
	}) {
		return false
	}
	return writeAndFlushEvent("response.content_part.added", ResponseContentPartAddedEvent{
		Type:         "response.content_part.added",
		ResponseID:   responseID,
		ItemID:       messageID,
		OutputIndex:  0,
		ContentIndex: 0,
		Part:         ResponseOutputText{Type: "output_text", Text: ""},
	})
}

func writeResponsesMessageDone(writeAndFlushEvent func(string, any) bool, responseID, messageID, text string) bool {
	if !writeAndFlushEvent("response.output_text.done", ResponseOutputTextDoneEvent{
		Type:         "response.output_text.done",
		ResponseID:   responseID,
		ItemID:       messageID,
		OutputIndex:  0,
		ContentIndex: 0,
		Text:         text,
	}) {
		return false
	}
	return writeAndFlushEvent("response.output_item.done", ResponseOutputItemDoneEvent{
		Type:        "response.output_item.done",
		ResponseID:  responseID,
		OutputIndex: 0,
		Item:        buildResponsesMessageItem(messageID, text),
	})
}

func buildToolProtocolErrorMessage(err error, upstreamText string) string {
	var builder strings.Builder
	builder.WriteString("Codex adapter error: ")
	builder.WriteString(err.Error())
	if trimmed := strings.TrimSpace(upstreamText); trimmed != "" {
		builder.WriteString("\n\nUpstream output:\n")
		builder.WriteString(trimmed)
	}
	return builder.String()
}

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

	currentMessages, requestItems, err := responseInputToMessagesAndItems(req.Input)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_input", "%s", err.Error())
		return
	}

	baseMessages := []ChatMessage(nil)
	baseHistoryItems := []json.RawMessage(nil)
	if req.PreviousResponseID != "" {
		entry, ok := p.responses.get(req.PreviousResponseID)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "previous_response_not_found", "previous_response_id %q was not found", req.PreviousResponseID)
			return
		}
		baseHistoryItems = entry.historyItems
		baseMessages = rawItemsToMessages(baseHistoryItems)
	}

	responseID := generateResponsesID()
	messageID := generateResponseMessageID()
	showThinking := resolveShowThinking(p.defaultShowThinking, req.ShowThinking)
	allowParallelToolCalls := req.ParallelToolCalls == nil || *req.ParallelToolCalls
	maxToolCalls := 0
	if !allowParallelToolCalls {
		maxToolCalls = 1
	}
	promptMessages := append(cloneMessages(baseMessages), currentMessages...)
	toolCatalog := buildResponseToolCatalog(req.Tools)
	toolChoice := resolveToolChoice(req.ToolChoice)
	if err := validateToolChoiceConfiguration(toolChoice, toolCatalog); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_tool_choice", "%s", err.Error())
		return
	}
	currentTask := stableActionableUserTask(promptMessages)
	autoRequireTool := len(toolCatalog) > 0 && !toolChoice.DisableTools && !toolChoice.RequireTool && taskLikelyNeedsTools(currentTask)
	policyHistoryItems := buildHistoryItems(baseHistoryItems, requestItems, nil)
	executionEvidence := buildExecutionEvidence(policyHistoryItems)
	executionPolicy := buildExecutionPolicy(req.Model, currentTask, policyHistoryItems, len(toolCatalog) > 0, toolChoice.DisableTools, autoRequireTool)
	disableToolsForFinalize := shouldDisableToolsForExecutionFinalize(executionPolicy, toolChoice)
	effectiveToolChoice := toolChoice
	if disableToolsForFinalize {
		effectiveToolChoice.DisableTools = true
	}
	if maxToolCalls == 0 && executionPolicy.ForceSingleToolCall {
		maxToolCalls = 1
	}
	promptTools := toolsForPrompt(req.Tools, effectiveToolChoice)
	prompt := buildResponsesPrompt(baseMessages, req.Instructions, currentMessages, promptTools, maxToolCalls, responsesPromptOptions{
		CompactForFinalize:  disableToolsForFinalize,
		SuppressMetaContext: len(toolCatalog) > 0 && taskLikelyNeedsTools(currentTask),
	})
	if policyBlock := buildExecutionPolicyPromptBlock(executionPolicy); policyBlock != "" {
		prompt += policyBlock
	}
	if writeHint := buildPendingWriteMutationHint(executionPolicy, toolCatalog); writeHint != "" {
		prompt += writeHint
	}
	if evidenceBlock := buildExecutionEvidencePromptBlock(executionEvidence); evidenceBlock != "" {
		prompt += evidenceBlock
	}
	toolConstraints := toolProtocolConstraints{
		RequiredTool:       toolChoice.RequiredTool,
		RequireTool:        toolChoice.RequireTool || executionPolicy.RequireTool,
		PreferredToolNames: nil,
		MaxCalls:           maxToolCalls,
		AllowTruncateToMax: executionPolicy.AllowTruncateToMax,
	}
	if executionPolicy.PendingWrite && toolChoice.RequiredTool == "" && !toolChoice.DisableTools {
		preferredMutationTools := availableMutationToolNames(toolCatalog)
		toolConstraints.PreferredToolNames = preferredMutationTools
		if executionPolicy.AllRequiredFilesSeen {
			if forced := preferredPendingWriteTool(preferredMutationTools); forced != "" {
				toolConstraints.RequiredTool = forced
				toolConstraints.RequireTool = true
			}
		}
	}
	if disableToolsForFinalize {
		toolConstraints.RequiredTool = ""
		toolConstraints.RequireTool = false
		toolConstraints.PreferredToolNames = nil
	}
	toolChoiceInstructions := buildToolChoiceInstructions(req.ToolChoice)
	if toolChoiceInstructions == "" && autoRequireTool && executionPolicy.RequireTool {
		toolChoiceInstructions = "This task requires real workspace operations. Do not emit mode final until required edits, commands, and output constraints are satisfied."
	}
	if toolChoiceInstructions == "" && disableToolsForFinalize {
		toolChoiceInstructions = "Execution policy reached finalize stage. Do not call any tools. Return plain text final answer now."
	}
	if toolChoiceInstructions != "" {
		prompt += "\n<TOOL_CHOICE>\n" + toolChoiceInstructions + "\n</TOOL_CHOICE>\n"
	}
	bufferForToolCalls := len(toolCatalog) > 0 && !effectiveToolChoice.DisableTools
	promptInputItems := buildHistoryItems(baseHistoryItems, requestItems, nil)

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
		"required_tool", toolConstraints.RequiredTool,
		"require_tool", toolConstraints.RequireTool,
		"auto_require_tool", autoRequireTool,
		"max_tool_calls", toolConstraints.MaxCalls,
		"execution_stage", executionPolicy.Stage,
		"execution_read_loop", executionPolicy.ReadLoop,
		"execution_next_command", executionPolicy.NextCommand,
		"task_summary", summarizeTaskForLog(currentTask),
		"task_write_targets", extractWriteTargetFiles(currentTask),
		"task_required_commands", compactCommandsForLog(executionPolicy.RequiredCommands),
		"execution_required_files", executionPolicy.RequiredFiles,
		"execution_missing_files", executionPolicy.MissingFiles,
		"execution_empty_files", executionPolicy.EmptyFiles,
		"execution_repeated_scaffold", executionPolicy.RepeatedScaffold,
		"execution_pending_write", executionPolicy.PendingWrite,
		"execution_disable_tools", disableToolsForFinalize,
	)

	if req.Stream {
		p.handleResponsesStream(w, r, responseID, messageID, req.Model, bodyBytes, showThinking, requestItems, baseHistoryItems, promptInputItems, toolCatalog, toolConstraints, bufferForToolCalls, executionPolicy, currentTask, executionEvidence, len(toolCatalog) > 0)
		return
	}
	p.handleResponsesNonStream(w, r, responseID, messageID, req.Model, bodyBytes, showThinking, requestItems, baseHistoryItems, promptInputItems, toolCatalog, toolConstraints, bufferForToolCalls, executionPolicy, currentTask, executionEvidence, len(toolCatalog) > 0)
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
		writeJSON(w, http.StatusOK, buildResponseInputItemList(entry.requestItems))
		return
	}

	entry, ok := p.responses.get(path)
	if !ok {
		writeError(w, http.StatusNotFound, "invalid_request_error", "response_not_found", "response %q was not found", path)
		return
	}
	writeJSON(w, http.StatusOK, entry.response)
}

func (p *Proxy) handleResponsesStream(w http.ResponseWriter, r *http.Request, responseID, messageID, model string, body []byte, showThinking bool, requestItems, baseHistoryItems, promptInputItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor, toolConstraints toolProtocolConstraints, bufferForToolCalls bool, executionPolicy executionPolicy, currentTask string, evidence executionEvidence, checkControlMarkup bool) {
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
		slog.Error("upstream responses stream error", "response_id", responseID, "error", err)
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream_failed", "upstream error")
		return
	}

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
	if !bufferForToolCalls && !writeResponsesMessageAdded(writeAndFlushEvent, responseID, messageID) {
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
				finalText := result.String()
				if bufferForToolCalls {
					parseResult := parseToolCallOutputsWithConstraints(finalText, toolCatalog, toolConstraints)
					parseResult = applyExecutionPolicyToParseResult(parseResult, executionPolicy, toolCatalog, toolConstraints)
					slog.Info("tool protocol outcome",
						"api", "responses",
						"response_id", responseID,
						"mode", parseResult.mode,
						"tool_calls", len(parseResult.calls),
						"required_tool", toolConstraints.RequiredTool,
						"require_tool", toolConstraints.RequireTool,
						"max_tool_calls", toolConstraints.MaxCalls,
						"execution_stage", executionPolicy.Stage,
						"execution_read_loop", executionPolicy.ReadLoop,
						"execution_next_command", executionPolicy.NextCommand,
						"task_summary", summarizeTaskForLog(currentTask),
						"execution_required_files", executionPolicy.RequiredFiles,
						"execution_empty_files", executionPolicy.EmptyFiles,
						"execution_repeated_scaffold", executionPolicy.RepeatedScaffold,
						"error", toolProtocolErrorString(parseResult.err),
					)
					if parseResult.err == nil && len(parseResult.calls) > 0 {
						outputItems := buildParsedToolOutputItems(parseResult.calls)
						completed := newCompletedResponse(responseID, messageID, model, createdAt, outputItems, promptInputItems)
						for index, item := range outputItems {
							if !writeAndFlushEvent("response.output_item.added", ResponseOutputItemAddedEvent{
								Type:        "response.output_item.added",
								ResponseID:  responseID,
								OutputIndex: index,
								Item:        item,
							}) {
								return false
							}
						}
						for index, item := range outputItems {
							if !writeAndFlushEvent("response.output_item.done", ResponseOutputItemDoneEvent{
								Type:        "response.output_item.done",
								ResponseID:  responseID,
								OutputIndex: index,
								Item:        item,
							}) {
								return false
							}
						}
						if !writeAndFlushEvent("response.completed", newResponseLifecycleEvent("response.completed", completed)) {
							return false
						}
						p.responses.put(completed, requestItems, buildHistoryItems(baseHistoryItems, requestItems, outputItems))
						return true
					}
					if parseResult.err != nil {
						finalText = buildToolProtocolErrorMessage(parseResult.err, finalText)
					} else {
						finalText = parseResult.visibleText
					}
					finalText = constrainFinalText(currentTask, finalText, evidence, checkControlMarkup)
					outputItems := []json.RawMessage{buildResponsesMessageItem(messageID, finalText)}
					if !writeResponsesMessageAdded(writeAndFlushEvent, responseID, messageID) {
						return false
					}
					if !writeResponsesMessageDone(writeAndFlushEvent, responseID, messageID, finalText) {
						return false
					}
					completed := newCompletedResponse(responseID, messageID, model, createdAt, outputItems, promptInputItems)
					if !writeAndFlushEvent("response.completed", newResponseLifecycleEvent("response.completed", completed)) {
						return false
					}
					p.responses.put(completed, requestItems, buildHistoryItems(baseHistoryItems, requestItems, outputItems))
					return true
				}
				finalText = constrainFinalText(currentTask, finalText, evidence, checkControlMarkup)
				outputItems := []json.RawMessage{buildResponsesMessageItem(messageID, finalText)}
				if !writeResponsesMessageDone(writeAndFlushEvent, responseID, messageID, finalText) {
					return false
				}
				completed := newCompletedResponse(responseID, messageID, model, createdAt, outputItems, promptInputItems)
				if !writeAndFlushEvent("response.completed", newResponseLifecycleEvent("response.completed", completed)) {
					return false
				}
				p.responses.put(completed, requestItems, buildHistoryItems(baseHistoryItems, requestItems, outputItems))
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
		_ = reader.Close()

		if shouldRetryEmptyResponsesAttempt(doneReceived, contentEmitted, result.Len(), scanErr, attempt, clientGone) {
			attempt++
			slog.Warn("responses stream empty attempt, retrying once", "response_id", responseID, "attempt", attempt, "error", scanErr)
			reader, err = openReader()
			if err != nil {
				scanErr = err
				break
			}
			continue
		}
		break
	}

	// If no done event but content was emitted, send completion events
	if !doneReceived && (contentEmitted || result.Len() > 0) {
		slog.Debug("responses stream ended without done event but content available", "response_id", responseID, "result_len", result.Len())
		finalText := result.String()
		if bufferForToolCalls {
			parseResult := parseToolCallOutputsWithConstraints(finalText, toolCatalog, toolConstraints)
			parseResult = applyExecutionPolicyToParseResult(parseResult, executionPolicy, toolCatalog, toolConstraints)
			slog.Info("tool protocol outcome",
				"api", "responses",
				"response_id", responseID,
				"mode", parseResult.mode,
				"tool_calls", len(parseResult.calls),
				"required_tool", toolConstraints.RequiredTool,
				"require_tool", toolConstraints.RequireTool,
				"max_tool_calls", toolConstraints.MaxCalls,
				"execution_stage", executionPolicy.Stage,
				"execution_read_loop", executionPolicy.ReadLoop,
				"execution_next_command", executionPolicy.NextCommand,
				"task_summary", summarizeTaskForLog(currentTask),
				"execution_required_files", executionPolicy.RequiredFiles,
				"execution_empty_files", executionPolicy.EmptyFiles,
				"execution_repeated_scaffold", executionPolicy.RepeatedScaffold,
				"error", toolProtocolErrorString(parseResult.err),
			)
			if parseResult.err == nil && len(parseResult.calls) > 0 {
				outputItems := buildParsedToolOutputItems(parseResult.calls)
				completed := newCompletedResponse(responseID, messageID, model, createdAt, outputItems, promptInputItems)
				for index, item := range outputItems {
					writeAndFlushEvent("response.output_item.added", ResponseOutputItemAddedEvent{
						Type:        "response.output_item.added",
						ResponseID:  responseID,
						OutputIndex: index,
						Item:        item,
					})
					writeAndFlushEvent("response.output_item.done", ResponseOutputItemDoneEvent{
						Type:        "response.output_item.done",
						ResponseID:  responseID,
						OutputIndex: index,
						Item:        item,
					})
				}
				writeAndFlushEvent("response.completed", newResponseLifecycleEvent("response.completed", completed))
				p.responses.put(completed, requestItems, buildHistoryItems(baseHistoryItems, requestItems, outputItems))
				return
			}
			if parseResult.err != nil {
				finalText = buildToolProtocolErrorMessage(parseResult.err, finalText)
			} else {
				finalText = parseResult.visibleText
			}
			finalText = constrainFinalText(currentTask, finalText, evidence, checkControlMarkup)
			outputItems := []json.RawMessage{buildResponsesMessageItem(messageID, finalText)}
			completed := newCompletedResponse(responseID, messageID, model, createdAt, outputItems, promptInputItems)
			writeResponsesMessageAdded(writeAndFlushEvent, responseID, messageID)
			writeResponsesMessageDone(writeAndFlushEvent, responseID, messageID, finalText)
			writeAndFlushEvent("response.completed", newResponseLifecycleEvent("response.completed", completed))
			p.responses.put(completed, requestItems, buildHistoryItems(baseHistoryItems, requestItems, outputItems))
			return
		}
		finalText = constrainFinalText(currentTask, finalText, evidence, checkControlMarkup)
		outputItems := []json.RawMessage{buildResponsesMessageItem(messageID, finalText)}
		writeResponsesMessageDone(writeAndFlushEvent, responseID, messageID, finalText)
		completed := newCompletedResponse(responseID, messageID, model, createdAt, outputItems, promptInputItems)
		writeAndFlushEvent("response.completed", newResponseLifecycleEvent("response.completed", completed))
		p.responses.put(completed, requestItems, buildHistoryItems(baseHistoryItems, requestItems, outputItems))
	}

	// If stream ended without done/content, emit a terminal error response instead of leaving client pending.
	if !doneReceived && !contentEmitted && result.Len() == 0 && !clientGone && !errors.Is(scanErr, context.Canceled) {
		finalText := "Codex adapter error: upstream response ended without a completion signal"
		if scanErr != nil {
			finalText = "Codex adapter error: upstream stream failed before content: " + scanErr.Error()
		}
		slog.Error("responses stream ended without completion payload", "response_id", responseID, "error", scanErr)
		outputItems := []json.RawMessage{buildResponsesMessageItem(messageID, finalText)}
		if bufferForToolCalls {
			if !writeResponsesMessageAdded(writeAndFlushEvent, responseID, messageID) {
				return
			}
		}
		if !writeResponsesMessageDone(writeAndFlushEvent, responseID, messageID, finalText) {
			return
		}
		completed := newCompletedResponse(responseID, messageID, model, createdAt, outputItems, promptInputItems)
		if !writeAndFlushEvent("response.completed", newResponseLifecycleEvent("response.completed", completed)) {
			return
		}
		p.responses.put(completed, requestItems, buildHistoryItems(baseHistoryItems, requestItems, outputItems))
		return
	}

	if scanErr != nil && !errors.Is(scanErr, context.Canceled) {
		slog.Error("responses stream read error", "response_id", responseID, "error", scanErr)
	}
}

func (p *Proxy) handleResponsesNonStream(w http.ResponseWriter, r *http.Request, responseID, messageID, model string, body []byte, showThinking bool, requestItems, baseHistoryItems, promptInputItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor, toolConstraints toolProtocolConstraints, bufferForToolCalls bool, executionPolicy executionPolicy, currentTask string, evidence executionEvidence, checkControlMarkup bool) {
	ctx, cancel := context.WithTimeout(r.Context(), p.transport.Timeout())
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
		slog.Error("upstream responses non-stream error", "response_id", responseID, "error", err)
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream_failed", "upstream error")
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
			case "thinking_separator":
				result.WriteString("\n\n--- Answer ---\n\n")
			case "content":
				result.WriteString(evt.Content)
			}
			return true
		})
		_ = reader.Close()

		if shouldRetryEmptyResponsesAttempt(doneReceived, contentEmitted, result.Len(), scanErr, attempt, false) {
			attempt++
			slog.Warn("responses non-stream empty attempt, retrying once", "response_id", responseID, "attempt", attempt, "error", scanErr)
			reader, err = openReader()
			if err != nil {
				scanErr = err
				break
			}
			continue
		}
		break
	}

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
		parseResult := parseToolCallOutputsWithConstraints(finalText, toolCatalog, toolConstraints)
		parseResult = applyExecutionPolicyToParseResult(parseResult, executionPolicy, toolCatalog, toolConstraints)
		slog.Info("tool protocol outcome",
			"api", "responses",
			"response_id", responseID,
			"mode", parseResult.mode,
			"tool_calls", len(parseResult.calls),
			"required_tool", toolConstraints.RequiredTool,
			"require_tool", toolConstraints.RequireTool,
			"max_tool_calls", toolConstraints.MaxCalls,
			"execution_stage", executionPolicy.Stage,
			"execution_read_loop", executionPolicy.ReadLoop,
			"execution_next_command", executionPolicy.NextCommand,
			"task_summary", summarizeTaskForLog(currentTask),
			"execution_required_files", executionPolicy.RequiredFiles,
			"execution_empty_files", executionPolicy.EmptyFiles,
			"execution_repeated_scaffold", executionPolicy.RepeatedScaffold,
			"error", toolProtocolErrorString(parseResult.err),
		)
		if parseResult.err == nil && len(parseResult.calls) > 0 {
			outputItems := buildParsedToolOutputItems(parseResult.calls)
			completed := newCompletedResponse(responseID, messageID, model, createdAt, outputItems, promptInputItems)
			p.responses.put(completed, requestItems, buildHistoryItems(baseHistoryItems, requestItems, outputItems))
			writeJSON(w, http.StatusOK, completed)
			return
		}
		if parseResult.err != nil {
			finalText = buildToolProtocolErrorMessage(parseResult.err, finalText)
		} else {
			finalText = parseResult.visibleText
		}
	}
	finalText = constrainFinalText(currentTask, finalText, evidence, checkControlMarkup)

	outputItems := []json.RawMessage{buildResponsesMessageItem(messageID, finalText)}
	completed := newCompletedResponse(responseID, messageID, model, createdAt, outputItems, promptInputItems)
	p.responses.put(completed, requestItems, buildHistoryItems(baseHistoryItems, requestItems, outputItems))
	writeJSON(w, http.StatusOK, completed)
}
