package proxy

import (
	"encoding/json"
	"strings"
)

type executionEvidence struct {
	Commands []string
	Outputs  []string
}

func buildExecutionEvidence(historyItems []json.RawMessage) executionEvidence {
	if len(historyItems) == 0 {
		return executionEvidence{}
	}

	callIDToAction := make(map[string]string, 8)
	commands := make([]string, 0, 8)
	outputs := make([]string, 0, 8)
	for _, raw := range historyItems {
		if len(raw) == 0 {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}

		itemType, _ := item["type"].(string)
		switch itemType {
		case "function_call", "mcp_tool_call":
			name := evidenceActionNameFromHistoryItem(item)
			normalizedName := normalizeToolName(name)
			action := normalizedName
			if normalizedName == "exec_command" {
				action = extractExecCommandFromFunctionCall(item, normalizedName)
			}
			action = strings.TrimSpace(action)
			if action == "" {
				continue
			}
			commands = append(commands, action)
			callID, _ := item["call_id"].(string)
			if callID != "" {
				callIDToAction[callID] = action
			}
			if itemType == "mcp_tool_call" {
				outputText, success := extractToolOutputText(item["result"])
				if success == nil {
					if _, hasError := item["error"]; hasError && item["error"] == nil {
						flag := true
						success = &flag
					}
				}
				if success == nil {
					if status, _ := item["status"].(string); strings.EqualFold(strings.TrimSpace(status), "completed") {
						flag := true
						success = &flag
					}
				}
				outputs = appendExecutionEvidenceOutput(outputs, action, outputText, success)
			}
		case "collab_tool_call":
			action := normalizeCollaborationEvidenceToolName(asString(item["tool"]))
			action = strings.TrimSpace(action)
			if action == "" {
				continue
			}
			commands = append(commands, action)
			text := buildCollaborationEvidenceOutputText(item, action)
			success := buildCollaborationEvidenceSuccess(item, action, text)
			outputs = appendExecutionEvidenceOutput(outputs, action, text, success)
		case "custom_tool_call":
			name, _ := item["name"].(string)
			normalizedName := normalizeToolName(name)
			action := normalizedName
			if normalizedName == "exec_command" {
				input, _ := item["input"].(string)
				action = strings.TrimSpace(input)
			}
			action = strings.TrimSpace(action)
			if action == "" {
				continue
			}
			commands = append(commands, action)
			callID, _ := item["call_id"].(string)
			if callID != "" {
				callIDToAction[callID] = action
			}
		case "function_call_output", "custom_tool_call_output", "mcp_tool_call_output":
			callID, _ := item["call_id"].(string)
			action := strings.TrimSpace(callIDToAction[callID])
			text, success := extractToolOutputText(item["output"])
			if success == nil {
				if isTestCommand(action) {
					success = inferTestCommandOutputSuccess(text)
				} else {
					success = inferToolOutputSuccess(text)
				}
			}
			if strings.TrimSpace(text) == "" {
				if encoded, err := json.Marshal(item["output"]); err == nil {
					text = string(encoded)
				}
			}
			if success == nil {
				success = inferCollaborationToolOutputSuccess(action, text)
			}
			outputs = appendExecutionEvidenceOutput(outputs, action, text, success)
		}
	}

	commands = dedupePreserveOrder(commands)
	outputs = dedupePreserveOrder(outputs)
	if len(commands) > 6 {
		commands = append([]string(nil), commands[len(commands)-6:]...)
	}
	if len(outputs) > 6 {
		outputs = append([]string(nil), outputs[len(outputs)-6:]...)
	}
	return executionEvidence{
		Commands: commands,
		Outputs:  outputs,
	}
}

func normalizeCollaborationEvidenceToolName(name string) string {
	normalized := normalizeToolName(name)
	for _, candidate := range toolNameAliasVariants(normalized) {
		if strings.EqualFold(candidate, "wait_agent") {
			return "wait_agent"
		}
	}
	return normalized
}

func buildCollaborationEvidenceOutputText(item map[string]any, action string) string {
	switch action {
	case "spawn_agent":
		if ids, ok := item["receiver_thread_ids"].([]any); ok {
			for _, raw := range ids {
				id := strings.TrimSpace(asString(raw))
				if id == "" {
					continue
				}
				return mustMarshalJSONText(map[string]any{"agent_id": id})
			}
		}
	case "wait_agent":
		if states, ok := item["agents_states"].(map[string]any); ok && len(states) > 0 {
			normalizedStates := make(map[string]any, len(states))
			for agentID, rawState := range states {
				stateMap, ok := rawState.(map[string]any)
				if !ok {
					normalizedStates[agentID] = rawState
					continue
				}
				normalized := cloneMap(stateMap)
				if strings.TrimSpace(asString(normalized["completed"])) == "" &&
					strings.EqualFold(strings.TrimSpace(asString(normalized["status"])), "completed") {
					if message := strings.TrimSpace(asString(normalized["message"])); message != "" {
						normalized["completed"] = message
					}
				}
				normalizedStates[agentID] = normalized
			}
			return mustMarshalJSONText(map[string]any{
				"status":    normalizedStates,
				"timed_out": false,
			})
		}
	case "close_agent":
		if previousStatus, ok := item["previous_status"].(map[string]any); ok && len(previousStatus) > 0 {
			return mustMarshalJSONText(map[string]any{"previous_status": previousStatus})
		}
		if states, ok := item["agents_states"].(map[string]any); ok && len(states) > 0 {
			for agentID, rawState := range states {
				stateMap, ok := rawState.(map[string]any)
				if !ok {
					continue
				}
				completed := strings.TrimSpace(asString(stateMap["completed"]))
				if completed == "" && strings.EqualFold(strings.TrimSpace(asString(stateMap["status"])), "completed") {
					completed = strings.TrimSpace(asString(stateMap["message"]))
				}
				if completed == "" {
					continue
				}
				return mustMarshalJSONText(map[string]any{
					"previous_status": map[string]any{
						"agent_id":  agentID,
						"completed": completed,
					},
				})
			}
		}
	}
	if encoded, err := json.Marshal(item); err == nil {
		return string(encoded)
	}
	return ""
}

func buildCollaborationEvidenceSuccess(item map[string]any, action, text string) *bool {
	if historyCollabToolCallSucceeded(item) {
		flag := true
		return &flag
	}
	if inferred := inferCollaborationToolOutputSuccess(action, text); inferred != nil {
		return inferred
	}
	return nil
}

func inferCollaborationToolOutputSuccess(action, text string) *bool {
	action = normalizeToolName(strings.TrimSpace(action))
	if action == "wait" {
		action = "wait_agent"
	}
	trimmed := strings.TrimSpace(text)
	lower := strings.ToLower(trimmed)
	switch action {
	case "spawn_agent":
		if strings.Contains(lower, `"agent_id"`) {
			flag := true
			return &flag
		}
	case "wait_agent":
		if containsJSONBoolLiteral(trimmed, "timed_out", true) {
			flag := false
			return &flag
		}
		if strings.Contains(lower, `"completed"`) || strings.Contains(lower, `"status"`) {
			flag := true
			return &flag
		}
	case "close_agent":
		if strings.Contains(lower, `"previous_status"`) || strings.Contains(lower, `"completed"`) {
			flag := true
			return &flag
		}
	}
	return nil
}

func containsJSONBoolLiteral(text, key string, value bool) bool {
	if key == "" {
		return false
	}
	for _, candidate := range jsonCandidates(text) {
		var decoded any
		decoder := json.NewDecoder(strings.NewReader(candidate))
		if err := decoder.Decode(&decoded); err != nil {
			continue
		}
		if jsonValueContainsBoolLiteral(decoded, key, value) {
			return true
		}
	}
	return false
}

func jsonCandidates(text string) []string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	candidates := []string{trimmed}
	for idx, r := range trimmed {
		if r == '{' || r == '[' {
			candidates = append(candidates, trimmed[idx:])
		}
	}
	return candidates
}

func jsonValueContainsBoolLiteral(value any, key string, expected bool) bool {
	switch typed := value.(type) {
	case map[string]any:
		if actual, ok := typed[key].(bool); ok && actual == expected {
			return true
		}
		for _, child := range typed {
			if jsonValueContainsBoolLiteral(child, key, expected) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if jsonValueContainsBoolLiteral(child, key, expected) {
				return true
			}
		}
	}
	return false
}

func evidenceActionNameFromHistoryItem(item map[string]any) string {
	name, _ := item["name"].(string)
	if namespace, _ := item["namespace"].(string); namespace != "" && name != "" {
		return joinNamespaceToolName(namespace, name)
	}
	server, _ := item["server"].(string)
	tool, _ := item["tool"].(string)
	server = strings.TrimSpace(server)
	tool = strings.TrimSpace(tool)
	if server != "" && tool != "" {
		return joinNamespaceToolName("mcp__"+server+"__", tool)
	}
	return name
}

func appendExecutionEvidenceOutput(outputs []string, action, text string, success *bool) []string {
	text = strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	if strings.Contains(strings.ToLower(text), "source:") {
		text = evidenceSourcePrefixPattern.ReplaceAllString(text, "")
		text = strings.TrimSpace(strings.TrimLeft(text, "# "))
	}
	if pathSummary := extractEvidencePathSummary(text); pathSummary != "" {
		text = pathSummary
	}
	if text == "" {
		return outputs
	}
	var summary strings.Builder
	if action != "" {
		summary.WriteString(action)
		summary.WriteString(" => ")
	}
	if success != nil {
		if *success {
			summary.WriteString("success=true ")
		} else {
			summary.WriteString("success=false ")
		}
	}
	summary.WriteString(truncateString(text, 180))
	return append(outputs, summary.String())
}

func buildExecutionEvidencePromptBlock(evidence executionEvidence) string {
	if len(evidence.Commands) == 0 && len(evidence.Outputs) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n<EXECUTION_EVIDENCE>\n")
	if len(evidence.Commands) > 0 {
		b.WriteString("Recent executed commands:\n")
		for _, command := range evidence.Commands {
			b.WriteString("- ")
			b.WriteString(command)
			b.WriteByte('\n')
		}
	}
	if len(evidence.Outputs) > 0 {
		b.WriteString("Recent tool outputs (truncated):\n")
		for _, output := range evidence.Outputs {
			b.WriteString("- ")
			b.WriteString(output)
			b.WriteByte('\n')
		}
	}
	b.WriteString("Use this evidence when deciding RESULT/TEST claims and avoid repeating completed commands.\n")
	b.WriteString("</EXECUTION_EVIDENCE>\n")
	return b.String()
}
