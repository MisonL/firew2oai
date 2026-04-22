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

func inferCollaborationToolOutputSuccess(action, text string) *bool {
	action = normalizeToolName(strings.TrimSpace(action))
	lower := strings.ToLower(strings.TrimSpace(text))
	switch action {
	case "spawn_agent":
		if strings.Contains(lower, `"agent_id"`) {
			flag := true
			return &flag
		}
	case "wait_agent":
		if strings.Contains(lower, `"timed_out":true`) {
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
