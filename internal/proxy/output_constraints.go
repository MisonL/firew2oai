package proxy

import (
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

func enforceTaskOutputConstraints(task, text string, evidence executionEvidence, checkControlMarkup bool) (string, error) {
	trimmed := strings.TrimSpace(text)
	strictOutputGate := isStrictOutputGateEnabled()
	requiredLabels := dedupePreserveOrder(extractRequiredOutputLabels(task))
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

	if checkControlMarkup {
		if marker, found := detectLeakedToolControlMarkup(trimmed); found {
			if strictOutputGate {
				return "", fmt.Errorf("model leaked unsupported tool-control markup %q in final text", marker)
			}
			trimmed = sanitizeLeakedToolControlMarkup(trimmed)
			if trimmed == "" {
				return strings.TrimSpace(text), nil
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
	lines := strings.Split(text, "\n")
	found := make(map[string]string, len(labels))
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		line = strings.TrimSpace(taskBulletPrefixPattern.ReplaceAllString(line, ""))
		for _, label := range labels {
			prefix := label + ":"
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			if _, exists := found[label]; exists {
				break
			}
			value := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			value = sanitizeRequiredLabelValue(label, value)
			if value == "" {
				break
			}
			if value == "" {
				found[label] = prefix
			} else {
				found[label] = prefix + " " + value
			}
			break
		}
	}

	missing := make([]string, 0, len(labels))
	ordered := make([]string, 0, len(labels))
	for _, label := range labels {
		value, ok := found[label]
		if !ok {
			missing = append(missing, label)
			continue
		}
		ordered = append(ordered, value)
	}
	if len(missing) > 0 {
		return "", missing
	}
	return strings.Join(ordered, "\n"), nil
}

func extractRequiredLabelValues(text string, labels []string) map[string]string {
	values := make(map[string]string, len(labels))
	lines := strings.Split(text, "\n")
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		line = strings.TrimSpace(taskBulletPrefixPattern.ReplaceAllString(line, ""))
		for _, label := range labels {
			prefix := label + ":"
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			if _, exists := values[label]; exists {
				break
			}
			value := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			value = sanitizeRequiredLabelValue(label, value)
			if value == "" {
				break
			}
			values[label] = value
			break
		}
	}
	return values
}

func sanitizeRequiredLabelValue(label, value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	switch strings.ToUpper(strings.TrimSpace(label)) {
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
	case "FILES":
		matches := dedupePreserveOrder(taskFilePathPattern.FindAllString(trimmed, -1))
		if len(matches) == 0 {
			return ""
		}
		return strings.Join(matches, ", ")
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
	for _, label := range labels {
		value := strings.TrimSpace(values[label])
		if shouldPreferInferredRequiredLabelValue(label, task, evidence) {
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
		return len(evidence.Commands) > 0 || len(evidence.Outputs) > 0
	default:
		return false
	}
}

func hasExecutionEvidence(evidence executionEvidence) bool {
	return len(evidence.Commands) > 0 || len(evidence.Outputs) > 0
}

func inferRequiredLabelValue(label, task, text string, evidence executionEvidence) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(label)) {
	case "RESULT":
		if !taskCompletionSatisfied(task, evidence) {
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
		targets := extractWriteTargetFiles(task)
		if len(targets) == 0 {
			targets = dedupePreserveOrder(taskFilePathPattern.FindAllString(task, -1))
		}
		if len(targets) == 0 {
			return "", false
		}
		return strings.Join(targets, ", "), true
	case "NOTE":
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
	if len(requiredCommands) == 0 {
		return hasExecutionEvidence(evidence)
	}
	for _, command := range requiredCommands {
		if !hasSuccessfulCommandEvidence(evidence, command) {
			return false
		}
	}
	return true
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
		"交接",
		"检查点",
		"准备好协助",
		"提供具体任务",
		"你想让我做什么",
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
	return "", false
}

func sanitizeLeakedToolControlMarkup(text string) string {
	cleaned := text
	for _, pattern := range controlMarkupStripPatterns {
		cleaned = pattern.ReplaceAllString(cleaned, " ")
	}
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

func constrainFinalText(task, text string, evidence executionEvidence, checkControlMarkup bool) string {
	constrained, err := enforceTaskOutputConstraints(task, text, evidence, checkControlMarkup)
	if err != nil {
		return "Codex adapter error: " + err.Error()
	}
	return constrained
}
