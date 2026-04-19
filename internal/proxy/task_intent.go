package proxy

import (
	"regexp"
	"strings"
)

var taskActionKeywords = []string{
	"edit", "modify", "update", "fix", "patch", "implement", "add", "create",
	"write", "run", "execute", "inspect", "check", "read", "open", "apply",
	"change", "debug", "refactor", "test", "verify",
	"修改", "修复", "新增", "添加", "运行", "执行", "读取", "检查", "排查", "重构", "测试",
}

var taskTargetKeywords = []string{
	".go", ".py", ".ts", ".js", ".md", "internal/", "src/", "tests/", "test/",
	"dockerfile", "makefile", "go test", "pytest", "cargo test", "npm test", "make test",
	"sed -n", "ls -", "cat ", "git diff", "git status", "命令", "文件", "目录",
}

var taskPlainResponseKeywords = []string{
	"summarize", "summary", "explain", "description", "概述", "总结", "解释", "介绍",
}

var taskWriteKeywords = []string{
	"edit", "modify", "update", "fix", "patch", "implement", "add", "create", "write", "change", "refactor",
	"修改", "修复", "新增", "添加", "实现", "重构", "优化", "完善", "补充",
}

var taskShellCommandPrefixes = []string{
	"go test",
	"pytest",
	"cargo test",
	"npm test",
	"pnpm test",
	"bun test",
	"make test",
	"golangci-lint",
	"go vet",
	"gofmt",
	"head ",
	"sed -n",
	"cat ",
	"tail ",
	"rg ",
	"grep ",
	"awk ",
	"wc ",
	"find ",
	"ls",
	"pwd",
	"tree",
	"stat ",
	"git diff",
	"git status",
	"git show",
}

var taskFilePathPattern = regexp.MustCompile(`(?i)(?:[a-z0-9_.-]+/)+[a-z0-9_.-]+\.(?:go|py|ts|js|jsx|tsx|md|json|yaml|yml|toml|sh|sql)`)
var taskCommandLinePattern = regexp.MustCompile(`(?im)^\s*(?:[-*]|\d+[.)])?\s*((?:go test|pytest|cargo test|npm test|make test|golangci-lint|go vet|gofmt)\b[^\n]*)$`)
var taskInlineCommandPattern = regexp.MustCompile(`(?i)\b(?:go test|pytest|cargo test|npm test|make test|golangci-lint|go vet|gofmt)\b[^,\n。；;]*`)
var taskCommandLabelPattern = regexp.MustCompile(`(?i)(?:执行命令|run command|execute command)\s*[:：]`)
var taskTrailingBulletSuffixPattern = regexp.MustCompile(`\s+[（(]?\d+[.)）]\s*$`)
var taskBacktickPattern = regexp.MustCompile("`([^`\\n]+)`")
var taskOutputLabelPattern = regexp.MustCompile(`\b([A-Z][A-Z0-9_]{1,24}):`)
var taskBulletPrefixPattern = regexp.MustCompile(`^\s*(?:[-*]|\d+[.)]|[（(]?\d+[）)])\s*`)

func latestUserTask(messages []ChatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if !strings.EqualFold(messages[i].Role, "user") {
			continue
		}
		text := strings.TrimSpace(messages[i].Content)
		if text != "" {
			return text
		}
	}
	return ""
}

func latestActionableUserTask(messages []ChatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if !strings.EqualFold(messages[i].Role, "user") {
			continue
		}
		text := strings.TrimSpace(messages[i].Content)
		if text == "" {
			continue
		}
		if isToolResultSummaryMessage(text) {
			continue
		}
		return text
	}
	return latestUserTask(messages)
}

func isToolResultSummaryMessage(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	return strings.HasPrefix(lower, "tool result") || strings.HasPrefix(lower, "tool output")
}

func taskLikelyNeedsTools(task string) bool {
	trimmed := strings.TrimSpace(task)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)

	// Explicit command/test tasks should always route through tools.
	for _, token := range []string{
		"go test", "pytest", "cargo test", "npm test", "make test",
		"sed -n", "ls -la", "git diff", "git status", "git show",
	} {
		if strings.Contains(lower, token) {
			return true
		}
	}

	hasAction := containsAny(lower, taskActionKeywords)
	hasTarget := containsAny(lower, taskTargetKeywords) || strings.ContainsAny(trimmed, "/\\`")
	if hasAction && hasTarget {
		return true
	}

	// Explicit plain-answer/summarization requests should not force tools.
	if containsAny(lower, taskPlainResponseKeywords) && !hasTarget {
		return false
	}
	return false
}

func taskLikelyNeedsWrite(task string) bool {
	trimmed := strings.TrimSpace(task)
	if trimmed == "" {
		return false
	}
	return containsAny(strings.ToLower(trimmed), taskWriteKeywords)
}

func containsAny(text string, keywords []string) bool {
	for _, keyword := range keywords {
		if strings.Contains(text, keyword) {
			return true
		}
	}
	return false
}

func buildTaskCompletionGate(task string) string {
	task = strings.TrimSpace(task)
	if task == "" || !taskLikelyNeedsTools(task) {
		return ""
	}

	requiredFiles := taskFilePathPattern.FindAllString(task, -1)
	requiredCommands := extractRequiredCommands(task)
	requiredLabels := extractRequiredOutputLabels(task)

	requiredFiles = dedupePreserveOrder(requiredFiles)
	requiredCommands = dedupePreserveOrder(requiredCommands)
	requiredLabels = dedupePreserveOrder(requiredLabels)
	if len(requiredFiles) == 0 && len(requiredCommands) == 0 && len(requiredLabels) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n<TASK_COMPLETION_GATE>\n")
	b.WriteString("Try to satisfy every required item below before mode final.\n")
	b.WriteString("Do not repeat the same successful read/list command; advance to the next unmet item.\n")
	if len(requiredFiles) > 0 {
		b.WriteString("Required file edits:\n")
		for _, file := range requiredFiles {
			b.WriteString("- ")
			b.WriteString(file)
			b.WriteByte('\n')
		}
	}
	if len(requiredCommands) > 0 {
		b.WriteString("Required commands to run via tools:\n")
		for _, cmd := range requiredCommands {
			b.WriteString("- ")
			b.WriteString(cmd)
			b.WriteByte('\n')
		}
	}
	if len(requiredLabels) > 0 {
		b.WriteString("Required final output labels (exact):\n")
		for _, label := range requiredLabels {
			b.WriteString("- ")
			b.WriteString(label)
			b.WriteString(":\n")
		}
	}
	b.WriteString("If required items are still pending, emit AI_ACTIONS with mode tool for the next concrete step.\n")
	b.WriteString("If you are blocked after repeated failures, emit mode final with a concise blocker report and list unmet items.\n")
	b.WriteString("</TASK_COMPLETION_GATE>\n")
	return b.String()
}

func extractRequiredCommands(task string) []string {
	commands := make([]string, 0, 8)

	for _, line := range strings.Split(task, "\n") {
		cmd := normalizeTaskCommandCandidate(line)
		if cmd == "" || !isLikelyTaskShellCommand(cmd) {
			continue
		}
		commands = append(commands, cmd)
	}

	commands = append(commands, extractLabeledCommands(task)...)

	for _, match := range taskBacktickPattern.FindAllStringSubmatch(task, -1) {
		if len(match) < 2 {
			continue
		}
		cmd := strings.TrimSpace(match[1])
		if isLikelyTaskShellCommand(cmd) {
			commands = append(commands, cmd)
		}
	}

	matches := taskCommandLinePattern.FindAllStringSubmatch(task, -1)
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		cmd := strings.TrimSpace(match[1])
		if cmd != "" {
			commands = append(commands, cmd)
		}
	}

	for _, match := range taskInlineCommandPattern.FindAllString(task, -1) {
		cmd := strings.TrimSpace(match)
		cmd = strings.Trim(cmd, "`")
		cmd = strings.TrimRight(cmd, "。；;，,`")
		if cmd == "" {
			continue
		}
		if isLikelyTaskShellCommand(cmd) {
			commands = append(commands, cmd)
		}
	}
	return commands
}

func normalizeTaskCommandCandidate(line string) string {
	candidate := strings.TrimSpace(taskBulletPrefixPattern.ReplaceAllString(line, ""))
	candidate = strings.Trim(candidate, "`")
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return ""
	}
	if idx := strings.IndexAny(candidate, ":："); idx >= 0 {
		tail := strings.TrimSpace(candidate[idx+1:])
		if isLikelyTaskShellCommand(tail) {
			candidate = tail
		}
	}
	candidate = strings.TrimRight(candidate, "。；;，,")
	return strings.TrimSpace(candidate)
}

func extractLabeledCommands(task string) []string {
	indices := taskCommandLabelPattern.FindAllStringIndex(task, -1)
	if len(indices) == 0 {
		return nil
	}

	commands := make([]string, 0, len(indices))
	for i, idx := range indices {
		start := idx[1]
		end := len(task)
		if i+1 < len(indices) {
			end = indices[i+1][0]
		}
		segment := normalizeTaskCommandCandidate(task[start:end])
		if segment == "" {
			continue
		}
		segment = strings.TrimSpace(taskTrailingBulletSuffixPattern.ReplaceAllString(segment, ""))
		segment = strings.TrimRight(segment, "。；;，,`")
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		if isLikelyTaskShellCommand(segment) {
			commands = append(commands, segment)
		}
	}
	return commands
}

func isLikelyTaskShellCommand(text string) bool {
	candidate := strings.TrimSpace(text)
	if candidate == "" {
		return false
	}
	lower := strings.ToLower(candidate)
	for _, prefix := range taskShellCommandPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func extractRequiredOutputLabels(task string) []string {
	lower := strings.ToLower(task)
	if !strings.Contains(lower, "只输出") && !strings.Contains(lower, "only output") && !strings.Contains(lower, "output only") {
		return nil
	}
	matches := taskOutputLabelPattern.FindAllStringSubmatch(task, -1)
	labels := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		label := strings.TrimSpace(match[1])
		if label != "" {
			labels = append(labels, label)
		}
	}
	return labels
}

func dedupePreserveOrder(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		v := strings.TrimSpace(value)
		if v == "" {
			continue
		}
		key := strings.ToLower(v)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, v)
	}
	return out
}
