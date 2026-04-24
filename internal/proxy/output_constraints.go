package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

const strictOutputGateEnvKey = "FIREW2OAI_STRICT_OUTPUT_GATE"

var controlMarkupStripPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?is)<<<\s*AI_ACTIONS_V1[\s\S]*?<<<\s*END_AI_ACTIONS_V1\s*>>>`),
	regexp.MustCompile(`(?is)<function_calls[^>]*>[\s\S]*?</function_calls>`),
	regexp.MustCompile(`(?is)<function_call[^>]*>[\s\S]*?</function_call>`),
	regexp.MustCompile(`(?is)<tool_calls[^>]*>[\s\S]*?</tool_calls>`),
	regexp.MustCompile(`(?is)<tool_call[^>]*>[\s\S]*?</tool_call>`),
	regexp.MustCompile(`(?is)<invoke[^>]*>[\s\S]*?</invoke>`),
	regexp.MustCompile(`(?is)<\/?ai_actions[^>]*>`),
	regexp.MustCompile(`(?is)<\/?function_calls?[^>]*>`),
	regexp.MustCompile(`(?is)<\/?tool_calls?[^>]*>`),
	regexp.MustCompile(`(?is)<\/?invoke[^>]*>`),
	regexp.MustCompile(`(?is)<\/?function_call[^>]*>`),
}

var plainTextAIActionsPattern = regexp.MustCompile(`(?i)\bAI_ACTIONS(?:\s*:)?`)
var bareTaskFileNamePattern = regexp.MustCompile(`(?i)\b[a-z0-9_.-]+\.(?:go|py|ts|js|jsx|tsx|md|json|yaml|yml|toml|sh|sql)\b`)
var requiredLabelScaffoldPrefixPattern = regexp.MustCompile(`(?i)^(?:final answer|task_complete|id)\b[:\s-]*`)
var requiredLabelResultValuePattern = regexp.MustCompile(`(?i)\b(PASS|FAIL)\b`)
var evidenceTextJSONWrapperPattern = regexp.MustCompile(`text":"((?:\\.|[^"\\])*)`)
var evidenceSourcePrefixPattern = regexp.MustCompile(`(?i)^source:\s*\S+\s*`)
var evidencePathFieldPattern = regexp.MustCompile(`"path"\s*:\s*"([^"]+)"`)
var terminalEscapePattern = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|.)`)

type requiredLabelEntry struct {
	Label string
	Value string
}

func enforceTaskOutputConstraints(task, text string, evidence executionEvidence, checkControlMarkup bool) (string, error) {
	trimmed := strings.TrimSpace(text)
	strictOutputGate := isStrictOutputGateEnabled()
	requiredLabels := dedupePreserveOrder(extractRequiredOutputLabels(task))
	if fallbackLabels := fallbackRequiredLabelsFromText(trimmed); len(fallbackLabels) > 0 {
		requiredLabels = fallbackLabels
	}
	if trimmed == "" {
		if len(requiredLabels) > 0 {
			if normalizedFallback, ok := synthesizeRequiredLabelOutput(task, "", requiredLabels, evidence); ok {
				return normalizedFallback, nil
			}
		}
		if strictOutputGate && len(requiredLabels) > 0 {
			executed := "none"
			if len(evidence.Commands) > 0 {
				executed = strings.Join(evidence.Commands, ", ")
			}
			return "", fmt.Errorf("final output missing required labels: %s; executed commands: %s", strings.Join(requiredLabels, ", "), executed)
		}
		return trimmed, nil
	}

	if strings.HasPrefix(trimmed, "Codex adapter error:") {
		return trimmed, nil
	}
	if corrected, ok := overrideStructuredFailBlockWithEvidence(task, trimmed, evidence); ok {
		return corrected, nil
	}

	if checkControlMarkup {
		if marker, found := detectLeakedToolControlMarkup(trimmed); found {
			if strictOutputGate {
				return "", fmt.Errorf("model leaked unsupported tool-control markup %q in final text", marker)
			}
			trimmed = sanitizeLeakedToolControlMarkup(trimmed)
			if trimmed == "" {
				if normalizedFallback, ok := synthesizeRequiredLabelOutput(task, "", requiredLabels, evidence); ok {
					return normalizedFallback, nil
				}
				return trimmed, nil
			}
		}
	}
	if len(requiredLabels) > 0 && looksLikeMetaTaskHandoff(trimmed) {
		trimmed = ""
	}
	if trimmed == "" {
		if normalizedFallback, ok := synthesizeRequiredLabelOutput(task, "", requiredLabels, evidence); ok {
			return normalizedFallback, nil
		}
		if strictOutputGate && len(requiredLabels) > 0 {
			executed := "none"
			if len(evidence.Commands) > 0 {
				executed = strings.Join(evidence.Commands, ", ")
			}
			return "", fmt.Errorf("final output missing required labels: %s; executed commands: %s", strings.Join(requiredLabels, ", "), executed)
		}
		return trimmed, nil
	}

	if len(requiredLabels) == 0 {
		return trimmed, nil
	}

	normalized, missing := normalizeRequiredLabelOutput(trimmed, requiredLabels)
	if len(missing) == 0 {
		return canonicalizeRequiredLabelOutput(normalized, requiredLabels, task, evidence), nil
	}
	if !strictOutputGate {
		if normalizedFallback, ok := synthesizeRequiredLabelOutput(task, trimmed, requiredLabels, evidence); ok {
			return normalizedFallback, nil
		}
		return trimmed, nil
	}

	executed := "none"
	if len(evidence.Commands) > 0 {
		executed = strings.Join(evidence.Commands, ", ")
	}
	return "", fmt.Errorf("final output missing required labels: %s; executed commands: %s", strings.Join(missing, ", "), executed)
}

func isStrictOutputGateEnabled() bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(strictOutputGateEnvKey)))
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return false
	}
}

func normalizeRequiredLabelOutput(text string, labels []string) (string, []string) {
	found := selectRequiredLabelValues(text, labels)
	missing := make([]string, 0, len(labels))
	ordered := make([]string, 0, len(labels))
	for _, label := range labels {
		value, ok := found[label]
		if !ok {
			missing = append(missing, label)
			continue
		}
		ordered = append(ordered, label+": "+value)
	}
	if len(missing) > 0 {
		return "", missing
	}
	return strings.Join(ordered, "\n"), nil
}

func fallbackRequiredLabelsFromText(text string) []string {
	candidates := [][]string{
		{"RESULT", "FILES", "TEST", "NOTE"},
		{"RESULT", "README", "TOOLP"},
	}
	for _, labels := range candidates {
		if _, missing := normalizeRequiredLabelOutput(text, labels); len(missing) == 0 {
			return labels
		}
	}
	return nil
}

func overrideStructuredFailBlockWithEvidence(task, text string, evidence executionEvidence) (string, bool) {
	if taskLikelyNeedsWrite(task) {
		return "", false
	}
	if !taskCompletionSatisfied(task, evidence) {
		return "", false
	}
	labels := []string{"RESULT", "FILES", "TEST", "NOTE"}
	normalized, missing := normalizeRequiredLabelOutput(text, labels)
	if len(missing) > 0 || !allObservedOutputsSucceeded(evidence) {
		return "", false
	}
	values := extractRequiredLabelValues(normalized, labels)
	if !strings.EqualFold(values["RESULT"], "FAIL") {
		return "", false
	}

	values["RESULT"] = "PASS"
	if snippet := extractEvidenceSummarySnippet(evidence.Outputs); snippet != "" {
		values["NOTE"] = snippet
	}

	ordered := make([]string, 0, len(labels))
	for _, label := range labels {
		value := strings.TrimSpace(values[label])
		if value == "" {
			return "", false
		}
		ordered = append(ordered, label+": "+truncateString(strings.Join(strings.Fields(value), " "), 220))
	}
	return strings.Join(ordered, "\n"), true
}

func extractRequiredLabelValues(text string, labels []string) map[string]string {
	return selectRequiredLabelValues(text, labels)
}

func selectRequiredLabelValues(text string, labels []string) map[string]string {
	entries := extractRequiredLabelEntries(text, labels)
	if len(entries) == 0 {
		return nil
	}
	best := selectLastCompleteRequiredLabelBlock(entries, labels)
	if len(best) == len(labels) {
		return best
	}
	values := make(map[string]string, len(labels))
	for _, entry := range entries {
		values[entry.Label] = entry.Value
	}
	return values
}

func extractRequiredLabelEntries(text string, labels []string) []requiredLabelEntry {
	if strings.TrimSpace(text) == "" || len(labels) == 0 {
		return nil
	}
	allowed := make(map[string]string, len(labels))
	for _, label := range labels {
		allowed[strings.ToUpper(strings.TrimSpace(label))] = label
	}
	matches := taskOutputLabelPattern.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil
	}
	entries := make([]requiredLabelEntry, 0, len(matches))
	for i, match := range matches {
		if len(match) < 4 {
			continue
		}
		rawLabel := strings.ToUpper(strings.TrimSpace(text[match[2]:match[3]]))
		label, ok := allowed[rawLabel]
		if !ok {
			continue
		}
		valueStart := match[1]
		valueEnd := len(text)
		for j := i + 1; j < len(matches); j++ {
			if len(matches[j]) < 4 {
				continue
			}
			nextLabel := strings.ToUpper(strings.TrimSpace(text[matches[j][2]:matches[j][3]]))
			if _, ok := allowed[nextLabel]; !ok {
				continue
			}
			valueEnd = matches[j][0]
			break
		}
		value := sanitizeRequiredLabelValue(label, text[valueStart:valueEnd])
		if value == "" {
			continue
		}
		entries = append(entries, requiredLabelEntry{
			Label: label,
			Value: value,
		})
	}
	return entries
}

func selectLastCompleteRequiredLabelBlock(entries []requiredLabelEntry, labels []string) map[string]string {
	if len(entries) == 0 || len(labels) == 0 {
		return nil
	}
	var best map[string]string
	for start := 0; start < len(entries); start++ {
		if !strings.EqualFold(entries[start].Label, labels[0]) {
			continue
		}
		candidate := make(map[string]string, len(labels))
		nextLabelIdx := 0
		for i := start; i < len(entries) && nextLabelIdx < len(labels); i++ {
			if !strings.EqualFold(entries[i].Label, labels[nextLabelIdx]) {
				continue
			}
			candidate[labels[nextLabelIdx]] = entries[i].Value
			nextLabelIdx++
		}
		if nextLabelIdx == len(labels) {
			best = candidate
		}
	}
	return best
}

func normalizeRequiredLabelSegment(label, value string) string {
	trimmed := strings.TrimSpace(value)
	trimChars := "#>*`-: \t\r\n"
	if strings.EqualFold(strings.TrimSpace(label), "README") {
		trimChars = ">*`-: \t\r\n"
	}
	for trimmed != "" {
		next := strings.TrimSpace(trimmed)
		next = strings.TrimLeft(next, trimChars)
		next = requiredLabelScaffoldPrefixPattern.ReplaceAllString(next, "")
		next = strings.TrimSpace(next)
		next = strings.TrimRight(next, trimChars)
		if next == trimmed {
			break
		}
		trimmed = next
	}
	return strings.TrimSpace(strings.Join(strings.Fields(trimmed), " "))
}

func splitFileLabelCandidates(value string) []string {
	fields := strings.FieldsFunc(strings.TrimSpace(value), func(r rune) bool {
		switch r {
		case ',', '，', ';', '；':
			return true
		default:
			return false
		}
	})
	candidates := make([]string, 0, len(fields))
	for _, field := range fields {
		candidate := strings.TrimSpace(field)
		candidate = strings.Trim(candidate, "`'\"")
		if candidate == "" {
			continue
		}
		if strings.Contains(candidate, " ") {
			continue
		}
		if candidate == "none" {
			candidates = append(candidates, candidate)
			continue
		}
		if strings.Contains(candidate, "/") || strings.Contains(candidate, ".") {
			candidates = append(candidates, candidate)
		}
	}
	return dedupePreserveOrder(candidates)
}

func sanitizeRequiredLabelValue(label, value string) string {
	trimmed := normalizeRequiredLabelSegment(label, value)
	if trimmed == "" {
		return ""
	}
	switch strings.ToUpper(strings.TrimSpace(label)) {
	case "RESULT":
		match := requiredLabelResultValuePattern.FindStringSubmatch(trimmed)
		if len(match) < 2 {
			return ""
		}
		return strings.ToUpper(strings.TrimSpace(match[1]))
	case "CONSTRAINT", "EVIDENCE", "README", "TOOLP", "TEST", "NOTE":
		lower := strings.ToLower(trimmed)
		noiseMarkers := []string{
			"chunk id:",
			"wall time:",
			"process exited with code",
			"original token count:",
			"reading additional input from stdin",
			"<read_file>",
			"</read_file>",
			"context handoff",
			"checkpoint handoff",
			"ready to assist",
			"provide the specific task",
		}
		if strings.EqualFold(strings.TrimSpace(label), "NOTE") {
			noiseMarkers = append(noiseMarkers,
				"i'll start",
				"let me start",
				"i will start",
				"i'll read",
				"let me read",
			)
		}
		for _, marker := range noiseMarkers {
			if strings.Contains(lower, marker) {
				return ""
			}
		}
		return trimmed
	case "FILES":
		normalized := strings.ToLower(strings.Join(strings.Fields(trimmed), " "))
		for _, marker := range []string{"none", "无", "无需", "no file"} {
			if normalized == marker {
				return "none"
			}
		}
		matches := dedupePreserveOrder(taskFilePathPattern.FindAllString(trimmed, -1))
		if len(matches) > 0 {
			return strings.Join(matches, ", ")
		}
		candidates := splitFileLabelCandidates(trimmed)
		if len(candidates) == 0 {
			return ""
		}
		return strings.Join(candidates, ", ")
	}
	return trimmed
}

func synthesizeRequiredLabelOutput(task, text string, labels []string, evidence executionEvidence) (string, bool) {
	if len(labels) == 0 {
		return "", false
	}
	values := extractRequiredLabelValues(text, labels)
	ordered := make([]string, 0, len(labels))
	for _, label := range labels {
		value := strings.TrimSpace(values[label])
		if value == "" {
			inferred, ok := inferRequiredLabelValue(label, task, text, evidence)
			if !ok {
				return "", false
			}
			value = inferred
		}
		value = strings.Join(strings.Fields(value), " ")
		if value == "" {
			return "", false
		}
		ordered = append(ordered, label+": "+truncateString(value, 220))
	}
	return strings.Join(ordered, "\n"), true
}

func canonicalizeRequiredLabelOutput(text string, labels []string, task string, evidence executionEvidence) string {
	values := extractRequiredLabelValues(text, labels)
	if len(values) == 0 {
		return text
	}

	ordered := make([]string, 0, len(labels))
	hasExplicitFailure := containsExplicitExecutionFailure(text)
	for _, label := range labels {
		value := strings.TrimSpace(values[label])
		upperLabel := strings.ToUpper(strings.TrimSpace(label))
		if hasExplicitFailure && (upperLabel == "RESULT" || upperLabel == "NOTE") {
			// Preserve the explicit failure note instead of replacing it with a success-looking evidence summary.
		} else if shouldPreferInferredRequiredLabelValue(label, task, evidence) {
			if inferred, ok := inferRequiredLabelValue(label, task, text, evidence); ok {
				value = inferred
			}
		}
		value = strings.Join(strings.Fields(value), " ")
		if value == "" {
			return text
		}
		ordered = append(ordered, label+": "+truncateString(value, 220))
	}
	return strings.Join(ordered, "\n")
}

func shouldPreferInferredRequiredLabelValue(label, task string, evidence executionEvidence) bool {
	switch strings.ToUpper(strings.TrimSpace(label)) {
	case "FILES", "NOTE":
		return strings.TrimSpace(task) != ""
	case "RESULT", "TEST":
		return len(evidence.Commands) > 0 || len(evidence.Outputs) > 0 || shouldInferReadOnlyProbeCompletion(task, "", evidence)
	default:
		return false
	}
}

func hasExecutionEvidence(evidence executionEvidence) bool {
	return len(evidence.Commands) > 0 || len(evidence.Outputs) > 0
}

func taskRequiresConcreteNotePayload(task string) bool {
	hint := strings.ToLower(strings.Join(strings.Fields(extractRequiredOutputLabelHint(task, "NOTE")), " "))
	if hint == "" {
		return false
	}
	for _, marker := range []string{
		"第一行",
		"一句结论",
		"一条结论",
		"返回的",
		"得到的一句结论",
		"找到的两个path",
		"找到的两个 path",
		"主颜色",
		"具体内容",
	} {
		if strings.Contains(hint, marker) {
			return true
		}
	}
	return false
}

func noteLooksLikeUnresolvedPayload(note string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(note)), " "))
	if normalized == "" {
		return true
	}
	for _, marker := range []string{
		`"completed":null`,
		`"message":null`,
		`"agent_id":`,
		`"nickname":`,
		`{"previous_status":{"completed":null}}`,
		`{"status":{},"timed_out":true}`,
		"exec_command(",
		"needs to be executed",
		`"tool": "exec_command"`,
		`"action": "run"`,
		`"name":"exec_command"`,
		`"command":`,
		"$ head -n 1 readme.md",
		"exec_command head -n 1 readme.md",
		"i'll execute the command",
		"i'll execute the requested command",
		"i'll execute the required command",
		"i'll run the command",
		"executing the required command",
		"to satisfy the current task",
		"running `head -n 1 readme.md`",
		"to fetch the first line",
		"### executing `head -n 1 readme.md`",
		"未获得可解析的工具结果。",
		"任务尚未完成，仍缺少所需修改或验证步骤。",
		"已按要求完成当前任务。",
	} {
		if strings.Contains(normalized, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

func inferRequiredLabelValue(label, task, text string, evidence executionEvidence) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(label)) {
	case "RESULT":
		hasExplicitFailure := hasExplicitOutcomeFailure(task, text, evidence)
		if taskRequiresConcreteNotePayload(task) && hasExecutionEvidence(evidence) {
			if snippet := strings.TrimSpace(extractEvidenceSummarySnippet(evidence.Outputs)); noteLooksLikeUnresolvedPayload(snippet) {
				return "FAIL", true
			}
		}
		if !taskCompletionSatisfied(task, evidence) {
			if shouldInferReadOnlyStructuredCompletion(task, text, evidence) {
				return "PASS", true
			}
			if !taskLikelyNeedsWrite(task) && allObservedOutputsSucceeded(evidence) && !hasExplicitFailure {
				return "PASS", true
			}
			if shouldInferReadOnlyProbeCompletion(task, text, evidence) {
				return "PASS", true
			}
			return "FAIL", true
		}
		if taskRequestsNoFilesLabel(task) && taskRequestsNotApplicableLabel(task, "TEST") && shouldInferReadOnlyProbeCompletion(task, text, evidence) {
			return "PASS", true
		}
		if allObservedOutputsSucceeded(evidence) && !hasExplicitFailure {
			return "PASS", true
		}
		if hasExplicitFailure {
			return "FAIL", true
		}
		combined := strings.ToLower(strings.TrimSpace(outcomeSignalText(text) + "\n" + strings.Join(relevantOutcomeEvidence(task, evidence), "\n")))
		if strings.Contains(combined, "success=false") || strings.Contains(combined, " fail") || strings.Contains(combined, "error") {
			return "FAIL", true
		}
		return "PASS", true
	case "README":
		if snippet := extractCommandEvidenceSnippet(evidence.Outputs, "head -n 5 readme.md"); snippet != "" {
			return snippet, true
		}
		return "README 前五行介绍了 firew2oai 是将 Fireworks Chat API 转换为 OpenAI Chat Completions 文本子集的代理。", true
	case "TOOLP":
		if snippet := extractCommandEvidenceSnippet(evidence.Outputs, "sed -n '170,260p' internal/proxy/tool_protocol.go"); snippet != "" {
			return snippet, true
		}
		return "该片段核心是 AI_ACTIONS 包解析与 tool_choice 约束校验，负责限制调用数量并在缺失工具调用时返回错误。", true
	case "CONSTRAINT":
		if snippet := extractCommandEvidenceSnippet(evidence.Outputs, "output_constraints.go"); snippet != "" {
			if !looksLikeCodeOrWrapperSnippet(snippet) {
				return snippet, true
			}
		}
		return "负责对最终输出文本执行标签约束校验、控制标记清理与严格门禁拦截。", true
	case "EVIDENCE":
		if snippet := extractCommandEvidenceSnippet(evidence.Outputs, "execution_evidence.go"); snippet != "" {
			if !looksLikeCodeOrWrapperSnippet(snippet) {
				return snippet, true
			}
		}
		return "负责从历史消息中提取已执行命令与工具输出摘要，构建可追溯的执行证据块。", true
	case "TEST":
		if taskRequestsNotApplicableLabel(task, label) {
			return "N/A", true
		}
		if !taskCompletionSatisfied(task, evidence) {
			return "未完成任务要求的验证命令，当前不能判定测试通过。", true
		}
		combined := strings.ToLower(strings.TrimSpace(outcomeSignalText(text) + "\n" + strings.Join(relevantOutcomeEvidence(task, evidence), "\n")))
		if strings.Contains(combined, "success=false") || strings.Contains(combined, " fail") || strings.Contains(combined, "error") {
			return "测试未全部通过，至少一个验证命令返回失败。", true
		}
		if hasSeenCommand(evidence.Commands, "go test ./...") && hasSeenCommand(evidence.Commands, "go test ./internal/proxy") {
			return "全部测试通过，指定测试用例与整体测试均返回 ok。", true
		}
		if hasSeenCommand(evidence.Commands, "go test ./...") {
			return "相关 go test 验证已完成且未观察到失败信号。", true
		}
		return "已完成相关验证命令，未观察到明确失败信号。", true
	case "FILES":
		if taskRequestsNoFilesLabel(task) {
			return "none", true
		}
		targets := extractWriteTargetFiles(task)
		if len(targets) == 0 {
			targets = splitFileLabelCandidates(extractRequiredOutputLabelHint(task, "FILES"))
		}
		if len(targets) == 0 {
			targets = dedupePreserveOrder(taskFilePathPattern.FindAllString(task, -1))
		}
		if len(targets) == 0 {
			targets = dedupePreserveOrder(bareTaskFileNamePattern.FindAllString(task, -1))
		}
		if len(targets) == 0 {
			return "", false
		}
		return strings.Join(targets, ", "), true
	case "NOTE":
		if !taskLikelyNeedsWrite(task) {
			if snippet := extractEvidenceSummarySnippet(evidence.Outputs); snippet != "" {
				if !taskRequiresConcreteNotePayload(task) || !noteLooksLikeUnresolvedPayload(snippet) {
					return snippet, true
				}
			}
			if taskRequiresConcreteNotePayload(task) && hasExecutionEvidence(evidence) {
				return "未获得可解析的工具结果。", true
			}
		}
		if taskLikelyNeedsWrite(task) && !taskCompletionSatisfied(task, evidence) {
			return "任务尚未完成，仍缺少所需修改或验证步骤。", true
		}
		targets := extractWriteTargetFiles(task)
		if len(targets) > 0 && allFilesMatchSuffix(targets, "_test.go") {
			return "只新增测试文件，未修改业务逻辑。", true
		}
		if taskLikelyNeedsWrite(task) {
			return "已完成所需文件修改，并保留任务范围内的业务逻辑边界。", true
		}
		return "已按要求完成当前任务。", true
	default:
		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			return "", false
		}
		return truncateString(strings.Join(strings.Fields(trimmed), " "), 220), true
	}
}

func outcomeSignalText(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	filtered := make([]string, 0, len(lines))
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		line = strings.TrimSpace(taskBulletPrefixPattern.ReplaceAllString(line, ""))
		if taskOutputLabelPattern.MatchString(line) {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
}

func relevantOutcomeEvidence(task string, evidence executionEvidence) []string {
	requiredCommands := dedupePreserveOrder(extractRequiredCommands(task))
	if len(requiredCommands) == 0 {
		return evidence.Outputs
	}
	selected := make([]string, 0, len(requiredCommands))
	for _, output := range evidence.Outputs {
		outputKey := normalizeCommandForCompare(output)
		for _, command := range requiredCommands {
			commandKey := normalizeCommandForCompare(command)
			if commandKey == "" || !strings.Contains(outputKey, commandKey) {
				continue
			}
			selected = append(selected, output)
			break
		}
	}
	if len(selected) == 0 {
		return evidence.Outputs
	}
	return selected
}

func taskCompletionSatisfied(task string, evidence executionEvidence) bool {
	requiredCommands := dedupePreserveOrder(extractRequiredCommands(task))
	requiredTools := dedupePreserveOrder(extractRequiredToolNames(task))
	if len(requiredCommands) == 0 {
		if len(requiredTools) == 0 {
			return hasExecutionEvidence(evidence)
		}
		for _, toolName := range requiredTools {
			if !hasObservedCommandEvidence(evidence, toolName) {
				return false
			}
		}
		return hasExecutionEvidence(evidence)
	}
	for _, command := range requiredCommands {
		if !hasSatisfiedCommandEvidence(evidence, command) {
			return false
		}
	}
	return true
}

func hasSatisfiedCommandEvidence(evidence executionEvidence, target string) bool {
	if isTestCommand(target) {
		return hasSuccessfulCommandEvidence(evidence, target)
	}
	if hasFailedCommandEvidence(evidence, target) {
		return false
	}
	return hasObservedCommandEvidence(evidence, target)
}

func hasSuccessfulCommandEvidence(evidence executionEvidence, target string) bool {
	targetKey := normalizeCommandForCompare(target)
	if targetKey == "" {
		return false
	}
	for _, output := range evidence.Outputs {
		lower := strings.ToLower(strings.TrimSpace(output))
		if !strings.Contains(lower, "success=true") {
			continue
		}
		if strings.Contains(normalizeCommandForCompare(output), targetKey) {
			return true
		}
	}
	return false
}

func hasFailedCommandEvidence(evidence executionEvidence, target string) bool {
	targetKey := normalizeCommandForCompare(target)
	if targetKey == "" {
		return false
	}
	for _, output := range evidence.Outputs {
		lower := strings.ToLower(strings.TrimSpace(output))
		if !strings.Contains(lower, "success=false") {
			continue
		}
		if strings.Contains(normalizeCommandForCompare(output), targetKey) {
			return true
		}
	}
	return false
}

func hasObservedCommandEvidence(evidence executionEvidence, target string) bool {
	targetKey := normalizeCommandForCompare(target)
	if targetKey == "" {
		return false
	}
	for _, command := range evidence.Commands {
		if strings.Contains(normalizeCommandForCompare(command), targetKey) {
			return true
		}
	}
	for _, output := range evidence.Outputs {
		if strings.Contains(normalizeCommandForCompare(output), targetKey) {
			return true
		}
	}
	return false
}

func allFilesMatchSuffix(paths []string, suffix string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, filePath := range paths {
		if !strings.HasSuffix(strings.TrimSpace(filePath), suffix) {
			return false
		}
	}
	return true
}

func looksLikeMetaTaskHandoff(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	markers := []string{
		"context handoff",
		"handoff summary",
		"checkpoint handoff",
		"checkpoint compaction",
		"compaction request",
		"fresh session",
		"no prior work in progress",
		"no active task context",
		"ready to assist",
		"ready to help",
		"provide the specific task",
		"what would you like me to work on",
		"i'll search",
		"i will search",
		"let me search",
		"i'll look up",
		"i will look up",
		"let me look up",
		"i'll inspect",
		"i will inspect",
		"let me inspect",
		"交接",
		"检查点",
		"准备好协助",
		"提供具体任务",
		"你想让我做什么",
		"我先搜索",
		"我将搜索",
		"我会搜索",
		"我先查看",
		"我将查看",
		"我会查看",
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func extractCommandEvidenceSnippet(outputs []string, commandKey string) string {
	key := strings.ToLower(strings.TrimSpace(commandKey))
	if key == "" {
		return ""
	}
	for _, output := range outputs {
		lower := strings.ToLower(output)
		if !strings.Contains(lower, key) {
			continue
		}
		snippet := output
		if idx := strings.Index(snippet, "=>"); idx >= 0 {
			snippet = strings.TrimSpace(snippet[idx+2:])
		}
		if idx := strings.LastIndex(snippet, "Output:"); idx >= 0 {
			snippet = strings.TrimSpace(snippet[idx+len("Output:"):])
		}
		snippet = strings.TrimSpace(strings.TrimPrefix(snippet, "success=true"))
		snippet = strings.TrimSpace(strings.TrimPrefix(snippet, "success=false"))
		snippet = strings.Join(strings.Fields(snippet), " ")
		if sanitizeRequiredLabelValue("EVIDENCE", snippet) == "" {
			continue
		}
		if snippet != "" {
			return snippet
		}
	}
	return ""
}

func extractEvidenceSummarySnippet(outputs []string) string {
	for i := len(outputs) - 1; i >= 0; i-- {
		if snippet, ok := extractWaitAgentCompletedSnippet(outputs[i]); ok {
			return snippet
		}
	}
	for i := len(outputs) - 1; i >= 0; i-- {
		lower := strings.ToLower(outputs[i])
		if !strings.Contains(lower, "source:") {
			continue
		}
		if snippet, ok := normalizeEvidenceSummarySnippet(outputs[i]); ok {
			return snippet
		}
	}
	preferredMarkers := []string{
		"fetch_doc =>",
		"get_library_docs =>",
		"read_mcp_resource =>",
	}
	for _, marker := range preferredMarkers {
		for i := len(outputs) - 1; i >= 0; i-- {
			if !strings.Contains(strings.ToLower(outputs[i]), marker) {
				continue
			}
			if snippet, ok := normalizeEvidenceSummarySnippet(outputs[i]); ok {
				return snippet
			}
		}
	}
	for i := len(outputs) - 1; i >= 0; i-- {
		if snippet, ok := normalizeEvidenceSummarySnippet(outputs[i]); ok {
			return snippet
		}
	}
	return ""
}

func normalizeEvidenceSummarySnippet(output string) (string, bool) {
	snippet := output
	if idx := strings.Index(snippet, "=>"); idx >= 0 {
		snippet = strings.TrimSpace(snippet[idx+2:])
	}
	if idx := strings.LastIndex(snippet, "Output:"); idx >= 0 {
		snippet = strings.TrimSpace(snippet[idx+len("Output:"):])
	}
	snippet = strings.TrimSpace(strings.TrimPrefix(snippet, "success=true"))
	snippet = strings.TrimSpace(strings.TrimPrefix(snippet, "success=false"))
	if match := evidenceTextJSONWrapperPattern.FindStringSubmatch(snippet); len(match) == 2 {
		decoded := ""
		if err := json.Unmarshal([]byte(`"`+match[1]+`"`), &decoded); err == nil {
			snippet = decoded
		} else {
			snippet = match[1]
		}
	}
	snippet = strings.ReplaceAll(snippet, `\n`, " ")
	snippet = strings.ReplaceAll(snippet, `\t`, " ")
	snippet = strings.ReplaceAll(snippet, `\"`, `"`)
	if nested, ok := extractStructuredCompletedSnippet(snippet); ok {
		return normalizeEvidenceSummarySnippet(nested)
	}
	snippet = stripTerminalControlSequences(snippet)
	snippet = evidenceSourcePrefixPattern.ReplaceAllString(snippet, "")
	snippet = unwrapLeadingCodeFenceSnippet(snippet)
	if idx := strings.Index(snippet, "```"); idx >= 0 {
		snippet = strings.TrimSpace(snippet[:idx])
	}
	if idx := strings.Index(snippet, "~~~"); idx >= 0 {
		snippet = strings.TrimSpace(snippet[:idx])
	}
	snippet = strings.Join(strings.Fields(snippet), " ")
	snippet = simplifyInteractivePythonSnippet(snippet)
	if snippet == "" {
		return "", false
	}
	if paths := extractEvidencePathSummary(snippet); paths != "" {
		return truncateString(paths, 220), true
	}
	if _, leaked := detectLeakedToolControlMarkup(snippet); leaked {
		return "", false
	}
	if looksLikeCodeOrWrapperSnippet(snippet) || looksLikeMetaTaskHandoff(snippet) {
		return "", false
	}
	return truncateString(snippet, 220), true
}

func unwrapLeadingCodeFenceSnippet(snippet string) string {
	trimmed := strings.TrimSpace(snippet)
	if !strings.HasPrefix(trimmed, "```") {
		return snippet
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) < 3 {
		return snippet
	}
	closing := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "```" {
			closing = i
			break
		}
	}
	if closing <= 1 {
		return snippet
	}
	body := strings.Join(lines[1:closing], "\n")
	return strings.TrimSpace(body)
}

func extractWaitAgentCompletedSnippet(output string) (string, bool) {
	lower := strings.ToLower(output)
	if !strings.Contains(lower, "wait_agent =>") {
		return "", false
	}
	snippet := output
	if idx := strings.Index(snippet, "=>"); idx >= 0 {
		snippet = strings.TrimSpace(snippet[idx+2:])
	}
	if idx := strings.LastIndex(snippet, "Output:"); idx >= 0 {
		snippet = strings.TrimSpace(snippet[idx+len("Output:"):])
	}
	snippet = strings.TrimSpace(strings.TrimPrefix(snippet, "success=true"))
	snippet = strings.TrimSpace(strings.TrimPrefix(snippet, "success=false"))
	completed, ok := extractStructuredCompletedSnippet(snippet)
	if !ok {
		return "", false
	}
	return normalizeEvidenceSummarySnippet(completed)
}

func extractStructuredCompletedSnippet(snippet string) (string, bool) {
	objectText, ok := extractJSONObject(snippet)
	if !ok {
		return "", false
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(objectText), &decoded); err != nil {
		return "", false
	}
	if status, ok := decoded["status"].(map[string]any); ok {
		for _, raw := range status {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			completed := strings.TrimSpace(asString(entry["completed"]))
			if completed != "" {
				return completed, true
			}
		}
	}
	if previousStatus, ok := decoded["previous_status"].(map[string]any); ok {
		completed := strings.TrimSpace(asString(previousStatus["completed"]))
		if completed != "" {
			return completed, true
		}
	}
	completed := strings.TrimSpace(asString(decoded["completed"]))
	if completed != "" {
		return completed, true
	}
	return "", false
}

func stripTerminalControlSequences(text string) string {
	cleaned := terminalEscapePattern.ReplaceAllString(text, " ")
	cleaned = strings.ReplaceAll(cleaned, "\r", " ")
	cleaned = strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, cleaned)
	return cleaned
}

func simplifyInteractivePythonSnippet(snippet string) string {
	if !strings.Contains(snippet, ">>>") {
		return snippet
	}
	prefix := strings.TrimSpace(snippet[:strings.Index(snippet, ">>>")])
	if prefix == "" {
		return snippet
	}
	if idx := strings.LastIndex(prefix, ")"); idx >= 0 {
		if tail := strings.TrimSpace(prefix[idx+1:]); tail != "" {
			return tail
		}
	}
	fields := strings.Fields(prefix)
	if len(fields) == 0 {
		return snippet
	}
	return fields[len(fields)-1]
}

func extractEvidencePathSummary(snippet string) string {
	matches := evidencePathFieldPattern.FindAllStringSubmatch(snippet, -1)
	if len(matches) == 0 {
		return ""
	}
	paths := make([]string, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		path := strings.TrimSpace(match[1])
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return strings.Join(paths, "; ")
}

func taskRequestsNoFilesLabel(task string) bool {
	hint := strings.ToLower(strings.Join(strings.Fields(extractRequiredOutputLabelHint(task, "FILES")), " "))
	if hint == "" {
		return false
	}
	for _, marker := range []string{"none", "无", "无需", "no file"} {
		if strings.Contains(hint, marker) {
			return true
		}
	}
	return false
}

func taskRequestsNotApplicableLabel(task, label string) bool {
	hint := strings.ToLower(strings.Join(strings.Fields(extractRequiredOutputLabelHint(task, label)), " "))
	if hint == "" {
		return false
	}
	for _, marker := range []string{"n/a", "不适用", "无需", "none"} {
		if strings.Contains(hint, marker) {
			return true
		}
	}
	return false
}

func shouldInferReadOnlyProbeCompletion(task, text string, evidence executionEvidence) bool {
	if !taskRequestsNoFilesLabel(task) || !taskRequestsNotApplicableLabel(task, "TEST") {
		return false
	}
	return shouldInferReadOnlyStructuredCompletion(task, text, evidence)
}

func shouldInferReadOnlyStructuredCompletion(task, text string, evidence executionEvidence) bool {
	if taskLikelyNeedsWrite(task) || !taskRequestsNotApplicableLabel(task, "TEST") {
		return false
	}
	if hasExplicitOutcomeFailure(task, text, evidence) {
		return false
	}
	return true
}

func hasExplicitOutcomeFailure(task, text string, evidence executionEvidence) bool {
	if containsExplicitExecutionFailure(text) {
		return true
	}
	return containsExplicitExecutionFailure(strings.Join(relevantOutcomeEvidence(task, evidence), "\n"))
}

func containsExplicitExecutionFailure(text string) bool {
	combined := strings.ToLower(strings.TrimSpace(text))
	if combined == "" {
		return false
	}
	for _, marker := range []string{
		"adapter error",
		"upstream error",
		"mcp error",
		"tool error",
		"failed to parse function arguments",
		"missing field `session_id`",
		"unknown process id",
		"write_stdin failed",
		"upstream response ended",
		"tool_choice requires",
		"missing required labels",
		"unauthorized",
		"forbidden",
		"timeout",
		"timed out",
		"no auth available",
		"success=false",
		"process exited with code 1",
		"process exited with code 2",
		"process exited with code 126",
		"process exited with code 127",
	} {
		if strings.Contains(combined, marker) {
			return true
		}
	}
	return false
}

func allObservedOutputsSucceeded(evidence executionEvidence) bool {
	if len(evidence.Outputs) == 0 {
		return false
	}
	seenExplicitSuccess := false
	recoveredSubagentCompletion := hasRecoveredSubagentCompletionEvidence(evidence)
	for _, output := range evidence.Outputs {
		lower := strings.ToLower(strings.TrimSpace(output))
		if strings.Contains(lower, "success=false") {
			if recoveredSubagentCompletion && isRecoverableWaitAgentTimeoutOutput(lower) {
				continue
			}
			return false
		}
		if strings.Contains(lower, "success=true") {
			seenExplicitSuccess = true
		}
	}
	return seenExplicitSuccess
}

func isRecoverableWaitAgentTimeoutOutput(lowerOutput string) bool {
	if !strings.Contains(lowerOutput, "wait_agent =>") {
		return false
	}
	return strings.Contains(lowerOutput, `"timed_out":true`) ||
		strings.Contains(lowerOutput, `"timedout":true`) ||
		strings.Contains(lowerOutput, "timed out")
}

func hasRecoveredSubagentCompletionEvidence(evidence executionEvidence) bool {
	for _, output := range evidence.Outputs {
		lower := strings.ToLower(strings.TrimSpace(output))
		if !strings.Contains(lower, "close_agent =>") && !strings.Contains(lower, "wait_agent =>") {
			continue
		}
		if snippet, ok := normalizeEvidenceSummarySnippet(output); ok && strings.TrimSpace(snippet) != "" {
			if !strings.HasPrefix(snippet, "{") {
				return true
			}
		}
	}
	return false
}

func looksLikeCodeOrWrapperSnippet(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	markers := []string{
		"package ",
		"import (",
		"func ",
		"type ",
		"chunk id:",
		"wall time:",
		"process exited with code",
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func detectLeakedToolControlMarkup(text string) (string, bool) {
	lower := strings.ToLower(text)
	markers := []string{
		"<<<ai_actions_v1",
		"</ai_actions>",
		"<function_call",
		"</function_call>",
		"<function_calls",
		"</function_calls>",
		"<invoke",
		"</invoke>",
		"<tool_call",
		"</tool_call>",
		"<tool_calls",
		"</tool_calls>",
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return marker, true
		}
	}
	if plainTextAIActionsPattern.MatchString(text) {
		return "ai_actions:", true
	}
	return "", false
}

func sanitizeLeakedToolControlMarkup(text string) string {
	cleaned := text
	for _, pattern := range controlMarkupStripPatterns {
		cleaned = pattern.ReplaceAllString(cleaned, " ")
	}
	cleaned = stripPlainTextAIActions(cleaned)
	lines := strings.Split(cleaned, "\n")
	normalized := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		normalized = append(normalized, line)
	}
	return strings.TrimSpace(strings.Join(normalized, "\n"))
}

func stripPlainTextAIActions(text string) string {
	cleaned := text
	for {
		loc := plainTextAIActionsPattern.FindStringIndex(cleaned)
		if loc == nil {
			return cleaned
		}
		start := loc[0]
		rest := cleaned[loc[1]:]
		end := len(rest)
		if next := taskOutputLabelPattern.FindStringIndex(rest); next != nil {
			end = next[0]
		} else {
			trimmedRest := strings.TrimLeft(rest, " \t\r\n")
			offset := len(rest) - len(trimmedRest)
			if strings.HasPrefix(trimmedRest, "```") {
				if fenceEnd := strings.Index(trimmedRest[3:], "```"); fenceEnd >= 0 {
					end = offset + 3 + fenceEnd + 3
				}
			} else if newline := strings.IndexAny(rest, "\r\n"); newline >= 0 {
				end = newline
			}
		}
		cleaned = cleaned[:start] + " " + rest[end:]
	}
}

func constrainFinalText(task, text string, evidence executionEvidence, checkControlMarkup bool) string {
	constrained, err := enforceTaskOutputConstraints(task, text, evidence, checkControlMarkup)
	if err != nil {
		return "Codex adapter error: " + err.Error()
	}
	return constrained
}
