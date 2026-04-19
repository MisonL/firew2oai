package proxy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestBuildExecutionPolicy_StrictLoopModelEnablesSingleStep(t *testing.T) {
	task := "修改 internal/proxy/responses.go 并运行 `go test ./internal/proxy`"
	policy := buildExecutionPolicy("minimax-m2p5", task, nil, true, false, true)

	if !policy.Enabled {
		t.Fatal("policy.Enabled = false, want true")
	}
	if !policy.RequireTool {
		t.Fatal("policy.RequireTool = false, want true")
	}
	if !policy.ForceSingleToolCall {
		t.Fatal("policy.ForceSingleToolCall = false, want true")
	}
	if !policy.AllowTruncateToMax {
		t.Fatal("policy.AllowTruncateToMax = false, want true")
	}
	if policy.NextCommand != "go test ./internal/proxy" {
		t.Fatalf("policy.NextCommand = %q, want go test ./internal/proxy", policy.NextCommand)
	}
}

func TestApplyExecutionPolicyToParseResult_SynthesizesCommandOnFinalMode(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	content := "这是最终文本。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"final\"}\n<<<END_AI_ACTIONS_V1>>>"
	constraints := toolProtocolConstraints{
		RequireTool: true,
	}
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, constraints)
	policy := executionPolicy{
		Enabled:     true,
		RequireTool: true,
		Stage:       "execute",
		NextCommand: "go test ./internal/proxy",
	}

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, constraints)
	if got.err != nil {
		t.Fatalf("got.err = %v, want nil", got.err)
	}
	if len(got.calls) != 1 {
		t.Fatalf("len(got.calls) = %d, want 1", len(got.calls))
	}
	command := parsedCallCommand(t, got.calls[0])
	if command != "go test ./internal/proxy" {
		t.Fatalf("synthetic command = %q, want go test ./internal/proxy", command)
	}
}

func TestApplyExecutionPolicyToParseResult_ReadLoopRewritesReadOnlyCall(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	content := "先看目录。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"pwd\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	constraints := toolProtocolConstraints{}
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, constraints)
	policy := executionPolicy{
		Enabled:     true,
		RequireTool: true,
		ReadLoop:    true,
		Stage:       "execute",
		NextCommand: "go test ./internal/proxy",
	}

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, constraints)
	if got.err != nil {
		t.Fatalf("got.err = %v, want nil", got.err)
	}
	if len(got.calls) != 1 {
		t.Fatalf("len(got.calls) = %d, want 1", len(got.calls))
	}
	command := parsedCallCommand(t, got.calls[0])
	if command != "go test ./internal/proxy" {
		t.Fatalf("rewritten command = %q, want go test ./internal/proxy", command)
	}
}

func TestApplyExecutionPolicyToParseResult_RewritesReadOnlyWhenMissingRequiredStep(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	content := "先列目录。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"ls -la\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	policy := executionPolicy{
		Enabled:     true,
		RequireTool: true,
		Stage:       "execute",
		NextCommand: "sed -n '170,260p' internal/proxy/tool_protocol.go",
	}

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{})
	if got.err != nil {
		t.Fatalf("got.err = %v, want nil", got.err)
	}
	if len(got.calls) != 1 {
		t.Fatalf("len(got.calls) = %d, want 1", len(got.calls))
	}
	command := parsedCallCommand(t, got.calls[0])
	if command != "sed -n '170,260p' internal/proxy/tool_protocol.go" {
		t.Fatalf("rewritten command = %q", command)
	}
}

func TestApplyExecutionPolicyToParseResult_DoesNotRewriteWhenAlreadyOnNextCommand(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	content := "按要求读取。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"sed -n '170,260p' internal/proxy/tool_protocol.go\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	policy := executionPolicy{
		Enabled:     true,
		RequireTool: true,
		Stage:       "execute",
		NextCommand: "sed -n '170,260p' internal/proxy/tool_protocol.go",
	}

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{})
	if got.err != nil {
		t.Fatalf("got.err = %v, want nil", got.err)
	}
	if len(got.calls) != 1 {
		t.Fatalf("len(got.calls) = %d, want 1", len(got.calls))
	}
	command := parsedCallCommand(t, got.calls[0])
	if command != "sed -n '170,260p' internal/proxy/tool_protocol.go" {
		t.Fatalf("command = %q, want unchanged next command", command)
	}
}

func TestApplyExecutionPolicyToParseResult_ExplicitCommandsDoNotRewriteModelCall(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	content := "按要求先读 README。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"head -n 5 README.md\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	policy := executionPolicy{
		Enabled:          true,
		RequireTool:      true,
		ReadLoop:         false,
		Stage:            "verify",
		NextCommand:      "go test ./internal/proxy",
		RequiredCommands: []string{"head -n 5 README.md", "go test ./internal/proxy"},
	}

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{})
	if got.err != nil {
		t.Fatalf("got.err = %v, want nil", got.err)
	}
	if len(got.calls) != 1 {
		t.Fatalf("len(got.calls) = %d, want 1", len(got.calls))
	}
	command := parsedCallCommand(t, got.calls[0])
	if command != "head -n 5 README.md" {
		t.Fatalf("command = %q, want keep model emitted explicit command", command)
	}
}

func TestApplyExecutionPolicyToParseResult_ExplicitCommandsRewriteOnReadLoop(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	content := "重复读取。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"head -n 5 README.md\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	policy := executionPolicy{
		Enabled:          true,
		RequireTool:      true,
		ReadLoop:         true,
		Stage:            "verify",
		NextCommand:      "sed -n '170,260p' internal/proxy/tool_protocol.go",
		RequiredCommands: []string{"head -n 5 README.md", "sed -n '170,260p' internal/proxy/tool_protocol.go", "go test ./internal/proxy"},
	}

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{})
	if got.err != nil {
		t.Fatalf("got.err = %v, want nil", got.err)
	}
	if len(got.calls) != 1 {
		t.Fatalf("len(got.calls) = %d, want 1", len(got.calls))
	}
	command := parsedCallCommand(t, got.calls[0])
	if command != "sed -n '170,260p' internal/proxy/tool_protocol.go" {
		t.Fatalf("command = %q, want rewritten to next required command", command)
	}
}

func TestApplyExecutionPolicyToParseResult_ExplicitCommandsRewriteRepeatedReadCommand(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	content := "重复读取。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"head -n 5 README.md\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	policy := executionPolicy{
		Enabled:          true,
		RequireTool:      true,
		ReadLoop:         false,
		Stage:            "verify",
		NextCommand:      "sed -n '170,260p' internal/proxy/tool_protocol.go",
		RequiredCommands: []string{"head -n 5 README.md", "sed -n '170,260p' internal/proxy/tool_protocol.go", "go test ./internal/proxy"},
		SeenCommands:     []string{"head -n 5 README.md"},
	}

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{})
	if got.err != nil {
		t.Fatalf("got.err = %v, want nil", got.err)
	}
	if len(got.calls) != 1 {
		t.Fatalf("len(got.calls) = %d, want 1", len(got.calls))
	}
	command := parsedCallCommand(t, got.calls[0])
	if command != "sed -n '170,260p' internal/proxy/tool_protocol.go" {
		t.Fatalf("command = %q, want rewritten to next required command after repeated read", command)
	}
}

func TestBuildParsedToolCall_SanitizesLeakedPromptInExecCommand(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	raw := map[string]any{
		"type": "function_call",
		"name": "exec_command",
		"arguments": map[string]any{
			"cmd": "go test ./internal/proxy\n\n完成后只输出三行，不要有任何其他文字：\nRESULT: <PASS 或 FAIL>",
		},
	}

	call, err := buildParsedToolCall(raw, toolCatalog, "", false)
	if err != nil {
		t.Fatalf("buildParsedToolCall error = %v", err)
	}
	command := parsedCallCommand(t, *call)
	if command != "go test ./internal/proxy" {
		t.Fatalf("command = %q, want sanitized go test command", command)
	}
}

func TestExtractExecCommandFromArgumentsText_SanitizesLeakedPromptInCommand(t *testing.T) {
	argsText := `{"cmd":"go test ./internal/proxy\n\n完成后只输出三行，不要有任何其他文字：\nRESULT: <PASS 或 FAIL>"}`
	got := extractExecCommandFromArgumentsText(argsText)
	if got != "go test ./internal/proxy" {
		t.Fatalf("extractExecCommandFromArgumentsText = %q, want sanitized go test command", got)
	}
}

func TestExtractRequiredCommands_IncludesReadCommands(t *testing.T) {
	task := "严格按顺序执行：\n" +
		"1) `head -n 5 README.md`\n" +
		"2) sed -n '170,260p' internal/proxy/tool_protocol.go\n" +
		"3) go test ./internal/proxy"

	got := dedupePreserveOrder(extractRequiredCommands(task))
	want := []string{
		"head -n 5 README.md",
		"sed -n '170,260p' internal/proxy/tool_protocol.go",
		"go test ./internal/proxy",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("required commands mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestExtractRequiredCommands_BacktickSedCommandInChineseSentence(t *testing.T) {
	task := "你在一个真实 Go 项目里做一次只读审计任务。严格执行：\n" +
		"1) 执行 `head -n 5 README.md`\n" +
		"2) 执行 `sed -n '170,260p' internal/proxy/tool_protocol.go`\n" +
		"3) 执行 `go test ./internal/proxy`\n"

	got := dedupePreserveOrder(extractRequiredCommands(task))
	want := []string{
		"head -n 5 README.md",
		"sed -n '170,260p' internal/proxy/tool_protocol.go",
		"go test ./internal/proxy",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("required commands mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestExtractRequiredCommands_ChineseColonCommandWithoutBackticks(t *testing.T) {
	task := "请严格完成以下任务：1) 执行命令：head -n 5 README.md 2) 执行命令：sed -n '170,260p' internal/proxy/tool_protocol.go 3) 执行命令：go test ./internal/proxy"

	got := dedupePreserveOrder(extractRequiredCommands(task))
	want := []string{
		"head -n 5 README.md",
		"sed -n '170,260p' internal/proxy/tool_protocol.go",
		"go test ./internal/proxy",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("required commands mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestChooseNextExecutionCommand_PrioritizesUnmetCommand(t *testing.T) {
	requiredCommands := []string{
		"head -n 5 README.md",
		"go test ./internal/proxy",
	}
	signals := executionHistorySignals{
		Commands: []string{"head -n 5 README.md"},
	}

	got := chooseNextExecutionCommand(requiredCommands, nil, signals, false)
	if got != "go test ./internal/proxy" {
		t.Fatalf("next command = %q, want go test ./internal/proxy", got)
	}
}

func TestChooseNextExecutionCommand_ReadLoopFocusesRequiredFile(t *testing.T) {
	requiredFile := "internal/proxy/tool_protocol.go"
	signals := executionHistorySignals{
		ReadCalls:  4,
		WriteCalls: 0,
		Commands: []string{
			"ls -la",
			"pwd",
			"ls -la",
		},
	}

	got := chooseNextExecutionCommand(nil, []string{requiredFile}, signals, false)
	want := buildReadFileCommand(requiredFile)
	if got != want {
		t.Fatalf("next command = %q, want %q", got, want)
	}
}

func TestChooseNextExecutionCommand_NoPendingWorkReturnsEmpty(t *testing.T) {
	signals := executionHistorySignals{
		TestCalls:  1,
		WriteCalls: 1,
		Commands:   []string{"go test ./internal/proxy"},
	}

	got := chooseNextExecutionCommand(nil, nil, signals, false)
	if got != "" {
		t.Fatalf("next command = %q, want empty", got)
	}
}

func TestChooseNextExecutionCommand_ExplicitCommandsDoneNoReadFallback(t *testing.T) {
	requiredCommands := []string{
		"head -n 5 README.md",
		"sed -n '170,260p' internal/proxy/tool_protocol.go",
		"go test ./internal/proxy",
	}
	signals := executionHistorySignals{
		Commands: []string{
			"head -n 5 README.md",
			"sed -n '170,260p' internal/proxy/tool_protocol.go",
			"go test ./internal/proxy",
		},
		ReadCalls:  2,
		TestCalls:  1,
		WriteCalls: 0,
	}

	got := chooseNextExecutionCommand(requiredCommands, []string{"internal/proxy/tool_protocol.go"}, signals, false)
	if got != "" {
		t.Fatalf("next command = %q, want empty when explicit required commands are already done", got)
	}
}

func TestChooseNextExecutionCommand_UsesSuccessfulCommandsWhenPresent(t *testing.T) {
	requiredCommands := []string{
		"head -n 5 README.md",
		"sed -n '170,260p' internal/proxy/tool_protocol.go",
		"go test ./internal/proxy",
	}
	signals := executionHistorySignals{
		Commands: []string{
			"head -n 5 README.md",
			"sed -n '170,260p' internal/proxy/tool_protocol.go",
			"go test ./internal/proxy",
		},
		SuccessfulCommands: []string{
			"head -n 5 README.md",
			"sed -n '170,260p' internal/proxy/tool_protocol.go",
		},
		FailedCommands: []string{
			"go test ./internal/proxy",
		},
	}

	got := chooseNextExecutionCommand(requiredCommands, nil, signals, false)
	if got != "go test ./internal/proxy" {
		t.Fatalf("next command = %q, want go test ./internal/proxy when only failed run exists", got)
	}
}

func TestLatestActionableUserTask_SkipsToolResultSummary(t *testing.T) {
	messages := []ChatMessage{
		{Role: "user", Content: "1) 执行 `head -n 5 README.md`\n2) 执行 `sed -n '170,260p' internal/proxy/tool_protocol.go`"},
		{Role: "assistant", Content: "Assistant requested tool: exec_command"},
		{Role: "user", Content: "Tool result (call_id=abc)\nSuccess: true\nOutput:\n# firew2oai"},
	}

	got := latestActionableUserTask(messages)
	want := "1) 执行 `head -n 5 README.md`\n2) 执行 `sed -n '170,260p' internal/proxy/tool_protocol.go`"
	if got != want {
		t.Fatalf("latest actionable task mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestBuildExecutionPolicy_ReadOnlyTaskKeepsNextRequiredCommand(t *testing.T) {
	task := "只读审计：\n1) 执行 `head -n 5 README.md`\n2) 执行 `sed -n '170,260p' internal/proxy/tool_protocol.go`\n3) 执行 `go test ./internal/proxy`"
	history := historyExecCommandItems("head -n 5 README.md")

	policy := buildExecutionPolicy("deepseek-v3p1", task, history, true, false, true)
	if !policy.Enabled || !policy.RequireTool {
		t.Fatalf("policy should require tool for pending read-only steps: %+v", policy)
	}
	if policy.NextCommand != "sed -n '170,260p' internal/proxy/tool_protocol.go" {
		t.Fatalf("next command = %q", policy.NextCommand)
	}
}

func TestBuildExecutionPolicy_ReadOnlyTaskFinalizesAfterAllRequiredCommands(t *testing.T) {
	task := "只读审计：\n1) 执行 `head -n 5 README.md`\n2) 执行 `sed -n '170,260p' internal/proxy/tool_protocol.go`\n3) 执行 `go test ./internal/proxy`"
	history := historyExecCommandItems(
		"head -n 5 README.md",
		"sed -n '170,260p' internal/proxy/tool_protocol.go",
		"go test ./internal/proxy",
	)

	policy := buildExecutionPolicy("deepseek-v3p1", task, history, true, false, true)
	if policy.RequireTool {
		t.Fatalf("read-only task should not force tool after required commands are done: %+v", policy)
	}
	if policy.Stage != "finalize" {
		t.Fatalf("stage = %q, want finalize", policy.Stage)
	}
	if policy.NextCommand != "" {
		t.Fatalf("next command = %q, want empty", policy.NextCommand)
	}
}

func TestBuildExecutionPolicyPromptBlock_FinalizeIncludesNoToolGuidance(t *testing.T) {
	policy := executionPolicy{
		Enabled: true,
		Stage:   "finalize",
	}

	block := buildExecutionPolicyPromptBlock(policy)
	if !strings.Contains(block, "Stage finalize reached.") {
		t.Fatalf("finalize policy block missing finalize guidance:\n%s", block)
	}
	if !strings.Contains(block, "Do not emit AI_ACTIONS mode tool.") {
		t.Fatalf("finalize policy block missing no-tool instruction:\n%s", block)
	}
}

func historyExecCommandItems(commands ...string) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(commands))
	for _, command := range commands {
		args, _ := json.Marshal(map[string]any{"cmd": command})
		item, _ := json.Marshal(map[string]any{
			"type":      "function_call",
			"name":      "exec_command",
			"arguments": string(args),
		})
		out = append(out, item)
	}
	return out
}

func parsedCallCommand(t *testing.T, call parsedToolCall) string {
	t.Helper()

	var item map[string]any
	if err := json.Unmarshal(call.item, &item); err != nil {
		t.Fatalf("unmarshal parsed call: %v", err)
	}
	argsText, _ := item["arguments"].(string)
	if argsText == "" {
		t.Fatalf("missing arguments in parsed call item: %s", string(call.item))
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(argsText), &args); err != nil {
		t.Fatalf("unmarshal arguments JSON: %v", err)
	}
	cmd, _ := args["cmd"].(string)
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		t.Fatalf("missing cmd in parsed call arguments: %s", argsText)
	}
	return cmd
}
