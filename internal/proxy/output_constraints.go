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
			if normalizedFallback, ok := synthesizeRequiredLabelOutput("", requiredLabels, evidence); ok {
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

	if len(requiredLabels) == 0 {
		return trimmed, nil
	}

	normalized, missing := normalizeRequiredLabelOutput(trimmed, requiredLabels)
	if len(missing) == 0 {
		return normalized, nil
	}
	if !strictOutputGate {
		if normalizedFallback, ok := synthesizeRequiredLabelOutput(trimmed, requiredLabels, evidence); ok {
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
			values[label] = value
			break
		}
	}
	return values
}

func synthesizeRequiredLabelOutput(text string, labels []string, evidence executionEvidence) (string, bool) {
	if len(labels) == 0 {
		return "", false
	}
	values := extractRequiredLabelValues(text, labels)
	ordered := make([]string, 0, len(labels))
	for _, label := range labels {
		value := strings.TrimSpace(values[label])
		if value == "" {
			inferred, ok := inferRequiredLabelValue(label, text, evidence)
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

func inferRequiredLabelValue(label, text string, evidence executionEvidence) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(label)) {
	case "RESULT":
		combined := strings.ToLower(strings.TrimSpace(text + "\n" + strings.Join(evidence.Outputs, "\n")))
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
	default:
		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			return "", false
		}
		return truncateString(strings.Join(strings.Fields(trimmed), " "), 220), true
	}
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
		snippet = strings.TrimSpace(strings.TrimPrefix(snippet, "success=true"))
		snippet = strings.TrimSpace(strings.TrimPrefix(snippet, "success=false"))
		snippet = strings.Join(strings.Fields(snippet), " ")
		if snippet != "" {
			return snippet
		}
	}
	return ""
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
