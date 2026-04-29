package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	aiActionsStartMarker       = "<<<AI_ACTIONS_V1>>>"
	aiActionsCompatStartMarker = "<<<AI_ACTIONS_V1>>"
	aiActionsEndMarker         = "<<<END_AI_ACTIONS_V1>>>"
)

type toolProtocolMode string

const (
	toolProtocolModePlainText      toolProtocolMode = "plain_text"
	toolProtocolModeLegacyJSON     toolProtocolMode = "legacy_json"
	toolProtocolModeAIActionsTool  toolProtocolMode = "ai_actions_tool"
	toolProtocolModeAIActionsFinal toolProtocolMode = "ai_actions_final"
)

type aiActionsBlock struct {
	VisibleText string
	JSONText    string
}

type aiActionsEnvelope struct {
	Mode  string           `json:"mode"`
	Calls []map[string]any `json:"calls,omitempty"`
}

type parsedToolCallBatchResult struct {
	calls          []parsedToolCall
	candidateFound bool
	err            error
	visibleText    string
	mode           toolProtocolMode
}

type toolProtocolConstraints struct {
	RequiredTool       string
	RequireTool        bool
	PreferredToolNames []string
	MaxCalls           int
	AllowTruncateToMax bool
}

func buildParsedToolOutputItems(calls []parsedToolCall) []json.RawMessage {
	items := make([]json.RawMessage, 0, len(calls))
	for _, call := range calls {
		items = append(items, call.item)
	}
	return items
}

func appendToolProtocolInstructions(builder *strings.Builder, supportsCustom bool, maxCalls int) {
	appendToolProtocolInstructionsForCatalog(builder, supportsCustom, maxCalls, nil)
}

func appendToolProtocolInstructionsForCatalog(builder *strings.Builder, supportsCustom bool, maxCalls int, toolCatalog map[string]responseToolDescriptor) {
	builder.WriteString("If you need tools, put the machine-readable control block at the very end of your reply.\n")
	builder.WriteString("Emit exactly one AI_ACTIONS block per reply.\n")
	builder.WriteString("Use only tool names listed in AVAILABLE_TOOLS. Never invent or rename a tool.\n")
	builder.WriteString("If CURRENT_USER_TASK explicitly requires a named tool, do not write narration before the tool call. Emit the AI_ACTIONS tool block immediately.\n")
	builder.WriteString("A sentence that only says you will use a tool is invalid unless it also contains a real AI_ACTIONS tool block.\n")
	builder.WriteString("If the tool is exec_command, arguments must be exactly an object containing cmd, for example {\"cmd\":\"pwd\"}.\n")
	if shouldForceExecCommandAliases(toolCatalog) {
		builder.WriteString("Do not emit read_file/cat/list_files aliases; use exec_command with cmd instead.\n")
	} else {
		builder.WriteString("If file tools are listed in AVAILABLE_TOOLS, use those exact names for file reads and writes.\n")
	}
	if _, ok := toolCatalog["update_plan"]; ok {
		builder.WriteString("For update_plan, arguments must be an object with key plan, not steps. Example: {\"plan\":[{\"step\":\"Inspect README.md\",\"status\":\"in_progress\"},{\"step\":\"Reply with OK\",\"status\":\"pending\"}]}. Optional explanation is allowed.\n")
	}
	if _, ok := toolCatalog["web_search"]; ok {
		builder.WriteString("For web_search, use the exact tool name web_search and pass arguments as {\"query\":\"...\"}. The proxy will convert that into a web_search_call item.\n")
	}
	if _, ok := toolCatalog["js_repl"]; ok {
		builder.WriteString("For js_repl, send raw JavaScript source in input. Do not wrap js_repl input in JSON, quotes, or markdown fences.\n")
	}
	if _, ok := toolCatalog["write_stdin"]; ok {
		builder.WriteString("For write_stdin, arguments must be an object with session_id:number and chars:string. Do not use input in place of chars.\n")
	}
	if catalogHasNamespacedTools(toolCatalog) {
		builder.WriteString("For namespaced MCP tools, use the full declared name exactly as shown, including the namespace prefix and double-underscore separator.\n")
	}
	builder.WriteString("After each tool result, continue CURRENT_USER_TASK. If it is not complete, emit another tool call instead of mode final.\n")
	builder.WriteString("Never ask for a new task when CURRENT_USER_TASK is already provided.\n")
	if maxCalls == 1 {
		builder.WriteString("If you need a tool, the calls array must contain exactly one item.\n")
		builder.WriteString("If the task needs multiple steps, emit only the next single tool call now.\n")
	} else {
		builder.WriteString("If you need tools, the calls array may contain one or more items.\n")
	}
	builder.WriteString("Use this format:\n")
	builder.WriteString(aiActionsStartMarker)
	builder.WriteByte('\n')
	builder.WriteString("{\"mode\":\"tool\",\"calls\":[{\"name\":\"<tool_name>\",\"arguments\":{...}}]}\n")
	builder.WriteString(aiActionsEndMarker)
	builder.WriteByte('\n')
	if supportsCustom {
		builder.WriteString("For freeform tools, use calls like {\"name\":\"<tool_name>\",\"input\":\"<raw input>\"} inside the same block.\n")
	}
	builder.WriteString("Use mode final only when the task is fully complete and no further tool calls are needed.\n")
	builder.WriteString("If no tool is needed, end with:\n")
	builder.WriteString(aiActionsStartMarker)
	builder.WriteByte('\n')
	builder.WriteString("{\"mode\":\"final\"}\n")
	builder.WriteString(aiActionsEndMarker)
	builder.WriteByte('\n')
}

func shouldForceExecCommandAliases(toolCatalog map[string]responseToolDescriptor) bool {
	if len(toolCatalog) == 0 {
		return true
	}
	for _, name := range []string{"read_file", "list_files", "write_file", "edit_file", "replace_in_file", "append_file", "create_file"} {
		if _, ok := toolCatalog[name]; ok {
			return false
		}
	}
	return true
}

func catalogHasNamespacedTools(toolCatalog map[string]responseToolDescriptor) bool {
	for name := range toolCatalog {
		if strings.HasPrefix(name, "mcp__") {
			return true
		}
	}
	return false
}

func extractAIActionsBlock(text string) (aiActionsBlock, bool) {
	start, startMarker := findAIActionsStartMarker(text)
	if start < 0 || startMarker == "" {
		return aiActionsBlock{}, false
	}

	end, endMarker := findAIActionsEndMarker(text)
	if end < 0 || end < start {
		return aiActionsBlock{}, false
	}

	suffix := text[end+len(endMarker):]
	if strings.TrimSpace(suffix) != "" {
		return aiActionsBlock{}, false
	}

	payloadStart := start + len(startMarker)
	payload := strings.TrimSpace(text[payloadStart:end])
	if payload == "" {
		return aiActionsBlock{}, false
	}

	return aiActionsBlock{
		VisibleText: strings.TrimSpace(text[:start]),
		JSONText:    payload,
	}, true
}

func findAIActionsEndMarker(text string) (int, string) {
	if end := strings.LastIndex(text, aiActionsEndMarker); end >= 0 {
		return end, aiActionsEndMarker
	}
	for _, candidate := range []string{"<<<END_AI_ACTIONS_V1>>}", "<<<END_AI_ACTIONS_V1>>"} {
		if end := strings.LastIndex(text, candidate); end >= 0 {
			return end, candidate
		}
	}
	return -1, ""
}

func findAIActionsStartMarker(text string) (int, string) {
	start := -1
	marker := ""
	for _, candidate := range []string{aiActionsStartMarker, aiActionsCompatStartMarker} {
		if idx := strings.LastIndex(text, candidate); idx >= 0 && idx > start {
			start = idx
			marker = candidate
		}
	}
	return start, marker
}

func findNextAIActionsStartMarker(text string) (int, string) {
	for i := 0; i < len(text); i++ {
		for _, candidate := range []string{aiActionsStartMarker, aiActionsCompatStartMarker} {
			if strings.HasPrefix(text[i:], candidate) {
				return i, candidate
			}
		}
	}
	return -1, ""
}

func parseToolCallOutputs(text string, allowedTools map[string]responseToolDescriptor, requiredTool string) parsedToolCallBatchResult {
	return parseToolCallOutputsWithConstraints(text, allowedTools, toolProtocolConstraints{RequiredTool: requiredTool})
}

func parseToolCallOutputsWithConstraints(text string, allowedTools map[string]responseToolDescriptor, constraints toolProtocolConstraints) parsedToolCallBatchResult {
	if block, ok := extractAIActionsBlock(text); ok {
		primary := applyToolProtocolConstraints(parseAIActionsToolCallOutputs(block, allowedTools, constraints.RequiredTool), constraints)
		if recovered, ok := recoverToolCallsFromAIActionsBlocks(text, allowedTools, constraints); ok && shouldPreferRecoveredBatch(primary, recovered, constraints) {
			return recovered
		}
		return primary
	}
	if recovered, ok := recoverToolCallsFromAIActionsBlocks(text, allowedTools, constraints); ok {
		return recovered
	}
	if shorthand, ok := parseInlineAIActionsShorthand(text, allowedTools, constraints); ok {
		return shorthand
	}

	legacy := parseLegacyToolCallOutputs(text, allowedTools, constraints.RequiredTool)
	return applyToolProtocolConstraints(legacy, constraints)
}

func parseInlineAIActionsShorthand(text string, allowedTools map[string]responseToolDescriptor, constraints toolProtocolConstraints) (parsedToolCallBatchResult, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return parsedToolCallBatchResult{}, false
	}
	idx := indexCaseInsensitiveASCII(trimmed, "ai_actions:")
	if idx < 0 {
		return parsedToolCallBatchResult{}, false
	}

	tail := strings.TrimSpace(trimmed[idx+len("ai_actions:"):])
	if tail == "" {
		return parsedToolCallBatchResult{}, false
	}
	fields := strings.Fields(tail)
	if len(fields) == 0 {
		return parsedToolCallBatchResult{}, false
	}
	name := strings.TrimSpace(fields[0])
	if name == "" {
		return parsedToolCallBatchResult{}, false
	}
	if strings.Contains(name, "`") || strings.Contains(name, "{") || strings.Contains(name, "}") {
		return parsedToolCallBatchResult{}, false
	}
	jsonStart := strings.Index(tail, "{")
	if jsonStart < 0 {
		return parsedToolCallBatchResult{}, false
	}
	objectText, ok := extractJSONObject(tail[jsonStart:])
	if !ok {
		return parsedToolCallBatchResult{}, false
	}

	var arguments map[string]any
	if err := json.Unmarshal([]byte(objectText), &arguments); err != nil {
		return parsedToolCallBatchResult{
			candidateFound: true,
			err:            fmt.Errorf("AI_ACTIONS shorthand JSON decode failed: %w", err),
			visibleText:    strings.TrimSpace(trimmed[:idx]),
			mode:           toolProtocolModePlainText,
		}, true
	}

	call, err := buildParsedToolCall(map[string]any{
		"name":      name,
		"arguments": arguments,
	}, allowedTools, constraints.RequiredTool, true)
	result := parsedToolCallBatchResult{
		candidateFound: true,
		visibleText:    strings.TrimSpace(trimmed[:idx]),
		mode:           toolProtocolModeAIActionsTool,
	}
	if err != nil {
		result.err = err
		return applyToolProtocolConstraints(result, constraints), true
	}
	result.calls = []parsedToolCall{*call}
	return applyToolProtocolConstraints(result, constraints), true
}

func indexCaseInsensitiveASCII(text, needle string) int {
	if needle == "" {
		return 0
	}
	if len(text) < len(needle) {
		return -1
	}
	for i := 0; i <= len(text)-len(needle); i++ {
		if strings.EqualFold(text[i:i+len(needle)], needle) {
			return i
		}
	}
	return -1
}

func recoverToolCallsFromAIActionsBlocks(text string, allowedTools map[string]responseToolDescriptor, constraints toolProtocolConstraints) (parsedToolCallBatchResult, bool) {
	blocks := extractSequentialAIActionsBlocks(text)
	if len(blocks) == 0 {
		return parsedToolCallBatchResult{}, false
	}

	valid := make([]parsedToolCallBatchResult, 0, len(blocks))
	var lastErr *parsedToolCallBatchResult
	for _, block := range blocks {
		result := parseAIActionsToolCallOutputs(block, allowedTools, constraints.RequiredTool)
		result = applyToolProtocolConstraints(result, constraints)
		if len(result.calls) > 0 {
			valid = append(valid, result)
			continue
		}
		if result.err != nil {
			copied := result
			lastErr = &copied
		}
	}
	if preferred, ok := selectPreferredRecoveredBatch(valid, constraints); ok {
		return preferred, true
	}
	if lastErr != nil {
		return *lastErr, true
	}
	return parsedToolCallBatchResult{}, false
}

func shouldPreferRecoveredBatch(primary, recovered parsedToolCallBatchResult, constraints toolProtocolConstraints) bool {
	if len(recovered.calls) == 0 {
		return false
	}
	if len(primary.calls) == 0 {
		return true
	}
	if parsedCallsContainPreferredTool(recovered.calls, constraints.PreferredToolNames) && !parsedCallsContainPreferredTool(primary.calls, constraints.PreferredToolNames) {
		return true
	}
	if parsedCallsContainMutationTool(recovered.calls) && !parsedCallsContainMutationTool(primary.calls) {
		return true
	}
	return false
}

func selectPreferredRecoveredBatch(valid []parsedToolCallBatchResult, constraints toolProtocolConstraints) (parsedToolCallBatchResult, bool) {
	if len(valid) == 0 {
		return parsedToolCallBatchResult{}, false
	}
	if len(constraints.PreferredToolNames) > 0 {
		for _, result := range valid {
			if parsedCallsContainPreferredTool(result.calls, constraints.PreferredToolNames) {
				return result, true
			}
		}
	}
	for _, result := range valid {
		if parsedCallsContainMutationTool(result.calls) {
			return result, true
		}
	}
	return valid[len(valid)-1], true
}

func extractSequentialAIActionsBlocks(text string) []aiActionsBlock {
	blocks := make([]aiActionsBlock, 0, 2)
	cursor := 0
	for cursor < len(text) {
		start, marker := findNextAIActionsStartMarker(text[cursor:])
		if start < 0 || marker == "" {
			break
		}
		start += cursor
		payloadStart := start + len(marker)
		end, endMarker := findNextAIActionsEndMarker(text[payloadStart:])
		if end < 0 {
			break
		}
		end += payloadStart
		payload := strings.TrimSpace(text[payloadStart:end])
		if payload != "" {
			blocks = append(blocks, aiActionsBlock{
				VisibleText: strings.TrimSpace(text[cursor:start]),
				JSONText:    payload,
			})
		}
		cursor = end + len(endMarker)
	}
	return blocks
}

func findNextAIActionsEndMarker(text string) (int, string) {
	best := -1
	bestMarker := ""
	for _, candidate := range []string{aiActionsEndMarker, "<<<END_AI_ACTIONS_V1>>}", "<<<END_AI_ACTIONS_V1>>"} {
		if idx := strings.Index(text, candidate); idx >= 0 && (best < 0 || idx < best) {
			best = idx
			bestMarker = candidate
		}
	}
	return best, bestMarker
}

func parseAIActionsToolCallOutputs(block aiActionsBlock, allowedTools map[string]responseToolDescriptor, requiredTool string) parsedToolCallBatchResult {
	result := parsedToolCallBatchResult{
		candidateFound: true,
		visibleText:    block.VisibleText,
	}

	jsonText := strings.TrimSpace(stripMarkdownCodeFence(block.JSONText))
	envelope, err := decodeAIActionsEnvelope(jsonText)
	if err != nil {
		result.err = fmt.Errorf("AI actions JSON decode failed: %w", err)
		return result
	}

	switch envelope.Mode {
	case "final":
		result.mode = toolProtocolModeAIActionsFinal
		if len(envelope.Calls) > 0 {
			result.err = errors.New("AI actions final mode must not include calls")
		}
		return result
	case "tool":
		result.mode = toolProtocolModeAIActionsTool
		if len(envelope.Calls) == 0 {
			result.err = errors.New("AI actions tool mode requires at least one call")
			return result
		}
		calls := make([]parsedToolCall, 0, len(envelope.Calls))
		for _, raw := range envelope.Calls {
			call, err := buildParsedToolCall(raw, allowedTools, requiredTool, true)
			if err != nil {
				result.err = err
				return result
			}
			calls = append(calls, *call)
		}
		result.calls = calls
		return result
	default:
		result.err = fmt.Errorf("unsupported AI actions mode %q", envelope.Mode)
		return result
	}
}

func decodeAIActionsEnvelope(jsonText string) (aiActionsEnvelope, error) {
	var envelope aiActionsEnvelope
	decodeErr := json.Unmarshal([]byte(jsonText), &envelope)
	if decodeErr == nil {
		return envelope, nil
	}
	repaired := repairAIActionsJSON(jsonText)
	if repaired != jsonText {
		if err := json.Unmarshal([]byte(repaired), &envelope); err == nil {
			return envelope, nil
		}
	}

	// Some upstream models append non-JSON narration inside the marker block.
	extracted, ok := extractJSONObject(jsonText)
	if !ok || extracted == jsonText {
		return aiActionsEnvelope{}, decodeErr
	}
	if err := json.Unmarshal([]byte(extracted), &envelope); err != nil {
		return aiActionsEnvelope{}, err
	}
	return envelope, nil
}

func repairAIActionsJSON(jsonText string) string {
	repaired := insertMissingArrayClosers(jsonText)
	arrayClose := strings.LastIndex(repaired, "]")
	if arrayClose < 0 {
		return repaired
	}

	curlyDepth := 0
	inString := false
	escaped := false
	for i := 0; i < arrayClose; i++ {
		ch := repaired[i]
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
			curlyDepth++
		case '}':
			if curlyDepth > 0 {
				curlyDepth--
			}
		}
	}
	if inString || curlyDepth <= 1 {
		return repaired
	}
	return repaired[:arrayClose] + strings.Repeat("}", curlyDepth-1) + repaired[arrayClose:]
}

func insertMissingArrayClosers(jsonText string) string {
	squareDepth, insertAt, ok := scanTrailingJSONClosers(jsonText)
	if !ok || squareDepth <= 0 {
		return jsonText
	}
	return jsonText[:insertAt] + strings.Repeat("]", squareDepth) + jsonText[insertAt:]
}

func scanTrailingJSONClosers(text string) (squareDepth int, insertAt int, ok bool) {
	inString := false
	escaped := false
	curlyDepth := 0
	squareDepth = 0
	for i := 0; i < len(text); i++ {
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
			curlyDepth++
		case '}':
			if curlyDepth > 0 {
				curlyDepth--
			}
		case '[':
			squareDepth++
		case ']':
			if squareDepth > 0 {
				squareDepth--
			}
		}
	}
	if inString || squareDepth <= 0 {
		return squareDepth, 0, false
	}

	insertAt = len(text)
	for insertAt > 0 {
		ch := text[insertAt-1]
		if ch == ' ' || ch == '\n' || ch == '\r' || ch == '\t' {
			insertAt--
			continue
		}
		if ch != '}' {
			return squareDepth, insertAt, false
		}
		break
	}
	if insertAt == 0 || text[insertAt-1] != '}' {
		return squareDepth, insertAt, false
	}
	return squareDepth, insertAt - 1, true
}

func applyToolProtocolConstraints(result parsedToolCallBatchResult, constraints toolProtocolConstraints) parsedToolCallBatchResult {
	if result.err != nil {
		return result
	}
	if constraints.MaxCalls > 0 && len(result.calls) > constraints.MaxCalls {
		if constraints.AllowTruncateToMax {
			result.calls = append([]parsedToolCall(nil), result.calls[:constraints.MaxCalls]...)
			return result
		}
		result.err = fmt.Errorf("tool protocol allows at most %d call(s), got %d", constraints.MaxCalls, len(result.calls))
		return result
	}
	if constraints.RequireTool && len(result.calls) == 0 {
		if constraints.RequiredTool != "" {
			result.err = fmt.Errorf("tool_choice requires %q, got non-tool response", constraints.RequiredTool)
		} else {
			result.err = errors.New("tool_choice requires a tool call, got non-tool response")
		}
		result.candidateFound = true
	}
	return result
}

func toolProtocolErrorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func parseLegacyToolCallOutputs(text string, allowedTools map[string]responseToolDescriptor, requiredTool string) parsedToolCallBatchResult {
	candidateSource := strings.TrimSpace(stripMarkdownCodeFence(text))
	if candidateSource == "" {
		return parsedToolCallBatchResult{
			visibleText: text,
			mode:        toolProtocolModePlainText,
		}
	}

	start := strings.IndexByte(candidateSource, '{')
	if start >= 0 {
		if !legacyJSONPrefixLooksLikeToolOutput(candidateSource[:start]) {
			return parsedToolCallBatchResult{
				visibleText: text,
				mode:        toolProtocolModePlainText,
			}
		}
		candidateSource = strings.TrimSpace(candidateSource[start:])
	}
	candidate := candidateSource
	if candidate == "" || !strings.HasPrefix(candidate, "{") {
		return parsedToolCallBatchResult{
			visibleText: text,
			mode:        toolProtocolModePlainText,
		}
	}

	raws, err := decodeLegacyToolCallSequence(candidate)
	if err != nil {
		return parsedToolCallBatchResult{
			candidateFound: true,
			err:            fmt.Errorf("tool call JSON decode failed: %w", err),
			visibleText:    text,
			mode:           toolProtocolModePlainText,
		}
	}
	if len(raws) == 0 {
		return parsedToolCallBatchResult{
			visibleText: text,
			mode:        toolProtocolModePlainText,
		}
	}
	for i := range raws {
		raws[i] = normalizeLegacyToolCallMap(raws[i])
	}
	if _, hasType := raws[0]["type"]; !hasType {
		return parsedToolCallBatchResult{
			visibleText: text,
			mode:        toolProtocolModePlainText,
		}
	}

	result := parsedToolCallBatchResult{
		candidateFound: true,
		visibleText:    text,
		mode:           toolProtocolModeLegacyJSON,
	}
	calls := make([]parsedToolCall, 0, len(raws))
	for _, raw := range raws {
		call, err := buildParsedToolCall(raw, allowedTools, requiredTool, false)
		if err != nil {
			result.err = err
			return result
		}
		calls = append(calls, *call)
	}
	result.calls = calls
	return result
}

func normalizeLegacyToolCallMap(raw map[string]any) map[string]any {
	if len(raw) == 0 {
		return raw
	}
	normalized := cloneMap(raw)
	inferredFromAlias := false
	if _, ok := normalized["name"]; !ok {
		if name, ok := firstStringField(normalized, "tool", "tool_name", "function", "function_name"); ok {
			normalized["name"] = name
			inferredFromAlias = true
		}
	}
	if _, ok := normalized["arguments"]; !ok {
		if args, ok := normalized["tool_args"]; ok {
			normalized["arguments"] = args
			inferredFromAlias = true
		} else if args, ok := normalized["tool_arguments"]; ok {
			normalized["arguments"] = args
			inferredFromAlias = true
		} else if args, ok := normalized["args"]; ok {
			normalized["arguments"] = args
			inferredFromAlias = true
		}
	}
	if _, ok := normalized["type"]; !ok && inferredFromAlias {
		if _, hasInput := normalized["input"]; hasInput {
			normalized["type"] = "custom_tool_call"
		} else if _, hasArguments := normalized["arguments"]; hasArguments {
			normalized["type"] = "function_call"
		}
	}
	return normalized
}

func legacyJSONPrefixLooksLikeToolOutput(prefix string) bool {
	trimmed := strings.TrimSpace(prefix)
	if trimmed == "" {
		return true
	}
	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "function_call") || strings.Contains(lower, "tool_call") || strings.Contains(lower, "ai_actions") {
		return true
	}
	// Allow short lead-in text such as "先做两步：" but avoid treating long prose/code as tool JSON.
	if len(trimmed) > 120 {
		return false
	}
	if strings.Count(trimmed, "\n") > 2 {
		return false
	}
	if strings.Contains(trimmed, "```") {
		return false
	}
	return true
}

func decodeLegacyToolCallSequence(candidate string) ([]map[string]any, error) {
	decoder := json.NewDecoder(strings.NewReader(candidate))
	raws := make([]map[string]any, 0, 1)
	for {
		var raw map[string]any
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if len(raws) > 0 {
				offset := int(decoder.InputOffset())
				if offset >= 0 && offset <= len(candidate) {
					if isIgnorableLegacyTail(candidate[offset:]) {
						break
					}
				}
			}
			return nil, err
		}
		raws = append(raws, raw)
	}
	return raws, nil
}

func isIgnorableLegacyTail(tail string) bool {
	remaining := strings.TrimSpace(tail)
	if remaining == "" {
		return true
	}

	tokens := []string{
		"```",
		"</function_call>",
		"</tool_call>",
		"</function>",
		"</tool>",
		"</ai_actions>",
		"</think>",
	}

	for remaining != "" {
		remaining = strings.TrimSpace(remaining)
		if remaining == "" {
			return true
		}

		matched := false
		for _, token := range tokens {
			if strings.HasPrefix(remaining, token) {
				remaining = remaining[len(token):]
				matched = true
				break
			}
			lowerRemaining := strings.ToLower(remaining)
			lowerToken := strings.ToLower(token)
			if strings.HasPrefix(lowerRemaining, lowerToken) {
				remaining = remaining[len(token):]
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	return true
}

func resolveToolNameForCatalog(rawName, normalized string, allowedTools map[string]responseToolDescriptor) (string, bool) {
	if len(allowedTools) == 0 {
		return normalized, true
	}
	for _, candidate := range toolNameAliasVariants(normalized) {
		if _, ok := allowedTools[candidate]; ok {
			return candidate, true
		}
	}
	for _, candidate := range toolNameAliasVariants(rawName) {
		if _, ok := allowedTools[candidate]; ok {
			return candidate, true
		}
	}
	for declared := range allowedTools {
		for _, candidate := range toolNameAliasVariants(rawName) {
			if strings.EqualFold(declared, candidate) {
				return declared, true
			}
		}
	}
	return normalized, false
}

func toolNameAliasVariants(name string) []string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return nil
	}

	variants := []string{trimmed}
	switch strings.ToLower(trimmed) {
	case "wait":
		variants = append(variants, "wait_agent")
	case "wait_agent":
		variants = append(variants, "wait")
	}
	if normalized, ok := normalizeNamespacedToolName(trimmed); ok {
		variants = append(variants, normalized)
	}
	if strings.HasPrefix(trimmed, "mcp__") {
		if idx := strings.LastIndex(trimmed, "__"); idx >= len("mcp__") && idx+2 < len(trimmed) {
			namespaceName := strings.TrimSpace(trimmed[:idx+2])
			toolPart := strings.TrimSpace(trimmed[idx+2:])
			if namespaceName != "" && toolPart != "" {
				variants = append(variants, strings.TrimSuffix(namespaceName, "__")+"."+toolPart)
			}
		}
	}
	if idx := strings.LastIndex(trimmed, "__"); idx >= 0 && idx+2 < len(trimmed) {
		prefix := trimmed[:idx+2]
		toolPart := trimmed[idx+2:]
		if strings.Contains(toolPart, "_") {
			variants = append(variants, prefix+strings.ReplaceAll(toolPart, "_", "-"))
		}
		if strings.Contains(toolPart, "-") {
			variants = append(variants, prefix+strings.ReplaceAll(toolPart, "-", "_"))
		}
	}
	if idx := strings.LastIndex(trimmed, "."); idx >= 0 && idx+1 < len(trimmed) {
		prefix := trimmed[:idx+1]
		toolPart := trimmed[idx+1:]
		if strings.Contains(toolPart, "_") {
			variants = append(variants, prefix+strings.ReplaceAll(toolPart, "_", "-"))
		}
		if strings.Contains(toolPart, "-") {
			variants = append(variants, prefix+strings.ReplaceAll(toolPart, "-", "_"))
		}
	}
	appendContext7DocToolVariants := func(prefix, toolPart string) {
		switch toolPart {
		case "query-docs", "query_docs":
			variants = append(variants, prefix+"get-library-docs", prefix+"get_library_docs")
		case "get-library-docs", "get_library_docs":
			variants = append(variants, prefix+"query-docs", prefix+"query_docs")
		}
	}
	appendContext7DocToolVariants("", trimmed)
	if idx := strings.LastIndex(trimmed, "__"); idx >= 0 && idx+2 < len(trimmed) {
		appendContext7DocToolVariants(trimmed[:idx+2], trimmed[idx+2:])
	}
	if idx := strings.LastIndex(trimmed, "."); idx >= 0 && idx+1 < len(trimmed) {
		appendContext7DocToolVariants(trimmed[:idx+1], trimmed[idx+1:])
	}
	return dedupePreserveOrder(variants)
}

func firstStringField(values map[string]any, keys ...string) (string, bool) {
	_, value, ok := firstStringFieldWithKey(values, keys...)
	return value, ok
}

func firstStringFieldWithKey(values map[string]any, keys ...string) (string, string, bool) {
	for _, key := range keys {
		raw, ok := values[key]
		if !ok {
			continue
		}
		text, ok := raw.(string)
		if !ok {
			continue
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		return key, text, true
	}
	return "", "", false
}

func isLikelyCommandContinuationLine(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	if lower == "" {
		return true
	}
	for _, prefix := range []string{
		"&&",
		"||",
		"|",
		"\\",
		"then",
		"do",
		"fi",
		"done",
		"elif",
		"else",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func sanitizeExecCommandText(command string) string {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) <= 1 {
		return trimmed
	}
	first := strings.TrimSpace(lines[0])
	if first == "" {
		return trimmed
	}
	for _, line := range lines[1:] {
		current := strings.TrimSpace(line)
		if current == "" {
			continue
		}
		if !isLikelyCommandContinuationLine(current) {
			return first
		}
	}
	return trimmed
}

func shellQuoteSingle(text string) string {
	return "'" + strings.ReplaceAll(text, "'", "'\"'\"'") + "'"
}

func buildReadFileCommand(path string) string {
	if strings.HasPrefix(path, "-") {
		return "sed -n '1,200p' -- " + shellQuoteSingle(path)
	}
	return "sed -n '1,200p' " + shellQuoteSingle(path)
}

func buildListFilesCommand(path string) string {
	return "ls -la -- " + shellQuoteSingle(path)
}

func buildCreateMissingFileCommand(filePath string) string {
	filePath = strings.TrimSpace(filePath)
	if filePath == "" {
		return ""
	}
	dir := strings.TrimSpace(filepath.Dir(filePath))
	if dir == "" || dir == "." {
		return "touch " + shellQuoteSingle(filePath)
	}
	return "mkdir -p -- " + shellQuoteSingle(dir) + " && touch " + shellQuoteSingle(filePath)
}

func normalizeExecCommandArguments(args any, sourceToolName string) (any, bool) {
	switch value := args.(type) {
	case string:
		command := sanitizeExecCommandText(value)
		if command == "" {
			return args, false
		}
		return map[string]any{"cmd": command}, true
	case map[string]any:
		if cmd, ok := firstStringField(value, "cmd"); ok {
			cmd = sanitizeExecCommandText(cmd)
			if cmd == "" {
				return args, false
			}
			normalized := cloneMap(value)
			normalized["cmd"] = cmd
			return normalized, true
		}
		if matchedKey, command, ok := firstStringFieldWithKey(value, "command", "command_line", "cmdline", "shell_command", "input"); ok {
			command = sanitizeExecCommandText(command)
			if command == "" {
				return args, false
			}
			normalized := cloneMap(value)
			delete(normalized, matchedKey)
			normalized["cmd"] = command
			return normalized, true
		}

		toolName := strings.ToLower(strings.TrimSpace(sourceToolName))
		if toolName == "read_file" || toolName == "readfile" || toolName == "read" || toolName == "cat" {
			if path, ok := firstStringField(value, "path", "file_path", "file"); ok {
				return map[string]any{"cmd": buildReadFileCommand(path)}, true
			}
		}
		if toolName == "list_files" || toolName == "listfiles" || toolName == "ls" {
			if path, ok := firstStringField(value, "path", "dir", "directory"); ok {
				return map[string]any{"cmd": buildListFilesCommand(path)}, true
			}
		}
	}
	return args, false
}

func normalizeWriteStdinArguments(args any) (any, bool) {
	value, ok := args.(map[string]any)
	if !ok {
		return args, false
	}

	normalized := cloneMap(value)
	changed := false
	if _, hasChars := normalized["chars"]; !hasChars {
		if matchedKey, chars, ok := firstWriteStdinCharsAlias(value, "input", "text", "stdin", "value", "message"); ok {
			delete(normalized, matchedKey)
			normalized["chars"] = chars
			changed = true
		}
	}

	if rawSessionID, ok := normalized["session_id"]; ok {
		if sessionID, sessionChanged := normalizeWriteStdinSessionID(rawSessionID); sessionChanged {
			normalized["session_id"] = sessionID
			changed = true
		}
	} else if matchedKey, sessionIDText, ok := firstStringFieldWithKey(value, "sessionId", "session", "id"); ok {
		sessionID := any(sessionIDText)
		if normalizedSessionID, sessionChanged := normalizeWriteStdinSessionID(sessionIDText); sessionChanged {
			sessionID = normalizedSessionID
		}
		delete(normalized, matchedKey)
		normalized["session_id"] = sessionID
		changed = true
	}

	if !changed {
		return args, false
	}
	return normalized, true
}

func firstWriteStdinCharsAlias(values map[string]any, keys ...string) (string, string, bool) {
	for _, key := range keys {
		raw, ok := values[key]
		if !ok {
			continue
		}
		text, ok := raw.(string)
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}
		return key, text, true
	}
	return "", "", false
}

func normalizeWriteStdinSessionID(value any) (any, bool) {
	switch raw := value.(type) {
	case string:
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return value, false
		}
		if parsed, err := strconv.Atoi(trimmed); err == nil {
			return parsed, true
		}
		if trimmed != raw {
			return trimmed, true
		}
	case float64:
		if raw == float64(int(raw)) {
			return int(raw), true
		}
	}
	return value, false
}

func normalizeStructuredToolArguments(toolName, sourceToolName string, args any) (any, bool) {
	switch firstNonEmptyNormalizedToolName(toolName, sourceToolName) {
	case "exec_command":
		return normalizeExecCommandArguments(args, sourceToolName)
	case "write_stdin":
		return normalizeWriteStdinArguments(args)
	case "mcp__context7__resolve-library-id":
		value, ok := args.(map[string]any)
		if !ok {
			return args, false
		}
		normalized := cloneMap(value)
		changed := false
		if _, hasLibraryName := normalized["libraryName"]; !hasLibraryName {
			if matchedKey, libraryName, ok := firstStringFieldWithKey(value, "library_name", "library", "name"); ok {
				delete(normalized, matchedKey)
				normalized["libraryName"] = libraryName
				changed = true
			}
		}
		if _, hasQuery := normalized["query"]; !hasQuery {
			if libraryName, ok := firstStringField(normalized, "libraryName"); ok {
				normalized["query"] = libraryName
				changed = true
			}
		}
		if !changed {
			return args, false
		}
		return normalized, true
	case "mcp__context7__get-library-docs":
		value, ok := args.(map[string]any)
		if !ok {
			return args, false
		}
		normalized := cloneMap(value)
		changed := false
		if _, hasLibraryID := normalized["context7CompatibleLibraryID"]; !hasLibraryID {
			if matchedKey, libraryID, ok := firstStringFieldWithKey(value, "libraryId", "library_id", "context7CompatibleLibraryId"); ok {
				delete(normalized, matchedKey)
				normalized["context7CompatibleLibraryID"] = libraryID
				changed = true
			}
		}
		if _, hasTopic := normalized["topic"]; !hasTopic {
			if matchedKey, topic, ok := firstStringFieldWithKey(value, "query", "topic_name"); ok {
				delete(normalized, matchedKey)
				normalized["topic"] = topic
				changed = true
			}
		}
		if !changed {
			return args, false
		}
		return normalized, true
	case "mcp__context7__query-docs":
		value, ok := args.(map[string]any)
		if !ok {
			return args, false
		}
		normalized := cloneMap(value)
		changed := false
		if _, hasLibraryID := normalized["libraryId"]; !hasLibraryID {
			if matchedKey, libraryID, ok := firstStringFieldWithKey(value, "context7CompatibleLibraryID", "context7CompatibleLibraryId", "library_id"); ok {
				delete(normalized, matchedKey)
				normalized["libraryId"] = libraryID
				changed = true
			}
		}
		if _, hasQuery := normalized["query"]; !hasQuery {
			if matchedKey, query, ok := firstStringFieldWithKey(value, "topic", "topic_name"); ok {
				delete(normalized, matchedKey)
				normalized["query"] = query
				changed = true
			}
		}
		if !changed {
			return args, false
		}
		return normalized, true
	case "mcp__docfork__search_docs":
		value, ok := args.(map[string]any)
		if !ok {
			return args, false
		}
		normalized := cloneMap(value)
		changed := false
		library, _ := firstStringField(value, "library")
		if library == "" {
			library, _ = firstStringField(value, "source", "docs", "docset", "library_name", "libraryName")
		}
		if library == "" {
			library = inferDocforkLibraryFromQuery(value)
		}
		if library != "" {
			if _, hasLibrary := value["library"]; !hasLibrary {
				normalized["library"] = library
				changed = true
			}
			for _, alias := range []string{"source", "docs", "docset", "library_name", "libraryName"} {
				if _, ok := normalized[alias]; ok {
					delete(normalized, alias)
					changed = true
				}
			}
			if query, ok := firstStringField(value, "query"); ok {
				if cleaned, ok := trimDocforkQueryLibraryPrefix(query, library); ok {
					normalized["query"] = cleaned
					changed = true
				}
			}
		}
		if !changed {
			return args, false
		}
		return normalized, true
	case "spawn_agent":
		value, ok := args.(map[string]any)
		if !ok {
			return args, false
		}
		if _, hasMessage := value["message"]; hasMessage {
			return args, false
		}
		if _, hasItems := value["items"]; hasItems {
			return args, false
		}
		if matchedKey, task, ok := firstStringFieldWithKey(value, "task", "prompt", "instruction"); ok {
			normalized := cloneMap(value)
			delete(normalized, matchedKey)
			normalized["message"] = task
			return normalized, true
		}
	}
	return args, false
}

func inferDocforkLibraryFromQuery(values map[string]any) string {
	query, ok := firstStringField(values, "query")
	if !ok {
		return ""
	}
	lower := strings.ToLower(query)
	for _, candidate := range []string{"react", "nextjs", "next.js", "vue", "svelte", "angular", "zod", "tailwind"} {
		if strings.Contains(lower, candidate) {
			return candidate
		}
	}
	return ""
}

func trimDocforkQueryLibraryPrefix(query, library string) (string, bool) {
	fields := strings.Fields(strings.TrimSpace(query))
	if len(fields) < 2 {
		return query, false
	}
	first := strings.ToLower(strings.Trim(fields[0], " \t\r\n`'\""))
	aliases := docforkLibraryAliases(library)
	for _, alias := range aliases {
		if first == alias {
			return strings.Join(fields[1:], " "), true
		}
	}
	return query, false
}

func docforkLibraryAliases(library string) []string {
	normalized := strings.ToLower(strings.TrimSpace(library))
	switch normalized {
	case "react", "react.dev":
		return []string{"react", "react.dev"}
	case "nextjs", "next.js":
		return []string{"nextjs", "next.js", "next"}
	default:
		if normalized == "" {
			return nil
		}
		return []string{normalized}
	}
}

func firstNonEmptyNormalizedToolName(names ...string) string {
	for _, name := range names {
		if normalized := normalizeToolName(name); normalized != "" {
			return normalized
		}
	}
	return ""
}

func normalizeStructuredToolInputArgument(toolName, sourceToolName string, input any) (any, bool) {
	text, ok := input.(string)
	if !ok {
		return nil, false
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, false
	}

	switch firstNonEmptyNormalizedToolName(toolName, sourceToolName) {
	case "exec_command":
		return map[string]any{"cmd": text}, true
	default:
		return nil, false
	}
}

func normalizeCustomToolInputArgument(args any) (string, bool) {
	value, ok := args.(map[string]any)
	if !ok || len(value) != 1 {
		return "", false
	}
	input, ok := value["input"].(string)
	if !ok || strings.TrimSpace(input) == "" {
		return "", false
	}
	return input, true
}

func buildParsedToolCall(raw map[string]any, allowedTools map[string]responseToolDescriptor, requiredTool string, allowImplicitType bool) (*parsedToolCall, error) {
	callType, _ := raw["type"].(string)
	rawName, _ := raw["name"].(string)
	rawName = strings.TrimSpace(rawName)
	normalizedName := normalizeToolName(rawName)
	if normalizedName == "" {
		return nil, errors.New("tool call name is empty")
	}
	name, _ := resolveToolNameForCatalog(rawName, normalizedName, allowedTools)

	toolDesc, ok := allowedTools[name]
	if len(allowedTools) > 0 && !ok {
		return nil, fmt.Errorf("tool %q is not declared in request tools", name)
	}
	if requiredTool != "" && name != requiredTool {
		return nil, fmt.Errorf("tool_choice requires %q, got %q", requiredTool, name)
	}

	_, hasArguments := raw["arguments"]
	_, hasInput := raw["input"]
	if hasArguments && !hasInput && ok && !toolDesc.Structured {
		if normalizedInput, changed := normalizeCustomToolInputArgument(raw["arguments"]); changed {
			raw["input"] = normalizedInput
			delete(raw, "arguments")
			hasArguments = false
			hasInput = true
			if callType == "" && allowImplicitType {
				callType = "custom_tool_call"
			}
		}
	}
	if hasArguments && hasInput {
		return nil, fmt.Errorf("tool call for %q must not provide both arguments and input", name)
	}
	if hasInput && !hasArguments && ok && toolDesc.Structured {
		if normalizedArgs, changed := normalizeStructuredToolInputArgument(name, rawName, raw["input"]); changed {
			raw["arguments"] = normalizedArgs
			delete(raw, "input")
			hasArguments = true
			hasInput = false
			if callType == "" && allowImplicitType {
				callType = "function_call"
			}
		}
	}
	if hasArguments && ok && toolDesc.Structured {
		if normalizedArgs, changed := normalizeStructuredToolArguments(name, rawName, raw["arguments"]); changed {
			raw["arguments"] = normalizedArgs
		}
	}
	if callType == "" {
		if !allowImplicitType {
			return nil, fmt.Errorf("tool call for %q must include type", name)
		}
		if ok && toolDesc.Type == "web_search" {
			callType = "web_search_call"
		} else if ok && !toolDesc.Structured {
			callType = "custom_tool_call"
		} else {
			callType = "function_call"
		}
	}

	generatedID, err := generateRequestID()
	if err != nil {
		return nil, err
	}
	callID := "call_" + strings.Replace(generatedID, "chatcmpl-", "", 1)
	itemID, err := generateToolProtocolItemID(callType)
	if err != nil {
		return nil, err
	}
	switch callType {
	case "web_search_call":
		if hasInput {
			return nil, fmt.Errorf("web_search_call for %q must use arguments, not input", name)
		}
		if !hasArguments {
			return nil, fmt.Errorf("web_search_call for %q must include arguments", name)
		}
		query, err := extractWebSearchQuery(raw["arguments"])
		if err != nil {
			return nil, fmt.Errorf("web_search_call for %q: %w", name, err)
		}
		item := mustMarshalRawJSON(map[string]any{
			"id":     itemID,
			"type":   "web_search_call",
			"status": "completed",
			"action": map[string]any{
				"type":    "search",
				"query":   query,
				"queries": []string{query},
			},
		})
		argsText := mustMarshalJSONText(map[string]any{"query": query})
		return &parsedToolCall{
			item:         item,
			conversation: ChatMessage{Role: "assistant", Content: formatToolCallSummary(name, callID, argsText)},
		}, nil
	case "function_call":
		if hasInput {
			return nil, fmt.Errorf("function_call for %q must use arguments, not input", name)
		}
		if !hasArguments {
			return nil, fmt.Errorf("function_call for %q must include arguments", name)
		}
		args := raw["arguments"]
		var argsText string
		switch value := args.(type) {
		case string:
			argsText = value
		default:
			data, err := json.Marshal(value)
			if err != nil {
				return nil, fmt.Errorf("marshal function arguments: %w", err)
			}
			argsText = string(data)
		}
		if ok && !toolDesc.Structured {
			return nil, fmt.Errorf("tool %q is declared as freeform but model emitted function_call", name)
		}
		if !json.Valid([]byte(argsText)) {
			return nil, fmt.Errorf("function_call arguments for %q are not valid JSON", name)
		}
		if !strings.HasPrefix(strings.TrimSpace(argsText), "{") {
			return nil, fmt.Errorf("function_call arguments for %q must be a JSON object", name)
		}
		namespace := ""
		if ok {
			namespace = toolDesc.Namespace
		}
		if namespace == "" {
			namespace = inferMCPToolNamespace(name)
		}
		itemName := name
		if namespace != "" {
			itemName = bareMCPToolName(name, namespace)
		}
		itemMap := map[string]any{
			"id":        itemID,
			"type":      "function_call",
			"name":      itemName,
			"arguments": argsText,
			"call_id":   callID,
			"status":    "completed",
		}
		if namespace != "" {
			itemMap["namespace"] = namespace
		}
		item := mustMarshalRawJSON(itemMap)
		return &parsedToolCall{
			item:         item,
			conversation: ChatMessage{Role: "assistant", Content: formatToolCallSummary(name, callID, argsText)},
		}, nil
	case "custom_tool_call":
		if hasArguments {
			return nil, fmt.Errorf("custom_tool_call for %q must use input, not arguments", name)
		}
		if !hasInput {
			return nil, fmt.Errorf("custom_tool_call for %q must include input", name)
		}
		var input string
		switch value := raw["input"].(type) {
		case nil:
			return nil, fmt.Errorf("custom_tool_call input for %q must not be null", name)
		case string:
			input = value
		default:
			inputData, err := json.Marshal(value)
			if err != nil {
				return nil, fmt.Errorf("custom_tool_call input for %q is not valid JSON: %w", name, err)
			}
			input = string(inputData)
		}
		if ok && toolDesc.Structured {
			return nil, fmt.Errorf("tool %q is declared as structured but model emitted custom_tool_call", name)
		}
		item := mustMarshalRawJSON(map[string]any{
			"id":      itemID,
			"type":    "custom_tool_call",
			"name":    name,
			"input":   input,
			"call_id": callID,
			"status":  "completed",
		})
		return &parsedToolCall{
			item:         item,
			conversation: ChatMessage{Role: "assistant", Content: formatToolCallSummary(name, callID, input)},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported tool call type %q", callType)
	}
}

func generateToolProtocolItemID(callType string) (string, error) {
	prefix := "item_"
	switch callType {
	case "web_search_call":
		prefix = "ws_"
	case "function_call":
		prefix = "fc_"
	case "custom_tool_call":
		prefix = "ctc_"
	}
	generatedID, err := generateRequestID()
	if err != nil {
		return "", err
	}
	return prefix + strings.Replace(generatedID, "chatcmpl-", "", 1), nil
}

func extractWebSearchQuery(args any) (string, error) {
	switch value := args.(type) {
	case string:
		var decoded map[string]any
		if err := json.Unmarshal([]byte(value), &decoded); err != nil {
			return "", fmt.Errorf("arguments are not valid JSON: %w", err)
		}
		return extractWebSearchQuery(decoded)
	case map[string]any:
		if query, ok := firstStringField(value, "query"); ok {
			return query, nil
		}
		if action, ok := value["action"].(map[string]any); ok {
			if query, ok := firstStringField(action, "query"); ok {
				return query, nil
			}
		}
		return "", errors.New("missing query")
	default:
		return "", errors.New("arguments must be an object")
	}
}

func mustMarshalJSONText(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}
