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

	callIDToCommand := make(map[string]string, 8)
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
		case "function_call":
			name, _ := item["name"].(string)
			normalizedName := normalizeToolName(name)
			if normalizedName != "exec_command" {
				continue
			}
			command := extractExecCommandFromFunctionCall(item, normalizedName)
			if command == "" {
				continue
			}
			commands = append(commands, command)
			callID, _ := item["call_id"].(string)
			if callID != "" {
				callIDToCommand[callID] = command
			}
		case "custom_tool_call":
			name, _ := item["name"].(string)
			normalizedName := normalizeToolName(name)
			if normalizedName != "exec_command" {
				continue
			}
			input, _ := item["input"].(string)
			command := strings.TrimSpace(input)
			if command == "" {
				continue
			}
			commands = append(commands, command)
			callID, _ := item["call_id"].(string)
			if callID != "" {
				callIDToCommand[callID] = command
			}
		case "function_call_output", "custom_tool_call_output":
			callID, _ := item["call_id"].(string)
			command := strings.TrimSpace(callIDToCommand[callID])
			text, success := extractToolOutputText(item["output"])
			if strings.TrimSpace(text) == "" {
				if encoded, err := json.Marshal(item["output"]); err == nil {
					text = string(encoded)
				}
			}
			text = strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
			if text == "" {
				continue
			}
			var summary strings.Builder
			if command != "" {
				summary.WriteString(command)
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
			outputs = append(outputs, summary.String())
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
