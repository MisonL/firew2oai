package proxy

import (
	"encoding/json"
	"log/slog"
	"path"
	"regexp"
	"strconv"
	"strings"
)

type executionPolicy struct {
	Enabled              bool
	Stage                string
	RequireTool          bool
	ReadLoop             bool
	PendingWrite         bool
	MissingFiles         []string
	EmptyFiles           []string
	RepeatedScaffold     []string
	NextCommand          string
	RequiredCommands     []string
	RequiredFiles        []string
	AllRequiredFilesSeen bool
	SeenCommands         []string
	ForceSingleToolCall  bool
	AllowTruncateToMax   bool
}

type executionHistorySignals struct {
	ToolCalls           int
	ReadCalls           int
	WriteCalls          int
	TestCalls           int
	Commands            []string
	CommandsWithResult  []string
	SuccessfulCommands  []string
	FailedCommands      []string
	EmptyCommands       []string
	CommandOutputs      map[string]string
	LastWritePos        int
	LastFailedTestPos   int
	ReadResultPosByFile map[string]int
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
	allMentionedFiles := dedupePreserveOrder(taskFilePathPattern.FindAllString(task, -1))
	requiredFiles := allMentionedFiles
	sequenceFiles := allMentionedFiles
	if needsWrite := taskLikelyNeedsWrite(task); needsWrite {
		writeTargets := dedupePreserveOrder(extractWriteTargetFiles(task))
		if len(writeTargets) > 0 {
			requiredFiles = writeTargets
			sequenceFiles = append(filterOutFiles(allMentionedFiles, writeTargets), writeTargets...)
		}
	}
	styleCommands := dedupePreserveOrder(extractStyleInspectionCommands(task))
	needsWrite := taskLikelyNeedsWrite(task)
	nextCommand := chooseNextExecutionCommandWithStyles(requiredCommands, sequenceFiles, styleCommands, signals, needsWrite)
	if seedWriteCommand := buildSeedWriteCommand(task, requiredFiles, signals); needsWrite && signals.WriteCalls == 0 && shouldPreferSeedWriteCommand(requiredFiles, signals, seedWriteCommand) {
		nextCommand = seedWriteCommand
	}
	missingFiles := collectMissingRequiredFiles(signals, requiredFiles)
	emptyFiles := collectEmptyRequiredFiles(signals, requiredFiles)
	repeatedScaffold := collectRepeatedScaffoldFiles(signals, requiredFiles)
	pendingWrite := needsWrite && (signals.WriteCalls == 0 || signals.LastFailedTestPos > signals.LastWritePos)
	if signals.LastFailedTestPos > signals.LastWritePos {
		if repairTarget := chooseRepairReadTarget(requiredFiles, signals); repairTarget != "" {
			nextCommand = buildReadFileCommand(repairTarget)
		}
	}

	policy.Enabled = true
	policy.NextCommand = nextCommand
	policy.RequiredCommands = requiredCommands
	policy.RequiredFiles = requiredFiles
	policy.MissingFiles = missingFiles
	policy.EmptyFiles = emptyFiles
	policy.RepeatedScaffold = repeatedScaffold
	policy.AllRequiredFilesSeen = allRequiredFilesSeen(signals.Commands, requiredFiles)
	policy.SeenCommands = dedupePreserveOrder(signals.Commands)
	policy.PendingWrite = pendingWrite
	switch {
	case signals.ToolCalls == 0:
		policy.Stage = "explore"
		policy.RequireTool = true
	case pendingWrite:
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

func filterOutFiles(allFiles, excluded []string) []string {
	if len(allFiles) == 0 {
		return nil
	}
	if len(excluded) == 0 {
		return append([]string(nil), allFiles...)
	}
	excludedSet := make(map[string]struct{}, len(excluded))
	for _, filePath := range excluded {
		excludedSet[strings.TrimSpace(filePath)] = struct{}{}
	}
	filtered := make([]string, 0, len(allFiles))
	for _, filePath := range allFiles {
		if _, ok := excludedSet[strings.TrimSpace(filePath)]; ok {
			continue
		}
		filtered = append(filtered, filePath)
	}
	return filtered
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
	if policy.PendingWrite {
		b.WriteString("The task still requires modifying files before mode final.\n")
		if len(policy.RequiredFiles) > 0 {
			b.WriteString("Focus file targets:\n")
			for _, filePath := range policy.RequiredFiles {
				b.WriteString("- ")
				b.WriteString(filePath)
				b.WriteByte('\n')
			}
		}
		if len(policy.MissingFiles) > 0 {
			b.WriteString("These target files do not exist yet. Create or update them with a declared mutation tool instead of reading them again:\n")
			for _, filePath := range policy.MissingFiles {
				b.WriteString("- ")
				b.WriteString(filePath)
				b.WriteByte('\n')
			}
		}
		if len(policy.EmptyFiles) > 0 {
			b.WriteString("These target files already exist but are still empty. Writing real non-empty content is still required:\n")
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
		b.WriteString("Do not stop after running tests or reading files. Emit a declared mutation tool call next.\n")
	}
	if cmd := strings.TrimSpace(policy.NextCommand); cmd != "" {
		b.WriteString("Next preferred command via exec_command:\n- ")
		b.WriteString(cmd)
		b.WriteByte('\n')
		if len(policy.RequiredCommands) > 0 {
			b.WriteString("This task already specifies required commands. Do not explore with pwd/ls; emit exactly the next unmet command.\n")
		}
	}
	b.WriteString("</EXECUTION_POLICY>\n")
	return b.String()
}

func applyExecutionPolicyToParseResult(result parsedToolCallBatchResult, policy executionPolicy, toolCatalog map[string]responseToolDescriptor, constraints toolProtocolConstraints) parsedToolCallBatchResult {
	if !policy.Enabled || !policy.RequireTool || constraints.RequiredTool != "" {
		return result
	}

	if len(result.calls) > 0 {
		if policy.PendingWrite && policy.NextCommand != "" && shouldRewriteReadOnlyCallsToNext(result.calls, policy.NextCommand) {
			slog.Info("execution policy rewrite to next command",
				"stage", policy.Stage,
				"pending_write", policy.PendingWrite,
				"next_command", policy.NextCommand,
				"required_files", policy.RequiredFiles,
			)
			if synthetic, ok := buildSyntheticExecCommandCall(policy.NextCommand, toolCatalog, constraints.RequiredTool); ok {
				result.calls = []parsedToolCall{synthetic}
				result.err = nil
				result.visibleText = ""
				result.mode = toolProtocolModeAIActionsTool
				result.candidateFound = true
			}
			return result
		}
		if guardCommand := buildPendingWriteGuardCommand(policy, result.calls); guardCommand != "" {
			slog.Info("execution policy guard rewrite",
				"stage", policy.Stage,
				"pending_write", policy.PendingWrite,
				"required_files", policy.RequiredFiles,
				"empty_files", policy.EmptyFiles,
				"repeated_scaffold", policy.RepeatedScaffold,
				"replacement_command", guardCommand,
			)
			if synthetic, ok := buildSyntheticExecCommandCall(guardCommand, toolCatalog, constraints.RequiredTool); ok {
				result.calls = []parsedToolCall{synthetic}
				result.err = nil
				result.visibleText = ""
				result.mode = toolProtocolModeAIActionsTool
				result.candidateFound = true
			}
			return result
		}
		if len(policy.RequiredCommands) > 0 {
			if callsAdvanceNextRequiredCommand(result.calls, policy.NextCommand, policy.SeenCommands) {
				return result
			}
			if policy.NextCommand != "" && parsedCallsContainOnlyExecCommands(result.calls) {
				if synthetic, ok := buildSyntheticExecCommandCall(policy.NextCommand, toolCatalog, constraints.RequiredTool); ok {
					result.calls = []parsedToolCall{synthetic}
					result.err = nil
					result.visibleText = ""
					result.mode = toolProtocolModeAIActionsTool
					result.candidateFound = true
				}
				return result
			}
			// For explicit command tasks, force progression to the next unmet command
			// instead of allowing exploratory pwd/ls/read loops.
			if shouldRewriteReadOnlyCallsToNext(result.calls, policy.NextCommand) {
				if synthetic, ok := buildSyntheticExecCommandCall(policy.NextCommand, toolCatalog, constraints.RequiredTool); ok {
					result.calls = []parsedToolCall{synthetic}
					result.err = nil
					result.visibleText = ""
					result.mode = toolProtocolModeAIActionsTool
					result.candidateFound = true
				}
			}
			// For explicit command tasks, avoid repeating already-seen read commands.
			if shouldAdvanceExplicitRequiredCommand(result.calls, policy.NextCommand, policy.SeenCommands) {
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

func buildPendingWriteGuardCommand(policy executionPolicy, calls []parsedToolCall) string {
	if !policy.PendingWrite || len(policy.RequiredFiles) == 0 || len(calls) == 0 {
		return ""
	}
	for _, call := range calls {
		name, command, ok := parsedToolCallInvocation(call)
		if !ok {
			continue
		}
		if name == "exec_command" {
			if isTestCommand(command) {
				return buildExecFailureCommand("Codex adapter guard: previous test run already failed after the last write; modify target files before rerunning tests: " + strings.Join(policy.RequiredFiles, ", "))
			}
			if isReadOnlyCommand(command) && shouldBlockReadOnlyDuringPendingWrite(policy) {
				targets := strings.Join(policy.RequiredFiles, ", ")
				if len(policy.EmptyFiles) > 0 {
					targets = strings.Join(policy.EmptyFiles, ", ")
				}
				return buildExecFailureCommand("Codex adapter guard: pending write stage already inspected required context; do not run more read-only commands; write non-empty content into target files now: " + targets)
			}
			if !isMutationCommand(command) {
				continue
			}
			if len(policy.RepeatedScaffold) > 0 && isScaffoldCommandForAnyFile(command, policy.RepeatedScaffold) {
				return buildExecFailureCommand("Codex adapter guard: target file already exists but is still empty; do not run mkdir/touch again; write non-empty content into " + strings.Join(policy.RepeatedScaffold, ", "))
			}
			if touchesOnlyNonTargetFiles(command, policy.RequiredFiles) {
				return buildExecFailureCommand("Codex adapter guard: pending write stage allows mutations only for target files: " + strings.Join(policy.RequiredFiles, ", "))
			}
			continue
		}
		if isMutationToolName(name) && touchesOnlyNonTargetFiles(command, policy.RequiredFiles) {
			return buildExecFailureCommand("Codex adapter guard: pending write stage allows mutations only for target files: " + strings.Join(policy.RequiredFiles, ", "))
		}
	}
	return ""
}

func shouldBlockReadOnlyDuringPendingWrite(policy executionPolicy) bool {
	if !policy.PendingWrite {
		return false
	}
	if len(policy.EmptyFiles) > 0 || len(policy.RepeatedScaffold) > 0 {
		return true
	}
	return policy.AllRequiredFilesSeen
}

func buildExecFailureCommand(message string) string {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		trimmed = "Codex adapter guard: blocked invalid mutation command"
	}
	return "printf '%s\\n' " + shellQuoteSingle(trimmed) + " 1>&2; exit 1"
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
		if name == "exec_command" && hasSatisfiedRequiredCommand([]string{command}, next) {
			matchedNext = true
		}
	}
	return allReadOnly && !matchedNext
}

func callsAdvanceNextRequiredCommand(calls []parsedToolCall, nextCommand string, seenCommands []string) bool {
	next := strings.TrimSpace(nextCommand)
	if len(calls) == 0 || next == "" {
		return false
	}
	for _, call := range calls {
		name, command, ok := parsedToolCallInvocation(call)
		if !ok || name != "exec_command" {
			return false
		}
		if !hasSatisfiedRequiredCommand([]string{command}, next) || hasSatisfiedRequiredCommand(seenCommands, command) {
			return false
		}
	}
	return true
}

func parsedCallsContainOnlyExecCommands(calls []parsedToolCall) bool {
	if len(calls) == 0 {
		return false
	}
	for _, call := range calls {
		name, _, ok := parsedToolCallInvocation(call)
		if !ok || name != "exec_command" {
			return false
		}
	}
	return true
}

func shouldAdvanceExplicitRequiredCommand(calls []parsedToolCall, nextCommand string, seenCommands []string) bool {
	next := strings.TrimSpace(nextCommand)
	if next == "" || len(calls) == 0 || len(seenCommands) == 0 {
		return false
	}

	repeatedSeenRead := false
	for _, call := range calls {
		name, command, ok := parsedToolCallInvocation(call)
		if !ok {
			return false
		}
		if !isReadOnlyInvocation(name, command) {
			return false
		}
		if name == "exec_command" && hasSatisfiedRequiredCommand([]string{command}, next) {
			return false
		}
		if hasSatisfiedRequiredCommand(seenCommands, command) {
			repeatedSeenRead = true
		}
	}
	return repeatedSeenRead
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
		Commands:            make([]string, 0, 8),
		CommandsWithResult:  make([]string, 0, 8),
		SuccessfulCommands:  make([]string, 0, 8),
		FailedCommands:      make([]string, 0, 4),
		EmptyCommands:       make([]string, 0, 4),
		CommandOutputs:      make(map[string]string, 8),
		ReadResultPosByFile: make(map[string]int, 8),
	}
	callIDToCommand := make(map[string]string, 8)
	callIDToPos := make(map[string]int, 8)
	callPos := 0

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
			callPos++
			signals.Commands = append(signals.Commands, command)
			callID, _ := item["call_id"].(string)
			if callID != "" {
				callIDToCommand[callID] = command
				callIDToPos[callID] = callPos
			}
			if isTestCommand(command) {
				signals.TestCalls++
				continue
			}
			if isMutationCommand(command) {
				if isScaffoldCreateCommand(command) {
					continue
				}
				signals.WriteCalls++
				signals.LastWritePos = callPos
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
				continue
			}
			if normalizedName != "exec_command" {
				continue
			}
			input, _ := item["input"].(string)
			command := strings.TrimSpace(input)
			if command == "" {
				continue
			}
			callPos++
			signals.Commands = append(signals.Commands, command)
			callID, _ := item["call_id"].(string)
			if callID != "" {
				callIDToCommand[callID] = command
				callIDToPos[callID] = callPos
			}
			if isTestCommand(command) {
				signals.TestCalls++
				continue
			}
			if isMutationCommand(command) {
				if isScaffoldCreateCommand(command) {
					continue
				}
				signals.WriteCalls++
				signals.LastWritePos = callPos
				continue
			}
			if isReadOnlyCommand(command) {
				signals.ReadCalls++
			}
		case "function_call_output", "custom_tool_call_output":
			callID, _ := item["call_id"].(string)
			command := strings.TrimSpace(callIDToCommand[callID])
			if command == "" {
				continue
			}
			pos := callIDToPos[callID]
			signals.CommandsWithResult = append(signals.CommandsWithResult, command)
			text, success := extractToolOutputText(item["output"])
			if success == nil {
				if isTestCommand(command) {
					success = inferTestCommandOutputSuccess(text)
				} else {
					success = inferToolOutputSuccess(text)
				}
			}
			if isReadOnlyCommand(command) {
				if filePath := strings.TrimSpace(taskFilePathPattern.FindString(command)); filePath != "" && pos > 0 {
					signals.ReadResultPosByFile[filePath] = pos
				}
			}
			successful := success == nil || *success
			if isTestCommand(command) {
				successful = success != nil && *success
			}
			if successful {
				signals.SuccessfulCommands = append(signals.SuccessfulCommands, command)
				if strings.TrimSpace(text) != "" {
					signals.CommandOutputs[command] = text
				}
				if isReadOnlyCommand(command) && strings.TrimSpace(text) == "" {
					signals.EmptyCommands = append(signals.EmptyCommands, command)
				}
			} else {
				signals.FailedCommands = append(signals.FailedCommands, command)
				if isTestCommand(command) && pos > 0 {
					signals.LastFailedTestPos = pos
				}
			}
		}
	}

	signals.SuccessfulCommands = dedupePreserveOrder(signals.SuccessfulCommands)
	signals.CommandsWithResult = dedupePreserveOrder(signals.CommandsWithResult)
	signals.FailedCommands = dedupePreserveOrder(signals.FailedCommands)
	signals.EmptyCommands = dedupePreserveOrder(signals.EmptyCommands)
	return signals
}

func chooseNextExecutionCommand(requiredCommands, requiredFiles []string, signals executionHistorySignals, needsWrite bool) string {
	return chooseNextExecutionCommandWithStyles(requiredCommands, requiredFiles, nil, signals, needsWrite)
}

func chooseNextExecutionCommandWithStyles(requiredCommands, requiredFiles, styleCommands []string, signals executionHistorySignals, needsWrite bool) string {
	resolveCommandDone := func(command string) bool {
		if hasSatisfiedRequiredCommand(signals.SuccessfulCommands, command) {
			return true
		}
		// Backward-compatible fallback for environments that do not emit success flags.
		if len(signals.SuccessfulCommands) == 0 {
			return hasSatisfiedRequiredCommand(signals.Commands, command)
		}
		return false
	}

	if needsWrite && signals.WriteCalls == 0 {
		if command := chooseNextStyleInspectionCommand(styleCommands, requiredFiles, signals, resolveCommandDone); command != "" {
			return command
		}
		for _, filePath := range requiredFiles {
			if hasScaffoldCreateForFile(signals.Commands, filePath) {
				if hasSatisfiedReadForFile(signals.SuccessfulCommands, signals.Commands, signals.CommandsWithResult, signals.FailedCommands, filePath) {
					continue
				}
				return buildReadFileCommand(filePath)
			}
		}
		for _, filePath := range requiredFiles {
			if hasFailedReadForFile(signals.FailedCommands, filePath) {
				if hasEmptyReadForFile(signals.EmptyCommands, filePath) {
					continue
				}
				if hasScaffoldCreateForFile(signals.Commands, filePath) && hasSeenReadForFile(signals.Commands, filePath) {
					continue
				}
				return buildCreateMissingFileCommand(filePath)
			}
		}
		for _, filePath := range requiredFiles {
			if hasSatisfiedReadForFile(signals.SuccessfulCommands, signals.Commands, signals.CommandsWithResult, signals.FailedCommands, filePath) {
				continue
			}
			return buildReadFileCommand(filePath)
		}
		return ""
	}

	if needsWrite && signals.LastFailedTestPos > 0 && signals.LastFailedTestPos > signals.LastWritePos {
		if filePath := chooseRepairReadTarget(requiredFiles, signals); filePath != "" {
			return buildReadFileCommand(filePath)
		}
		return ""
	}

	for _, command := range requiredCommands {
		if resolveCommandDone(command) {
			continue
		}
		return command
	}
	if len(requiredCommands) > 0 {
		return ""
	}

	for _, filePath := range requiredFiles {
		if hasSeenReadForFile(signals.Commands, filePath) {
			continue
		}
		return buildReadFileCommand(filePath)
	}

	if needsWrite && signals.WriteCalls == 0 && len(requiredFiles) > 0 {
		return ""
	}
	return ""
}

func chooseRepairReadTarget(requiredFiles []string, signals executionHistorySignals) string {
	for _, filePath := range requiredFiles {
		if signals.ReadResultPosByFile[strings.TrimSpace(filePath)] > signals.LastFailedTestPos {
			continue
		}
		return filePath
	}
	return ""
}

func chooseNextStyleInspectionCommand(styleCommands, requiredFiles []string, signals executionHistorySignals, resolveCommandDone func(string) bool) string {
	if len(styleCommands) == 0 {
		return ""
	}
	for _, command := range styleCommands {
		if !resolveCommandDone(command) {
			return command
		}
		if candidate := chooseUnreadStyleReferenceFile(signals.CommandOutputs[command], requiredFiles, signals.Commands); candidate != "" {
			return buildReadFileCommand(candidate)
		}
	}
	return ""
}

func chooseUnreadStyleReferenceFile(output string, requiredFiles, seenCommands []string) string {
	candidates := collectStyleReferenceCandidates(output, requiredFiles)
	if len(candidates) == 0 {
		return ""
	}
	for _, candidate := range candidates {
		if hasSeenReadForFile(seenCommands, candidate) {
			return ""
		}
		return candidate
	}
	return ""
}

func buildSeedWriteCommand(task string, requiredFiles []string, signals executionHistorySignals) string {
	if len(requiredFiles) != 1 {
		return ""
	}
	targetFile := strings.TrimSpace(requiredFiles[0])
	if !strings.HasSuffix(strings.ToLower(targetFile), "_test.go") {
		return ""
	}
	testNames := extractNamedGoTests(task)
	if len(testNames) == 0 {
		return ""
	}
	packageName := inferGoPackageNameForTarget(targetFile, signals)
	if packageName == "" {
		return ""
	}
	var content strings.Builder
	content.WriteString("package ")
	content.WriteString(packageName)
	content.WriteString("\n\nimport \"testing\"\n")
	for _, testName := range testNames {
		content.WriteString(buildSeedGoTestFunction(task, testName))
	}
	pythonCode := "from pathlib import Path; Path(" + strconv.Quote(targetFile) + ").write_text(" + strconv.Quote(content.String()) + ", encoding='utf-8')"
	return "python3 -c " + shellQuoteSingle(pythonCode)
}

func buildSeedGoTestFunction(task, testName string) string {
	callExpr, expectEmptyString := extractGoEmptyStringExpectation(task)
	var b strings.Builder
	b.WriteString("\nfunc ")
	b.WriteString(testName)
	b.WriteString("(t *testing.T) {\n")
	if callExpr != "" && expectEmptyString {
		b.WriteString("\tgot := ")
		b.WriteString(callExpr)
		b.WriteString("\n\tif got != \"\" {\n")
		b.WriteString("\t\tt.Fatalf(\"got %q, want empty string\", got)\n")
		b.WriteString("\t}\n")
		b.WriteString("}\n")
		return b.String()
	}
	b.WriteString("\tt.Fatal(\"TODO: implement test\")\n")
	b.WriteString("}\n")
	return b.String()
}

var goCallPattern = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*\([^\n]*\))`)

func extractGoEmptyStringExpectation(task string) (string, bool) {
	lower := strings.ToLower(task)
	if !strings.Contains(task, "空字符串") && !strings.Contains(lower, "empty string") {
		return "", false
	}
	matches := goCallPattern.FindAllString(task, -1)
	for _, match := range matches {
		match = strings.TrimSpace(match)
		if strings.HasPrefix(match, "Test") {
			continue
		}
		if strings.HasPrefix(strings.ToLower(match), "go test ") {
			continue
		}
		return match, true
	}
	return "", false
}

func shouldPreferSeedWriteCommand(requiredFiles []string, signals executionHistorySignals, seedWriteCommand string) bool {
	if strings.TrimSpace(seedWriteCommand) == "" || len(requiredFiles) == 0 {
		return false
	}
	if !hasSuccessfulReferenceRead(requiredFiles, signals) {
		return false
	}
	for _, filePath := range requiredFiles {
		if hasFailedReadForFile(signals.FailedCommands, filePath) || hasEmptyReadForFile(signals.EmptyCommands, filePath) || hasScaffoldCreateForFile(signals.Commands, filePath) {
			return true
		}
	}
	return false
}

func hasSuccessfulReferenceRead(requiredFiles []string, signals executionHistorySignals) bool {
	requiredSet := make(map[string]struct{}, len(requiredFiles))
	for _, filePath := range requiredFiles {
		requiredSet[strings.TrimSpace(filePath)] = struct{}{}
	}
	for _, command := range signals.SuccessfulCommands {
		if !isReadOnlyCommand(command) {
			continue
		}
		filePath := strings.TrimSpace(taskFilePathPattern.FindString(command))
		if filePath == "" {
			continue
		}
		if _, isTarget := requiredSet[filePath]; isTarget {
			continue
		}
		return true
	}
	return false
}

func inferGoPackageNameForTarget(targetFile string, signals executionHistorySignals) string {
	targetDir := strings.TrimSpace(path.Dir(targetFile))
	for _, command := range signals.Commands {
		if !isReadOnlyCommand(command) {
			continue
		}
		output := strings.TrimSpace(signals.CommandOutputs[command])
		if output == "" {
			continue
		}
		filePath := strings.TrimSpace(taskFilePathPattern.FindString(command))
		if filePath == "" {
			continue
		}
		if strings.TrimSpace(path.Dir(filePath)) != targetDir {
			continue
		}
		if packageName := extractGoPackageName(output); packageName != "" {
			return packageName
		}
	}
	return normalizeGoPackageFallback(path.Base(targetDir))
}

func extractGoPackageName(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "package ") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(line, "package "))
		if name != "" {
			return normalizeGoPackageFallback(name)
		}
	}
	return ""
}

func normalizeGoPackageFallback(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			if b.Len() == 0 {
				continue
			}
			b.WriteRune(r)
		case r == '_':
			if b.Len() == 0 {
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

func collectStyleReferenceCandidates(output string, requiredFiles []string) []string {
	paths := dedupePreserveOrder(taskFilePathPattern.FindAllString(output, -1))
	if len(paths) == 0 {
		return nil
	}
	requiredSet := make(map[string]struct{}, len(requiredFiles))
	for _, filePath := range requiredFiles {
		requiredSet[strings.TrimSpace(filePath)] = struct{}{}
	}
	candidates := make([]string, 0, len(paths))
	for _, candidate := range paths {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || !strings.HasSuffix(strings.ToLower(candidate), "_test.go") {
			continue
		}
		if _, ok := requiredSet[candidate]; ok {
			continue
		}
		base := strings.ToLower(strings.TrimSpace(candidate))
		if strings.HasSuffix(base, "_benchmark_test.go") || strings.Contains(base, "style_test.go") {
			continue
		}
		candidates = append(candidates, candidate)
	}
	return candidates
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

func isScaffoldCommandForAnyFile(command string, filePaths []string) bool {
	if !isScaffoldCreateCommand(command) || len(filePaths) == 0 {
		return false
	}
	for _, filePath := range filePaths {
		if commandTouchesFile(command, filePath) {
			return true
		}
	}
	return false
}

func touchesOnlyNonTargetFiles(command string, requiredFiles []string) bool {
	if len(requiredFiles) == 0 {
		return false
	}
	paths := dedupePreserveOrder(taskFilePathPattern.FindAllString(command, -1))
	if len(paths) == 0 {
		return false
	}
	touchedRequired := false
	for _, path := range paths {
		for _, required := range requiredFiles {
			if strings.TrimSpace(path) == strings.TrimSpace(required) {
				touchedRequired = true
				break
			}
		}
	}
	return !touchedRequired
}

func commandTouchesFile(command, filePath string) bool {
	return strings.Contains(normalizeCommandForCompare(command), normalizeCommandForCompare(filePath))
}

func inferToolOutputSuccess(text string) *bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return nil
	}
	for _, marker := range []string{
		"no such file or directory",
		"cannot access",
		"not found",
		"permission denied",
		"codex adapter guard:",
		"process exited with code 1",
		"process exited with code 2",
		"process exited with code 126",
		"process exited with code 127",
		"success=false",
	} {
		if strings.Contains(lower, marker) {
			failed := false
			return &failed
		}
	}
	return nil
}

func inferTestCommandOutputSuccess(text string) *bool {
	if inferred := inferToolOutputSuccess(text); inferred != nil {
		return inferred
	}

	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}

	lines := strings.Split(trimmed, "\n")
	if len(lines) != 1 {
		return nil
	}

	lower := strings.ToLower(strings.TrimSpace(lines[0]))
	switch {
	case strings.HasPrefix(lower, "ok\t"),
		strings.HasPrefix(lower, "ok "),
		strings.HasPrefix(lower, "pass\t"),
		strings.HasPrefix(lower, "pass "),
		strings.Contains(lower, "test result: ok"),
		strings.Contains(lower, " passed in "),
		strings.HasSuffix(lower, " passed"):
		succeeded := true
		return &succeeded
	default:
		return nil
	}
}

func normalizeCommandForCompare(command string) string {
	if command == "" {
		return ""
	}
	return strings.Join(strings.Fields(strings.ToLower(command)), " ")
}

func hasSatisfiedRequiredCommand(seen []string, target string) bool {
	targetKey := normalizeCommandForCompare(target)
	if targetKey == "" {
		return false
	}
	for _, command := range seen {
		if normalizeCommandForCompare(command) == targetKey {
			return true
		}
	}
	return false
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

func hasSatisfiedReadForFile(successful, seen, withResult, failed []string, filePath string) bool {
	if hasSeenReadForFile(successful, filePath) {
		return true
	}
	if hasSeenReadForFile(failed, filePath) {
		return false
	}
	if !hasSeenReadForFile(seen, filePath) {
		return false
	}
	if len(withResult) == 0 {
		return true
	}
	return hasSeenReadForFile(withResult, filePath)
}

func hasScaffoldCreateForFile(seen []string, filePath string) bool {
	return countScaffoldCreateForFile(seen, filePath) > 0
}

func hasEmptyReadForFile(empty []string, filePath string) bool {
	return hasSeenReadForFile(empty, filePath)
}

func countScaffoldCreateForFile(seen []string, filePath string) int {
	pathKey := normalizeCommandForCompare(filePath)
	if pathKey == "" {
		return 0
	}
	count := 0
	for _, command := range seen {
		if !isScaffoldCreateCommand(command) {
			continue
		}
		commandKey := normalizeCommandForCompare(command)
		if strings.Contains(commandKey, pathKey) {
			count++
		}
	}
	return count
}

func collectMissingRequiredFiles(signals executionHistorySignals, requiredFiles []string) []string {
	if len(requiredFiles) == 0 || len(signals.FailedCommands) == 0 {
		return nil
	}
	missing := make([]string, 0, len(requiredFiles))
	for _, filePath := range requiredFiles {
		if !hasFailedReadForFile(signals.FailedCommands, filePath) {
			continue
		}
		if hasSeenReadForFile(signals.SuccessfulCommands, filePath) || hasSuccessfulMutationForFile(signals.SuccessfulCommands, filePath) {
			continue
		}
		missing = append(missing, filePath)
	}
	return missing
}

func collectEmptyRequiredFiles(signals executionHistorySignals, requiredFiles []string) []string {
	if len(requiredFiles) == 0 || len(signals.EmptyCommands) == 0 {
		return nil
	}
	empty := make([]string, 0, len(requiredFiles))
	for _, filePath := range requiredFiles {
		if !hasSeenReadForFile(signals.EmptyCommands, filePath) {
			continue
		}
		if hasSuccessfulMutationForFile(signals.SuccessfulCommands, filePath) {
			continue
		}
		empty = append(empty, filePath)
	}
	return empty
}

func collectRepeatedScaffoldFiles(signals executionHistorySignals, requiredFiles []string) []string {
	if len(requiredFiles) == 0 || len(signals.Commands) == 0 {
		return nil
	}
	repeated := make([]string, 0, len(requiredFiles))
	for _, filePath := range requiredFiles {
		if countScaffoldCreateForFile(signals.Commands, filePath) < 2 {
			continue
		}
		repeated = append(repeated, filePath)
	}
	return repeated
}

func hasFailedReadForFile(failed []string, filePath string) bool {
	pathKey := normalizeCommandForCompare(filePath)
	if pathKey == "" {
		return false
	}
	for _, command := range failed {
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

func hasSuccessfulMutationForFile(successful []string, filePath string) bool {
	pathKey := normalizeCommandForCompare(filePath)
	if pathKey == "" {
		return false
	}
	for _, command := range successful {
		if !isMutationCommand(command) {
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

func allRequiredFilesSeen(seen []string, requiredFiles []string) bool {
	if len(requiredFiles) == 0 {
		return false
	}
	for _, filePath := range requiredFiles {
		if !hasSeenReadForFile(seen, filePath) {
			return false
		}
	}
	return true
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
	if isGuardFailureCommand(lower) {
		return false
	}
	if strings.Contains(lower, "write_text(") {
		return true
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

func isGuardFailureCommand(command string) bool {
	lower := strings.ToLower(strings.TrimSpace(command))
	if lower == "" {
		return false
	}
	return strings.Contains(lower, "codex adapter guard:") && strings.Contains(lower, "exit 1")
}

func isScaffoldCreateCommand(command string) bool {
	lower := strings.ToLower(strings.TrimSpace(command))
	if lower == "" {
		return false
	}
	return strings.Contains(lower, "touch ")
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
					return sanitizeExecCommandText(command)
				}
			case string:
				return sanitizeExecCommandText(value)
			}
		}
		if rawMap, ok := decoded.(map[string]any); ok {
			if command, ok := firstStringField(rawMap, "cmd", "command", "input"); ok {
				return sanitizeExecCommandText(command)
			}
		}
	}

	if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
		return ""
	}
	return sanitizeExecCommandText(trimmed)
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

func parsedCallsContainMutationTool(calls []parsedToolCall) bool {
	for _, call := range calls {
		name, _, ok := parsedToolCallInvocation(call)
		if !ok {
			continue
		}
		if isMutationToolName(name) {
			return true
		}
	}
	return false
}

func parsedCallsContainPreferredTool(calls []parsedToolCall, preferred []string) bool {
	if len(calls) == 0 || len(preferred) == 0 {
		return false
	}
	preferredSet := make(map[string]struct{}, len(preferred))
	for _, name := range preferred {
		normalized := normalizeToolName(name)
		if normalized == "" {
			continue
		}
		preferredSet[normalized] = struct{}{}
	}
	if len(preferredSet) == 0 {
		return false
	}
	for _, call := range calls {
		name, _, ok := parsedToolCallInvocation(call)
		if !ok {
			continue
		}
		if _, ok := preferredSet[name]; ok {
			return true
		}
	}
	return false
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
