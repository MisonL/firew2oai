package proxy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestExtractStyleInspectionCommands_FromWildcardTestGlob(t *testing.T) {
	task := "请先阅读 internal/proxy/*_test.go 风格，再新增 internal/proxy/output_constraints_test.go。"

	got := extractStyleInspectionCommands(task)
	want := []string{
		"find 'internal/proxy' -maxdepth 1 -name '*_test.go' | sort | head -n 5",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extractStyleInspectionCommands mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestExtractWriteTargetFiles_OnlyReturnsMutationTargets(t *testing.T) {
	task := "请在当前仓库完成一个真实但边界清晰的测试补强任务：\n" +
		"1) 阅读 internal/proxy/output_constraints.go 与现有 internal/proxy/*_test.go 风格。\n" +
		"2) 新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`。"

	got := extractWriteTargetFiles(task)
	want := []string{"internal/proxy/output_constraints_test.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extractWriteTargetFiles mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestExtractWriteTargetFiles_OnlyReturnsMutationTargetsFromSingleLineSteps(t *testing.T) {
	task := "请在当前仓库完成一个真实但边界清晰的测试补强任务：1) 阅读 internal/proxy/output_constraints.go 与现有 internal/proxy/*_test.go 风格。 2) 新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`。 3) 执行命令：go test ./internal/proxy"

	got := extractWriteTargetFiles(task)
	want := []string{"internal/proxy/output_constraints_test.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extractWriteTargetFiles mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestExtractWriteTargetFiles_ReadOnlyInlineStepsReturnEmpty(t *testing.T) {
	task := "在当前仓库执行只读核验任务：1) 运行 `sed -n '1,80p' internal/proxy/task_intent.go`；2) 运行 `go test ./internal/proxy -run TestStableActionableUserTask_PrefersEarlierSpecificPromptOverWeakContinuation`；3) 不要修改文件；4) 最后只输出两行。"

	got := extractWriteTargetFiles(task)
	if len(got) != 0 {
		t.Fatalf("extractWriteTargetFiles = %#v, want empty for read-only task", got)
	}
}

func TestExtractBestActionableTaskBlock_PrefersConcreteTaskOverRepositoryGuidelines(t *testing.T) {
	text := "## Repository Guidelines\n" +
		"提交前扫描命令：rg -n \"#\\[test\\]|#\\[cfg\\(test\\)\\]\" src/ -g \"*.rs\"\n\n" +
		"你是资深 Go 工程师。请在当前仓库完成一个真实但边界清晰的测试补强任务：\n" +
		"1) 阅读 internal/proxy/output_constraints.go 与现有 internal/proxy/*_test.go 风格。\n" +
		"2) 新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`。\n" +
		"最后只输出四行。"

	got := extractBestActionableTaskBlock(text)
	if !strings.Contains(got, "internal/proxy/output_constraints_test.go") {
		t.Fatalf("best actionable block should keep concrete task, got: %q", got)
	}
	if strings.Contains(got, "rg -n") {
		t.Fatalf("best actionable block should exclude repository guideline command pollution, got: %q", got)
	}
}

func TestExtractBestActionableTaskBlock_PrefersMinimalPromptOverRepositoryGuidelines(t *testing.T) {
	text := "## Repository Guidelines\n" +
		"## Project Structure & Module Organization\n" +
		"## Build, Test, and Development Commands\n" +
		"- `make test`：执行 `go test -v -race ./...`\n\n" +
		"只回答 ok"

	got := extractBestActionableTaskBlock(text)
	if got != "只回答 ok" {
		t.Fatalf("best actionable block = %q, want simple prompt", got)
	}
}

func TestStableActionableUserTask_PrefersLatestSimplePromptOverOlderRepositoryGuidelines(t *testing.T) {
	messages := []ChatMessage{
		{
			Role: "user",
			Content: "## Repository Guidelines\n" +
				"## Project Structure & Module Organization\n" +
				"## Build, Test, and Development Commands\n" +
				"- `make test`：执行 `go test -v -race ./...`",
		},
		{Role: "user", Content: "只回答 ok"},
	}

	got := stableActionableUserTask(messages)
	if got != "只回答 ok" {
		t.Fatalf("stable actionable task = %q, want latest simple prompt", got)
	}
}

func TestChooseNextExecutionCommandWithStyles_PrioritizesStyleInspectionBeforeScaffold(t *testing.T) {
	requiredFiles := []string{
		"internal/proxy/output_constraints.go",
		"internal/proxy/output_constraints_test.go",
	}
	styleCommands := []string{
		"find 'internal/proxy' -maxdepth 1 -name '*_test.go' | sort | head -n 5",
	}

	got := chooseNextExecutionCommandWithStyles(nil, requiredFiles, styleCommands, executionHistorySignals{}, true)
	if got != styleCommands[0] {
		t.Fatalf("next command = %q, want style inspection command %q", got, styleCommands[0])
	}
}

func TestChooseNextExecutionCommandWithStyles_ReadsExistingStyleFileAfterListing(t *testing.T) {
	requiredFiles := []string{
		"internal/proxy/output_constraints.go",
		"internal/proxy/output_constraints_test.go",
	}
	styleCommand := "find 'internal/proxy' -maxdepth 1 -name '*_test.go' | sort | head -n 5"
	signals := executionHistorySignals{
		Commands: []string{
			styleCommand,
		},
		SuccessfulCommands: []string{
			styleCommand,
		},
		CommandOutputs: map[string]string{
			styleCommand: "internal/proxy/execution_policy_style_test.go\ninternal/proxy/proxy_benchmark_test.go\ninternal/proxy/proxy_test.go\ninternal/proxy/responses_test.go\ninternal/proxy/output_constraints_test.go\n",
		},
	}

	got := chooseNextExecutionCommandWithStyles(nil, requiredFiles, []string{styleCommand}, signals, true)
	want := buildReadFileCommand("internal/proxy/proxy_test.go")
	if got != want {
		t.Fatalf("next command = %q, want %q", got, want)
	}
}

func TestChooseUnreadStyleReferenceFile_SkipsTargetAndAlreadyReadFiles(t *testing.T) {
	output := "internal/proxy/execution_policy_style_test.go\ninternal/proxy/proxy_benchmark_test.go\ninternal/proxy/proxy_test.go\ninternal/proxy/responses_test.go\ninternal/proxy/output_constraints_test.go\n"
	requiredFiles := []string{
		"internal/proxy/output_constraints.go",
		"internal/proxy/output_constraints_test.go",
	}
	seenCommands := []string{
		buildReadFileCommand("internal/proxy/proxy_test.go"),
	}

	got := chooseUnreadStyleReferenceFile(output, requiredFiles, seenCommands)
	if got != "" {
		t.Fatalf("style reference file = %q, want empty after one reference file is already read", got)
	}
}

func TestCollectStyleReferenceCandidates_SkipsStyleBenchmarkAndTargetFiles(t *testing.T) {
	output := "internal/proxy/execution_policy_style_test.go\ninternal/proxy/proxy_benchmark_test.go\ninternal/proxy/proxy_test.go\ninternal/proxy/responses_test.go\ninternal/proxy/output_constraints_test.go\n"
	requiredFiles := []string{
		"internal/proxy/output_constraints.go",
		"internal/proxy/output_constraints_test.go",
	}

	got := collectStyleReferenceCandidates(output, requiredFiles)
	want := []string{
		"internal/proxy/proxy_test.go",
		"internal/proxy/responses_test.go",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("collectStyleReferenceCandidates mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestChooseNextExecutionCommandWithStyles_DoesNotReissueScaffoldAfterEmptyRead(t *testing.T) {
	requiredFiles := []string{
		"internal/proxy/output_constraints.go",
		"internal/proxy/output_constraints_test.go",
	}
	signals := executionHistorySignals{
		Commands: []string{
			buildReadFileCommand("internal/proxy/output_constraints.go"),
			buildReadFileCommand("internal/proxy/output_constraints_test.go"),
			buildCreateMissingFileCommand("internal/proxy/output_constraints_test.go"),
			buildReadFileCommand("internal/proxy/output_constraints_test.go"),
		},
		SuccessfulCommands: []string{
			buildReadFileCommand("internal/proxy/output_constraints.go"),
			buildReadFileCommand("internal/proxy/output_constraints_test.go"),
		},
		FailedCommands: []string{
			buildReadFileCommand("internal/proxy/output_constraints_test.go"),
		},
		EmptyCommands: []string{
			buildReadFileCommand("internal/proxy/output_constraints_test.go"),
		},
	}

	got := chooseNextExecutionCommandWithStyles(nil, requiredFiles, nil, signals, true)
	if got != "" {
		t.Fatalf("next command = %q, want empty after scaffolded target is already re-read as empty", got)
	}
}

func TestBuildExecutionPolicy_WriteTaskTargetsOnlyMutationFile(t *testing.T) {
	task := "你是资深 Go 工程师。请在当前仓库完成一个真实但边界清晰的测试补强任务：\n" +
		"1) 阅读 internal/proxy/output_constraints.go 与现有 internal/proxy/*_test.go 风格。\n" +
		"2) 新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`。\n" +
		"3) 执行命令：go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'\n" +
		"4) 执行命令：go test ./internal/proxy"

	policy := buildExecutionPolicy("minimax-m2p5", task, nil, true, false, true)
	if !reflect.DeepEqual(policy.RequiredFiles, []string{"internal/proxy/output_constraints_test.go"}) {
		t.Fatalf("policy.RequiredFiles = %#v, want only mutation target", policy.RequiredFiles)
	}
	wantNext := "find 'internal/proxy' -maxdepth 1 -name '*_test.go' | sort | head -n 5"
	if policy.NextCommand != wantNext {
		t.Fatalf("policy.NextCommand = %q, want %q", policy.NextCommand, wantNext)
	}
}

func TestBuildExecutionPolicy_PrefersSeedWriteCommandForMissingNamedTestFile(t *testing.T) {
	task := "你是资深 Go 工程师。请在当前仓库完成一个真实但边界清晰的测试补强任务：\n" +
		"1) 阅读 internal/proxy/output_constraints.go 与现有 internal/proxy/*_test.go 风格。\n" +
		"2) 新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`，要求验证：\n" +
		"   sanitizeRequiredLabelValue(\"CONSTRAINT\", \"Chunk ID: 123 Wall time: 0.000 seconds Process exited with code 0 Output: package proxy ...\")\n" +
		"   会返回空字符串。\n" +
		"3) 执行命令：go test ./internal/proxy"
	readArgs, _ := json.Marshal(map[string]any{"cmd": "sed -n '1,120p' 'internal/proxy/output_constraints.go'"})
	missingArgs, _ := json.Marshal(map[string]any{"cmd": "sed -n '1,120p' 'internal/proxy/output_constraints_test.go'"})
	history := []json.RawMessage{
		mustMarshalRawJSON(map[string]any{
			"type":      "function_call",
			"name":      "exec_command",
			"call_id":   "read_source",
			"arguments": string(readArgs),
		}),
		mustMarshalRawJSON(map[string]any{
			"type":    "function_call_output",
			"call_id": "read_source",
			"output":  "package proxy\n\nfunc sanitizeRequiredLabelValue() {}\n",
		}),
		mustMarshalRawJSON(map[string]any{
			"type":      "function_call",
			"name":      "exec_command",
			"call_id":   "read_target",
			"arguments": string(missingArgs),
		}),
		mustMarshalRawJSON(map[string]any{
			"type":    "function_call_output",
			"call_id": "read_target",
			"output":  "sed: internal/proxy/output_constraints_test.go: No such file or directory\n",
		}),
	}

	policy := buildExecutionPolicy("minimax-m2p5", task, history, true, false, true)
	if !strings.Contains(policy.NextCommand, "python3 -c") {
		t.Fatalf("policy.NextCommand = %q, want seed write command", policy.NextCommand)
	}
	if !strings.Contains(policy.NextCommand, "write_text(") {
		t.Fatalf("policy.NextCommand should write file content directly, got %q", policy.NextCommand)
	}
	if !strings.Contains(policy.NextCommand, "func TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise") {
		t.Fatalf("policy.NextCommand should include named test scaffold, got %q", policy.NextCommand)
	}
	if !strings.Contains(policy.NextCommand, "got := sanitizeRequiredLabelValue(") {
		t.Fatalf("policy.NextCommand should synthesize a concrete assertion body, got %q", policy.NextCommand)
	}
	if strings.Contains(policy.NextCommand, "TODO: implement test") {
		t.Fatalf("policy.NextCommand should not fall back to TODO scaffold when task already states expected behavior, got %q", policy.NextCommand)
	}
}

func TestApplyExecutionPolicyToParseResult_PendingWriteBlocksRepeatedScaffold(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	content := "继续 touch。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"mkdir -p -- 'internal/proxy' && touch 'internal/proxy/output_constraints_test.go'\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	policy := executionPolicy{
		Enabled:          true,
		RequireTool:      true,
		PendingWrite:     true,
		RequiredFiles:    []string{"internal/proxy/output_constraints_test.go"},
		RepeatedScaffold: []string{"internal/proxy/output_constraints_test.go"},
		EmptyFiles:       []string{"internal/proxy/output_constraints_test.go"},
	}

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{})
	command := parsedCallCommand(t, got.calls[0])
	if command == "mkdir -p -- 'internal/proxy' && touch 'internal/proxy/output_constraints_test.go'" {
		t.Fatalf("repeated scaffold command should be guarded, got original command")
	}
	if want := "do not run mkdir/touch again"; !containsNormalized(command, want) {
		t.Fatalf("guard command = %q, want message containing %q", command, want)
	}
}

func TestApplyExecutionPolicyToParseResult_PendingWriteBlocksReadOnlyLoopAfterEmptyTarget(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	content := "继续读源码。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"cat internal/proxy/output_constraints.go\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	policy := executionPolicy{
		Enabled:          true,
		RequireTool:      true,
		PendingWrite:     true,
		RequiredFiles:    []string{"internal/proxy/output_constraints_test.go"},
		EmptyFiles:       []string{"internal/proxy/output_constraints_test.go"},
		RepeatedScaffold: []string{"internal/proxy/output_constraints_test.go"},
	}

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{})
	command := parsedCallCommand(t, got.calls[0])
	if command == "cat internal/proxy/output_constraints.go" {
		t.Fatalf("read-only loop command should be guarded, got original command")
	}
	if want := "do not run more read-only commands"; !containsNormalized(command, want) {
		t.Fatalf("guard command = %q, want message containing %q", command, want)
	}
}

func TestApplyExecutionPolicyToParseResult_PendingWriteRewritesReadOnlyToNextCommand(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	content := "继续读文件。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"cat internal/proxy/output_constraints.go\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	nextCommand := "mkdir -p -- 'internal/proxy' && touch 'internal/proxy/output_constraints_test.go'"
	policy := executionPolicy{
		Enabled:       true,
		RequireTool:   true,
		PendingWrite:  true,
		RequiredFiles: []string{"internal/proxy/output_constraints_test.go"},
		NextCommand:   nextCommand,
	}

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{})
	command := parsedCallCommand(t, got.calls[0])
	if command != nextCommand {
		t.Fatalf("rewritten command = %q, want %q", command, nextCommand)
	}
}

func TestApplyExecutionPolicyToParseResult_PendingWriteAfterRealWriteRewritesReadOnlyToNextCommand(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	content := "继续读文件。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"sed -n '1,200p' 'internal/proxy/output_constraints.go'\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	nextCommand := "go test ./internal/proxy"
	policy := executionPolicy{
		Enabled:              true,
		RequireTool:          true,
		PendingWrite:         true,
		RequiredFiles:        []string{"internal/proxy/output_constraints_test.go"},
		RequiredCommands:     []string{"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'", "go test ./internal/proxy"},
		AllRequiredFilesSeen: true,
		HasWriteObserved:     true,
		NextCommand:          nextCommand,
	}

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{})
	command := parsedCallCommand(t, got.calls[0])
	if command != nextCommand {
		t.Fatalf("rewritten command = %q, want %q", command, nextCommand)
	}
}

func TestApplyExecutionPolicyToParseResult_RewritesSequentialExecDriftToNextCommand(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	content := "继续验证。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"go test ./internal/proxy\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	nextCommand := buildReadFileCommand("README.md")
	policy := executionPolicy{
		Enabled:     true,
		RequireTool: true,
		NextCommand: nextCommand,
	}

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{})
	command := parsedCallCommand(t, got.calls[0])
	if command != nextCommand {
		t.Fatalf("rewritten command = %q, want %q", command, nextCommand)
	}
}

func TestApplyExecutionPolicyToParseResult_PendingWriteBlocksRepeatedFailedTestRetry(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	content := "继续跑测试。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	policy := executionPolicy{
		Enabled:       true,
		RequireTool:   true,
		PendingWrite:  true,
		RequiredFiles: []string{"internal/proxy/output_constraints_test.go"},
	}

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{})
	command := parsedCallCommand(t, got.calls[0])
	if command == "go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'" {
		t.Fatalf("failed test retry should be guarded, got original command")
	}
	if want := "modify target files before rerunning tests"; !containsNormalized(command, want) {
		t.Fatalf("guard command = %q, want message containing %q", command, want)
	}
}

func TestApplyExecutionPolicyToParseResult_PendingWriteBlocksNonTargetMutation(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	content := "改源码。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"mkdir -p -- 'internal/proxy' && touch 'internal/proxy/output_constraints.go'\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	policy := executionPolicy{
		Enabled:       true,
		RequireTool:   true,
		PendingWrite:  true,
		RequiredFiles: []string{"internal/proxy/output_constraints_test.go"},
	}

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{})
	command := parsedCallCommand(t, got.calls[0])
	if command == "mkdir -p -- 'internal/proxy' && touch 'internal/proxy/output_constraints.go'" {
		t.Fatalf("non-target mutation command should be guarded, got original command")
	}
	if want := "allows mutations only for target files"; !containsNormalized(command, want) {
		t.Fatalf("guard command = %q, want message containing %q", command, want)
	}
}

func containsNormalized(text, want string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(want))
}
