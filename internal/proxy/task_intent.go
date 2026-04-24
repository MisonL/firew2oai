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

var taskToolKeywords = []string{
	"exec_command",
	"write_stdin",
	"update_plan",
	"js_repl",
	"js_repl_reset",
	"web_search",
	"view_image",
	"spawn_agent",
	"send_input",
	"resume_agent",
	"wait_agent",
	"close_agent",
	"list_mcp_resources",
	"list_mcp_resource_templates",
	"read_mcp_resource",
	"mcp__",
	"chrome devtools",
	"docfork",
	"cloudflare api",
	"search_docs",
	"fetch_doc",
	"new_page",
	"take_snapshot",
	"wait_for",
	"必须使用",
	"must use",
	"use the exact tool",
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

var taskFilePathPattern = regexp.MustCompile(`(?i)(?:[a-z0-9_.-]+/)+[a-z0-9_.-]+\.(?:go|py|ts|js|jsx|tsx|md|json|yaml|yml|toml|sh|sql|txt)`)
var taskTestStyleGlobPattern = regexp.MustCompile(`(?i)(?:[a-z0-9_.-]+/)+\*_test\.go`)
var taskCommandLinePattern = regexp.MustCompile(`(?im)^\s*(?:[-*]|\d+[.)])?\s*((?:go test|pytest|cargo test|npm test|make test|golangci-lint|go vet|gofmt)\b[^\n]*)$`)
var taskCommandLabelPattern = regexp.MustCompile(`(?i)(?:执行命令|run command|execute command)\s*[:：]`)
var taskTrailingBulletSuffixPattern = regexp.MustCompile(`\s+[（(]?\d+[.)）]\s*$`)
var taskCommandTrailingStepPattern = regexp.MustCompile(`\s+\d+[.)][\s\S]*$`)
var taskBacktickPattern = regexp.MustCompile("`([^`\\n]+)`")
var goTestNamePattern = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]*\b`)
var taskOutputLabelPattern = regexp.MustCompile(`\b([A-Z][A-Z0-9_]{1,24}):`)
var taskPlainOutputLabelListPattern = regexp.MustCompile(`(?i)(?:只输出|仅输出|最终输出|最后输出|only output|output only)[^A-Z\n]{0,40}([A-Z][A-Z0-9_]{1,24}(?:\s*[、，,;；/]\s*[A-Z][A-Z0-9_]{1,24}){1,7})`)
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
		if !isActionableTaskMessageRole(messages[i].Role) {
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
		if !isActionableTaskMessageRole(messages[i].Role) {
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

func isActionableTaskMessageRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user", "developer":
		return true
	default:
		return false
	}
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
	trimmed := strings.TrimSpace(text)
	lower := strings.ToLower(trimmed)
	if lower == "" {
		return false
	}
	if strings.HasPrefix(lower, "tool result") || strings.HasPrefix(lower, "tool output") {
		return true
	}
	return strings.HasPrefix(trimmed, "<subagent_notification>")
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
	if isEnvironmentContextBlock(lower) {
		score -= 160
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
		"# agents.md instructions for",
		"<instructions>",
		"</instructions>",
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

func isEnvironmentContextBlock(lower string) bool {
	if lower == "" {
		return false
	}
	markers := []string{
		"<environment_context>",
		"</environment_context>",
		"<cwd>",
		"<current_date>",
		"<timezone>",
		"<subagents>",
	}
	matchCount := 0
	for _, marker := range markers {
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
	if containsAny(lower, taskToolKeywords) {
		return true
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

func extractExplicitToolMentions(task string, toolCatalog map[string]responseToolDescriptor) []string {
	task = strings.TrimSpace(task)
	if task == "" || len(toolCatalog) == 0 {
		return nil
	}

	type toolAlias struct {
		declared string
		alias    string
		strict   bool
	}
	type matchedTool struct {
		declared string
		index    int
		end      int
	}

	shortNameOwners := make(map[string]string)
	shortNameAmbiguous := make(map[string]struct{})
	for declared := range toolCatalog {
		short := shortToolName(declared)
		if short == "" || short == declared {
			continue
		}
		key := strings.ToLower(short)
		if owner, ok := shortNameOwners[key]; ok && owner != declared {
			shortNameAmbiguous[key] = struct{}{}
			continue
		}
		shortNameOwners[key] = declared
	}

	aliases := make([]toolAlias, 0, len(toolCatalog)*2)
	for declared := range toolCatalog {
		for _, alias := range toolNameAliasVariants(declared) {
			alias = strings.TrimSpace(alias)
			if alias == "" {
				continue
			}
			aliases = append(aliases, toolAlias{
				declared: declared,
				alias:    alias,
				strict:   strings.Contains(alias, ".") || strings.HasPrefix(alias, "mcp__"),
			})
		}

		short := shortToolName(declared)
		key := strings.ToLower(short)
		if short == "" || short == declared {
			continue
		}
		if _, ambiguous := shortNameAmbiguous[key]; ambiguous {
			continue
		}
		if owner := shortNameOwners[key]; owner != declared {
			continue
		}
		aliases = append(aliases, toolAlias{
			declared: declared,
			alias:    short,
			strict:   false,
		})
	}

	lowerTask := strings.ToLower(task)
	matches := make([]matchedTool, 0, len(aliases))
	for _, candidate := range aliases {
		alias := strings.TrimSpace(candidate.alias)
		if alias == "" {
			continue
		}
		aliasLower := strings.ToLower(alias)
		indices := []int(nil)
		if candidate.strict {
			searchFrom := 0
			for searchFrom <= len(lowerTask)-len(aliasLower) {
				index := strings.Index(lowerTask[searchFrom:], aliasLower)
				if index < 0 {
					break
				}
				index += searchFrom
				indices = append(indices, index)
				searchFrom = index + len(aliasLower)
			}
		} else {
			indices = findAllTaskKeywordIndices(lowerTask, aliasLower)
		}
		if len(indices) == 0 {
			continue
		}
		for _, index := range indices {
			if isNegatedToolMention(lowerTask, aliasLower, index) {
				continue
			}
			if isDescriptiveToolMention(lowerTask, aliasLower, index) {
				continue
			}
			matches = append(matches, matchedTool{
				declared: candidate.declared,
				index:    index,
				end:      index + len(aliasLower),
			})
		}
	}

	if len(matches) == 0 {
		return nil
	}

	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].index == matches[j].index {
			return matches[i].declared < matches[j].declared
		}
		return matches[i].index < matches[j].index
	})

	ordered := make([]string, 0, len(matches))
	lastRangeByTool := make(map[string][2]int, len(matches))
	for _, match := range matches {
		if lastRange, ok := lastRangeByTool[match.declared]; ok {
			if match.index <= lastRange[1] && match.end >= lastRange[0] {
				continue
			}
		}
		lastRangeByTool[match.declared] = [2]int{match.index, match.end}
		ordered = append(ordered, match.declared)
	}
	return ordered
}

func isNegatedToolMention(lowerTask, aliasLower string, index int) bool {
	if index < 0 || aliasLower == "" {
		return false
	}
	start := index - 64
	if start < 0 {
		start = 0
	}
	prefix := lowerTask[start:index]
	suffixStart := index + len(aliasLower)
	if suffixStart > len(lowerTask) {
		suffixStart = len(lowerTask)
	}
	suffixEnd := suffixStart + 64
	if suffixEnd > len(lowerTask) {
		suffixEnd = len(lowerTask)
	}
	suffix := lowerTask[suffixStart:suffixEnd]

	for _, marker := range []string{
		"禁止使用",
		"禁止调用",
		"不要使用",
		"不要调用",
		"不得使用",
		"不得调用",
		"不能使用",
		"不能调用",
		"勿使用",
		"勿调用",
		"禁用",
		"do not use",
		"do not call",
		"don't use",
		"don't call",
		"must not use",
		"must not call",
		"avoid using",
		"avoid calling",
		"rather than use",
	} {
		if strings.Contains(prefix, marker) {
			return true
		}
	}
	for _, marker := range []string{
		"代替",
		"替代",
		"instead of",
		"rather than",
	} {
		if strings.Contains(prefix, marker) || strings.Contains(suffix, marker) {
			if strings.Contains(prefix, "禁止") || strings.Contains(prefix, "不要") || strings.Contains(prefix, "不得") || strings.Contains(prefix, "不能") || strings.Contains(prefix, "勿") || strings.Contains(prefix, "禁用") || strings.Contains(prefix, "do not") || strings.Contains(prefix, "don't") || strings.Contains(prefix, "must not") || strings.Contains(prefix, "avoid") {
				return true
			}
		}
	}
	return false
}

func isDescriptiveToolMention(lowerTask, aliasLower string, index int) bool {
	if index < 0 || aliasLower == "" {
		return false
	}
	if isOutputLabelToolMention(lowerTask, aliasLower, index) {
		return true
	}

	start := index - 64
	if start < 0 {
		start = 0
	}
	prefix := lowerTask[start:index]

	suffixStart := index + len(aliasLower)
	if suffixStart > len(lowerTask) {
		suffixStart = len(lowerTask)
	}
	suffixEnd := suffixStart + 8
	if suffixEnd > len(lowerTask) {
		suffixEnd = len(lowerTask)
	}
	suffix := strings.TrimSpace(lowerTask[suffixStart:suffixEnd])
	if !strings.HasPrefix(suffix, ":") && !strings.HasPrefix(suffix, "：") {
		return false
	}
	for _, marker := range []string{
		"验证",
		"测试",
		"核验",
		"check",
		"verify",
		"probe",
	} {
		if strings.Contains(prefix, marker) {
			return true
		}
	}
	return false
}

func isOutputLabelToolMention(lowerTask, aliasLower string, index int) bool {
	if index < 0 || aliasLower == "" {
		return false
	}
	start := index - 32
	if start < 0 {
		start = 0
	}
	prefix := lowerTask[start:index]
	for _, marker := range []string{
		"result:",
		"files:",
		"test:",
		"note:",
		"constraint:",
		"evidence:",
	} {
		if strings.Contains(prefix, marker) {
			return true
		}
	}
	return false
}

func shortToolName(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return ""
	}
	if idx := strings.LastIndex(trimmed, "__"); strings.HasPrefix(trimmed, "mcp__") && idx >= len("mcp__") && idx+2 < len(trimmed) {
		return strings.TrimSpace(trimmed[idx+2:])
	}
	if idx := strings.LastIndex(trimmed, "."); idx >= 0 && idx+1 < len(trimmed) {
		return strings.TrimSpace(trimmed[idx+1:])
	}
	return strings.TrimSpace(path.Base(trimmed))
}

func buildExplicitToolUseBlock(task string, toolCatalog map[string]responseToolDescriptor) string {
	tools := extractExplicitToolMentions(task, toolCatalog)
	if len(tools) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n<EXPLICIT_TOOL_REQUIREMENTS>\n")
	b.WriteString("CURRENT_USER_TASK explicitly requires these tool names. Use them exactly as declared:\n")
	for _, name := range tools {
		b.WriteString("- ")
		b.WriteString(name)
		b.WriteByte('\n')
	}
	b.WriteString("Do not describe intended tool use in prose before the tool call.\n")
	b.WriteString("Narration like \"I'll use web_search\" without a real tool call is invalid.\n")
	b.WriteString("If the next required tool has not been called yet, emit AI_ACTIONS mode tool immediately with no visible text before the block.\n")
	b.WriteString("Do not emit mode final until the explicit tool requirement is satisfied or a tool error blocks progress.\n")
	b.WriteString("</EXPLICIT_TOOL_REQUIREMENTS>\n")
	return b.String()
}

func extractRequiredToolNames(task string) []string {
	candidateTools := []string{
		"exec_command",
		"update_plan",
		"write_stdin",
		"js_repl",
		"js_repl_reset",
		"web_search",
		"view_image",
		"spawn_agent",
		"send_input",
		"resume_agent",
		"wait_agent",
		"close_agent",
		"list_mcp_resources",
		"list_mcp_resource_templates",
		"read_mcp_resource",
		"mcp__docfork__search_docs",
		"mcp__docfork__fetch_doc",
		"mcp__cloudflare_api__search",
		"mcp__cloudflare_api__execute",
		"mcp__chrome_devtools__new_page",
		"mcp__chrome_devtools__take_snapshot",
		"mcp__chrome_devtools__click",
		"mcp__chrome_devtools__wait_for",
	}
	toolCatalog := make(map[string]responseToolDescriptor, len(candidateTools))
	for _, name := range candidateTools {
		toolCatalog[name] = responseToolDescriptor{Name: name}
	}
	return extractExplicitToolMentions(task, toolCatalog)
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
		writeIdx := firstKeywordIndex(lower, taskWriteKeywords)
		if writeIdx < 0 {
			continue
		}
		targets = append(targets, taskFilePathPattern.FindAllString(segment[writeIdx:], -1)...)
	}
	return dedupePreserveOrder(targets)
}

func firstKeywordIndex(text string, keywords []string) int {
	best := -1
	for _, keyword := range keywords {
		if keyword == "" {
			continue
		}
		idx := findTaskKeywordIndex(text, strings.ToLower(keyword))
		if idx < 0 {
			continue
		}
		if best < 0 || idx < best {
			best = idx
		}
	}
	return best
}

func findTaskKeywordIndex(text, keyword string) int {
	if text == "" || keyword == "" {
		return -1
	}
	if !isASCIIAlphaNumericKeyword(keyword) {
		return strings.Index(text, keyword)
	}
	searchFrom := 0
	for searchFrom <= len(text)-len(keyword) {
		idx := strings.Index(text[searchFrom:], keyword)
		if idx < 0 {
			return -1
		}
		idx += searchFrom
		beforeOK := idx == 0 || !isASCIIAlphaNumericUnderscore(text[idx-1])
		afterPos := idx + len(keyword)
		afterOK := afterPos >= len(text) || !isASCIIAlphaNumericUnderscore(text[afterPos])
		if beforeOK && afterOK {
			return idx
		}
		searchFrom = idx + len(keyword)
	}
	return -1
}

func findAllTaskKeywordIndices(text, keyword string) []int {
	if text == "" || keyword == "" {
		return nil
	}
	indices := make([]int, 0, 2)
	searchFrom := 0
	for searchFrom <= len(text)-len(keyword) {
		idx := findTaskKeywordIndex(text[searchFrom:], keyword)
		if idx < 0 {
			break
		}
		idx += searchFrom
		indices = append(indices, idx)
		searchFrom = idx + len(keyword)
	}
	return indices
}

func isASCIIAlphaNumericKeyword(keyword string) bool {
	for i := 0; i < len(keyword); i++ {
		c := keyword[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' {
			continue
		}
		return false
	}
	return true
}

func isASCIIAlphaNumericUnderscore(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_'
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
		if punct := strings.IndexAny(task[start:end], "\n。；;，"); punct >= 0 {
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
	if !strings.Contains(lower, "只输出") &&
		!strings.Contains(lower, "仅输出") &&
		!strings.Contains(lower, "最终输出") &&
		!strings.Contains(lower, "最后输出") &&
		!strings.Contains(lower, "only output") &&
		!strings.Contains(lower, "output only") {
		return nil
	}
	if labels := extractPlainRequiredOutputLabels(task); len(labels) > 0 {
		return labels
	}
	section := requiredOutputSection(task)
	matches := taskOutputLabelPattern.FindAllStringSubmatch(section, -1)
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
	return dedupePreserveOrder(labels)
}

func extractRequiredOutputLabelHints(task string) map[string]string {
	labels := extractRequiredOutputLabels(task)
	if len(labels) == 0 {
		return nil
	}
	section := requiredOutputSection(task)
	matches := taskOutputLabelPattern.FindAllStringSubmatchIndex(section, -1)
	if len(matches) == 0 {
		return nil
	}
	allowed := make(map[string]string, len(labels))
	for _, label := range labels {
		allowed[strings.ToUpper(strings.TrimSpace(label))] = label
	}
	hints := make(map[string]string, len(labels))
	for i, match := range matches {
		if len(match) < 4 {
			continue
		}
		rawLabel := strings.ToUpper(strings.TrimSpace(section[match[2]:match[3]]))
		label, ok := allowed[rawLabel]
		if !ok {
			continue
		}
		valueStart := match[1]
		valueEnd := len(section)
		for j := i + 1; j < len(matches); j++ {
			if len(matches[j]) < 4 {
				continue
			}
			nextLabel := strings.ToUpper(strings.TrimSpace(section[matches[j][2]:matches[j][3]]))
			if _, ok := allowed[nextLabel]; !ok {
				continue
			}
			valueEnd = matches[j][0]
			break
		}
		value := strings.TrimSpace(section[valueStart:valueEnd])
		value = strings.Trim(value, " \t\r\n;；")
		if value == "" {
			continue
		}
		hints[label] = value
	}
	if len(hints) == 0 {
		return nil
	}
	return hints
}

func extractRequiredOutputLabelHint(task, label string) string {
	hints := extractRequiredOutputLabelHints(task)
	if len(hints) == 0 {
		return ""
	}
	return strings.TrimSpace(hints[label])
}

func requiredOutputSection(task string) string {
	trimmed := strings.TrimSpace(task)
	if trimmed == "" {
		return ""
	}
	lower := strings.ToLower(trimmed)
	best := -1
	for _, marker := range []string{
		"最终只输出",
		"完成后只输出",
		"最后只输出",
		"只输出",
		"最终仅输出",
		"完成后仅输出",
		"仅输出",
		"最终输出",
		"最后输出",
		"only output",
		"output only",
	} {
		if idx := strings.LastIndex(lower, marker); idx > best {
			best = idx
		}
	}
	if best < 0 || best >= len(trimmed) {
		return trimmed
	}
	return trimmed[best:]
}

func extractPlainRequiredOutputLabels(task string) []string {
	match := taskPlainOutputLabelListPattern.FindStringSubmatch(task)
	if len(match) < 2 {
		return nil
	}
	fields := strings.FieldsFunc(strings.TrimSpace(match[1]), func(r rune) bool {
		switch r {
		case '、', '，', ',', ';', '；', '/':
			return true
		default:
			return false
		}
	})
	labels := make([]string, 0, len(fields))
	for _, field := range fields {
		label := strings.TrimSpace(field)
		if label != "" {
			labels = append(labels, label)
		}
	}
	return dedupePreserveOrder(labels)
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
