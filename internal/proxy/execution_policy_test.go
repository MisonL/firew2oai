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
	want := buildReadFileCommand("internal/proxy/responses.go")
	if policy.NextCommand != want {
		t.Fatalf("policy.NextCommand = %q, want %q", policy.NextCommand, want)
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

func TestApplyExecutionPolicyToParseResult_ExplicitCommandsRewriteEarlierStepAfterProgressedState(t *testing.T) {
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
	if command != "go test ./internal/proxy" {
		t.Fatalf("command = %q, want rewritten to next required command", command)
	}
}

func TestApplyExecutionPolicyToParseResult_ExplicitCommandsRewriteExplorationCallImmediately(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	content := "先看看目录。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"pwd && ls -la\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	policy := executionPolicy{
		Enabled:          true,
		RequireTool:      true,
		ReadLoop:         false,
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

func TestApplyExecutionPolicyToParseResult_ExplicitCommandsRewriteOutOfOrderExecCommand(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	content := "先跑测试。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"go test ./internal/proxy\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	policy := executionPolicy{
		Enabled:          true,
		RequireTool:      true,
		Stage:            "verify",
		NextCommand:      "head -n 5 README.md",
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
	if command != "head -n 5 README.md" {
		t.Fatalf("command = %q, want rewritten to next required command", command)
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

func TestExtractRequiredCommands_StripsTrailingOutputInstructions(t *testing.T) {
	task := "你是资深 Go 工程师。请执行一个真实编码排障任务（只读分析，不修改文件）：\n" +
		"1) 执行命令：sed -n '1,220p' internal/proxy/output_constraints.go\n" +
		"2) 执行命令：sed -n '1,220p' internal/proxy/execution_evidence.go\n" +
		"3) 执行命令：go test ./internal/proxy -run 'TestHandleResponses_StreamAIActionsBlock_(RequiredToolRejectsFinalMode|ParallelToolCallsFalseRejectsMultipleCalls)'\n" +
		"4) 执行命令：go test ./...\n\n" +
		"完成后只输出四行，不要有任何额外内容：\n" +
		"RESULT: <PASS 或 FAIL>\n" +
		"CONSTRAINT: <一句话说明 output_constraints 这一层的核心职责>\n" +
		"EVIDENCE: <一句话说明 execution_evidence 这一层的核心职责>\n" +
		"TEST: <一句话给出测试是否通过与关键结果>\n"

	got := dedupePreserveOrder(extractRequiredCommands(task))
	want := []string{
		"sed -n '1,220p' internal/proxy/output_constraints.go",
		"sed -n '1,220p' internal/proxy/execution_evidence.go",
		"go test ./internal/proxy -run 'TestHandleResponses_StreamAIActionsBlock_(RequiredToolRejectsFinalMode|ParallelToolCallsFalseRejectsMultipleCalls)'",
		"go test ./...",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("required commands mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestExtractRequiredCommands_StripsInlineOutputDirectiveOnSameLine(t *testing.T) {
	task := "只读审计任务：1) 执行 head -n 5 README.md 2) 执行 sed -n '170,260p' internal/proxy/tool_protocol.go 3) 执行 go test ./internal/proxy 最终只输出三行。"

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

func TestChooseNextExecutionCommand_TargetedTestDoesNotSatisfyFullPackageTest(t *testing.T) {
	requiredCommands := []string{
		"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'",
		"go test ./internal/proxy",
	}
	signals := executionHistorySignals{
		SuccessfulCommands: []string{
			"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'",
		},
	}

	got := chooseNextExecutionCommand(requiredCommands, nil, signals, false)
	if got != "go test ./internal/proxy" {
		t.Fatalf("next command = %q, want go test ./internal/proxy", got)
	}
}

func TestChooseNextExecutionCommand_WriteTaskReadsTargetsBeforeTests(t *testing.T) {
	requiredCommands := []string{
		"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'",
		"go test ./internal/proxy",
	}
	requiredFiles := []string{
		"internal/proxy/output_constraints.go",
		"internal/proxy/output_constraints_test.go",
	}

	got := chooseNextExecutionCommand(requiredCommands, requiredFiles, executionHistorySignals{}, true)
	want := buildReadFileCommand("internal/proxy/output_constraints.go")
	if got != want {
		t.Fatalf("next command = %q, want %q", got, want)
	}
}

func TestChooseNextExecutionCommand_WriteTaskFallsBackToTargetFileAfterCommands(t *testing.T) {
	requiredCommands := []string{
		"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'",
		"go test ./internal/proxy",
	}
	requiredFiles := []string{
		"internal/proxy/output_constraints.go",
		"internal/proxy/output_constraints_test.go",
	}
	signals := executionHistorySignals{
		SuccessfulCommands: []string{
			"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'",
			"go test ./internal/proxy",
			"sed -n '1,200p' 'internal/proxy/output_constraints.go'",
		},
		Commands: []string{
			"sed -n '1,200p' 'internal/proxy/output_constraints.go'",
		},
	}

	got := chooseNextExecutionCommand(requiredCommands, requiredFiles, signals, true)
	want := buildReadFileCommand("internal/proxy/output_constraints_test.go")
	if got != want {
		t.Fatalf("next command = %q, want %q", got, want)
	}
}

func TestChooseNextExecutionCommand_WriteTaskCreatesMissingFileAfterFailedRead(t *testing.T) {
	requiredCommands := []string{
		"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'",
		"go test ./internal/proxy",
	}
	requiredFiles := []string{
		"internal/proxy/output_constraints.go",
		"internal/proxy/output_constraints_test.go",
	}
	signals := executionHistorySignals{
		Commands: []string{
			"sed -n '1,200p' 'internal/proxy/output_constraints.go'",
			"sed -n '1,200p' 'internal/proxy/output_constraints_test.go'",
		},
		SuccessfulCommands: []string{
			"sed -n '1,200p' 'internal/proxy/output_constraints.go'",
		},
		FailedCommands: []string{
			"sed -n '1,200p' 'internal/proxy/output_constraints_test.go'",
		},
	}

	got := chooseNextExecutionCommand(requiredCommands, requiredFiles, signals, true)
	want := buildCreateMissingFileCommand("internal/proxy/output_constraints_test.go")
	if got != want {
		t.Fatalf("next command = %q, want %q", got, want)
	}
}

func TestChooseNextExecutionCommand_WriteTaskPrioritizesMissingTargetBeforeReReadingEarlierReference(t *testing.T) {
	requiredFiles := []string{
		"internal/proxy/output_constraints.go",
		"internal/proxy/output_constraints_test.go",
	}
	signals := executionHistorySignals{
		Commands: []string{
			"sed -n '1,200p' 'internal/proxy/output_constraints.go'",
			"sed -n '1,200p' 'internal/proxy/output_constraints_test.go'",
		},
		SuccessfulCommands: []string{
			"pwd",
		},
		FailedCommands: []string{
			"sed -n '1,200p' 'internal/proxy/output_constraints_test.go'",
		},
	}

	got := chooseNextExecutionCommand(nil, requiredFiles, signals, true)
	want := buildCreateMissingFileCommand("internal/proxy/output_constraints_test.go")
	if got != want {
		t.Fatalf("next command = %q, want %q", got, want)
	}
}

func TestCollectExecutionHistorySignals_InfersFailedReadFromToolOutputText(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"cmd": "sed -n '1,200p' 'internal/proxy/output_constraints_test.go'"})
	history := []json.RawMessage{
		mustMarshalRawJSON(map[string]any{
			"type":      "function_call",
			"name":      "exec_command",
			"call_id":   "call_1",
			"arguments": string(args),
		}),
		mustMarshalRawJSON(map[string]any{
			"type":    "function_call_output",
			"call_id": "call_1",
			"output":  "sed: internal/proxy/output_constraints_test.go: No such file or directory\n",
		}),
	}

	signals := collectExecutionHistorySignals(history)
	want := "sed -n '1,200p' 'internal/proxy/output_constraints_test.go'"
	if !reflect.DeepEqual(signals.FailedCommands, []string{want}) {
		t.Fatalf("FailedCommands = %#v, want %#v", signals.FailedCommands, []string{want})
	}
	if len(signals.SuccessfulCommands) != 0 {
		t.Fatalf("SuccessfulCommands = %#v, want empty", signals.SuccessfulCommands)
	}
}

func TestInferToolOutputSuccess_RecognizesMissingFileError(t *testing.T) {
	got := inferToolOutputSuccess("sed: internal/proxy/output_constraints_test.go: No such file or directory")
	if got == nil || *got {
		t.Fatalf("inferToolOutputSuccess should return false for missing file error, got %#v", got)
	}
}

func TestInferTestCommandOutputSuccess_RecognizesSingleLineGoTestOK(t *testing.T) {
	got := inferTestCommandOutputSuccess("ok\tgithub.com/mison/firew2oai/internal/proxy\t0.321s")
	if got == nil || !*got {
		t.Fatalf("inferTestCommandOutputSuccess should return true for single-line go test ok, got %#v", got)
	}
}

func TestInferTestCommandOutputSuccess_DoesNotAssumeSuccessForPartialMultiLineOutput(t *testing.T) {
	got := inferTestCommandOutputSuccess("ok\tgithub.com/mison/firew2oai/internal/config\t(cached)\nok\tgithub.com/mison/firew2oai/internal/tokenauth\t(cached)")
	if got != nil {
		t.Fatalf("inferTestCommandOutputSuccess should return nil for partial multi-line output, got %#v", got)
	}
}

func TestCollectExecutionHistorySignals_DoesNotMarkPartialTestOutputAsSuccessful(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"cmd": "go test ./..."})
	history := []json.RawMessage{
		mustMarshalRawJSON(map[string]any{
			"type":      "function_call",
			"name":      "exec_command",
			"call_id":   "call_1",
			"arguments": string(args),
		}),
		mustMarshalRawJSON(map[string]any{
			"type":    "function_call_output",
			"call_id": "call_1",
			"output":  "ok\tgithub.com/mison/firew2oai/internal/config\t(cached)\nok\tgithub.com/mison/firew2oai/internal/tokenauth\t(cached)\n",
		}),
	}

	signals := collectExecutionHistorySignals(history)
	if len(signals.SuccessfulCommands) != 0 {
		t.Fatalf("SuccessfulCommands = %#v, want empty for partial test output", signals.SuccessfulCommands)
	}
	if !reflect.DeepEqual(signals.CommandsWithResult, []string{"go test ./..."}) {
		t.Fatalf("CommandsWithResult = %#v, want recorded test command", signals.CommandsWithResult)
	}
}

func TestCollectExecutionHistorySignals_ScaffoldCreateDoesNotCountAsWrite(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"cmd": "mkdir -p -- 'internal/proxy' && touch 'internal/proxy/output_constraints_test.go'"})
	history := []json.RawMessage{
		mustMarshalRawJSON(map[string]any{
			"type":      "function_call",
			"name":      "exec_command",
			"call_id":   "call_1",
			"arguments": string(args),
		}),
	}

	signals := collectExecutionHistorySignals(history)
	if signals.WriteCalls != 0 {
		t.Fatalf("WriteCalls = %d, want 0 for scaffold create", signals.WriteCalls)
	}
}

func TestCollectExecutionHistorySignals_GuardFailureDoesNotCountAsWrite(t *testing.T) {
	command := buildExecFailureCommand("Codex adapter guard: pending write stage already inspected required context")
	args, _ := json.Marshal(map[string]any{"cmd": command})
	history := []json.RawMessage{
		mustMarshalRawJSON(map[string]any{
			"type":      "function_call",
			"name":      "exec_command",
			"call_id":   "call_1",
			"arguments": string(args),
		}),
		mustMarshalRawJSON(map[string]any{
			"type":    "function_call_output",
			"call_id": "call_1",
			"output":  "Codex adapter guard: pending write stage already inspected required context\n",
		}),
	}

	signals := collectExecutionHistorySignals(history)
	if signals.WriteCalls != 0 {
		t.Fatalf("WriteCalls = %d, want 0 for guard failure command", signals.WriteCalls)
	}
	if !reflect.DeepEqual(signals.FailedCommands, []string{command}) {
		t.Fatalf("FailedCommands = %#v, want guard failure command", signals.FailedCommands)
	}
}

func TestCollectExecutionHistorySignals_SeedWriteCommandCountsAsWrite(t *testing.T) {
	command := "python3 -c 'from pathlib import Path; Path(\"internal/proxy/output_constraints_test.go\").write_text(\"package proxy\\n\", encoding='\"'\"'utf-8'\"'\"')'"
	args, _ := json.Marshal(map[string]any{"cmd": command})
	history := []json.RawMessage{
		mustMarshalRawJSON(map[string]any{
			"type":      "function_call",
			"name":      "exec_command",
			"call_id":   "call_1",
			"arguments": string(args),
		}),
		mustMarshalRawJSON(map[string]any{
			"type":    "function_call_output",
			"call_id": "call_1",
			"output":  "",
		}),
	}

	signals := collectExecutionHistorySignals(history)
	if signals.WriteCalls != 1 {
		t.Fatalf("WriteCalls = %d, want 1 for seed write command", signals.WriteCalls)
	}
	if !reflect.DeepEqual(signals.SuccessfulCommands, []string{command}) {
		t.Fatalf("SuccessfulCommands = %#v, want seed write command", signals.SuccessfulCommands)
	}
}

func TestBuildSeedGoTestFunction_UsesConcreteEmptyStringAssertionWhenTaskProvidesExpectation(t *testing.T) {
	task := "新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`，要求验证：\n" +
		"sanitizeRequiredLabelValue(\"CONSTRAINT\", \"Chunk ID: 123 Wall time: 0.000 seconds Process exited with code 0 Output: package proxy ...\")\n" +
		"会返回空字符串。"

	got := buildSeedGoTestFunction(task, "TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise")
	if !strings.Contains(got, "got := sanitizeRequiredLabelValue(") {
		t.Fatalf("seed test body = %q, want concrete function assertion", got)
	}
	if !strings.Contains(got, "want empty string") {
		t.Fatalf("seed test body = %q, want empty string assertion", got)
	}
	if strings.Contains(got, "TODO: implement test") {
		t.Fatalf("seed test body should not contain TODO scaffold, got %q", got)
	}
}

func TestHasSatisfiedReadForFile_RequiresObservedResultWhenAvailable(t *testing.T) {
	if hasSatisfiedReadForFile(
		nil,
		[]string{"sed -n '1,200p' 'internal/proxy/output_constraints_test.go'"},
		[]string{"sed -n '1,200p' 'internal/proxy/output_constraints.go'"},
		nil,
		"internal/proxy/output_constraints_test.go",
	) {
		t.Fatal("missing target read result should not count as satisfied when other command results are present")
	}
}

func TestCollectExecutionHistorySignals_TracksEmptyReadOutput(t *testing.T) {
	readArgs, _ := json.Marshal(map[string]any{"cmd": "sed -n '1,200p' 'internal/proxy/output_constraints_test.go'"})
	history := []json.RawMessage{
		mustMarshalRawJSON(map[string]any{
			"type":      "function_call",
			"name":      "exec_command",
			"call_id":   "call_1",
			"arguments": string(readArgs),
		}),
		mustMarshalRawJSON(map[string]any{
			"type":    "function_call_output",
			"call_id": "call_1",
			"output":  "",
		}),
	}

	signals := collectExecutionHistorySignals(history)
	want := "sed -n '1,200p' 'internal/proxy/output_constraints_test.go'"
	if !reflect.DeepEqual(signals.EmptyCommands, []string{want}) {
		t.Fatalf("EmptyCommands = %#v, want %#v", signals.EmptyCommands, []string{want})
	}
}

func TestCollectRepeatedScaffoldFiles_TracksRepeatedTouchLoop(t *testing.T) {
	signals := executionHistorySignals{
		Commands: []string{
			"mkdir -p -- 'internal/proxy' && touch 'internal/proxy/output_constraints_test.go'",
			"mkdir -p -- 'internal/proxy' && touch 'internal/proxy/output_constraints_test.go'",
		},
	}

	got := collectRepeatedScaffoldFiles(signals, []string{"internal/proxy/output_constraints_test.go"})
	if !reflect.DeepEqual(got, []string{"internal/proxy/output_constraints_test.go"}) {
		t.Fatalf("collectRepeatedScaffoldFiles = %#v", got)
	}
}

func TestChooseNextExecutionCommand_WriteTaskReadsScaffoldedFileBeforeVerify(t *testing.T) {
	requiredCommands := []string{
		"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'",
		"go test ./internal/proxy",
	}
	requiredFiles := []string{
		"internal/proxy/output_constraints.go",
		"internal/proxy/output_constraints_test.go",
	}
	signals := executionHistorySignals{
		Commands: []string{
			"sed -n '1,200p' 'internal/proxy/output_constraints.go'",
			"sed -n '1,200p' 'internal/proxy/output_constraints_test.go'",
			"mkdir -p -- 'internal/proxy' && touch 'internal/proxy/output_constraints_test.go'",
		},
		SuccessfulCommands: []string{
			"sed -n '1,200p' 'internal/proxy/output_constraints.go'",
		},
		FailedCommands: []string{
			"sed -n '1,200p' 'internal/proxy/output_constraints_test.go'",
		},
	}

	got := chooseNextExecutionCommand(requiredCommands, requiredFiles, signals, true)
	want := buildReadFileCommand("internal/proxy/output_constraints_test.go")
	if got != want {
		t.Fatalf("next command = %q, want %q", got, want)
	}
}

func TestChooseNextExecutionCommand_WriteTaskTreatsSeenReferenceReadAsSatisfiedWithoutExplicitSuccessFlag(t *testing.T) {
	requiredFiles := []string{
		"internal/proxy/output_constraints.go",
		"internal/proxy/output_constraints_test.go",
	}
	signals := executionHistorySignals{
		Commands: []string{
			"sed -n '1,200p' 'internal/proxy/output_constraints.go'",
			"mkdir -p -- 'internal/proxy' && touch 'internal/proxy/output_constraints_test.go'",
			"sed -n '1,200p' 'internal/proxy/output_constraints_test.go'",
		},
		SuccessfulCommands: []string{
			"sed -n '1,200p' 'internal/proxy/output_constraints_test.go'",
		},
	}

	got := chooseNextExecutionCommand(nil, requiredFiles, signals, true)
	if got != "" {
		t.Fatalf("next command = %q, want empty after both required files are effectively inspected", got)
	}
}

func TestChooseNextExecutionCommand_WriteTaskFailedTestAfterWriteReadsTargetBeforeRetry(t *testing.T) {
	requiredCommands := []string{
		"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'",
		"go test ./internal/proxy",
	}
	requiredFiles := []string{
		"internal/proxy/output_constraints_test.go",
	}
	signals := executionHistorySignals{
		WriteCalls:        1,
		LastWritePos:      3,
		LastFailedTestPos: 4,
		ReadResultPosByFile: map[string]int{
			"internal/proxy/output_constraints_test.go": 2,
		},
		Commands: []string{
			"sed -n '1,200p' 'internal/proxy/output_constraints.go'",
			"sed -n '1,200p' 'internal/proxy/output_constraints_test.go'",
			"python3 -c 'from pathlib import Path; Path(\"internal/proxy/output_constraints_test.go\").write_text(\"package proxy\\n\", encoding='\"'\"'utf-8'\"'\"')'",
			"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'",
		},
		FailedCommands: []string{
			"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'",
		},
	}

	got := chooseNextExecutionCommand(requiredCommands, requiredFiles, signals, true)
	want := buildReadFileCommand("internal/proxy/output_constraints_test.go")
	if got != want {
		t.Fatalf("next command = %q, want %q", got, want)
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

func TestStableActionableUserTask_PrefersEarlierSpecificPromptOverContinuation(t *testing.T) {
	messages := []ChatMessage{
		{Role: "user", Content: "你是资深 Go 工程师。请在当前仓库完成一个真实但边界清晰的测试补强任务：\n1) 阅读 internal/proxy/output_constraints.go 与现有 internal/proxy/*_test.go 风格。\n2) 新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`。\n3) 执行命令：go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'\n4) 执行命令：go test ./internal/proxy"},
		{Role: "assistant", Content: "Assistant requested tool: exec_command"},
		{Role: "user", Content: "继续完成这个测试补强任务"},
		{Role: "user", Content: "Tool result (call_id=abc)\nSuccess: true\nOutput:\npackage proxy"},
	}

	got := stableActionableUserTask(messages)
	if !strings.Contains(got, "internal/proxy/output_constraints_test.go") {
		t.Fatalf("stable actionable task should preserve original anchored prompt, got: %q", got)
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

func TestBuildExecutionPolicy_ReadOnlyCodingTaskDoesNotStickInExecute(t *testing.T) {
	task := "你是资深 Go 工程师。请执行一个真实编码排障任务（只读分析，不修改文件）：\n1) 执行 `sed -n '1,220p' internal/proxy/output_constraints.go`\n2) 执行 `sed -n '1,220p' internal/proxy/execution_evidence.go`\n3) 执行 `go test ./internal/proxy`\n4) 执行 `go test ./...`\n完成后只输出四行。"
	history := historyExecCommandItems(
		"sed -n '1,220p' internal/proxy/output_constraints.go",
		"sed -n '1,220p' internal/proxy/execution_evidence.go",
		"go test ./internal/proxy",
		"go test ./...",
	)

	policy := buildExecutionPolicy("glm-5", task, history, true, false, true)
	if policy.Stage != "finalize" {
		t.Fatalf("stage = %q, want finalize; policy=%+v", policy.Stage, policy)
	}
	if policy.RequireTool {
		t.Fatalf("policy.RequireTool = true, want false after read-only task completed: %+v", policy)
	}
}

func TestBuildExecutionPolicy_ReadOnlyCodingTaskKeepsVerifyWhenFinalTestOutputIsPartial(t *testing.T) {
	task := "你是资深 Go 工程师。请执行一个真实编码排障任务（只读分析，不修改文件）：\n1) 执行 `sed -n '1,220p' internal/proxy/output_constraints.go`\n2) 执行 `sed -n '1,220p' internal/proxy/execution_evidence.go`\n3) 执行 `go test ./internal/proxy`\n4) 执行 `go test ./...`\n完成后只输出四行。"
	readOneArgs, _ := json.Marshal(map[string]any{"cmd": "sed -n '1,220p' internal/proxy/output_constraints.go"})
	readTwoArgs, _ := json.Marshal(map[string]any{"cmd": "sed -n '1,220p' internal/proxy/execution_evidence.go"})
	testOneArgs, _ := json.Marshal(map[string]any{"cmd": "go test ./internal/proxy"})
	testAllArgs, _ := json.Marshal(map[string]any{"cmd": "go test ./..."})
	history := []json.RawMessage{
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "read_1", "arguments": string(readOneArgs)}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "read_1", "output": map[string]any{"content": "package proxy", "success": true}}),
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "read_2", "arguments": string(readTwoArgs)}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "read_2", "output": map[string]any{"content": "package proxy", "success": true}}),
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "test_1", "arguments": string(testOneArgs)}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "test_1", "output": map[string]any{"content": "ok\tgithub.com/mison/firew2oai/internal/proxy\t0.321s"}}),
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "test_all", "arguments": string(testAllArgs)}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "test_all", "output": map[string]any{"content": "ok\tgithub.com/mison/firew2oai/internal/config\t(cached)\nok\tgithub.com/mison/firew2oai/internal/tokenauth\t(cached)\n"}}),
	}

	policy := buildExecutionPolicy("deepseek-v3p1", task, history, true, false, true)
	if policy.Stage != "verify" {
		t.Fatalf("stage = %q, want verify; policy=%+v", policy.Stage, policy)
	}
	if !policy.RequireTool {
		t.Fatalf("policy.RequireTool = false, want true while final test output is partial: %+v", policy)
	}
	if strings.TrimSpace(policy.NextCommand) == "" {
		t.Fatalf("policy.NextCommand = empty, want a follow-up tool step while final test output is partial")
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

func TestBuildExecutionPolicyPromptBlock_PendingWriteGuidance(t *testing.T) {
	policy := executionPolicy{
		Enabled:          true,
		Stage:            "execute",
		RequireTool:      true,
		PendingWrite:     true,
		MissingFiles:     []string{"internal/proxy/output_constraints_test.go"},
		EmptyFiles:       []string{"internal/proxy/output_constraints_test.go"},
		RepeatedScaffold: []string{"internal/proxy/output_constraints_test.go"},
		RequiredFiles:    []string{"internal/proxy/output_constraints_test.go"},
	}

	block := buildExecutionPolicyPromptBlock(policy)
	for _, want := range []string{
		"The task still requires modifying files before mode final.",
		"internal/proxy/output_constraints_test.go",
		"These target files do not exist yet.",
		"These target files already exist but are still empty.",
		"Repeated scaffold-only commands were already observed",
		"Emit a declared mutation tool call next.",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("pending-write policy block missing %q:\n%s", want, block)
		}
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
