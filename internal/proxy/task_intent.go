package proxy

import (
	"path"
	"regexp"
	"sort"
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

var taskReadOnlyMarkers = []string{
	"read-only",
	"readonly",
	"without modifying",
	"without changes",
	"do not modify",
	"don't modify",
	"no file changes",
	"只读",
	"不修改",
	"不要修改",
	"无需修改",
	"不改文件",
	"不修改文件",
	"不要改文件",
	"仅读",
	"只做分析",
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
var taskTestStyleGlobPattern = regexp.MustCompile(`(?i)(?:[a-z0-9_.-]+/)+\*_test\.go`)
var taskCommandLinePattern = regexp.MustCompile(`(?im)^\s*(?:[-*]|\d+[.)])?\s*((?:go test|pytest|cargo test|npm test|make test|golangci-lint|go vet|gofmt)\b[^\n]*)$`)
var taskCommandLabelPattern = regexp.MustCompile(`(?i)(?:执行命令|run command|execute command)\s*[:：]`)
var taskTrailingBulletSuffixPattern = regexp.MustCompile(`\s+[（(]?\d+[.)）]\s*$`)
var taskCommandTrailingStepPattern = regexp.MustCompile(`\s+\d+[.)][\s\S]*$`)
var taskBacktickPattern = regexp.MustCompile("`([^`\\n]+)`")
var goTestNamePattern = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]*\b`)
var taskOutputLabelPattern = regexp.MustCompile(`\b([A-Z][A-Z0-9_]{1,24}):`)
var taskBulletPrefixPattern = regexp.MustCompile(`^\s*(?:[-*]|\d+[.)]|[（(]?\d+[）)])\s*`)
var taskCommandTrailingDirectivePattern = regexp.MustCompile(`(?i)\s+(?:only output|output only)\b[\s\S]*$`)
var taskCommandTrailingConnectorPattern = regexp.MustCompile(`(?:\s|，|,|、|；|;)*(?:和|以及|及|and|then|然后)\s*$`)
var taskInlineStepMarkerPattern = regexp.MustCompile(`(?:^|\s)(?:\d+[.)]|[（(]?\d+[）)])\s+`)

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
		return extractBestActionableTaskBlock(text)
	}
	return latestUserTask(messages)
}

func stableActionableUserTask(messages []ChatMessage) string {
	latest := latestActionableUserTask(messages)
	if latest == "" {
		return latestUserTask(messages)
	}
	if !isWeakFollowupTask(latest) {
		return latest
	}

	best := latest
	bestScore := actionableTaskSpecificityScore(latest)
	for i := len(messages) - 1; i >= 0; i-- {
		if !strings.EqualFold(messages[i].Role, "user") {
			continue
		}
		text := strings.TrimSpace(messages[i].Content)
		if text == "" || isToolResultSummaryMessage(text) {
			continue
		}
		candidate := extractBestActionableTaskBlock(text)
		score := actionableTaskBlockScore(candidate)
		if score > bestScore {
			best = candidate
			bestScore = score
		}
	}
	if strings.TrimSpace(best) != "" {
		return best
	}
	return latest
}

func isWeakFollowupTask(task string) bool {
	trimmed := strings.TrimSpace(task)
	if trimmed == "" {
		return true
	}
	lower := strings.ToLower(trimmed)
	if isInstructionDocumentBlock(lower) {
		return true
	}
	if actionableTaskSpecificityScore(trimmed) >= 20 {
		return false
	}
	for _, marker := range []string{
		"继续",
		"继续推进",
		"继续处理",
		"继续完成",
		"继续分析",
		"继续优化",
		"ok，那继续",
		"ok, continue",
		"continue",
		"continue please",
		"go on",
		"keep going",
	} {
		if lower == marker {
			return true
		}
	}
	return false
}

func isToolResultSummaryMessage(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	return strings.HasPrefix(lower, "tool result") || strings.HasPrefix(lower, "tool output")
}

func extractBestActionableTaskBlock(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	best := trimmed
	bestScore := actionableTaskBlockScore(trimmed)
	for _, block := range splitTaskBlocks(trimmed) {
		score := actionableTaskBlockScore(block)
		if score > bestScore {
			best = block
			bestScore = score
		}
	}
	return strings.TrimSpace(best)
}

func splitTaskBlocks(text string) []string {
	parts := strings.Split(text, "\n\n")
	blocks := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		blocks = append(blocks, part)
	}
	return blocks
}

func actionableTaskBlockScore(block string) int {
	score := actionableTaskSpecificityScore(block)
	lower := strings.ToLower(strings.TrimSpace(block))
	if lower == "" {
		return 0
	}
	if isInstructionDocumentBlock(lower) {
		score -= 120
	}
	for _, marker := range []string{
		"agents.md",
		"repository guidelines",
		"任务工作流",
		"质量红线",
		"测试体系",
		"工程质量基线",
		"附录：skills 使用规则",
		"schema-sensitive",
		"rust/python",
	} {
		if strings.Contains(lower, marker) {
			score -= 40
		}
	}
	if strings.Contains(lower, "请在当前仓库完成") || strings.Contains(lower, "最后只输出") {
		score += 20
	}
	if strings.Count(block, "\n") >= 12 {
		score -= 20
	}
	return score
}

func isInstructionDocumentBlock(lower string) bool {
	if lower == "" {
		return false
	}
	docMarkers := []string{
		"# repository guidelines",
		"## project structure & module organization",
		"## build, test, and development commands",
		"## coding style & naming conventions",
		"## testing guidelines",
		"## commit & pull request guidelines",
		"## security & configuration tips",
		"document requirements",
		"recommended sections",
		"任务工作流",
		"质量红线",
		"测试体系",
		"仓库执行约束",
		"附录：skills 使用规则",
	}
	matchCount := 0
	for _, marker := range docMarkers {
		if strings.Contains(lower, marker) {
			matchCount++
		}
	}
	return matchCount >= 2
}

func actionableTaskSpecificityScore(task string) int {
	trimmed := strings.TrimSpace(task)
	if trimmed == "" {
		return 0
	}

	score := 0
	score += len(dedupePreserveOrder(taskFilePathPattern.FindAllString(trimmed, -1))) * 20
	score += len(dedupePreserveOrder(extractRequiredCommands(trimmed))) * 15
	score += len(dedupePreserveOrder(extractRequiredOutputLabels(trimmed))) * 10
	if taskLikelyNeedsWrite(trimmed) {
		score += 10
	}
	if taskLikelyNeedsTools(trimmed) {
		score += 5
	}
	return score
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
	lower := strings.ToLower(trimmed)
	if containsAny(lower, taskReadOnlyMarkers) {
		return false
	}
	return containsAny(lower, taskWriteKeywords)
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
		cmd := stripTaskCommandTrailingDirectives(strings.TrimSpace(match[1]))
		if isLikelyTaskShellCommand(cmd) {
			commands = append(commands, cmd)
		}
	}

	matches := taskCommandLinePattern.FindAllStringSubmatch(task, -1)
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		cmd := stripTaskCommandTrailingDirectives(strings.TrimSpace(match[1]))
		if cmd != "" {
			commands = append(commands, cmd)
		}
	}

	commands = append(commands, extractInlineCommands(task)...)
	return commands
}

func extractStyleInspectionCommands(task string) []string {
	matches := dedupePreserveOrder(taskTestStyleGlobPattern.FindAllString(task, -1))
	if len(matches) == 0 {
		return nil
	}

	commands := make([]string, 0, len(matches))
	for _, glob := range matches {
		glob = strings.TrimSpace(glob)
		if glob == "" {
			continue
		}
		dir := strings.TrimSpace(path.Dir(glob))
		pattern := strings.TrimSpace(path.Base(glob))
		if dir == "" || dir == "." || pattern == "" {
			continue
		}
		cmd := "find " + shellQuoteSingle(dir) + " -maxdepth 1 -name " + shellQuoteSingle(pattern) + " | sort | head -n 5"
		commands = append(commands, cmd)
	}
	return dedupePreserveOrder(commands)
}

func extractWriteTargetFiles(task string) []string {
	if !taskLikelyNeedsWrite(task) {
		return nil
	}
	segments := splitTaskActionSegments(task)
	targets := make([]string, 0, 4)
	for _, segment := range segments {
		lower := strings.ToLower(segment)
		if !containsAny(lower, taskWriteKeywords) {
			continue
		}
		targets = append(targets, taskFilePathPattern.FindAllString(segment, -1)...)
	}
	return dedupePreserveOrder(targets)
}

func splitTaskActionSegments(task string) []string {
	lines := strings.Split(task, "\n")
	segments := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		inline := splitInlineTaskSteps(line)
		if len(inline) == 0 {
			segments = append(segments, line)
			continue
		}
		segments = append(segments, inline...)
	}
	return dedupePreserveOrder(segments)
}

func splitInlineTaskSteps(line string) []string {
	matches := taskInlineStepMarkerPattern.FindAllStringIndex(line, -1)
	if len(matches) <= 1 {
		return []string{strings.TrimSpace(line)}
	}
	segments := make([]string, 0, len(matches))
	for i, match := range matches {
		start := match[0]
		if start > 0 && line[start] == ' ' {
			start++
		}
		end := len(line)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		segment := strings.TrimSpace(line[start:end])
		if segment != "" {
			segments = append(segments, segment)
		}
	}
	if len(segments) == 0 {
		return []string{strings.TrimSpace(line)}
	}
	return segments
}

func extractNamedGoTests(task string) []string {
	return dedupePreserveOrder(goTestNamePattern.FindAllString(task, -1))
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
	candidate = stripTaskCommandTrailingDirectives(candidate)
	candidate = sanitizeExecCommandText(candidate)
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
		segment = stripTaskCommandTrailingDirectives(segment)
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

func stripTaskCommandTrailingDirectives(command string) string {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return ""
	}

	trimmed = strings.TrimSpace(taskCommandTrailingStepPattern.ReplaceAllString(trimmed, ""))

	for _, marker := range []string{
		"最终只输出",
		" 最终只输出",
		"完成后只输出",
		" 完成后只输出",
		"最后只输出",
		" 最后只输出",
		"只输出",
		" 只输出",
		"最终仅输出",
		" 最终仅输出",
		"完成后仅输出",
		" 完成后仅输出",
		"仅输出",
		" 仅输出",
	} {
		if idx := strings.Index(trimmed, marker); idx >= 0 {
			return strings.TrimSpace(trimmed[:idx])
		}
	}

	if loc := taskCommandTrailingDirectivePattern.FindStringIndex(trimmed); loc != nil {
		return strings.TrimSpace(trimmed[:loc[0]])
	}
	return trimmed
}

func extractInlineCommands(task string) []string {
	lower := strings.ToLower(task)
	if strings.TrimSpace(lower) == "" {
		return nil
	}

	type span struct {
		start int
	}
	spans := make([]span, 0, 8)
	for _, prefix := range taskShellCommandPrefixes {
		search := 0
		needle := strings.ToLower(prefix)
		for {
			idx := strings.Index(lower[search:], needle)
			if idx < 0 {
				break
			}
			start := search + idx
			search = start + len(needle)
			if !isInlineCommandBoundary(lower, start, needle) {
				continue
			}
			spans = append(spans, span{start: start})
		}
	}
	if len(spans) == 0 {
		return nil
	}

	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	starts := make([]int, 0, len(spans))
	last := -1
	for _, item := range spans {
		if item.start == last {
			continue
		}
		starts = append(starts, item.start)
		last = item.start
	}

	commands := make([]string, 0, len(starts))
	for i, start := range starts {
		end := len(task)
		if i+1 < len(starts) && starts[i+1] < end {
			end = starts[i+1]
		}
		if punct := strings.IndexAny(task[start:end], "\n。；;"); punct >= 0 {
			end = start + punct
		}
		cmd := strings.TrimSpace(task[start:end])
		cmd = strings.Trim(cmd, "`")
		cmd = stripTaskCommandTrailingDirectives(cmd)
		cmd = trimTaskCommandTrailingConnector(cmd)
		cmd = strings.TrimRight(cmd, "，,`")
		cmd = strings.TrimSpace(cmd)
		for _, segment := range splitCompoundTaskCommand(cmd) {
			if isLikelyTaskShellCommand(segment) {
				commands = append(commands, segment)
			}
		}
	}
	return commands
}

func splitCompoundTaskCommand(command string) []string {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return nil
	}

	type span struct {
		start int
	}
	spans := []span{{start: 0}}
	lower := strings.ToLower(trimmed)
	for _, prefix := range taskShellCommandPrefixes {
		needle := strings.ToLower(prefix)
		search := len(needle)
		for search < len(lower) {
			idx := strings.Index(lower[search:], needle)
			if idx < 0 {
				break
			}
			start := search + idx
			search = start + len(needle)
			if !isInlineCommandBoundary(lower, start, needle) {
				continue
			}
			spans = append(spans, span{start: start})
		}
	}
	if len(spans) == 1 {
		return []string{trimmed}
	}

	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	segments := make([]string, 0, len(spans))
	lastStart := -1
	for i, current := range spans {
		if current.start == lastStart {
			continue
		}
		lastStart = current.start

		end := len(trimmed)
		if i+1 < len(spans) && spans[i+1].start < end {
			end = spans[i+1].start
		}
		segment := strings.TrimSpace(trimmed[current.start:end])
		segment = stripTaskCommandTrailingDirectives(segment)
		segment = trimTaskCommandTrailingConnector(segment)
		segment = strings.TrimSpace(segment)
		if segment != "" {
			segments = append(segments, segment)
		}
	}
	if len(segments) == 0 {
		return []string{trimmed}
	}
	return segments
}

func trimTaskCommandTrailingConnector(command string) string {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return ""
	}
	trimmed = strings.TrimSpace(taskCommandTrailingConnectorPattern.ReplaceAllString(trimmed, ""))
	return strings.TrimSpace(trimmed)
}

func isInlineCommandBoundary(text string, start int, prefix string) bool {
	if start > 0 {
		prev := text[start-1]
		if (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9') || prev == '_' {
			return false
		}
	}

	switch prefix {
	case "ls":
		next := start + len(prefix)
		if next < len(text) {
			ch := text[next]
			if ch != ' ' && ch != '\t' && ch != '-' && ch != '\n' {
				return false
			}
		}
	case "pwd", "tree":
		next := start + len(prefix)
		if next < len(text) {
			ch := text[next]
			if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_' {
				return false
			}
		}
	}

	return true
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
