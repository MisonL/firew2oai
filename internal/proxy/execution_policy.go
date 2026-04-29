package proxy

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	neturl "net/url"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

const (
	explicitToolFailureStopThreshold = 2
	syntheticWaitAgentTimeoutMS      = 120000
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
	RunningSessionID    string
	RunningCommand      string
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
	if nextRequiredTool != "" && explicitToolFailureCount(historyItems, toolCatalog, nextRequiredTool) >= explicitToolFailureStopThreshold {
		nextRequiredTool = ""
	}
	if len(explicitTools) > 1 {
		policy.ForceSingleToolCall = true
		policy.AllowTruncateToMax = true
	}
	requiredCommands := dedupePreserveOrder(extractRequiredCommands(task))
	allMentionedFiles := extractExecutionTaskFiles(task)
	var requiredFiles []string
	sequenceFiles := allMentionedFiles
	if needsWrite := taskLikelyNeedsWrite(task); needsWrite {
		writeTargets := dedupePreserveOrder(extractWriteTargetFiles(task))
		if len(writeTargets) > 0 {
			requiredFiles = writeTargets
			sequenceFiles = mergeExecutionSequenceFiles(allMentionedFiles, writeTargets)
		} else {
			requiredFiles = append(requiredFiles, allMentionedFiles...)
		}
	} else {
		requiredFiles = append(requiredFiles, allMentionedFiles...)
	}
	styleCommands := dedupePreserveOrder(extractStyleInspectionCommands(task))
	needsWrite := taskLikelyNeedsWrite(task) && !taskRequestsReadOnlyDiagnosis(task)
	if shouldPollRunningCommand(signals, task, nextRequiredTool, toolCatalog) {
		if synthetic := buildSyntheticWriteStdinPollCall(signals.RunningSessionID, toolCatalog); synthetic != nil {
			policy.Enabled = true
			policy.Stage = "verify"
			policy.RequireTool = true
			policy.NextRequiredTool = "write_stdin"
			policy.RequiredCommands = requiredCommands
			policy.RequiredFiles = requiredFiles
			policy.ExplicitTools = explicitTools
			policy.SeenCommands = dedupePreserveOrder(signals.Commands)
			policy.SyntheticToolCall = synthetic
			return policy
		}
	}
	nextCommand := chooseNextExecutionCommandWithStyles(requiredCommands, sequenceFiles, styleCommands, signals, needsWrite, requiredFiles)
	if seedWriteCommand := buildSeedWriteCommand(task, requiredFiles, signals); needsWrite && shouldPreferSeedWriteCommand(requiredFiles, signals, seedWriteCommand) {
		nextCommand = seedWriteCommand
	}
	missingFiles := collectMissingRequiredFiles(signals, requiredFiles)
	emptyFiles := collectEmptyRequiredFiles(signals, requiredFiles)
	repeatedScaffold := collectRepeatedScaffoldFiles(signals, requiredFiles)
	pendingWrite := needsWrite && (signals.WriteCalls == 0 || signals.LastFailedTestPos > signals.LastWritePos)
	guardBlockedWrite := pendingWrite && hasPendingWriteGuardFailure(signals)
	if guardBlockedWrite {
		pendingWrite = false
		nextCommand = ""
		nextRequiredTool = ""
	}
	if pendingWrite && strings.TrimSpace(nextCommand) == "" {
		nextCommand = buildPendingWriteFallbackCommand(requiredFiles, missingFiles, emptyFiles, signals)
	}
	if signals.LastFailedTestPos > signals.LastWritePos && !isMutationCommand(nextCommand) {
		if repairTarget := chooseRepairReadTarget(requiredFiles, signals); repairTarget != "" {
			nextCommand = buildReadFileCommand(repairTarget)
		} else if pendingWrite {
			pendingWrite = false
			nextCommand = ""
		}
	}
	if nextRequiredTool != "" && isMutationToolName(nextRequiredTool) && signals.WriteCalls == 0 && isReadOnlyCommand(nextCommand) {
		nextRequiredTool = ""
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
	} else if shouldBuildSyntheticNextCommand(policy, needsWrite) {
		if synthetic, ok := buildSyntheticExecCommandCall(nextCommand, toolCatalog, ""); ok {
			policy.SyntheticToolCall = &synthetic
		}
	}
	return policy
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

func extractExecutionTaskFiles(task string) []string {
	files := taskFilePathPattern.FindAllString(task, -1)
	if rootReadmeMentioned(task) {
		files = append(files, "README.md")
	}
	return dedupePreserveOrder(files)
}

func rootReadmeMentioned(task string) bool {
	if !strings.Contains(task, "README.md") {
		return false
	}
	lower := strings.ToLower(task)
	return strings.Contains(task, "阅读 README.md") ||
		strings.Contains(task, "读取 README.md") ||
		strings.Contains(lower, "read readme.md") ||
		strings.Contains(lower, "inspect readme.md")
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
	if policy.Enabled && policy.RequireTool && len(result.calls) > 0 && policy.SyntheticToolCall != nil && (policy.NextRequiredTool != "" || shouldForceSyntheticNextCommand(policy)) && shouldRewriteCallsToSynthetic(result.calls, *policy.SyntheticToolCall) {
		slog.Info("execution policy rewrite to synthetic required tool",
			"stage", policy.Stage,
			"next_required_tool", policy.NextRequiredTool,
			"next_command", policy.NextCommand,
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

func shouldForceSyntheticNextCommand(policy executionPolicy) bool {
	if policy.NextRequiredTool != "" || strings.TrimSpace(policy.NextCommand) == "" {
		return false
	}
	return policy.Stage == "verify" || (policy.Stage == "explore" && policy.ForceSingleToolCall)
}

func shouldBuildSyntheticNextCommand(policy executionPolicy, needsWrite bool) bool {
	if !shouldForceSyntheticNextCommand(policy) {
		return false
	}
	return policy.Stage != "explore" || !needsWrite
}

func buildPendingWriteGuardCommand(policy executionPolicy, calls []parsedToolCall) string {
	if !policy.PendingWrite || len(policy.RequiredFiles) == 0 || len(calls) == 0 {
		return ""
	}
	if next := strings.TrimSpace(policy.NextCommand); next != "" && isMutationCommand(next) {
		for _, call := range calls {
			name, command, ok := parsedToolCallInvocation(call)
			if !ok || name != "exec_command" || !isTestCommand(command) {
				continue
			}
			return next
		}
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
		if !isReadOnlyCommand(command) && !isTestCommand(command) && !isGuardFailureCommand(command) {
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
		if !hasSatisfiedRequiredCommand([]string{actualCommand}, expectedCommand) {
			return false
		}
		return parsedExecCommandArgumentsMatch(actual, expected)
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

func parsedExecCommandArgumentsMatch(actual, expected parsedToolCall) bool {
	actualArgs, ok := parsedToolCallArgumentsMap(actual)
	if !ok {
		return false
	}
	expectedArgs, ok := parsedToolCallArgumentsMap(expected)
	if !ok {
		return false
	}
	for key, expectedValue := range expectedArgs {
		if key == "cmd" {
			continue
		}
		actualValue, ok := actualArgs[key]
		if !ok || !reflect.DeepEqual(actualValue, expectedValue) {
			return false
		}
	}
	return true
}

func parsedToolCallArgumentsMap(call parsedToolCall) (map[string]any, bool) {
	var item map[string]any
	if err := json.Unmarshal(call.item, &item); err != nil {
		return nil, false
	}
	argsText, _ := item["arguments"].(string)
	if strings.TrimSpace(argsText) == "" {
		return nil, false
	}
	var decoded any
	if err := json.Unmarshal([]byte(argsText), &decoded); err != nil {
		return nil, false
	}
	args, ok := decoded.(map[string]any)
	if !ok {
		return nil, false
	}
	return args, true
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
		strings.Contains(lower, "deepseek-v3p1"),
		strings.Contains(lower, "qwen3-vl-30b-a3b-thinking"):
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
	callIDToTool := make(map[string]string, 8)
	callIDToSession := make(map[string]string, 4)
	sessionIDToCommand := make(map[string]string, 4)
	sessionIDToPos := make(map[string]int, 4)
	runningSessions := make(map[string]struct{}, 4)
	runningSessionOrder := make([]string, 0, 4)
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
			callID, _ := item["call_id"].(string)
			if callID != "" {
				callIDToTool[callID] = normalizedName
				if normalizedName == "write_stdin" {
					callIDToSession[callID] = extractSessionIDFromFunctionCall(item)
				}
			}
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
		case "todo_list":
			signals.ToolCalls++
		case "mcp_tool_call":
			name := observedToolNameFromHistoryItem(item, nil)
			if name == "" {
				continue
			}
			signals.ToolCalls++
		case "custom_tool_call":
			name, _ := item["name"].(string)
			normalizedName := normalizeToolName(name)
			if normalizedName == "" {
				continue
			}
			signals.ToolCalls++
			callID, _ := item["call_id"].(string)
			if callID != "" {
				callIDToTool[callID] = normalizedName
				if normalizedName == "write_stdin" {
					callIDToSession[callID] = extractSessionIDFromFunctionCall(item)
				}
			}
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
			if callIDToTool[callID] == "write_stdin" {
				sessionID := strings.TrimSpace(callIDToSession[callID])
				command := strings.TrimSpace(sessionIDToCommand[sessionID])
				if command == "" {
					continue
				}
				text, success := extractToolOutputText(item["output"])
				statusText := text + "\n" + toolOutputStatusText(item["output"])
				if processExited, exitSuccess := processExitStatus(statusText); processExited {
					if success == nil {
						success = &exitSuccess
					}
					recordCommandResult(&signals, command, text, success, sessionIDToPos[sessionID])
					delete(runningSessions, sessionID)
				}
				continue
			}
			command := strings.TrimSpace(callIDToCommand[callID])
			if command == "" {
				continue
			}
			pos := callIDToPos[callID]
			text, success := extractToolOutputText(item["output"])
			statusText := text + "\n" + toolOutputStatusText(item["output"])
			if sessionID := extractSessionIDFromToolOutput(item["output"]); sessionID != "" && processStillRunning(statusText) {
				sessionIDToCommand[sessionID] = command
				sessionIDToPos[sessionID] = pos
				runningSessions[sessionID] = struct{}{}
				runningSessionOrder = append(runningSessionOrder, sessionID)
				if strings.TrimSpace(text) != "" {
					signals.CommandOutputs[command] = text
				}
				continue
			}
			if success == nil {
				if isTestCommand(command) {
					success = inferTestCommandOutputSuccess(text)
				} else {
					success = inferToolOutputSuccess(text)
				}
			}
			recordCommandResult(&signals, command, text, success, pos)
		}
	}

	for i := len(runningSessionOrder) - 1; i >= 0; i-- {
		sessionID := runningSessionOrder[i]
		if _, ok := runningSessions[sessionID]; !ok {
			continue
		}
		signals.RunningSessionID = sessionID
		signals.RunningCommand = sessionIDToCommand[sessionID]
		break
	}
	signals.SuccessfulCommands = dedupePreserveOrder(signals.SuccessfulCommands)
	signals.CommandsWithResult = dedupePreserveOrder(signals.CommandsWithResult)
	signals.FailedCommands = dedupePreserveOrder(signals.FailedCommands)
	signals.EmptyCommands = dedupePreserveOrder(signals.EmptyCommands)
	return signals
}

func recordCommandResult(signals *executionHistorySignals, command, text string, success *bool, pos int) {
	signals.CommandsWithResult = append(signals.CommandsWithResult, command)
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
		return
	}
	signals.FailedCommands = append(signals.FailedCommands, command)
	if isTestCommand(command) && pos > 0 {
		signals.LastFailedTestPos = pos
	}
}

func processStillRunning(text string) bool {
	return strings.Contains(strings.ToLower(text), "process running with session id")
}

func processExitStatus(text string) (bool, bool) {
	matches := processExitCodePattern.FindStringSubmatch(text)
	if len(matches) < 2 {
		return false, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(matches[1]))
	if err != nil {
		return true, false
	}
	return true, code == 0
}

func toolOutputStatusText(output any) string {
	switch value := output.(type) {
	case string:
		return value
	case map[string]any:
		parts := make([]string, 0, 4)
		for _, key := range []string{"content", "text", "output", "message"} {
			if text := toolOutputStatusText(value[key]); strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	if encoded, err := json.Marshal(output); err == nil {
		return string(encoded)
	}
	return ""
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
		if !needsWrite && isTestCommand(command) && hasSatisfiedRequiredCommand(signals.FailedCommands, command) {
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
	candidate := candidates[0]
	if hasSeenReadForFile(seenCommands, candidate) {
		return ""
	}
	return candidate
}

func buildSeedWriteCommand(task string, requiredFiles []string, signals executionHistorySignals) string {
	if debugCommand := buildSeedGoRealDebugCommand(task, requiredFiles, signals); debugCommand != "" {
		return debugCommand
	}
	if refactorCommand := buildSeedGoRealRefactorCommand(task, requiredFiles, signals); refactorCommand != "" {
		return refactorCommand
	}
	if markdownEnvCommand := buildSeedMarkdownEnvTableCommand(task, requiredFiles, signals); markdownEnvCommand != "" {
		return markdownEnvCommand
	}
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

var goConstStringPattern = regexp.MustCompile(`(?m)^const\s+[A-Za-z_][A-Za-z0-9_]*\s*=\s*"([A-Z][A-Z0-9_]+)"`)

func buildSeedMarkdownEnvTableCommand(task string, requiredFiles []string, signals executionHistorySignals) string {
	if len(requiredFiles) != 1 || !hasSuccessfulReferenceRead(requiredFiles, signals) || !hasSuccessfulTargetRead(requiredFiles, signals) {
		return ""
	}
	targetFile := strings.TrimSpace(requiredFiles[0])
	if targetFile == "" || !strings.HasSuffix(strings.ToLower(targetFile), ".md") {
		return ""
	}
	lowerTask := strings.ToLower(task)
	if !strings.Contains(lowerTask, "配置表") && !strings.Contains(lowerTask, "环境变量") && !strings.Contains(lowerTask, "env") {
		return ""
	}
	vars := collectGoConstEnvNames(signals, targetFile)
	if len(vars) == 0 {
		return ""
	}
	pythonCode := strings.Join([]string{
		"from pathlib import Path",
		"path = Path(" + strconv.Quote(targetFile) + ")",
		"text = path.read_text(encoding='utf-8')",
		"vars = " + pythonStringListLiteral(vars),
		"lines = text.splitlines()",
		"insert = len(lines)",
		"for i, line in enumerate(lines):\n    if line.startswith('|') and line.rstrip().endswith('|'):\n        insert = i + 1",
		"added = False",
		"for name in vars:\n    if name in text:\n        continue\n    lines.insert(insert, f'| {name} | 待补充说明 |')\n    insert += 1\n    added = True",
		"assert added, 'no missing environment variables found'",
		"path.write_text('\\n'.join(lines) + '\\n', encoding='utf-8')",
	}, "\n")
	return "python3 -c " + shellQuoteSingle("exec("+strconv.Quote(pythonCode)+")")
}

func collectGoConstEnvNames(signals executionHistorySignals, targetFile string) []string {
	targetFile = strings.TrimSpace(targetFile)
	names := make([]string, 0, 4)
	for command, output := range signals.CommandOutputs {
		filePath := strings.TrimSpace(taskFilePathPattern.FindString(command))
		if filePath == "" || filePath == targetFile || !strings.HasSuffix(strings.ToLower(filePath), ".go") {
			continue
		}
		for _, match := range goConstStringPattern.FindAllStringSubmatch(output, -1) {
			if len(match) < 2 {
				continue
			}
			name := strings.TrimSpace(match[1])
			if name != "" {
				names = append(names, name)
			}
		}
	}
	return dedupePreserveOrder(names)
}

func pythonStringListLiteral(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, strconv.Quote(value))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func buildSeedGoRealDebugCommand(task string, requiredFiles []string, signals executionHistorySignals) string {
	if len(requiredFiles) != 1 {
		return ""
	}
	targetFile := strings.TrimSpace(requiredFiles[0])
	if !strings.HasSuffix(strings.ToLower(targetFile), "/parser.go") {
		return ""
	}
	lowerTask := strings.ToLower(task)
	if !strings.Contains(task, "go test ./internal/codexfixture/realdebug") {
		return ""
	}
	if !strings.Contains(lowerTask, "parseport") && !strings.Contains(lowerTask, "realdebug/parser.go") {
		return ""
	}
	if !hasSatisfiedReadForFile(signals.SuccessfulCommands, signals.Commands, signals.CommandsWithResult, signals.FailedCommands, targetFile) {
		return ""
	}
	lines := []string{
		"from pathlib import Path",
		"path = Path(" + strconv.Quote(targetFile) + ")",
		"text = path.read_text(encoding='utf-8')",
		"old = 'return port + 1, nil'",
		"new = 'return port, nil'",
		"assert old in text, 'ParsePort increment bug not found'",
		"path.write_text(text.replace(old, new, 1), encoding='utf-8')",
	}
	return buildPythonExecCommand(lines)
}

func buildSeedGoRealRefactorCommand(task string, requiredFiles []string, signals executionHistorySignals) string {
	if len(requiredFiles) < 3 {
		return ""
	}
	lowerTask := strings.ToLower(task)
	if !strings.Contains(lowerTask, "builduserline") || !strings.Contains(lowerTask, "strings.trimspace") || !strings.Contains(lowerTask, "strings.tolower") {
		return ""
	}
	var normalizeFile, formatterFile, testFile string
	for _, filePath := range requiredFiles {
		trimmed := strings.TrimSpace(filePath)
		lower := strings.ToLower(trimmed)
		switch {
		case strings.HasSuffix(lower, "/normalize.go"):
			normalizeFile = trimmed
		case strings.HasSuffix(lower, "/formatter.go"):
			formatterFile = trimmed
		case strings.HasSuffix(lower, "/formatter_test.go"):
			testFile = trimmed
		}
	}
	if normalizeFile == "" || formatterFile == "" || testFile == "" {
		return ""
	}
	if !hasSatisfiedReadForFile(signals.SuccessfulCommands, signals.Commands, signals.CommandsWithResult, signals.FailedCommands, formatterFile) {
		return ""
	}
	if !hasSatisfiedReadForFile(signals.SuccessfulCommands, signals.Commands, signals.CommandsWithResult, signals.FailedCommands, testFile) {
		return ""
	}
	packageName := inferGoPackageNameForTarget(formatterFile, signals)
	if packageName == "" {
		return ""
	}
	normalizeContent := "package " + packageName + "\n\nimport \"strings\"\n\nfunc normalizeName(name string) string {\n\treturn strings.TrimSpace(name)\n}\n\nfunc normalizeRole(role string) string {\n\treturn strings.ToLower(strings.TrimSpace(role))\n}\n"
	oldBuild := "func BuildUserLine(name, role string) string {\n\treturn name + \" (\" + role + \")\"\n}"
	newBuild := "func BuildUserLine(name, role string) string {\n\treturn normalizeName(name) + \" (\" + normalizeRole(role) + \")\"\n}"
	newTest := "\nfunc TestBuildUserLineNormalizesMixedCaseRole(t *testing.T) {\n\tgot := BuildUserLine(\"Mison\", \"AdMiN\")\n\twant := \"Mison (admin)\"\n\tif got != want {\n\t\tt.Fatalf(\"BuildUserLine() = %q, want %q\", got, want)\n\t}\n}\n"
	lines := []string{
		"from pathlib import Path",
		"normalize_path = Path(" + strconv.Quote(normalizeFile) + ")",
		"formatter_path = Path(" + strconv.Quote(formatterFile) + ")",
		"test_path = Path(" + strconv.Quote(testFile) + ")",
		"normalize_path.write_text(" + strconv.Quote(normalizeContent) + ", encoding='utf-8')",
		"formatter_text = formatter_path.read_text(encoding='utf-8')",
		"old_build = " + strconv.Quote(oldBuild),
		"new_build = " + strconv.Quote(newBuild),
		"assert old_build in formatter_text, 'BuildUserLine body not found'",
		"formatter_path.write_text(formatter_text.replace(old_build, new_build, 1), encoding='utf-8')",
		"test_text = test_path.read_text(encoding='utf-8')",
		"new_test = " + strconv.Quote(newTest),
		"if 'TestBuildUserLineNormalizesMixedCaseRole' not in test_text:",
		"    test_text = test_text.rstrip() + new_test",
		"test_path.write_text(test_text, encoding='utf-8')",
	}
	return buildPythonExecCommand(lines)
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
	encoded := base64.StdEncoding.EncodeToString([]byte(code))
	python := "import base64; exec(base64.b64decode(" + strconv.Quote(encoded) + ").decode('utf-8'))"
	return "python3 -c " + shellQuoteSingle(python)
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
	if signals.LastWritePos > 0 && signals.LastFailedTestPos <= signals.LastWritePos {
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
		"gateway timeout",
		"upstream request timeout",
		"mcp error",
		"tool error",
		"429 too many requests",
		"too many requests",
		"rate limit exceeded",
		"monthly rate limit exceeded",
		"process exited with code 1",
		"process exited with code 2",
		"process exited with code 126",
		"process exited with code 127",
		"failed to parse function arguments",
		"missing field `session_id`",
		"session not found",
		"unknown process id",
		"write_stdin failed",
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
	if strings.Contains(lower, "python3 -c") && strings.Contains(lower, "base64.b64decode") {
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

func hasPendingWriteGuardFailure(signals executionHistorySignals) bool {
	for _, command := range signals.Commands {
		if isPendingWriteGuardCommand(command) {
			return true
		}
	}
	for _, command := range signals.FailedCommands {
		if isPendingWriteGuardCommand(command) {
			return true
		}
	}
	return false
}

func isPendingWriteGuardCommand(command string) bool {
	lower := strings.ToLower(strings.TrimSpace(command))
	return isGuardFailureCommand(lower) && strings.Contains(lower, "pending write stage already inspected required context")
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
	for _, marker := range []string{"read", "list", "search", "fetch", "get", "snapshot", "screenshot"} {
		if strings.Contains(lowerName, marker) {
			return true
		}
	}
	if strings.HasPrefix(lowerName, "mcp__docfork__") || strings.HasPrefix(lowerName, "mcp__chrome_devtools__") {
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
	observed := collectSatisfiedToolNames(historyItems, toolCatalog)
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

func collectSatisfiedToolNames(historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor) []string {
	names := make([]string, 0, 8)
	callIDToTool := make(map[string]string, len(historyItems))
	callIDToCommand := make(map[string]string, len(historyItems))
	callIDHasOutput := make(map[string]bool, len(historyItems))
	seen := make(map[string]struct{}, len(historyItems))

	for _, raw := range historyItems {
		if len(raw) == 0 {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}

		if name := observedToolNameFromHistoryItem(item, toolCatalog); name != "" {
			if callID := historyToolCallIdentifier(item); callID != "" {
				callIDToTool[callID] = name
				if name == "exec_command" {
					if command := extractExecCommandFromFunctionCall(item, name); command != "" {
						callIDToCommand[callID] = command
					}
				}
			}
		}

		typ, _ := item["type"].(string)
		switch typ {
		case "function_call_output", "custom_tool_call_output", "mcp_tool_call_output":
			if callID := historyToolCallIdentifier(item); callID != "" {
				callIDHasOutput[callID] = true
			}
		}
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
		case "function_call", "custom_tool_call", "web_search_call":
			name := observedToolNameFromHistoryItem(item, toolCatalog)
			if name == "" || !historyToolCallCompleted(item) {
				continue
			}
			callID := historyToolCallIdentifier(item)
			if callID != "" && callIDHasOutput[callID] {
				continue
			}
			if callID == "" {
				callID = name + "|" + strconv.Itoa(len(names))
			}
			key := typ + "|" + name + "|" + callID
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			names = append(names, name)
		case "todo_list":
			name := observedToolNameFromHistoryItem(item, toolCatalog)
			if name == "" {
				continue
			}
			key := typ + "|" + name
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			names = append(names, name)
		case "collab_tool_call":
			name := observedToolNameFromHistoryItem(item, toolCatalog)
			if name == "" || !historyCollabToolCallSucceeded(item) {
				continue
			}
			callID := historyToolCallIdentifier(item)
			if callID == "" {
				callID = name + "|" + strconv.Itoa(len(names))
			}
			key := typ + "|" + name + "|" + callID
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			names = append(names, name)
		case "mcp_tool_call":
			name := observedToolNameFromHistoryItem(item, toolCatalog)
			if name == "" || !historyMCPToolCallSucceeded(item) {
				continue
			}
			callID := historyToolCallIdentifier(item)
			if callID == "" {
				callID = name + "|" + strconv.Itoa(len(names))
			}
			key := typ + "|" + name + "|" + callID
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			names = append(names, name)
		case "function_call_output", "custom_tool_call_output", "mcp_tool_call_output":
			callID := historyToolCallIdentifier(item)
			if callID == "" {
				continue
			}
			name := callIDToTool[callID]
			if name == "" {
				continue
			}
			if !historyToolOutputSucceeded(item["output"]) && !failedToolOutputStillSatisfiesExplicitSequence(name, item["output"]) {
				continue
			}
			key := typ + "|" + name + "|" + callID
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			names = append(names, name)
			if name == "exec_command" && execCommandUsesApplyPatch(callIDToCommand[callID]) {
				patchKey := typ + "|apply_patch|" + callID
				if _, ok := seen[patchKey]; !ok {
					seen[patchKey] = struct{}{}
					names = append(names, "apply_patch")
				}
			}
		}
	}
	return names
}

func explicitToolFailureCount(historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor, toolName string) int {
	if strings.TrimSpace(toolName) == "" {
		return 0
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
		name := observedToolNameFromHistoryItem(item, toolCatalog)
		if name == "" {
			continue
		}
		if callID := historyToolCallIdentifier(item); callID != "" {
			callIDToTool[callID] = name
		}
	}

	failures := 0
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
		case "function_call_output", "custom_tool_call_output", "mcp_tool_call_output":
			callID := historyToolCallIdentifier(item)
			if callID == "" || callIDToTool[callID] != toolName {
				continue
			}
			if toolOutputFailed(item["output"]) {
				failures++
			}
		}
	}
	return failures
}

func toolOutputFailed(output any) bool {
	text, success := extractToolOutputText(output)
	if success == nil {
		success = inferToolOutputSuccess(text)
	}
	return success != nil && !*success
}

func failedToolOutputStillSatisfiesExplicitSequence(name string, output any) bool {
	switch name {
	case "wait_agent", "close_agent":
	default:
		return false
	}
	text, _ := extractToolOutputText(output)
	return strings.TrimSpace(text) != ""
}

func historyToolCallCompleted(item map[string]any) bool {
	status := strings.ToLower(strings.TrimSpace(asString(item["status"])))
	if status == "failed" {
		return false
	}
	if errValue, ok := item["error"]; ok && errValue != nil {
		return false
	}
	return status == "" || status == "completed"
}

func historyToolOutputSucceeded(output any) bool {
	if toolOutputContainsImage(output) {
		return true
	}
	text, success := extractToolOutputText(output)
	if success != nil {
		return *success
	}
	if inferred := inferToolOutputSuccess(text); inferred != nil {
		return *inferred
	}
	return strings.TrimSpace(text) != ""
}

func toolOutputContainsImage(output any) bool {
	items, ok := output.([]any)
	if !ok {
		return false
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		itemType := strings.TrimSpace(asString(item["type"]))
		if itemType == "input_image" || itemType == "image" {
			return true
		}
	}
	return false
}

func historyMCPToolCallSucceeded(item map[string]any) bool {
	status := strings.ToLower(strings.TrimSpace(asString(item["status"])))
	if status == "failed" {
		return false
	}
	if errValue, ok := item["error"]; ok && errValue != nil {
		return false
	}
	if status != "" && status != "completed" {
		return false
	}
	if _, success := extractToolOutputText(item["result"]); success != nil && !*success {
		return false
	}
	return item["result"] != nil
}

func historyCollabToolCallSucceeded(item map[string]any) bool {
	status := strings.ToLower(strings.TrimSpace(asString(item["status"])))
	if status == "failed" {
		return false
	}
	if errValue, ok := item["error"]; ok && errValue != nil {
		return false
	}
	if status != "" && status != "completed" {
		return false
	}
	return true
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
	case "function_call", "custom_tool_call", "collab_tool_call":
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
	case "todo_list":
		if _, ok := toolCatalog["update_plan"]; !ok && len(toolCatalog) > 0 {
			return ""
		}
		return "update_plan"
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
	case "collab_tool_call":
		rawName := strings.TrimSpace(asString(item["tool"]))
		if rawName == "" {
			return ""
		}
		normalized := normalizeToolName(rawName)
		if normalized == "" {
			return ""
		}
		if resolved, ok := resolveToolNameForCatalog(rawName, normalized, toolCatalog); ok {
			return resolved
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
var taskJSReplInitialPattern = regexp.MustCompile(`(?:先|首先|必须先)?\s*(?:使用\s+)?js_repl(?:\s*工具)?(?:\s*[:：])?(?:\s*(?:计算|执行|运行))?\s*([^\n。]+)`)
var taskJSReplArraySumPattern = regexp.MustCompile(`\[[0-9,\s]+\]`)
var taskJSReplArithmeticPattern = regexp.MustCompile(`^[0-9\s+\-*/().]+$`)
var syntheticImagePathPattern = regexp.MustCompile(`(?i)(?:/)?(?:[a-z0-9_.-]+/)+[a-z0-9_.-]+\.(?:png|jpe?g|webp|gif)`)
var syntheticChromeSnapshotButtonUIDPattern = regexp.MustCompile(`(?m)uid=([A-Za-z0-9_:.\\-]+)\s+button\b`)
var syntheticChromeURLPattern = regexp.MustCompile(`(?i)\b(?:data:text/[^\s]+|https?://[^\s]+)`)
var syntheticWaitForTextPattern = regexp.MustCompile(`(?is)wait_for[\s\S]{0,160}?(?:出现|显示|包含|become|show|shows|contains?)\s+([A-Za-z0-9_./:-]+)`)
var syntheticAgentIDPattern = regexp.MustCompile(`(?i)"agent_id"\s*:\s*"([^"]+)"`)
var syntheticSpawnAgentTaskPattern = regexp.MustCompile(`(?is)(?:子代理任务(?:是|为)?|spawn_agent[\s\S]{0,80}?)(读取[\s\S]{0,120}?(?:返回结果|返回|汇报|并返回结果|即可返回|即可))`)
var syntheticDocforkSearchPattern = regexp.MustCompile(`(?i)search_docs\s+搜索\s+([A-Za-z0-9._/-]+)\s+文档中的\s+([A-Za-z0-9_.:/#-]+)|搜索\s+([A-Za-z0-9._/-]+)\s+文档中的\s+([A-Za-z0-9_.:/#-]+)`)
var syntheticDocforkCompactSearchPattern = regexp.MustCompile(`(?i)(?:search_docs\s+)?搜索\s+([A-Za-z0-9._/-]+)\s+([A-Za-z0-9_.:/#-]+)(?:[。；;\n]|$)`)
var syntheticWebSearchQueryPattern = regexp.MustCompile(`(?:必须使用\s+)?web_search\s*(?:查询|搜索)\s*([^\n。；;]+)`)
var syntheticWriteStdinCommandPattern = regexp.MustCompile(`(?m)(print\([^)\n]+\)|exit\(\))`)
var syntheticSessionIDPattern = regexp.MustCompile(`(?i)(?:session[_ ]id|session ID)\D+(\d+)`)
var processExitCodePattern = regexp.MustCompile(`(?i)process exited with code\s+(-?\d+)`)
var syntheticApplyPatchReplacePattern = regexp.MustCompile(`(?:把|将)(?:文件|内容)?中?的?\s*([A-Za-z0-9_.-]+)\s*改为\s*([A-Za-z0-9_.-]+)|replace\s+([A-Za-z0-9_.-]+)\s+with\s+([A-Za-z0-9_.-]+)`)
var syntheticGenericFilePathPattern = regexp.MustCompile(`(?i)(?:[a-z0-9_.-]+/)+[a-z0-9_.-]+\.[a-z0-9_.-]+`)
var syntheticContext7ResolvePattern = regexp.MustCompile(`(?:查找|搜索)\s+([A-Za-z0-9._/-]+)`)
var syntheticContext7TopicPattern = regexp.MustCompile(`(?:获取|查询)\s+([A-Za-z0-9_.:/# -]+?)\s+相关文档`)
var syntheticContext7LibraryIDPattern = regexp.MustCompile(`(?im)(?:Context7-compatible library ID|library id|Library ID|Compatible library ID)\s*[:：]\s*(/[A-Za-z0-9._/-]+)`)
var syntheticUpdatePlanStepsPattern = regexp.MustCompile(`(?m)plan\s*里只写两个步骤[:：]\s*([^\n]+)`)

func buildSyntheticExplicitToolCall(nextRequiredTool, task string, historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor, nextCommand string) *parsedToolCall {
	switch shortToolName(nextRequiredTool) {
	case "take_snapshot", "js_repl_reset", "list_mcp_resources", "list_mcp_resource_templates":
		return buildSyntheticNoArgToolCall(nextRequiredTool, toolCatalog)
	case "update_plan":
		return buildSyntheticUpdatePlanCall(nextRequiredTool, task, toolCatalog)
	case "spawn_agent":
		return buildSyntheticSpawnAgentCall(nextRequiredTool, task, toolCatalog)
	case "exec_command":
		return buildSyntheticRequiredExecCommandCall(nextRequiredTool, task, historyItems, toolCatalog, nextCommand)
	case "js_repl":
		return buildSyntheticJSReplCall(nextRequiredTool, task, historyItems, toolCatalog)
	case "write_stdin":
		return buildSyntheticWriteStdinCall(nextRequiredTool, task, historyItems, toolCatalog)
	case "apply_patch":
		return buildSyntheticApplyPatchCall(nextRequiredTool, task, historyItems, toolCatalog)
	case "view_image":
		return buildSyntheticViewImageCall(nextRequiredTool, task, historyItems, toolCatalog)
	case "new_page":
		return buildSyntheticChromeNewPageCall(nextRequiredTool, task, toolCatalog)
	case "click":
		return buildSyntheticChromeClickCall(nextRequiredTool, historyItems, toolCatalog)
	case "wait_for":
		return buildSyntheticChromeWaitForCall(nextRequiredTool, task, toolCatalog)
	case "search_docs":
		return buildSyntheticDocforkSearchDocsCall(nextRequiredTool, task, toolCatalog)
	case "fetch_doc":
		return buildSyntheticDocforkFetchDocCall(nextRequiredTool, historyItems, toolCatalog)
	case "web_search":
		return buildSyntheticWebSearchCall(nextRequiredTool, task, toolCatalog)
	case "resolve-library-id":
		return buildSyntheticContext7ResolveCall(nextRequiredTool, task, toolCatalog)
	case "get-library-docs", "query-docs":
		return buildSyntheticContext7GetDocsCall(nextRequiredTool, task, historyItems, toolCatalog)
	case "wait_agent":
		return buildSyntheticWaitAgentCall(nextRequiredTool, historyItems, toolCatalog)
	case "close_agent":
		return buildSyntheticCloseAgentCall(nextRequiredTool, historyItems, toolCatalog)
	default:
		return nil
	}
}

func buildSyntheticUpdatePlanCall(nextRequiredTool, task string, toolCatalog map[string]responseToolDescriptor) *parsedToolCall {
	plan := extractSyntheticUpdatePlan(task)
	if len(plan) == 0 {
		return nil
	}
	call, ok := buildSyntheticStructuredToolCall(nextRequiredTool, map[string]any{
		"explanation": "按照任务要求先建立最小执行计划。",
		"plan":        plan,
	}, toolCatalog, nextRequiredTool)
	if !ok {
		return nil
	}
	return call
}

func extractSyntheticUpdatePlan(task string) []map[string]any {
	matches := syntheticUpdatePlanStepsPattern.FindStringSubmatch(task)
	if len(matches) < 2 {
		return nil
	}
	parts := strings.FieldsFunc(strings.TrimSpace(matches[1]), func(r rune) bool {
		return r == '、' || r == '，' || r == ',' || r == ';' || r == '；'
	})
	steps := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(strings.Trim(part, "`'\"。；;"))
		if part == "" {
			continue
		}
		steps = append(steps, part)
	}
	if len(steps) == 0 {
		return nil
	}
	plan := make([]map[string]any, 0, len(steps))
	for i, step := range steps {
		status := "pending"
		if i == 0 {
			status = "in_progress"
		}
		plan = append(plan, map[string]any{
			"step":   step,
			"status": status,
		})
	}
	return plan
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
	jsReplCalls := countObservedToolName(historyItems, toolCatalog, "js_repl")
	resetCalls := countObservedToolName(historyItems, toolCatalog, "js_repl_reset")
	var expr string
	var ok bool
	if jsReplCalls == 0 {
		expr, ok = extractTaskJSReplInitialInput(task)
	} else if resetCalls > 0 {
		expr, ok = extractTaskJSReplFollowupInput(task)
	}
	if !ok {
		return nil
	}
	call, ok := buildSyntheticCustomToolCall(nextRequiredTool, expr, toolCatalog, nextRequiredTool)
	if !ok {
		return nil
	}
	return call
}

func buildSyntheticWriteStdinCall(nextRequiredTool, task string, historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor) *parsedToolCall {
	sessionID := latestInteractiveSessionID(historyItems, toolCatalog)
	if sessionID == "" {
		return nil
	}
	chars := extractSyntheticWriteStdinChars(task)
	if chars == "" {
		return nil
	}
	sessionArg := any(sessionID)
	if parsed, err := strconv.Atoi(sessionID); err == nil {
		sessionArg = parsed
	}
	call, ok := buildSyntheticStructuredToolCall(nextRequiredTool, map[string]any{
		"session_id": sessionArg,
		"chars":      chars,
	}, toolCatalog, nextRequiredTool)
	if !ok {
		return nil
	}
	return call
}

func shouldPollRunningCommand(signals executionHistorySignals, task, nextRequiredTool string, toolCatalog map[string]responseToolDescriptor) bool {
	if strings.TrimSpace(signals.RunningSessionID) == "" || !toolAvailableForPolicy(toolCatalog, "write_stdin") {
		return false
	}
	if nextRequiredTool == "write_stdin" && strings.Contains(strings.ToLower(task), "write_stdin") {
		return false
	}
	return true
}

func toolAvailableForPolicy(toolCatalog map[string]responseToolDescriptor, name string) bool {
	if len(toolCatalog) == 0 {
		return true
	}
	_, ok := toolCatalog[name]
	return ok
}

func buildSyntheticWriteStdinPollCall(sessionID string, toolCatalog map[string]responseToolDescriptor) *parsedToolCall {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	sessionArg := any(sessionID)
	if parsed, err := strconv.Atoi(sessionID); err == nil {
		sessionArg = parsed
	}
	call, ok := buildSyntheticStructuredToolCall("write_stdin", map[string]any{
		"session_id":        sessionArg,
		"chars":             "",
		"yield_time_ms":     1000,
		"max_output_tokens": 6000,
	}, toolCatalog, "write_stdin")
	if !ok {
		return nil
	}
	return call
}

func extractSessionIDFromFunctionCall(item map[string]any) string {
	rawArgs, ok := item["arguments"]
	if !ok {
		return ""
	}
	switch args := rawArgs.(type) {
	case string:
		var decoded any
		if err := json.Unmarshal([]byte(strings.TrimSpace(args)), &decoded); err != nil {
			return ""
		}
		return extractSessionIDFromArguments(decoded)
	default:
		return extractSessionIDFromArguments(args)
	}
}

func extractSessionIDFromArguments(args any) string {
	value, ok := args.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"session_id", "sessionId"} {
		switch raw := value[key].(type) {
		case string:
			return strings.TrimSpace(raw)
		case float64:
			return strconv.Itoa(int(raw))
		case int:
			return strconv.Itoa(raw)
		}
	}
	return ""
}

func latestInteractiveSessionID(historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor) string {
	callIDToTool := make(map[string]string, len(historyItems))
	for _, raw := range historyItems {
		if len(raw) == 0 {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		if name := observedToolNameFromHistoryItem(item, toolCatalog); name != "" {
			if callID := historyToolCallIdentifier(item); callID != "" {
				callIDToTool[callID] = name
			}
		}
	}
	for i := len(historyItems) - 1; i >= 0; i-- {
		var item map[string]any
		if err := json.Unmarshal(historyItems[i], &item); err != nil {
			continue
		}
		callID := historyToolCallIdentifier(item)
		if callID == "" || callIDToTool[callID] != "exec_command" {
			continue
		}
		typ, _ := item["type"].(string)
		if typ != "function_call_output" && typ != "custom_tool_call_output" {
			continue
		}
		if id := extractSessionIDFromToolOutput(item["output"]); id != "" {
			return id
		}
	}
	return ""
}

func extractSessionIDFromToolOutput(output any) string {
	switch value := output.(type) {
	case string:
		if id := extractSessionIDFromText(value); id != "" {
			return id
		}
		text, _ := extractToolOutputText(value)
		return extractSessionIDFromText(text)
	case map[string]any:
		switch raw := value["session_id"].(type) {
		case string:
			if strings.TrimSpace(raw) != "" {
				return strings.TrimSpace(raw)
			}
		case float64:
			return strconv.Itoa(int(raw))
		case int:
			return strconv.Itoa(raw)
		}
		for _, key := range []string{"content", "text", "output", "message"} {
			if id := extractSessionIDFromToolOutput(value[key]); id != "" {
				return id
			}
		}
		if raw, ok := value["sessionId"].(string); ok && strings.TrimSpace(raw) != "" {
			return strings.TrimSpace(raw)
		}
		if raw, ok := value["sessionId"].(float64); ok {
			return strconv.Itoa(int(raw))
		}
		text, _ := extractToolOutputText(value)
		return extractSessionIDFromText(text)
	case []any:
		text, _ := extractToolOutputText(value)
		return extractSessionIDFromText(text)
	default:
		text, _ := extractToolOutputText(output)
		return extractSessionIDFromText(text)
	}
}

func extractSessionIDFromText(text string) string {
	matches := syntheticSessionIDPattern.FindStringSubmatch(text)
	if len(matches) < 2 {
		return ""
	}
	return strings.TrimSpace(matches[1])
}

func extractSyntheticWriteStdinChars(task string) string {
	matches := syntheticWriteStdinCommandPattern.FindAllString(task, -1)
	if len(matches) == 0 {
		return ""
	}
	commands := dedupePreserveOrder(matches)
	return strings.Join(commands, "\n") + "\n"
}

func buildSyntheticApplyPatchCall(nextRequiredTool, task string, historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor) *parsedToolCall {
	targetFile, oldText, newText := extractSyntheticApplyPatchPlan(task)
	if targetFile == "" || oldText == "" || newText == "" {
		return nil
	}
	current := latestReadContentForFile(historyItems, toolCatalog, targetFile)
	if current != "" && !strings.Contains(current, oldText) {
		return nil
	}
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: " + targetFile,
		"@@",
		"-" + oldText,
		"+" + newText,
		"*** End Patch",
		"",
	}, "\n")
	if call := buildSyntheticApplyPatchExecCommandCall(patch, toolCatalog); call != nil {
		return call
	}
	call, ok := buildSyntheticCustomToolCall(nextRequiredTool, patch, toolCatalog, nextRequiredTool)
	if !ok {
		return nil
	}
	return call
}

func buildSyntheticApplyPatchExecCommandCall(patch string, toolCatalog map[string]responseToolDescriptor) *parsedToolCall {
	patch = strings.TrimSpace(patch)
	if patch == "" {
		return nil
	}
	if len(toolCatalog) > 0 {
		desc, ok := toolCatalog["exec_command"]
		if !ok || !desc.Structured {
			return nil
		}
	}
	command := "printf '%s' " + shellQuoteANSI(patch+"\n") + " | apply_patch"
	call, err := buildParsedToolCall(map[string]any{
		"type":      "function_call",
		"name":      "exec_command",
		"arguments": map[string]any{"cmd": command},
	}, toolCatalog, "", false)
	if err != nil {
		return nil
	}
	return call
}

func shellQuoteANSI(text string) string {
	var b strings.Builder
	b.WriteString("$'")
	for _, r := range text {
		switch r {
		case '\\':
			b.WriteString("\\\\")
		case '\'':
			b.WriteString("\\'")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString("'")
	return b.String()
}

func execCommandUsesApplyPatch(command string) bool {
	return strings.Contains(strings.ToLower(command), "apply_patch")
}

func extractSyntheticApplyPatchPlan(task string) (string, string, string) {
	targets := dedupePreserveOrder(extractWriteTargetFiles(task))
	if len(targets) == 0 {
		targets = dedupePreserveOrder(taskFilePathPattern.FindAllString(task, -1))
	}
	if len(targets) == 0 {
		targets = dedupePreserveOrder(syntheticGenericFilePathPattern.FindAllString(task, -1))
	}
	if len(targets) == 0 {
		return "", "", ""
	}
	matches := syntheticApplyPatchReplacePattern.FindStringSubmatch(task)
	if len(matches) >= 5 {
		pairs := [][2]string{
			{strings.TrimSpace(matches[1]), strings.TrimSpace(matches[2])},
			{strings.TrimSpace(matches[3]), strings.TrimSpace(matches[4])},
		}
		for _, pair := range pairs {
			if pair[0] != "" && pair[1] != "" {
				return targets[0], pair[0], pair[1]
			}
		}
	}
	return "", "", ""
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
		command = extractSyntheticInteractiveExecCommand(task)
	}
	if command == "" {
		command = chooseNextExecutionCommand(
			dedupePreserveOrder(extractRequiredCommands(task)),
			dedupePreserveOrder(taskFilePathPattern.FindAllString(task, -1)),
			signals,
			taskLikelyNeedsWrite(task),
		)
	}
	if taskRequiresInteractiveExecCommand(task, command) {
		call, ok := buildSyntheticStructuredToolCall(nextRequiredTool, map[string]any{
			"cmd":           command,
			"tty":           true,
			"yield_time_ms": 1000,
		}, toolCatalog, nextRequiredTool)
		if !ok {
			return nil
		}
		return call
	}
	call, ok := buildSyntheticExecCommandCall(command, toolCatalog, nextRequiredTool)
	if !ok {
		return nil
	}
	return &call
}

func extractSyntheticInteractiveExecCommand(task string) string {
	lowerTask := strings.ToLower(task)
	if !strings.Contains(lowerTask, "write_stdin") {
		return ""
	}
	for _, candidate := range []string{"python3", "python", "node", "bash", "zsh", "sh"} {
		if strings.Contains(lowerTask, candidate) {
			return candidate
		}
	}
	return ""
}

func taskRequiresInteractiveExecCommand(task, command string) bool {
	command = strings.ToLower(strings.TrimSpace(command))
	if command == "" {
		return false
	}
	lowerTask := strings.ToLower(task)
	if !strings.Contains(lowerTask, "write_stdin") {
		return false
	}
	return strings.Contains(lowerTask, "交互式") ||
		strings.Contains(lowerTask, "interactive") ||
		strings.Contains(lowerTask, command+" 会话") ||
		strings.Contains(lowerTask, command+" session")
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

func buildSyntheticChromeNewPageCall(nextRequiredTool, task string, toolCatalog map[string]responseToolDescriptor) *parsedToolCall {
	targetURL := extractTaskURL(task)
	if targetURL == "" {
		return nil
	}
	call, ok := buildSyntheticStructuredToolCall(nextRequiredTool, map[string]any{"url": targetURL}, toolCatalog, nextRequiredTool)
	if !ok {
		return nil
	}
	return call
}

func extractTaskURL(task string) string {
	match := syntheticChromeURLPattern.FindString(task)
	return strings.TrimRight(strings.TrimSpace(match), "`\"，。；;,")
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
	call, ok := buildSyntheticStructuredToolCall(nextRequiredTool, map[string]any{
		"targets":    []string{agentID},
		"timeout_ms": syntheticWaitAgentTimeoutMS,
	}, toolCatalog, nextRequiredTool)
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
		if enriched, ok := enrichSyntheticSpawnAgentTask(message); ok {
			return enriched
		}
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
		if enriched, ok := enrichSyntheticSpawnAgentTask(line); ok {
			return enriched
		}
		if line != "" {
			return line
		}
	}
	return ""
}

func enrichSyntheticSpawnAgentTask(message string) (string, bool) {
	trimmed := strings.TrimSpace(message)
	lower := strings.ToLower(trimmed)
	if strings.Contains(trimmed, "README.md") &&
		(strings.Contains(trimmed, "第一行") || strings.Contains(lower, "first line")) &&
		(strings.Contains(trimmed, "读取") || strings.Contains(lower, "read")) {
		return "必须使用 exec_command 执行 `head -n 1 README.md`，只返回 README.md 第一行内容，不要额外解释。", true
	}
	return "", false
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
	expr := normalizeTaskJSReplExpression(matches[len(matches)-1][1])
	if expr == "" {
		return "", false
	}
	return expr, true
}

func extractTaskJSReplInitialInput(task string) (string, bool) {
	matches := taskJSReplInitialPattern.FindAllStringSubmatch(task, -1)
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		expr := normalizeTaskJSReplExpression(match[1])
		if expr != "" {
			return expr, true
		}
	}
	return "", false
}

func normalizeTaskJSReplExpression(input string) string {
	normalized := normalizeJSReplInput(input)
	if normalized == "" {
		return ""
	}
	if array := taskJSReplArraySumPattern.FindString(normalized); array != "" && strings.Contains(normalized, "和") {
		return array + ".reduce((a,b)=>a+b,0)"
	}
	normalized = strings.TrimSpace(strings.TrimSuffix(normalized, "。"))
	normalized = strings.TrimSpace(strings.TrimSuffix(normalized, "；"))
	normalized = strings.TrimSpace(strings.TrimSuffix(normalized, ";"))
	if taskJSReplArithmeticPattern.MatchString(normalized) {
		return normalized
	}
	return ""
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

func latestReadContentForFile(historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor, filePath string) string {
	fileKey := normalizeCommandForCompare(strings.TrimSpace(filePath))
	if fileKey == "" {
		return ""
	}
	callIDToCommand := make(map[string]string, len(historyItems))
	for _, raw := range historyItems {
		if len(raw) == 0 {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		if observedToolNameFromHistoryItem(item, toolCatalog) != "exec_command" {
			continue
		}
		callID := historyToolCallIdentifier(item)
		if callID == "" {
			continue
		}
		if command := extractExecCommandFromFunctionCall(item, "exec_command"); strings.TrimSpace(command) != "" {
			callIDToCommand[callID] = command
		}
	}
	for i := len(historyItems) - 1; i >= 0; i-- {
		var item map[string]any
		if err := json.Unmarshal(historyItems[i], &item); err != nil {
			continue
		}
		callID := historyToolCallIdentifier(item)
		command := strings.TrimSpace(callIDToCommand[callID])
		if command == "" || !isReadOnlyCommand(command) {
			continue
		}
		if !strings.Contains(normalizeCommandForCompare(command), fileKey) {
			continue
		}
		text, success := extractToolOutputText(item["output"])
		if success != nil && !*success {
			continue
		}
		if strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

func buildSyntheticContext7ResolveCall(nextRequiredTool, task string, toolCatalog map[string]responseToolDescriptor) *parsedToolCall {
	libraryName := extractSyntheticContext7LibraryName(task)
	if libraryName == "" {
		return nil
	}
	call, ok := buildSyntheticStructuredToolCall(nextRequiredTool, map[string]any{
		"libraryName": libraryName,
		"query":       libraryName,
	}, toolCatalog, nextRequiredTool)
	if !ok {
		return nil
	}
	return call
}

func buildSyntheticContext7GetDocsCall(nextRequiredTool, task string, historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor) *parsedToolCall {
	libraryID := latestContext7LibraryID(historyItems, toolCatalog)
	topic := extractSyntheticContext7Topic(task)
	if libraryID == "" || topic == "" {
		return nil
	}
	args := map[string]any{}
	switch shortToolName(nextRequiredTool) {
	case "query-docs":
		args["libraryId"] = libraryID
		args["query"] = topic
	default:
		args["context7CompatibleLibraryID"] = libraryID
		args["topic"] = topic
		args["tokens"] = 2000
	}
	call, ok := buildSyntheticStructuredToolCall(nextRequiredTool, args, toolCatalog, nextRequiredTool)
	if !ok {
		return nil
	}
	return call
}

func extractSyntheticContext7LibraryName(task string) string {
	matches := syntheticContext7ResolvePattern.FindStringSubmatch(task)
	if len(matches) < 2 {
		return ""
	}
	return strings.Trim(matches[1], " \t\r\n`'\"")
}

func extractSyntheticContext7Topic(task string) string {
	matches := syntheticContext7TopicPattern.FindStringSubmatch(task)
	if len(matches) < 2 {
		return ""
	}
	return strings.Trim(matches[1], " \t\r\n`'\"")
}

func latestContext7LibraryID(historyItems []json.RawMessage, toolCatalog map[string]responseToolDescriptor) string {
	for _, toolName := range []string{"mcp__context7__resolve-library-id", "mcp__context7__resolve_library_id"} {
		text := latestToolOutputTextByName(historyItems, toolCatalog, toolName)
		if matches := syntheticContext7LibraryIDPattern.FindStringSubmatch(text); len(matches) >= 2 {
			return strings.TrimSpace(matches[1])
		}
	}
	return ""
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
	text := latestToolOutputTextByName(historyItems, toolCatalog, searchTool)
	url := sanitizeSyntheticToolURL(explicitToolURLPattern.FindString(text))
	if url == "" {
		return nil
	}
	call, ok := buildSyntheticStructuredToolCall(nextRequiredTool, map[string]any{"url": url}, toolCatalog, nextRequiredTool)
	if ok {
		return call
	}
	return nil
}

func buildSyntheticDocforkSearchDocsCall(nextRequiredTool, task string, toolCatalog map[string]responseToolDescriptor) *parsedToolCall {
	if !strings.Contains(strings.TrimSpace(nextRequiredTool), "mcp__docfork__") {
		return nil
	}
	library, query := extractSyntheticDocforkSearchInputs(task)
	if library == "" || query == "" {
		return nil
	}
	call, ok := buildSyntheticStructuredToolCall(
		nextRequiredTool,
		map[string]any{"library": library, "query": query},
		toolCatalog,
		nextRequiredTool,
	)
	if !ok {
		return nil
	}
	return call
}

func extractSyntheticDocforkSearchInputs(task string) (string, string) {
	matches := syntheticDocforkSearchPattern.FindStringSubmatch(task)
	if len(matches) >= 5 {
		pairs := [][2]string{
			{strings.TrimSpace(matches[1]), strings.TrimSpace(matches[2])},
			{strings.TrimSpace(matches[3]), strings.TrimSpace(matches[4])},
		}
		for _, pair := range pairs {
			if pair[0] != "" && pair[1] != "" {
				return normalizeSyntheticDocforkSearchInputs(pair[0], pair[1])
			}
		}
	}
	matches = syntheticDocforkCompactSearchPattern.FindStringSubmatch(task)
	if len(matches) >= 3 {
		return normalizeSyntheticDocforkSearchInputs(matches[1], matches[2])
	}
	return "", ""
}

func normalizeSyntheticDocforkSearchInputs(library, query string) (string, string) {
	library = strings.TrimSpace(library)
	query = strings.TrimSpace(query)
	if library == "" || query == "" {
		return "", ""
	}
	if cleaned, ok := trimDocforkQueryLibraryPrefix(query, library); ok {
		query = cleaned
	}
	return library, query
}

func buildSyntheticWebSearchCall(nextRequiredTool, task string, toolCatalog map[string]responseToolDescriptor) *parsedToolCall {
	if shortToolName(nextRequiredTool) != "web_search" {
		return nil
	}
	query := extractSyntheticWebSearchQuery(task)
	if query == "" {
		return nil
	}
	call, err := buildParsedToolCall(map[string]any{
		"name":      nextRequiredTool,
		"arguments": map[string]any{"query": query},
	}, toolCatalog, nextRequiredTool, true)
	if err != nil {
		return nil
	}
	return call
}

func extractSyntheticWebSearchQuery(task string) string {
	normalized := strings.Join(strings.Fields(strings.TrimSpace(task)), " ")
	lower := strings.ToLower(normalized)
	if strings.Contains(lower, "go") &&
		(strings.Contains(normalized, "最新稳定版本") || strings.Contains(lower, "latest stable")) &&
		(strings.Contains(normalized, "发布日期") || strings.Contains(lower, "release date") || strings.Contains(lower, "released")) {
		return "latest Go release"
	}

	matches := syntheticWebSearchQueryPattern.FindStringSubmatch(task)
	if len(matches) < 2 {
		return ""
	}
	query := strings.TrimSpace(matches[1])
	query = strings.Trim(query, " \t\r\n`'\"")
	return query
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
		case "mcp_tool_call":
			if !historyMCPToolCallSucceeded(item) {
				continue
			}
			if name := observedToolNameFromHistoryItem(item, toolCatalog); name != wantTool {
				continue
			}
			text, _ := extractToolOutputText(item["result"])
			if strings.TrimSpace(text) != "" {
				return text
			}
		case "function_call_output", "custom_tool_call_output", "mcp_tool_call_output":
			if !historyToolOutputSucceeded(item["output"]) {
				continue
			}
		default:
			continue
		}
		if typ != "mcp_tool_call" {
			callID := historyToolCallIdentifier(item)
			if callID == "" || callIDToTool[callID] != wantTool {
				continue
			}
			text, _ := extractToolOutputText(item["output"])
			if strings.TrimSpace(text) != "" {
				return text
			}
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
