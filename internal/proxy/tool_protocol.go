package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	aiActionsStartMarker = "<<<AI_ACTIONS_V1>>>"
	aiActionsEndMarker   = "<<<END_AI_ACTIONS_V1>>>"
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
	RequiredTool string
	RequireTool  bool
	MaxCalls     int
}

func buildParsedToolOutputItems(calls []parsedToolCall) []json.RawMessage {
	items := make([]json.RawMessage, 0, len(calls))
	for _, call := range calls {
		items = append(items, call.item)
	}
	return items
}

func appendToolProtocolInstructions(builder *strings.Builder, supportsCustom bool, maxCalls int) {
	builder.WriteString("If you need tools, put the machine-readable control block at the very end of your reply.\n")
	builder.WriteString("Emit exactly one AI_ACTIONS block per reply.\n")
	builder.WriteString("Use only tool names listed in AVAILABLE_TOOLS. Never invent or rename a tool.\n")
	builder.WriteString("If the tool is exec_command, arguments must be exactly an object containing cmd, for example {\"cmd\":\"pwd\"}.\n")
	builder.WriteString("Do not emit read_file/cat/list_files aliases; use exec_command with cmd instead.\n")
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

func extractAIActionsBlock(text string) (aiActionsBlock, bool) {
	start := strings.LastIndex(text, aiActionsStartMarker)
	if start < 0 {
		return aiActionsBlock{}, false
	}

	end := strings.LastIndex(text, aiActionsEndMarker)
	if end < 0 || end < start {
		return aiActionsBlock{}, false
	}

	suffix := text[end+len(aiActionsEndMarker):]
	if strings.TrimSpace(suffix) != "" {
		return aiActionsBlock{}, false
	}

	payloadStart := start + len(aiActionsStartMarker)
	payload := strings.TrimSpace(text[payloadStart:end])
	if payload == "" {
		return aiActionsBlock{}, false
	}

	return aiActionsBlock{
		VisibleText: strings.TrimSpace(text[:start]),
		JSONText:    payload,
	}, true
}

func parseToolCallOutputs(text string, allowedTools map[string]responseToolDescriptor, requiredTool string) parsedToolCallBatchResult {
	return parseToolCallOutputsWithConstraints(text, allowedTools, toolProtocolConstraints{RequiredTool: requiredTool})
}

func parseToolCallOutputsWithConstraints(text string, allowedTools map[string]responseToolDescriptor, constraints toolProtocolConstraints) parsedToolCallBatchResult {
	if block, ok := extractAIActionsBlock(text); ok {
		return applyToolProtocolConstraints(parseAIActionsToolCallOutputs(block, allowedTools, constraints.RequiredTool), constraints)
	}

	legacy := parseLegacyToolCallOutputs(text, allowedTools, constraints.RequiredTool)
	return applyToolProtocolConstraints(legacy, constraints)
}

func parseAIActionsToolCallOutputs(block aiActionsBlock, allowedTools map[string]responseToolDescriptor, requiredTool string) parsedToolCallBatchResult {
	result := parsedToolCallBatchResult{
		candidateFound: true,
		visibleText:    block.VisibleText,
	}

	var envelope aiActionsEnvelope
	jsonText := strings.TrimSpace(stripMarkdownCodeFence(block.JSONText))
	if err := json.Unmarshal([]byte(jsonText), &envelope); err != nil {
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

func applyToolProtocolConstraints(result parsedToolCallBatchResult, constraints toolProtocolConstraints) parsedToolCallBatchResult {
	if result.err != nil {
		return result
	}
	if constraints.MaxCalls > 0 && len(result.calls) > constraints.MaxCalls {
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

func parseLegacyToolCallOutput(text string, allowedTools map[string]responseToolDescriptor, requiredTool string) parsedToolCallResult {
	batch := parseLegacyToolCallOutputs(text, allowedTools, requiredTool)
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

func parseLegacyToolCallOutputs(text string, allowedTools map[string]responseToolDescriptor, requiredTool string) parsedToolCallBatchResult {
	candidate := strings.TrimSpace(stripMarkdownCodeFence(text))
	if start := strings.IndexByte(candidate, '{'); start >= 0 {
		candidate = strings.TrimSpace(candidate[start:])
	}
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

func decodeLegacyToolCallSequence(candidate string) ([]map[string]any, error) {
	decoder := json.NewDecoder(strings.NewReader(candidate))
	raws := make([]map[string]any, 0, 1)
	for {
		var raw map[string]any
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		raws = append(raws, raw)
	}
	return raws, nil
}

func resolveToolNameForCatalog(rawName, normalized string, allowedTools map[string]responseToolDescriptor) (string, bool) {
	if len(allowedTools) == 0 {
		return normalized, true
	}
	if _, ok := allowedTools[normalized]; ok {
		return normalized, true
	}
	if _, ok := allowedTools[rawName]; ok {
		return rawName, true
	}
	for declared := range allowedTools {
		if strings.EqualFold(declared, rawName) {
			return declared, true
		}
	}
	return normalized, false
}

func firstStringField(values map[string]any, keys ...string) (string, bool) {
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
		return text, true
	}
	return "", false
}

func shellQuoteSingle(text string) string {
	return "'" + strings.ReplaceAll(text, "'", "'\"'\"'") + "'"
}

func buildReadFileCommand(path string) string {
	return "sed -n '1,200p' -- " + shellQuoteSingle(path)
}

func buildListFilesCommand(path string) string {
	return "ls -la -- " + shellQuoteSingle(path)
}

func normalizeExecCommandArguments(args any, sourceToolName string) (any, bool) {
	switch value := args.(type) {
	case string:
		command := strings.TrimSpace(value)
		if command == "" {
			return args, false
		}
		return map[string]any{"cmd": command}, true
	case map[string]any:
		if cmd, ok := firstStringField(value, "cmd"); ok {
			return map[string]any{"cmd": cmd}, true
		}
		if command, ok := firstStringField(value, "command", "command_line", "cmdline", "shell_command", "input"); ok {
			return map[string]any{"cmd": command}, true
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
	if hasArguments && hasInput {
		return nil, fmt.Errorf("tool call for %q must not provide both arguments and input", name)
	}
	if hasInput && !hasArguments && name == "exec_command" && ok && toolDesc.Structured {
		if inputText, inputOK := raw["input"].(string); inputOK && strings.TrimSpace(inputText) != "" {
			raw["arguments"] = map[string]any{"cmd": inputText}
			delete(raw, "input")
			hasArguments = true
			hasInput = false
			if callType == "" && allowImplicitType {
				callType = "function_call"
			}
		}
	}
	if hasArguments && name == "exec_command" && ok && toolDesc.Structured {
		if normalizedArgs, changed := normalizeExecCommandArguments(raw["arguments"], rawName); changed {
			raw["arguments"] = normalizedArgs
		}
	}
	if callType == "" {
		if !allowImplicitType {
			return nil, fmt.Errorf("tool call for %q must include type", name)
		}
		if ok && !toolDesc.Structured {
			callType = "custom_tool_call"
		} else {
			callType = "function_call"
		}
	}

	callID := "call_" + strings.Replace(generateRequestID(), "chatcmpl-", "", 1)
	switch callType {
	case "function_call":
		if hasInput {
			return nil, fmt.Errorf("function_call for %q must use arguments, not input", name)
		}
		if !hasArguments {
			return nil, fmt.Errorf("function_call for %q must include arguments", name)
		}
		args := raw["arguments"]
		argsText := "{}"
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
		item := mustMarshalRawJSON(map[string]any{
			"type":      "function_call",
			"name":      name,
			"arguments": argsText,
			"call_id":   callID,
			"status":    "completed",
		})
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
		input := ""
		if value, ok := raw["input"].(string); ok {
			input = value
		}
		if ok && toolDesc.Structured {
			return nil, fmt.Errorf("tool %q is declared as structured but model emitted custom_tool_call", name)
		}
		item := mustMarshalRawJSON(map[string]any{
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
