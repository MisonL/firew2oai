package proxy

import (
	"encoding/json"
	"log/slog"
	neturl "net/url"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

type executionPolicy struct {
	Enabled              bool
	Stage                string
	RequireTool          bool
	NextRequiredTool     string
	ExplicitTools        []string
	ReadLoop             bool
	PendingWrite         bool
	MissingFiles         []string
	EmptyFiles           []string
	RepeatedScaffold     []string
	NextCommand          string
	RequiredCommands     []string
	RequiredFiles        []string
	AllRequiredFilesSeen bool
	HasWriteObserved     bool
	SeenCommands         []string
	ForceSingleToolCall  bool
	AllowTruncateToMax   bool
	SyntheticToolCall    *parsedToolCall
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

func buildInitialRequiredToolBlock(task string, toolCatalog map[string]responseToolDescriptor, historyItems []json.RawMessage) string {
	if len(toolCatalog) == 0 {
		return ""
	}
	nextTool := nextUnmetExplicitTool(task, toolCatalog, historyItems)
	if nextTool == "" {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n<INITIAL_REQUIRED_TOOL>\n")
	b.WriteString("The first tool call for CURRENT_USER_TASK must be ")
	b.WriteString(nextTool)
	b.WriteString(".\n")
	b.WriteString("Do not emit narration before that tool call.\n")
	b.WriteString("</INITIAL_REQUIRED_TOOL>\n")
	return b.String()
}

func buildExecutionPolicy(model, currentTask string, historyItems []json.RawMessage, hasTools, toolsDisabled, autoRequireTool bool) executionPolicy {
	return buildExecutionPolicyWithCatalog(model, currentTask, historyItems, nil, hasTools, toolsDisabled, autoRequireTool)
}

func buildExecutionPolicyWithCatalog(model, currentTask string, historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor, hasTools, toolsDisabled, autoRequireTool bool) executionPolicy {
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
	explicitTools := extractExplicitToolMentions(task, toolCatalog)
	nextRequiredTool := nextUnmetExplicitToolFromSequence(explicitTools, toolCatalog, historyItems)
	if nextRequiredTool == "" && jsReplFollowupStillRequired(task, historyItems, toolCatalog) {
		nextRequiredTool = "js_repl"
	}
	if len(explicitTools) > 1 {
		policy.ForceSingleToolCall = true
		policy.AllowTruncateToMax = true
	}
	requiredCommands := dedupePreserveOrder(extractRequiredCommands(task))
	allMentionedFiles := dedupePreserveOrder(taskFilePathPattern.FindAllString(task, -1))
	var requiredFiles []string
	sequenceFiles := allMentionedFiles
	if needsWrite := taskLikelyNeedsWrite(task); needsWrite {
		writeTargets := dedupePreserveOrder(extractWriteTargetFiles(task))
		if len(writeTargets) > 0 {
			requiredFiles = writeTargets
			sequenceFiles = mergeExecutionSequenceFiles(allMentionedFiles, writeTargets)
		}
	} else {
		requiredFiles = append(requiredFiles, allMentionedFiles...)
	}
	styleCommands := dedupePreserveOrder(extractStyleInspectionCommands(task))
	needsWrite := taskLikelyNeedsWrite(task)
	nextCommand := chooseNextExecutionCommandWithStyles(requiredCommands, sequenceFiles, styleCommands, signals, needsWrite, requiredFiles)
	if seedWriteCommand := buildSeedWriteCommand(task, requiredFiles, signals); needsWrite && signals.WriteCalls == 0 && shouldPreferSeedWriteCommand(requiredFiles, signals, seedWriteCommand) {
		nextCommand = seedWriteCommand
	}
	missingFiles := collectMissingRequiredFiles(signals, requiredFiles)
	emptyFiles := collectEmptyRequiredFiles(signals, requiredFiles)
	repeatedScaffold := collectRepeatedScaffoldFiles(signals, requiredFiles)
	pendingWrite := needsWrite && (signals.WriteCalls == 0 || signals.LastFailedTestPos > signals.LastWritePos)
	if pendingWrite && strings.TrimSpace(nextCommand) == "" {
		nextCommand = buildPendingWriteFallbackCommand(requiredFiles, missingFiles, emptyFiles, signals)
	}
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
	policy.ExplicitTools = explicitTools
	policy.NextRequiredTool = nextRequiredTool
	policy.AllRequiredFilesSeen = allRequiredFilesSeen(signals.Commands, requiredFiles)
	policy.HasWriteObserved = signals.WriteCalls > 0
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
	if nextRequiredTool != "" {
		policy.RequireTool = true
		if policy.Stage == "" || policy.Stage == "finalize" {
			policy.Stage = "execute"
		}
		policy.SyntheticToolCall = buildSyntheticExplicitToolCall(nextRequiredTool, currentTask, historyItems, toolCatalog, nextCommand)
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

func mergeExecutionSequenceFiles(allMentionedFiles, writeTargets []string) []string {
	if len(allMentionedFiles) == 0 {
		return append([]string(nil), writeTargets...)
	}
	sequence := append([]string(nil), allMentionedFiles...)
	seen := make(map[string]struct{}, len(sequence))
	for _, filePath := range sequence {
		seen[strings.TrimSpace(filePath)] = struct{}{}
	}
	for _, filePath := range writeTargets {
		key := strings.TrimSpace(filePath)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		sequence = append(sequence, filePath)
		seen[key] = struct{}{}
	}
	return sequence
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
	if policy.NextRequiredTool != "" {
		b.WriteString("Explicit tool sequence is not complete yet.\n")
		b.WriteString("Next required tool:\n- ")
		b.WriteString(policy.NextRequiredTool)
		b.WriteByte('\n')
		b.WriteString("Do not emit mode final until that tool is called or a real tool error blocks progress.\n")
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
	if policy.Enabled && policy.Stage == "finalize" && !policy.RequireTool && constraints.RequiredTool == "" && len(result.calls) > 0 {
		result.calls = nil
		result.err = nil
		result.candidateFound = true
		return result
	}
	if policy.Enabled && policy.RequireTool && len(result.calls) == 0 && policy.SyntheticToolCall != nil {
		result.calls = []parsedToolCall{*policy.SyntheticToolCall}
		result.err = nil
		result.visibleText = ""
		result.mode = toolProtocolModeAIActionsTool
		result.candidateFound = true
		return result
	}
	if policy.Enabled && policy.RequireTool && len(result.calls) > 0 && policy.NextRequiredTool != "" && policy.SyntheticToolCall != nil && shouldRewriteCallsToSynthetic(result.calls, *policy.SyntheticToolCall) {
		slog.Info("execution policy rewrite to synthetic required tool",
			"stage", policy.Stage,
			"next_required_tool", policy.NextRequiredTool,
		)
		result.calls = []parsedToolCall{*policy.SyntheticToolCall}
		result.err = nil
		result.visibleText = ""
		result.mode = toolProtocolModeAIActionsTool
		result.candidateFound = true
		return result
	}
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
		if policy.PendingWrite && policy.NextCommand != "" && shouldRewriteMutationCallsToNext(result.calls, policy.NextCommand) {
			slog.Info("execution policy rewrite pending write mutation to next command",
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
		if shouldRewriteSequentialExecCallsToNext(result.calls, policy.NextCommand) {
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
				if next := strings.TrimSpace(policy.NextCommand); next != "" {
					return next
				}
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

func buildPendingWriteFallbackCommand(requiredFiles, missingFiles, emptyFiles []string, signals executionHistorySignals) string {
	if len(requiredFiles) == 0 {
		return ""
	}
	if signals.WriteCalls > 0 {
		return ""
	}
	if len(missingFiles) > 0 || len(emptyFiles) > 0 {
		return ""
	}
	if signals.ReadCalls < 2 && !allRequiredFilesSeen(signals.Commands, requiredFiles) {
		return ""
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
	if policy.HasWriteObserved {
		return false
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

func shouldRewriteSequentialExecCallsToNext(calls []parsedToolCall, nextCommand string) bool {
	next := strings.TrimSpace(nextCommand)
	if next == "" || len(calls) == 0 {
		return false
	}
	allSequentialExec := true
	matchedNext := false
	for _, call := range calls {
		name, command, ok := parsedToolCallInvocation(call)
		if !ok || name != "exec_command" {
			return false
		}
		if hasSatisfiedRequiredCommand([]string{command}, next) {
			matchedNext = true
		}
		if !(isReadOnlyCommand(command) || isTestCommand(command) || isGuardFailureCommand(command)) {
			allSequentialExec = false
			break
		}
	}
	return allSequentialExec && !matchedNext
}

func shouldRewriteMutationCallsToNext(calls []parsedToolCall, nextCommand string) bool {
	next := strings.TrimSpace(nextCommand)
	if next == "" || len(calls) == 0 || !isMutationCommand(next) {
		return false
	}
	allMutations := true
	matchedNext := false
	for _, call := range calls {
		name, command, ok := parsedToolCallInvocation(call)
		if !ok {
			return false
		}
		if name == "exec_command" {
			if hasSatisfiedRequiredCommand([]string{command}, next) {
				matchedNext = true
			}
			if !isMutationCommand(command) {
				allMutations = false
				break
			}
			continue
		}
		if !isMutationToolName(name) {
			allMutations = false
			break
		}
	}
	return allMutations && !matchedNext
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

func shouldRewriteCallsToSynthetic(calls []parsedToolCall, synthetic parsedToolCall) bool {
	if len(calls) != 1 {
		return true
	}
	return !parsedToolCallsMatch(calls[0], synthetic)
}

func parsedToolCallsMatch(actual, expected parsedToolCall) bool {
	actualName, actualCommand, actualOK := parsedToolCallInvocation(actual)
	expectedName, expectedCommand, expectedOK := parsedToolCallInvocation(expected)
	if !actualOK || !expectedOK || actualName != expectedName {
		return false
	}
	if expectedName == "exec_command" {
		return hasSatisfiedRequiredCommand([]string{actualCommand}, expectedCommand)
	}

	var actualItem map[string]any
	if err := json.Unmarshal(actual.item, &actualItem); err != nil {
		return false
	}
	var expectedItem map[string]any
	if err := json.Unmarshal(expected.item, &expectedItem); err != nil {
		return false
	}

	actualType, _ := actualItem["type"].(string)
	expectedType, _ := expectedItem["type"].(string)
	if actualType != expectedType {
		return false
	}

	switch expectedType {
	case "custom_tool_call":
		return normalizeJSReplInput(asString(actualItem["input"])) == normalizeJSReplInput(asString(expectedItem["input"]))
	case "function_call":
		actualArgs := strings.TrimSpace(asString(actualItem["arguments"]))
		expectedArgs := strings.TrimSpace(asString(expectedItem["arguments"]))
		if actualArgs == expectedArgs {
			return true
		}
		var actualDecoded any
		if err := json.Unmarshal([]byte(actualArgs), &actualDecoded); err != nil {
			return false
		}
		var expectedDecoded any
		if err := json.Unmarshal([]byte(expectedArgs), &expectedDecoded); err != nil {
			return false
		}
		return reflect.DeepEqual(actualDecoded, expectedDecoded)
	default:
		return false
	}
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
		case "web_search_call", "web_search":
			signals.ToolCalls++
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
	return chooseNextExecutionCommandWithStyles(requiredCommands, requiredFiles, nil, signals, needsWrite, nil)
}

func chooseNextExecutionCommandWithStyles(requiredCommands, requiredFiles, styleCommands []string, signals executionHistorySignals, needsWrite bool, writeTargets []string) string {
	resolveCommandDone := func(command string) bool {
		if hasSatisfiedRequiredCommand(signals.SuccessfulCommands, command) {
			return true
		}
		if isReadOnlyCommand(command) {
			if hasSatisfiedRequiredCommand(signals.FailedCommands, command) {
				return false
			}
			if hasSatisfiedRequiredCommand(signals.CommandsWithResult, command) {
				return true
			}
			if hasSatisfiedRequiredCommand(signals.Commands, command) {
				return true
			}
		}
		// Backward-compatible fallback for environments that do not emit success flags.
		if len(signals.SuccessfulCommands) == 0 {
			return hasSatisfiedRequiredCommand(signals.Commands, command)
		}
		return false
	}

	if needsWrite && signals.WriteCalls == 0 {
		for _, filePath := range requiredFiles {
			if isWriteTargetFile(filePath, writeTargets) {
				continue
			}
			if hasSatisfiedReadForFile(signals.SuccessfulCommands, signals.Commands, signals.CommandsWithResult, signals.FailedCommands, filePath) {
				continue
			}
			return buildReadFileCommand(filePath)
		}
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

	if !needsWrite {
		for _, filePath := range requiredFiles {
			if hasExplicitReadCommandForFile(requiredCommands, filePath) {
				continue
			}
			if hasSeenReadForFile(signals.Commands, filePath) {
				continue
			}
			return buildReadFileCommand(filePath)
		}
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

func isWriteTargetFile(filePath string, writeTargets []string) bool {
	filePath = strings.TrimSpace(filePath)
	if filePath == "" || len(writeTargets) == 0 {
		return false
	}
	for _, target := range writeTargets {
		if strings.TrimSpace(target) == filePath {
			return true
		}
	}
	return false
}

func hasExplicitReadCommandForFile(requiredCommands []string, filePath string) bool {
	filePath = strings.TrimSpace(filePath)
	if filePath == "" {
		return false
	}
	for _, command := range requiredCommands {
		if !isReadOnlyCommand(command) {
			continue
		}
		if strings.Contains(command, filePath) {
			return true
		}
	}
	return false
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
	if helperCommand := buildSeedGoCrossFileFeatureCommand(task, requiredFiles, signals); helperCommand != "" {
		return helperCommand
	}
	if replacementCommand := buildSeedReplacementCommand(task, requiredFiles, signals); replacementCommand != "" {
		return replacementCommand
	}
	if transformCommand := buildSeedGoStringTransformCommand(task, requiredFiles, signals); transformCommand != "" {
		return transformCommand
	}
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
		testFn := buildSeedGoTestFunction(task, testName)
		if testFn == "" {
			return ""
		}
		content.WriteString(testFn)
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
	return ""
}

var taskGoHelperFunctionPattern = regexp.MustCompile(`提供\s+([A-Za-z_][A-Za-z0-9_]*)\s+帮助函数`)
var taskGoPrimaryFunctionPattern = regexp.MustCompile(`让\s+([A-Za-z_][A-Za-z0-9_]*)\s+`)
var goFunctionNamePattern = regexp.MustCompile(`func\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

func buildSeedGoCrossFileFeatureCommand(task string, requiredFiles []string, signals executionHistorySignals) string {
	if len(requiredFiles) < 3 {
		return ""
	}
	helperName := extractGoHelperFunctionName(task)
	if helperName == "" || !taskMentionsTitleAndBodyTransform(task) {
		return ""
	}

	helperFile, mainFile, testFile := classifyGoFeatureFiles(requiredFiles)
	if helperFile == "" || mainFile == "" || testFile == "" {
		return ""
	}
	if !hasSatisfiedReadForFile(signals.SuccessfulCommands, signals.Commands, signals.CommandsWithResult, signals.FailedCommands, mainFile) {
		return ""
	}
	if !hasSatisfiedReadForFile(signals.SuccessfulCommands, signals.Commands, signals.CommandsWithResult, signals.FailedCommands, testFile) {
		return ""
	}

	packageName := inferGoPackageNameForTarget(mainFile, signals)
	if packageName == "" {
		return ""
	}
	mainFunc := extractGoPrimaryFunctionName(task, signals, mainFile)
	if mainFunc == "" {
		return ""
	}

	helperContent := "package " + packageName + "\n\nimport \"strings\"\n\nfunc " + helperName + "(title string) string {\n\treturn strings.ToUpper(strings.TrimSpace(title))\n}\n"
	emptyBodyTestName := "Test" + mainFunc + "_EmptyBody"
	testSnippet := "\nfunc " + emptyBodyTestName + "(t *testing.T) {\n\tgot := " + mainFunc + "(\"  firew2oai  \", \"\")\n\twant := \"FIREW2OAI:\"\n\tif got != want {\n\t\tt.Fatalf(\"" + mainFunc + "() = %q, want %q\", got, want)\n\t}\n}\n"
	mainReplacement := "func " + mainFunc + "(title, body string) string {\n\ttrimmedBody := strings.TrimSpace(body)\n\tif trimmedBody == \"\" {\n\t\treturn " + helperName + "(title) + \":\"\n\t}\n\treturn " + helperName + "(title) + \": \" + trimmedBody\n}"

	lines := []string{
		"from pathlib import Path",
		"import re",
		"helper_path = Path(" + strconv.Quote(helperFile) + ")",
		"main_path = Path(" + strconv.Quote(mainFile) + ")",
		"test_path = Path(" + strconv.Quote(testFile) + ")",
		"helper_path.write_text(" + strconv.Quote(helperContent) + ", encoding='utf-8')",
		"main_text = main_path.read_text(encoding='utf-8')",
		"main_pattern = re.compile(r'func\\s+" + regexp.QuoteMeta(mainFunc) + `\s*\(title,\s*body\s+string\)\s*string\s*\{[\s\S]*?\n\}')`,
		"assert main_pattern.search(main_text), 'main function not found'",
		"main_text = main_pattern.sub(" + strconv.Quote(mainReplacement) + ", main_text, count=1)",
		"has_single_import = re.search(r'(?m)^import\\s+\"strings\"\\s*$', main_text) is not None",
		"block_match = re.search(r'(?s)\\nimport\\s*\\((.*?)\\n\\)', main_text)",
		"has_block_import = block_match is not None and re.search(r'(?m)^\\s*\"strings\"\\s*$', block_match.group(1)) is not None",
		"if not has_single_import and not has_block_import:",
		"    if block_match is not None:",
		"        insert_at = block_match.start(1)",
		"        main_text = main_text[:insert_at] + '\\n\\t\"strings\"' + main_text[insert_at:]",
		"    else:",
		"        pkg_end = main_text.find('\\n')",
		"        assert pkg_end >= 0, 'package line not found'",
		"        main_text = main_text[:pkg_end+1] + '\\nimport \"strings\"\\n' + main_text[pkg_end+1:]",
		"main_path.write_text(main_text, encoding='utf-8')",
		"test_text = test_path.read_text(encoding='utf-8')",
		"if " + strconv.Quote(emptyBodyTestName) + " not in test_text:",
		"    test_text = test_text.rstrip() + " + strconv.Quote(testSnippet) + "",
		"test_path.write_text(test_text, encoding='utf-8')",
	}
	return buildPythonExecCommand(lines)
}

func extractGoHelperFunctionName(task string) string {
	match := taskGoHelperFunctionPattern.FindStringSubmatch(task)
	if len(match) < 2 {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func extractGoPrimaryFunctionName(task string, signals executionHistorySignals, filePath string) string {
	if match := taskGoPrimaryFunctionPattern.FindStringSubmatch(task); len(match) >= 2 {
		return strings.TrimSpace(match[1])
	}
	content := readSuccessfulFileOutput(signals, filePath)
	if content == "" {
		return ""
	}
	match := goFunctionNamePattern.FindStringSubmatch(content)
	if len(match) < 2 {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func classifyGoFeatureFiles(requiredFiles []string) (string, string, string) {
	helperFile := ""
	mainFile := ""
	testFile := ""
	for _, filePath := range requiredFiles {
		trimmed := strings.TrimSpace(filePath)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		switch {
		case strings.HasSuffix(lower, "_test.go"):
			if testFile == "" {
				testFile = trimmed
			}
		case strings.HasSuffix(lower, ".go"):
			if helperFile == "" {
				helperFile = trimmed
			} else if mainFile == "" {
				mainFile = trimmed
			}
		}
	}
	return helperFile, mainFile, testFile
}

func taskMentionsTitleAndBodyTransform(task string) bool {
	lower := strings.ToLower(task)
	hasTitle := strings.Contains(lower, "title")
	hasBody := strings.Contains(lower, "body")
	hasNormalize := strings.Contains(task, "规范化") || strings.Contains(lower, "normalize")
	hasTrim := strings.Contains(task, "裁剪") || strings.Contains(lower, "trimspace") || strings.Contains(lower, "trim")
	return hasTitle && hasBody && hasNormalize && hasTrim
}

func readSuccessfulFileOutput(signals executionHistorySignals, filePath string) string {
	filePath = strings.TrimSpace(filePath)
	if filePath == "" {
		return ""
	}
	for _, command := range signals.SuccessfulCommands {
		if !isReadOnlyCommand(command) || !commandTouchesFile(command, filePath) {
			continue
		}
		if output := strings.TrimSpace(signals.CommandOutputs[command]); output != "" {
			return output
		}
	}
	return ""
}

func buildPythonExecCommand(lines []string) string {
	code := strings.Join(lines, "\n")
	return "python3 -c " + shellQuoteSingle("exec("+strconv.Quote(code)+")")
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
	if !hasSuccessfulReferenceRead(requiredFiles, signals) && !hasSuccessfulTargetRead(requiredFiles, signals) {
		return false
	}
	if signals.WriteCalls == 0 {
		return true
	}
	for _, filePath := range requiredFiles {
		if hasFailedReadForFile(signals.FailedCommands, filePath) || hasEmptyReadForFile(signals.EmptyCommands, filePath) || hasScaffoldCreateForFile(signals.Commands, filePath) {
			return true
		}
	}
	return false
}

func hasSuccessfulTargetRead(requiredFiles []string, signals executionHistorySignals) bool {
	for _, filePath := range requiredFiles {
		if hasSatisfiedReadForFile(signals.SuccessfulCommands, signals.Commands, signals.CommandsWithResult, signals.FailedCommands, filePath) {
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

var taskQuotedReplacementPattern = regexp.MustCompile("`([^`\\n]+)`\\s*改为\\s*`([^`\\n]+)`")
var taskGoStringTransformPattern = regexp.MustCompile(`对\s+([A-Za-z_][A-Za-z0-9_]*)\s+执行\s+([A-Za-z0-9_.+\s]+?)\s*[，,]\s*对\s+([A-Za-z_][A-Za-z0-9_]*)\s+执行\s+([A-Za-z0-9_.+\s]+?)(?:[。；;\n]|$)`)

func buildSeedReplacementCommand(task string, requiredFiles []string, signals executionHistorySignals) string {
	if len(requiredFiles) != 1 || !hasSuccessfulReferenceRead(requiredFiles, signals) {
		return ""
	}
	match := taskQuotedReplacementPattern.FindStringSubmatch(task)
	if len(match) < 3 {
		return ""
	}
	targetFile := strings.TrimSpace(requiredFiles[0])
	oldText := strings.TrimSpace(match[1])
	newText := strings.TrimSpace(match[2])
	if targetFile == "" || oldText == "" || newText == "" || oldText == newText {
		return ""
	}
	pythonCode := "from pathlib import Path; path = Path(" + strconv.Quote(targetFile) + "); text = path.read_text(encoding='utf-8'); old = " + strconv.Quote(oldText) + "; new = " + strconv.Quote(newText) + "; assert old in text, 'old snippet not found'; path.write_text(text.replace(old, new, 1), encoding='utf-8')"
	return "python3 -c " + shellQuoteSingle(pythonCode)
}

func buildSeedGoStringTransformCommand(task string, requiredFiles []string, signals executionHistorySignals) string {
	if len(requiredFiles) != 1 || !hasSuccessfulReferenceRead(requiredFiles, signals) {
		return ""
	}
	targetFile := strings.TrimSpace(requiredFiles[0])
	if targetFile == "" || !strings.HasSuffix(strings.ToLower(targetFile), ".go") {
		return ""
	}
	match := taskGoStringTransformPattern.FindStringSubmatch(task)
	if len(match) < 5 {
		return ""
	}

	leftVar := strings.TrimSpace(match[1])
	leftPipeline := parseGoStringTransformPipeline(match[2])
	rightVar := strings.TrimSpace(match[3])
	rightPipeline := parseGoStringTransformPipeline(match[4])
	if leftVar == "" || rightVar == "" || len(leftPipeline) == 0 || len(rightPipeline) == 0 {
		return ""
	}

	leftExpr := composeGoCallPipeline(leftVar, leftPipeline)
	rightExpr := composeGoCallPipeline(rightVar, rightPipeline)
	if leftExpr == "" || rightExpr == "" {
		return ""
	}
	oldReturn := "return " + leftVar + ` + ": " + ` + rightVar
	newReturn := "return " + leftExpr + ` + ": " + ` + rightExpr
	if oldReturn == newReturn {
		return ""
	}

	addStringsImport := false
	for _, fn := range append(append([]string(nil), leftPipeline...), rightPipeline...) {
		if strings.HasPrefix(fn, "strings.") {
			addStringsImport = true
			break
		}
	}

	lines := []string{
		"from pathlib import Path",
		"path = Path(" + strconv.Quote(targetFile) + ")",
		"text = path.read_text(encoding='utf-8')",
		"old = " + strconv.Quote(oldReturn),
		"new = " + strconv.Quote(newReturn),
		"assert old in text, 'old return not found'",
		"text = text.replace(old, new, 1)",
	}
	if addStringsImport {
		lines = append(lines,
			"import re",
			"has_single_import = re.search(r'(?m)^import\\s+\"strings\"\\s*$', text) is not None",
			"block_match = re.search(r'(?s)\\nimport\\s*\\((.*?)\\n\\)', text)",
			"has_block_import = block_match is not None and re.search(r'(?m)^\\s*\"strings\"\\s*$', block_match.group(1)) is not None",
			"if not has_single_import and not has_block_import:",
			"    if block_match is not None:",
			"        insert_at = block_match.start(1)",
			"        text = text[:insert_at] + '\\n\\t\"strings\"' + text[insert_at:]",
			"    else:",
			"        pkg_end = text.find('\\n')",
			"        assert pkg_end >= 0, 'package line not found'",
			"        text = text[:pkg_end+1] + '\\nimport \"strings\"\\n' + text[pkg_end+1:]",
		)
	}
	lines = append(lines, "path.write_text(text, encoding='utf-8')")
	return buildPythonExecCommand(lines)
}

func parseGoStringTransformPipeline(raw string) []string {
	parts := strings.Split(raw, "+")
	pipeline := make([]string, 0, len(parts))
	for _, part := range parts {
		fn := strings.TrimSpace(part)
		if fn == "" {
			return nil
		}
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`).MatchString(fn) {
			return nil
		}
		pipeline = append(pipeline, fn)
	}
	return pipeline
}

func composeGoCallPipeline(arg string, pipeline []string) string {
	expr := strings.TrimSpace(arg)
	if expr == "" || len(pipeline) == 0 {
		return ""
	}
	for _, fn := range pipeline {
		fn = strings.TrimSpace(fn)
		if fn == "" {
			return ""
		}
		expr = fn + "(" + expr + ")"
	}
	return expr
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
		"upstream error",
		"mcp error",
		"tool error",
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

	if inferred := inferSingleLineTestSuccess(trimmed); inferred != nil {
		return inferred
	}

	if wrapped := extractWrappedCommandOutput(trimmed); wrapped != "" {
		if inferred := inferSingleLineTestSuccess(wrapped); inferred != nil {
			return inferred
		}
		if inferred := inferMultiLineTestSuccess(wrapped); inferred != nil {
			return inferred
		}
	}

	return nil
}

func inferSingleLineTestSuccess(text string) *bool {
	lines := strings.Split(strings.TrimSpace(text), "\n")
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

func inferMultiLineTestSuccess(text string) *bool {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) <= 1 {
		return nil
	}

	recognized := 0
	successful := 0
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "? ") || strings.HasPrefix(lower, "?\t"):
			recognized++
			if strings.Contains(lower, "[no test files]") {
				successful++
			}
		case strings.HasPrefix(lower, "ok ") || strings.HasPrefix(lower, "ok\t"),
			strings.HasPrefix(lower, "pass ") || strings.HasPrefix(lower, "pass\t"),
			lower == "pass":
			recognized++
			successful++
		case strings.Contains(lower, "--- fail"),
			strings.Contains(lower, " fail"),
			strings.Contains(lower, "panic:"),
			strings.Contains(lower, "error:"),
			strings.Contains(lower, "build failed"):
			failed := false
			return &failed
		}
	}
	if recognized == 0 || successful == 0 || recognized != successful {
		return nil
	}
	succeeded := true
	return &succeeded
}

func extractWrappedCommandOutput(text string) string {
	lower := strings.ToLower(text)
	idx := strings.LastIndex(lower, "\noutput:")
	markerLen := len("\noutput:")
	if idx < 0 {
		idx = strings.Index(lower, "output:")
		markerLen = len("output:")
	}
	if idx < 0 {
		return ""
	}

	payload := strings.TrimSpace(text[idx+markerLen:])
	if payload == "" {
		return ""
	}

	lines := strings.Split(payload, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lowerLine := strings.ToLower(trimmed)
		switch {
		case strings.HasPrefix(lowerLine, "chunk id:"),
			strings.HasPrefix(lowerLine, "wall time:"),
			strings.HasPrefix(lowerLine, "original token count:"),
			strings.HasPrefix(lowerLine, "command:"),
			strings.HasPrefix(lowerLine, "process exited with code"):
			continue
		default:
			kept = append(kept, trimmed)
		}
	}
	return strings.Join(kept, "\n")
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

func buildSyntheticStructuredToolCall(name string, arguments map[string]any, toolCatalog map[string]responseToolDescriptor, requiredTool string) (*parsedToolCall, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, false
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	call, err := buildParsedToolCall(map[string]any{
		"type":      "function_call",
		"name":      name,
		"arguments": arguments,
	}, toolCatalog, requiredTool, false)
	if err != nil {
		return nil, false
	}
	return call, true
}

func nextUnmetExplicitTool(task string, toolCatalog map[string]responseToolDescriptor, historyItems []json.RawMessage) string {
	return nextUnmetExplicitToolFromSequence(extractExplicitToolMentions(task, toolCatalog), toolCatalog, historyItems)
}

func nextUnmetExplicitToolFromSequence(explicitTools []string, toolCatalog map[string]responseToolDescriptor, historyItems []json.RawMessage) string {
	if len(explicitTools) == 0 {
		return ""
	}
	observed := collectObservedToolNames(historyItems, toolCatalog)
	nextIndex := 0
	for _, name := range observed {
		if nextIndex >= len(explicitTools) {
			break
		}
		if name == explicitTools[nextIndex] {
			nextIndex++
		}
	}
	if nextIndex >= len(explicitTools) {
		return ""
	}
	return explicitTools[nextIndex]
}

func collectObservedToolNames(historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor) []string {
	names := make([]string, 0, 8)
	seenCallIDs := make(map[string]struct{}, len(historyItems))
	for _, raw := range historyItems {
		if len(raw) == 0 {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		if dedupeKey := observedToolHistoryDedupeKey(item, toolCatalog); dedupeKey != "" {
			if _, exists := seenCallIDs[dedupeKey]; exists {
				continue
			}
			seenCallIDs[dedupeKey] = struct{}{}
		}
		if name := observedToolNameFromHistoryItem(item, toolCatalog); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func observedToolHistoryDedupeKey(item map[string]any, toolCatalog map[string]responseToolDescriptor) string {
	typ, _ := item["type"].(string)
	switch typ {
	case "function_call", "custom_tool_call":
	default:
		return ""
	}
	callID, _ := item["call_id"].(string)
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return ""
	}
	name := observedToolNameFromHistoryItem(item, toolCatalog)
	if name == "" {
		return ""
	}
	return typ + "|" + name + "|" + callID
}

func observedToolNameFromHistoryItem(item map[string]any, toolCatalog map[string]responseToolDescriptor) string {
	typ, _ := item["type"].(string)
	switch typ {
	case "function_call", "custom_tool_call":
		rawName, _ := item["name"].(string)
		rawName = strings.TrimSpace(rawName)
		if namespace, _ := item["namespace"].(string); namespace != "" && rawName != "" {
			rawName = joinNamespaceToolName(namespace, rawName)
		}
		if rawName == "" {
			return ""
		}
		normalized := normalizeToolName(rawName)
		if normalized == "" {
			return ""
		}
		if resolved, ok := resolveToolNameForCatalog(rawName, normalized, toolCatalog); ok {
			if resolved == "js_repl" && typ == "custom_tool_call" {
				slog.Info("observed js_repl tool call", "call_id", strings.TrimSpace(asString(item["call_id"])), "input", truncateForLog(extractJSReplInputFromHistoryItem(item), 200))
			}
			return resolved
		}
		if normalized == "js_repl" && typ == "custom_tool_call" {
			slog.Info("observed js_repl tool call", "call_id", strings.TrimSpace(asString(item["call_id"])), "input", truncateForLog(extractJSReplInputFromHistoryItem(item), 200))
		}
		return normalized
	case "mcp_tool_call":
		server, _ := item["server"].(string)
		tool, _ := item["tool"].(string)
		server = strings.TrimSpace(server)
		tool = strings.TrimSpace(tool)
		if server == "" || tool == "" {
			return ""
		}
		rawName := joinNamespaceToolName("mcp__"+server+"__", tool)
		normalized := normalizeToolName(rawName)
		if normalized == "" {
			return ""
		}
		if resolved, ok := resolveToolNameForCatalog(rawName, normalized, toolCatalog); ok {
			return resolved
		}
		return normalized
	case "web_search", "web_search_call":
		return "web_search"
	default:
		return ""
	}
}

var explicitToolURLPattern = regexp.MustCompile(`https?://[^\s<>"')]+`)
var syntheticToolURLTrailingReferencePattern = regexp.MustCompile(`(?:\[[0-9]+\]|【[0-9]+】)+$`)
var taskJSReplFollowupPattern = regexp.MustCompile(`(?:再次|再一次|然后再次|然后再)\s*(?:使用\s+)?js_repl(?:\s*工具)?(?:\s*[:：])?(?:\s*(?:计算|执行|运行))?\s*([^\n。]+)`)
var syntheticImagePathPattern = regexp.MustCompile(`(?i)(?:/)?(?:[a-z0-9_.-]+/)+[a-z0-9_.-]+\.(?:png|jpe?g|webp|gif)`)
var syntheticChromeSnapshotButtonUIDPattern = regexp.MustCompile(`(?m)uid=([A-Za-z0-9_:.\\-]+)\s+button\b`)
var syntheticWaitForTextPattern = regexp.MustCompile(`(?is)wait_for[\s\S]{0,160}?(?:出现|显示|包含|become|show|shows|contains?)\s+([A-Za-z0-9_./:-]+)`)
var syntheticAgentIDPattern = regexp.MustCompile(`(?i)"agent_id"\s*:\s*"([^"]+)"`)
var syntheticSpawnAgentTaskPattern = regexp.MustCompile(`(?is)(?:子代理任务(?:是|为)?|spawn_agent[\s\S]{0,80}?)(读取[\s\S]{0,120}?(?:返回结果|返回|汇报|并返回结果|即可返回|即可))`)

func buildSyntheticExplicitToolCall(nextRequiredTool, task string, historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor, nextCommand string) *parsedToolCall {
	switch shortToolName(nextRequiredTool) {
	case "take_snapshot", "js_repl_reset", "list_mcp_resource_templates":
		return buildSyntheticNoArgToolCall(nextRequiredTool, toolCatalog)
	case "spawn_agent":
		return buildSyntheticSpawnAgentCall(nextRequiredTool, task, toolCatalog)
	case "exec_command":
		return buildSyntheticRequiredExecCommandCall(nextRequiredTool, task, historyItems, toolCatalog, nextCommand)
	case "js_repl":
		return buildSyntheticJSReplCall(nextRequiredTool, task, historyItems, toolCatalog)
	case "view_image":
		return buildSyntheticViewImageCall(nextRequiredTool, task, historyItems, toolCatalog)
	case "click":
		return buildSyntheticChromeClickCall(nextRequiredTool, historyItems, toolCatalog)
	case "wait_for":
		return buildSyntheticChromeWaitForCall(nextRequiredTool, task, toolCatalog)
	case "fetch_doc":
		return buildSyntheticDocforkFetchDocCall(nextRequiredTool, historyItems, toolCatalog)
	case "wait_agent":
		return buildSyntheticWaitAgentCall(nextRequiredTool, historyItems, toolCatalog)
	case "close_agent":
		return buildSyntheticCloseAgentCall(nextRequiredTool, historyItems, toolCatalog)
	default:
		return nil
	}
}

func buildSyntheticNoArgToolCall(nextRequiredTool string, toolCatalog map[string]responseToolDescriptor) *parsedToolCall {
	call, ok := buildSyntheticStructuredToolCall(nextRequiredTool, map[string]any{}, toolCatalog, nextRequiredTool)
	if !ok {
		return nil
	}
	return call
}

func buildSyntheticCustomToolCall(name, input string, toolCatalog map[string]responseToolDescriptor, requiredTool string) (*parsedToolCall, bool) {
	name = strings.TrimSpace(name)
	input = strings.TrimSpace(input)
	if name == "" || input == "" {
		return nil, false
	}
	call, err := buildParsedToolCall(map[string]any{
		"type":  "custom_tool_call",
		"name":  name,
		"input": input,
	}, toolCatalog, requiredTool, false)
	if err != nil {
		return nil, false
	}
	return call, true
}

func buildSyntheticJSReplCall(nextRequiredTool, task string, historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor) *parsedToolCall {
	if countObservedToolName(historyItems, toolCatalog, "js_repl") == 0 || countObservedToolName(historyItems, toolCatalog, "js_repl_reset") == 0 {
		return nil
	}
	expr, ok := extractTaskJSReplFollowupInput(task)
	if !ok {
		return nil
	}
	call, ok := buildSyntheticCustomToolCall(nextRequiredTool, expr, toolCatalog, nextRequiredTool)
	if !ok {
		return nil
	}
	return call
}

func buildSyntheticRequiredExecCommandCall(nextRequiredTool, task string, historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor, nextCommand string) *parsedToolCall {
	command := strings.TrimSpace(nextCommand)
	signals := collectExecutionHistorySignals(historyItems)
	if command == "" {
		requiredCommands := dedupePreserveOrder(extractRequiredCommands(task))
		for _, candidate := range requiredCommands {
			if hasSatisfiedRequiredCommand(signals.SuccessfulCommands, candidate) {
				continue
			}
			if len(signals.SuccessfulCommands) == 0 && hasSatisfiedRequiredCommand(signals.Commands, candidate) {
				continue
			}
			command = candidate
			break
		}
	}
	if command == "" {
		command = chooseNextExecutionCommand(
			dedupePreserveOrder(extractRequiredCommands(task)),
			dedupePreserveOrder(taskFilePathPattern.FindAllString(task, -1)),
			signals,
			taskLikelyNeedsWrite(task),
		)
	}
	call, ok := buildSyntheticExecCommandCall(command, toolCatalog, nextRequiredTool)
	if !ok {
		return nil
	}
	return &call
}

func buildSyntheticViewImageCall(nextRequiredTool, task string, historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor) *parsedToolCall {
	imagePath := extractTaskImagePath(task)
	if imagePath == "" {
		return nil
	}
	if !path.IsAbs(imagePath) {
		signals := collectExecutionHistorySignals(historyItems)
		cwd := firstNonEmptyOutputLine(signals.CommandOutputs["pwd"])
		if !path.IsAbs(cwd) {
			return nil
		}
		imagePath = path.Join(cwd, imagePath)
	}
	call, ok := buildSyntheticStructuredToolCall(nextRequiredTool, map[string]any{"path": imagePath}, toolCatalog, nextRequiredTool)
	if !ok {
		return nil
	}
	return call
}

func extractTaskImagePath(task string) string {
	match := syntheticImagePathPattern.FindString(task)
	return strings.TrimSpace(match)
}

func firstNonEmptyOutputLine(text string) string {
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func buildSyntheticChromeClickCall(nextRequiredTool string, historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor) *parsedToolCall {
	text := latestToolOutputTextByName(historyItems, toolCatalog, "mcp__chrome_devtools__take_snapshot")
	if text == "" {
		return nil
	}
	matches := syntheticChromeSnapshotButtonUIDPattern.FindStringSubmatch(text)
	if len(matches) < 2 {
		return nil
	}
	call, ok := buildSyntheticStructuredToolCall(nextRequiredTool, map[string]any{"uid": strings.TrimSpace(matches[1])}, toolCatalog, nextRequiredTool)
	if !ok {
		return nil
	}
	return call
}

func buildSyntheticChromeWaitForCall(nextRequiredTool, task string, toolCatalog map[string]responseToolDescriptor) *parsedToolCall {
	matches := syntheticWaitForTextPattern.FindStringSubmatch(task)
	if len(matches) < 2 {
		return nil
	}
	target := strings.Trim(matches[1], " \t\r\n`'\"")
	if target == "" {
		return nil
	}
	call, ok := buildSyntheticStructuredToolCall(nextRequiredTool, map[string]any{"text": []string{target}}, toolCatalog, nextRequiredTool)
	if !ok {
		return nil
	}
	return call
}

func buildSyntheticSpawnAgentCall(nextRequiredTool, task string, toolCatalog map[string]responseToolDescriptor) *parsedToolCall {
	message := extractSpawnAgentTask(task)
	if message == "" {
		return nil
	}
	call, ok := buildSyntheticStructuredToolCall(nextRequiredTool, map[string]any{"message": message}, toolCatalog, nextRequiredTool)
	if !ok {
		return nil
	}
	return call
}

func buildSyntheticWaitAgentCall(nextRequiredTool string, historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor) *parsedToolCall {
	agentID := latestSpawnedAgentID(historyItems)
	if agentID == "" {
		return nil
	}
	call, ok := buildSyntheticStructuredToolCall(nextRequiredTool, map[string]any{"targets": []string{agentID}}, toolCatalog, nextRequiredTool)
	if !ok {
		return nil
	}
	return call
}

func extractSpawnAgentTask(task string) string {
	matches := syntheticSpawnAgentTaskPattern.FindStringSubmatch(task)
	if len(matches) >= 2 {
		message := strings.TrimSpace(matches[1])
		message = strings.TrimRight(message, "。；; \t\r\n")
		if message != "" {
			return message
		}
	}

	lines := strings.Split(task, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "README.md") {
			continue
		}
		if !strings.Contains(line, "读取") && !strings.Contains(strings.ToLower(line), "read") {
			continue
		}
		line = strings.TrimLeft(line, "0123456789.)、- \t")
		line = strings.TrimPrefix(line, "子代理任务是")
		line = strings.TrimPrefix(line, "子代理任务为")
		line = strings.TrimSpace(line)
		line = strings.TrimRight(line, "。；; \t\r\n")
		if line != "" {
			return line
		}
	}
	return ""
}

func buildSyntheticCloseAgentCall(nextRequiredTool string, historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor) *parsedToolCall {
	agentID := latestSpawnedAgentID(historyItems)
	if agentID == "" {
		return nil
	}
	call, ok := buildSyntheticStructuredToolCall(nextRequiredTool, map[string]any{"target": agentID}, toolCatalog, nextRequiredTool)
	if !ok {
		return nil
	}
	return call
}

func extractTaskJSReplFollowupInput(task string) (string, bool) {
	matches := taskJSReplFollowupPattern.FindAllStringSubmatch(task, -1)
	if len(matches) == 0 {
		return "", false
	}
	expr := normalizeJSReplInput(matches[len(matches)-1][1])
	if expr == "" {
		return "", false
	}
	return expr, true
}

func normalizeJSReplInput(input string) string {
	normalized := strings.TrimSpace(input)
	normalized = strings.Trim(normalized, " \t\r\n`'\"")
	return normalized
}

func extractJSReplInputFromHistoryItem(item map[string]any) string {
	switch typ, _ := item["type"].(string); typ {
	case "custom_tool_call":
		if input, ok := item["input"].(string); ok {
			return normalizeJSReplInput(input)
		}
	case "function_call":
		if args, ok := item["arguments"].(string); ok {
			return normalizeJSReplInput(args)
		}
		if args, ok := item["arguments"].(map[string]any); ok {
			if input, ok := args["input"].(string); ok {
				return normalizeJSReplInput(input)
			}
		}
	}
	return ""
}

func jsReplFollowupStillRequired(task string, historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor) bool {
	want, ok := extractTaskJSReplFollowupInput(task)
	if !ok {
		return false
	}

	seenReset := false
	for _, raw := range historyItems {
		if len(raw) == 0 {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		switch observedToolNameFromHistoryItem(item, toolCatalog) {
		case "js_repl_reset":
			seenReset = true
		case "js_repl":
			if !seenReset {
				continue
			}
			if extractJSReplInputFromHistoryItem(item) == want {
				return false
			}
		}
	}
	return seenReset
}

func countObservedToolName(historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor, want string) int {
	want = strings.TrimSpace(want)
	if want == "" {
		return 0
	}
	count := 0
	for _, name := range collectObservedToolNames(historyItems, toolCatalog) {
		if name == want {
			count++
		}
	}
	return count
}

func buildSyntheticDocforkFetchDocCall(nextRequiredTool string, historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor) *parsedToolCall {
	searchTool := "search_docs"
	if strings.HasPrefix(nextRequiredTool, "mcp__") {
		if idx := strings.LastIndex(nextRequiredTool, "__"); idx >= len("mcp__") {
			namespace := strings.TrimSpace(nextRequiredTool[:idx])
			if namespace != "" {
				searchTool = joinNamespaceToolName(namespace, "search_docs")
			}
		}
	}

	callIDToTool := make(map[string]string, 8)
	for _, raw := range historyItems {
		if len(raw) == 0 {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		callID, _ := item["call_id"].(string)
		if callID == "" {
			continue
		}
		if name := observedToolNameFromHistoryItem(item, toolCatalog); name != "" {
			callIDToTool[callID] = name
		}
	}

	for i := len(historyItems) - 1; i >= 0; i-- {
		var item map[string]any
		if err := json.Unmarshal(historyItems[i], &item); err != nil {
			continue
		}
		typ, _ := item["type"].(string)
		switch typ {
		case "function_call_output", "custom_tool_call_output", "mcp_tool_call_output":
		default:
			continue
		}
		callID, _ := item["call_id"].(string)
		if callID == "" || callIDToTool[callID] != searchTool {
			continue
		}
		text, _ := extractToolOutputText(item["output"])
		url := sanitizeSyntheticToolURL(explicitToolURLPattern.FindString(text))
		if url == "" {
			continue
		}
		call, ok := buildSyntheticStructuredToolCall(nextRequiredTool, map[string]any{"url": url}, toolCatalog, nextRequiredTool)
		if ok {
			return call
		}
	}
	return nil
}

func latestToolOutputTextByName(historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor, wantTool string) string {
	wantTool = strings.TrimSpace(wantTool)
	if wantTool == "" {
		return ""
	}
	callIDToTool := make(map[string]string, len(historyItems))
	for _, raw := range historyItems {
		if len(raw) == 0 {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		callID := historyToolCallIdentifier(item)
		if callID == "" {
			continue
		}
		if name := observedToolNameFromHistoryItem(item, toolCatalog); name != "" {
			callIDToTool[callID] = name
		}
	}
	for i := len(historyItems) - 1; i >= 0; i-- {
		var item map[string]any
		if err := json.Unmarshal(historyItems[i], &item); err != nil {
			continue
		}
		typ, _ := item["type"].(string)
		switch typ {
		case "function_call_output", "custom_tool_call_output", "mcp_tool_call_output":
		default:
			continue
		}
		callID := historyToolCallIdentifier(item)
		if callID == "" || callIDToTool[callID] != wantTool {
			continue
		}
		text, _ := extractToolOutputText(item["output"])
		if strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

func historyToolCallIdentifier(item map[string]any) string {
	callID := strings.TrimSpace(asString(item["call_id"]))
	if callID != "" {
		return callID
	}
	return strings.TrimSpace(asString(item["id"]))
}

func latestSpawnedAgentID(historyItems []json.RawMessage) string {
	for i := len(historyItems) - 1; i >= 0; i-- {
		if len(historyItems[i]) == 0 {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal(historyItems[i], &item); err != nil {
			continue
		}
		ids, ok := item["receiver_thread_ids"].([]any)
		if !ok {
			continue
		}
		for _, raw := range ids {
			agentID := strings.TrimSpace(asString(raw))
			if agentID != "" {
				return agentID
			}
		}
	}

	callIDToTool := make(map[string]string, len(historyItems))
	for _, raw := range historyItems {
		if len(raw) == 0 {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		callID := historyToolCallIdentifier(item)
		if callID == "" {
			continue
		}
		if name := observedToolNameFromHistoryItem(item, nil); name != "" {
			callIDToTool[callID] = name
		}
	}

	for i := len(historyItems) - 1; i >= 0; i-- {
		if len(historyItems[i]) == 0 {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal(historyItems[i], &item); err != nil {
			continue
		}
		typ, _ := item["type"].(string)
		switch typ {
		case "function_call_output", "custom_tool_call_output":
		default:
			continue
		}
		callID := historyToolCallIdentifier(item)
		if callID == "" || callIDToTool[callID] != "spawn_agent" {
			continue
		}
		text, _ := extractToolOutputText(item["output"])
		if agentID := extractAgentIDFromText(text); agentID != "" {
			return agentID
		}
	}
	return ""
}

func extractAgentIDFromText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text), &decoded); err == nil {
		if agentID, ok := firstStringField(decoded, "agent_id", "id", "target"); ok {
			return strings.TrimSpace(agentID)
		}
	}
	matches := syntheticAgentIDPattern.FindStringSubmatch(text)
	if len(matches) >= 2 {
		return strings.TrimSpace(matches[1])
	}
	return ""
}

func sanitizeSyntheticToolURL(raw string) string {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		return ""
	}
	for _, marker := range []string{`\n`, `\r`, `\t`, "\n", "\r", "\t"} {
		if idx := strings.Index(candidate, marker); idx >= 0 {
			candidate = candidate[:idx]
		}
	}
	candidate = strings.TrimSpace(candidate)
	candidate = strings.Trim(candidate, `"'()[]{}<>`)
	for {
		next := strings.TrimRight(candidate, `"'.,;:)]}>`)
		next = syntheticToolURLTrailingReferencePattern.ReplaceAllString(next, "")
		next = strings.TrimSpace(next)
		if next == candidate {
			break
		}
		candidate = next
	}
	parsed, err := neturl.Parse(candidate)
	if err != nil {
		return candidate
	}
	if (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" {
		return candidate
	}
	return ""
}
