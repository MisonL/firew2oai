package proxy

import (
	"encoding/json"
	"strings"
)

type executionPolicy struct {
	Enabled             bool
	Stage               string
	RequireTool         bool
	ReadLoop            bool
	NextCommand         string
	ForceSingleToolCall bool
	AllowTruncateToMax  bool
}

type executionHistorySignals struct {
	ToolCalls  int
	ReadCalls  int
	WriteCalls int
	TestCalls  int
	Commands   []string
}

func buildExecutionPolicy(model, currentTask string, historyItems []json.RawMessage, hasTools, toolsDisabled, autoRequireTool bool) executionPolicy {
	strictLoop := modelNeedsStrictToolLoop(model)
	policy := executionPolicy{
		ForceSingleToolCall: strictLoop,
		AllowTruncateToMax:  strictLoop,
	}

	task := strings.TrimSpace(currentTask)
	if task == "" || !hasTools || toolsDisabled || !taskLikelyNeedsTools(task) {
		return policy
	}

	signals := collectExecutionHistorySignals(historyItems)
	requiredCommands := dedupePreserveOrder(extractRequiredCommands(task))
	requiredFiles := dedupePreserveOrder(taskFilePathPattern.FindAllString(task, -1))
	needsWrite := taskLikelyNeedsWrite(task)
	nextCommand := chooseNextExecutionCommand(requiredCommands, requiredFiles, signals, needsWrite)

	policy.Enabled = true
	policy.NextCommand = nextCommand
	switch {
	case signals.ToolCalls == 0:
		policy.Stage = "explore"
		policy.RequireTool = true
	case needsWrite && signals.WriteCalls == 0:
		policy.Stage = "execute"
		policy.RequireTool = true
		policy.ReadLoop = signals.ReadCalls >= 2
	case nextCommand != "":
		policy.Stage = "verify"
		policy.RequireTool = true
		policy.ReadLoop = signals.ReadCalls >= 2
	default:
		policy.Stage = "finalize"
	}

	if autoRequireTool && signals.ToolCalls == 0 {
		policy.RequireTool = true
	}
	return policy
}

func buildExecutionPolicyPromptBlock(policy executionPolicy) string {
	if !policy.Enabled {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n<EXECUTION_POLICY>\n")
	b.WriteString("Stage: ")
	b.WriteString(policy.Stage)
	b.WriteByte('\n')
	if policy.RequireTool {
		b.WriteString("This turn must emit AI_ACTIONS mode tool. Do not emit mode final yet.\n")
	}
	if policy.Stage == "finalize" {
		b.WriteString("Stage finalize reached. Do not emit AI_ACTIONS mode tool. Return the final text answer now.\n")
	}
	if policy.ReadLoop {
		b.WriteString("Read loop detected. Do not repeat pwd/ls/cat/sed -n style commands.\n")
	}
	if cmd := strings.TrimSpace(policy.NextCommand); cmd != "" {
		b.WriteString("Next preferred command via exec_command:\n- ")
		b.WriteString(cmd)
		b.WriteByte('\n')
	}
	b.WriteString("</EXECUTION_POLICY>\n")
	return b.String()
}

func applyExecutionPolicyToParseResult(result parsedToolCallBatchResult, policy executionPolicy, toolCatalog map[string]responseToolDescriptor, constraints toolProtocolConstraints) parsedToolCallBatchResult {
	if !policy.Enabled || !policy.RequireTool || constraints.RequiredTool != "" {
		return result
	}

	if len(result.calls) > 0 {
		if shouldRewriteReadOnlyCallsToNext(result.calls, policy.NextCommand) {
			if synthetic, ok := buildSyntheticExecCommandCall(policy.NextCommand, toolCatalog, constraints.RequiredTool); ok {
				result.calls = []parsedToolCall{synthetic}
				result.err = nil
				result.visibleText = ""
				result.mode = toolProtocolModeAIActionsTool
				result.candidateFound = true
			}
		}
		return result
	}

	if synthetic, ok := buildSyntheticExecCommandCall(policy.NextCommand, toolCatalog, constraints.RequiredTool); ok {
		result.calls = []parsedToolCall{synthetic}
		result.err = nil
		result.visibleText = ""
		result.mode = toolProtocolModeAIActionsTool
		result.candidateFound = true
	}
	return result
}

func shouldRewriteReadOnlyCallsToNext(calls []parsedToolCall, nextCommand string) bool {
	next := strings.TrimSpace(nextCommand)
	if next == "" || len(calls) == 0 {
		return false
	}
	allReadOnly := true
	matchedNext := false
	for _, call := range calls {
		name, command, ok := parsedToolCallInvocation(call)
		if !ok {
			return false
		}
		if !isReadOnlyInvocation(name, command) {
			allReadOnly = false
			break
		}
		if name == "exec_command" && hasSeenCommand([]string{command}, next) {
			matchedNext = true
		}
	}
	return allReadOnly && !matchedNext
}

func modelNeedsStrictToolLoop(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(lower, "minimax-m2p5"),
		strings.Contains(lower, "kimi-k2p5"),
		strings.Contains(lower, "glm-4p7"),
		strings.Contains(lower, "deepseek-v3p1"):
		return true
	default:
		return false
	}
}

func collectExecutionHistorySignals(historyItems []json.RawMessage) executionHistorySignals {
	signals := executionHistorySignals{
		Commands: make([]string, 0, 8),
	}

	for _, raw := range historyItems {
		if len(raw) == 0 {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}

		typ, _ := item["type"].(string)
		switch typ {
		case "function_call":
			name, _ := item["name"].(string)
			normalizedName := normalizeToolName(name)
			if normalizedName == "" {
				continue
			}
			signals.ToolCalls++
			if isMutationToolName(normalizedName) {
				signals.WriteCalls++
				continue
			}
			if normalizedName != "exec_command" {
				continue
			}
			command := extractExecCommandFromFunctionCall(item, normalizedName)
			if command == "" {
				continue
			}
			signals.Commands = append(signals.Commands, command)
			if isTestCommand(command) {
				signals.TestCalls++
				continue
			}
			if isMutationCommand(command) {
				signals.WriteCalls++
				continue
			}
			if isReadOnlyCommand(command) {
				signals.ReadCalls++
			}
		case "custom_tool_call":
			name, _ := item["name"].(string)
			normalizedName := normalizeToolName(name)
			if normalizedName == "" {
				continue
			}
			signals.ToolCalls++
			if isMutationToolName(normalizedName) {
				signals.WriteCalls++
			}
		}
	}

	return signals
}

func chooseNextExecutionCommand(requiredCommands, requiredFiles []string, signals executionHistorySignals, needsWrite bool) string {
	for _, command := range requiredCommands {
		if hasSeenCommand(signals.Commands, command) {
			continue
		}
		return command
	}

	for _, filePath := range requiredFiles {
		if hasSeenReadForFile(signals.Commands, filePath) {
			continue
		}
		return buildReadFileCommand(filePath)
	}

	if needsWrite && signals.WriteCalls == 0 && len(requiredFiles) > 0 {
		// Keep the model focused on task files instead of drifting into ls/pwd loops.
		return buildReadFileCommand(requiredFiles[0])
	}
	if needsWrite && signals.TestCalls == 0 {
		return "go test ./internal/proxy"
	}
	return ""
}

func hasSeenCommand(seen []string, target string) bool {
	targetKey := normalizeCommandForCompare(target)
	if targetKey == "" {
		return false
	}
	for _, command := range seen {
		key := normalizeCommandForCompare(command)
		if key == "" {
			continue
		}
		if key == targetKey || strings.Contains(key, targetKey) || strings.Contains(targetKey, key) {
			return true
		}
	}
	return false
}

func normalizeCommandForCompare(command string) string {
	if command == "" {
		return ""
	}
	return strings.Join(strings.Fields(strings.ToLower(command)), " ")
}

func hasSeenReadForFile(seen []string, filePath string) bool {
	pathKey := normalizeCommandForCompare(filePath)
	if pathKey == "" {
		return false
	}
	for _, command := range seen {
		if !isReadOnlyCommand(command) {
			continue
		}
		commandKey := normalizeCommandForCompare(command)
		if commandKey == "" {
			continue
		}
		if strings.Contains(commandKey, pathKey) {
			return true
		}
	}
	return false
}

func isReadOnlyCommand(command string) bool {
	lower := strings.ToLower(strings.TrimSpace(command))
	if lower == "" {
		return false
	}

	prefixes := []string{
		"pwd",
		"ls",
		"cat ",
		"sed -n",
		"head ",
		"tail ",
		"find ",
		"rg ",
		"grep ",
		"awk ",
		"wc ",
		"tree",
		"stat ",
		"git status",
		"git diff",
		"git show",
		"go env",
		"go list",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func isMutationCommand(command string) bool {
	lower := strings.ToLower(strings.TrimSpace(command))
	if lower == "" {
		return false
	}
	for _, token := range []string{
		"apply_patch",
		"git apply",
		"sed -i",
		"perl -pi",
		"tee ",
		" >",
		" >>",
		"mv ",
		"cp ",
		"rm ",
		"touch ",
		"mkdir ",
		"gofmt -w",
		"goimports -w",
	} {
		if strings.Contains(lower, token) {
			return true
		}
	}
	return false
}

func isTestCommand(command string) bool {
	lower := " " + strings.ToLower(strings.TrimSpace(command)) + " "
	for _, token := range []string{
		" go test ",
		" pytest ",
		" cargo test ",
		" npm test ",
		" pnpm test ",
		" bun test ",
		" make test ",
		" golangci-lint ",
		" go vet ",
	} {
		if strings.Contains(lower, token) {
			return true
		}
	}
	return false
}

func isMutationToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "apply_patch", "write_file", "edit_file", "replace_in_file", "append_file", "create_file":
		return true
	default:
		return false
	}
}

func extractExecCommandFromFunctionCall(item map[string]any, normalizedName string) string {
	if normalizedName != "exec_command" {
		return ""
	}
	argsText, _ := item["arguments"].(string)
	return extractExecCommandFromArgumentsText(argsText)
}

func extractExecCommandFromArgumentsText(argsText string) string {
	trimmed := strings.TrimSpace(argsText)
	if trimmed == "" {
		return ""
	}

	var decoded any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err == nil {
		if normalizedArgs, changed := normalizeExecCommandArguments(decoded, "exec_command"); changed {
			switch value := normalizedArgs.(type) {
			case map[string]any:
				if command, ok := firstStringField(value, "cmd", "command", "input"); ok {
					return command
				}
			case string:
				return strings.TrimSpace(value)
			}
		}
		if rawMap, ok := decoded.(map[string]any); ok {
			if command, ok := firstStringField(rawMap, "cmd", "command", "input"); ok {
				return command
			}
		}
	}

	if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
		return ""
	}
	return trimmed
}

func allParsedCallsReadOnly(calls []parsedToolCall) bool {
	if len(calls) == 0 {
		return false
	}
	for _, call := range calls {
		name, command, ok := parsedToolCallInvocation(call)
		if !ok {
			return false
		}
		if !isReadOnlyInvocation(name, command) {
			return false
		}
	}
	return true
}

func parsedToolCallInvocation(call parsedToolCall) (name, command string, ok bool) {
	var item map[string]any
	if err := json.Unmarshal(call.item, &item); err != nil {
		return "", "", false
	}
	name, _ = item["name"].(string)
	name = normalizeToolName(name)
	if name == "" {
		return "", "", false
	}

	callType, _ := item["type"].(string)
	switch callType {
	case "function_call":
		argsText, _ := item["arguments"].(string)
		if name == "exec_command" {
			command = extractExecCommandFromArgumentsText(argsText)
		}
		return name, command, true
	case "custom_tool_call":
		input, _ := item["input"].(string)
		if name == "exec_command" {
			command = strings.TrimSpace(input)
		}
		return name, command, true
	default:
		return name, "", true
	}
}

func isReadOnlyInvocation(name, command string) bool {
	if isMutationToolName(name) {
		return false
	}
	if name == "exec_command" {
		return isReadOnlyCommand(command)
	}
	lowerName := strings.ToLower(strings.TrimSpace(name))
	if strings.Contains(lowerName, "read") || strings.Contains(lowerName, "list") {
		return true
	}
	return false
}

func buildSyntheticExecCommandCall(command string, toolCatalog map[string]responseToolDescriptor, requiredTool string) (parsedToolCall, bool) {
	command = strings.TrimSpace(command)
	if command == "" {
		return parsedToolCall{}, false
	}
	if requiredTool != "" && requiredTool != "exec_command" {
		return parsedToolCall{}, false
	}
	if len(toolCatalog) > 0 {
		desc, ok := toolCatalog["exec_command"]
		if !ok || !desc.Structured {
			return parsedToolCall{}, false
		}
	}

	call, err := buildParsedToolCall(map[string]any{
		"type":      "function_call",
		"name":      "exec_command",
		"arguments": map[string]any{"cmd": command},
	}, toolCatalog, requiredTool, false)
	if err != nil {
		return parsedToolCall{}, false
	}
	return *call, true
}
