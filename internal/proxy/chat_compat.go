package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func (m *ChatMessage) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		Name       *string         `json:"name,omitempty"`
		ToolCallID string          `json:"tool_call_id,omitempty"`
		ToolCalls  []ChatToolCall  `json:"tool_calls,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	content, err := parseChatMessageContent(raw.Content)
	if err != nil {
		return err
	}
	m.Role = raw.Role
	m.Content = content
	m.Name = raw.Name
	m.ToolCallID = raw.ToolCallID
	m.ToolCalls = raw.ToolCalls
	return nil
}

func parseChatMessageContent(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}

	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return text, nil
	}

	var parts []map[string]any
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return "", fmt.Errorf("message content must be a string or text content array")
	}

	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		typ, _ := part["type"].(string)
		if typ == "" {
			typ = "text"
		}
		if typ == "text" || typ == "input_text" {
			if value, ok := part["text"].(string); ok {
				texts = append(texts, value)
			}
			continue
		}
		return "", fmt.Errorf("unsupported message content block type %q", typ)
	}
	return strings.Join(texts, ""), nil
}

func buildChatPrompt(messages []ChatMessage, tools json.RawMessage, toolChoice json.RawMessage, maxToolCalls int) string {
	if resolveToolChoice(toolChoice).DisableTools {
		tools = nil
	}
	prompt := messagesToPrompt(messages)
	toolInstructions := summarizeResponsesTools(tools)
	toolCatalog := buildResponseToolCatalog(tools)
	choiceInstructions := buildToolChoiceInstructions(toolChoice)
	currentTask := latestUserTask(messages)
	requiresToolLoop := toolInstructions != "" && taskLikelyNeedsTools(currentTask)
	if toolInstructions == "" && choiceInstructions == "" {
		return prompt
	}

	var builder strings.Builder
	builder.Grow(len(prompt) + len(toolInstructions) + len(choiceInstructions) + 512)
	builder.WriteString("You are serving an OpenAI Chat Completions request through a text-only upstream model.\n")
	builder.WriteString("Follow the conversation exactly and answer the latest user request.\n")
	builder.WriteString("Do not summarize repository guidelines or system/developer instructions as the final answer.\n")
	builder.WriteString("If the latest user request names specific files or commands, prioritize those tool calls first.\n")
	builder.WriteString("Do not ask for more task context when the latest user request is already actionable.\n")
	builder.WriteString("If the latest user request defines an exact output format, follow it exactly and output nothing extra.\n")
	if requiresToolLoop {
		builder.WriteString("The latest user request requires workspace execution. Emit tool calls before any final answer text.\n")
	}
	if explicitToolBlock := buildExplicitToolUseBlock(currentTask, toolCatalog); explicitToolBlock != "" {
		builder.WriteString(explicitToolBlock)
	}
	if gate := buildTaskCompletionGate(currentTask); gate != "" {
		builder.WriteString(gate)
	}
	if toolInstructions != "" {
		appendToolProtocolInstructions(&builder, false, maxToolCalls)
	}
	if prompt != "" {
		builder.WriteString("\n<CONVERSATION>\n")
		builder.WriteString(prompt)
		builder.WriteString("\n</CONVERSATION>\n")
	}
	if toolInstructions != "" {
		builder.WriteString("\n<AVAILABLE_TOOLS>\n")
		builder.WriteString(toolInstructions)
		builder.WriteString("\n</AVAILABLE_TOOLS>\n")
	}
	if choiceInstructions != "" {
		builder.WriteString("\n<TOOL_CHOICE>\n")
		builder.WriteString(choiceInstructions)
		builder.WriteString("\n</TOOL_CHOICE>\n")
	}
	return builder.String()
}

func normalizeChatTools(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("[]")) {
		return nil
	}

	var tools []map[string]any
	if err := json.Unmarshal(trimmed, &tools); err != nil {
		return nil
	}

	normalized := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		toolType, _ := tool["type"].(string)
		if toolType == "function" {
			if fn, ok := tool["function"].(map[string]any); ok {
				item := map[string]any{
					"type":        "function",
					"name":        fn["name"],
					"description": fn["description"],
					"parameters":  fn["parameters"],
				}
				normalized = append(normalized, item)
				continue
			}
		}
		normalized = append(normalized, cloneMap(tool))
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		return nil
	}
	return data
}

func normalizeChatToolChoice(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}

	var decoded any
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return nil
	}
	switch value := decoded.(type) {
	case string:
		switch value {
		case "none", "required":
			return mustMarshalRawJSON(value)
		default:
			return nil
		}
	case map[string]any:
		typ, _ := value["type"].(string)
		switch typ {
		case "none":
			return mustMarshalRawJSON("none")
		case "required":
			return mustMarshalRawJSON("required")
		case "function":
			fn, _ := value["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if name == "" {
				return nil
			}
			return mustMarshalRawJSON(map[string]any{"name": name})
		default:
			name, _ := value["name"].(string)
			if name == "" {
				return nil
			}
			return mustMarshalRawJSON(map[string]any{"name": name})
		}
	default:
		return nil
	}
}

func parseChatToolCallOutput(text string, allowedTools map[string]responseToolDescriptor, constraints toolProtocolConstraints) ([]ChatToolCall, string, error) {
	result := parseToolCallOutputsWithConstraints(text, allowedTools, constraints)
	if result.err != nil {
		return nil, result.visibleText, result.err
	}
	if len(result.calls) == 0 {
		return nil, result.visibleText, nil
	}

	toolCalls := make([]ChatToolCall, 0, len(result.calls))
	for _, parsed := range result.calls {
		var item map[string]any
		if err := json.Unmarshal(parsed.item, &item); err != nil {
			return nil, result.visibleText, err
		}
		typ, _ := item["type"].(string)
		if typ != "function_call" {
			return nil, result.visibleText, errors.New("OpenAI chat tool_calls only support structured function calls")
		}
		name, _ := item["name"].(string)
		args, err := chatToolCallArgumentsString(item["arguments"])
		if err != nil {
			return nil, result.visibleText, err
		}
		callID, _ := item["call_id"].(string)
		if callID == "" {
			generatedID, err := generateRequestID()
			if err != nil {
				return nil, result.visibleText, err
			}
			callID = "call_" + strings.Replace(generatedID, "chatcmpl-", "", 1)
		}
		toolCalls = append(toolCalls, ChatToolCall{
			ID:   callID,
			Type: "function",
			Function: ChatToolFunction{
				Name:      name,
				Arguments: args,
			},
		})
	}
	return toolCalls, result.visibleText, nil
}

func chatToolCallArgumentsString(value any) (string, error) {
	switch typed := value.(type) {
	case nil:
		return "{}", nil
	case string:
		if typed == "" {
			return "{}", nil
		}
		return typed, nil
	case map[string]any, []any:
		data, err := json.Marshal(typed)
		if err != nil {
			return "", err
		}
		return string(data), nil
	default:
		return "", fmt.Errorf("chat tool call arguments must be string or JSON object")
	}
}
