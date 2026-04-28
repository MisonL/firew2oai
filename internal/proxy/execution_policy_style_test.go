package proxy

import (
	"encoding/json"
	"os"
	"os/exec"
	"path"
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

func TestExtractWriteTargetFiles_FixExistingFileDoesNotCaptureReadOnlyTestReference(t *testing.T) {
	task := "你是资深 Go 工程师。请修复一个真实但边界清晰的现有 bug：\n" +
		"1) 阅读 internal/codexfixture/bugfix/port.go 与 internal/codexfixture/bugfix/port_test.go。\n" +
		"2) 修复现有文件 internal/codexfixture/bugfix/port.go，使全部测试通过。\n" +
		"3) 不要改包路径，不要新增无关文件。\n" +
		"4) 执行 go test ./internal/codexfixture/bugfix。"

	got := extractWriteTargetFiles(task)
	want := []string{"internal/codexfixture/bugfix/port.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extractWriteTargetFiles mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestExtractWriteTargetFiles_DocsSyncIgnoresDoNotModifyGoCodeScope(t *testing.T) {
	task := "你是资深 Go 工程师。请模拟真实 Codex 文档同步任务：\n" +
		"1) 阅读 internal/codexfixture/realdocs/config.go。\n" +
		"2) 阅读 docs/codexfixture/realdocs.md。\n" +
		"3) 发现 config.go 中支持的环境变量，并更新 docs/codexfixture/realdocs.md 的配置表。\n" +
		"4) 不要修改 Go 代码。\n" +
		"5) 执行 rg -n \"REALDOCS_TIMEOUT|REALDOCS_RETRIES\" docs/codexfixture/realdocs.md。"

	got := extractWriteTargetFiles(task)
	want := []string{"docs/codexfixture/realdocs.md"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extractWriteTargetFiles mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestExtractWriteTargetFiles_ApplyPatchUsesMentionedFile(t *testing.T) {
	task := "你是测试代理。请验证 apply_patch：\n" +
		"1) 先读取 internal/codexfixture/patchprobe/message.txt。\n" +
		"2) 必须使用 apply_patch，把文件中的 alpha 改为 beta。"

	got := extractWriteTargetFiles(task)
	want := []string{"internal/codexfixture/patchprobe/message.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extractWriteTargetFiles mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestExtractRequiredCommands_IgnoresOutputLabelDescription(t *testing.T) {
	task := "执行 rg -n \"REALDOCS_TIMEOUT|REALDOCS_RETRIES\" docs/codexfixture/realdocs.md。\n" +
		"最后只输出四行：RESULT: PASS 或 FAIL；FILES: 你修改的文件；TEST: rg 结果；NOTE: 文档同步摘要。"

	got := dedupePreserveOrder(extractRequiredCommands(task))
	want := []string{"rg -n \"REALDOCS_TIMEOUT|REALDOCS_RETRIES\" docs/codexfixture/realdocs.md"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("required commands mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestExtractRequiredOutputLabels_InferPlainUppercaseList(t *testing.T) {
	task := "读取 internal/proxy/output_constraints.go 和 internal/proxy/execution_evidence.go，运行 go test ./internal/proxy 和 go test ./...，最终只输出四行：RESULT、CONSTRAINT、EVIDENCE、TEST。"

	got := extractRequiredOutputLabels(task)
	want := []string{"RESULT", "CONSTRAINT", "EVIDENCE", "TEST"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extractRequiredOutputLabels mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestExtractRequiredOutputLabels_IgnoresIncidentalLabelsBeforeOutputSection(t *testing.T) {
	task := "你是资深 Go 工程师。请在当前仓库完成一个真实但边界清晰的测试补强任务：\n" +
		"1) 阅读 internal/proxy/output_constraints.go 与现有 internal/proxy/*_test.go 风格。\n" +
		"2) 新增文件 internal/proxy/output_constraints_test.go，添加测试 TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise。\n" +
		"3) 该测试需要断言 sanitizeRequiredLabelValue(\"CONSTRAINT\", \"Chunk ID: 123 Wall time: 0.000 seconds Process exited with code 0 Output: package proxy ...\") 返回空字符串。\n" +
		"4) 执行 go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'。\n" +
		"最后只输出四行：RESULT: PASS 或 FAIL；FILES: 你新增或修改的文件；TEST: 测试结果；NOTE: 你完成的补强动作。"

	got := extractRequiredOutputLabels(task)
	want := []string{"RESULT", "FILES", "TEST", "NOTE"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extractRequiredOutputLabels mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestChooseNextExecutionCommandWithStyles_ReadOnlyTaskPrefersImplicitFileRead(t *testing.T) {
	requiredCommands := []string{
		"go test ./internal/proxy",
		"go test ./...",
	}
	requiredFiles := []string{
		"internal/proxy/output_constraints.go",
		"internal/proxy/execution_evidence.go",
	}

	got := chooseNextExecutionCommandWithStyles(requiredCommands, requiredFiles, nil, executionHistorySignals{}, false, nil)
	want := buildReadFileCommand("internal/proxy/output_constraints.go")
	if got != want {
		t.Fatalf("next command = %q, want %q", got, want)
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

func TestStableActionableUserTask_IgnoresAgentsAndEnvironmentContextBlocks(t *testing.T) {
	messages := []ChatMessage{
		{
			Role: "user",
			Content: "# AGENTS.md instructions for /Volumes/Work/code/firew2oai\n\n" +
				"<INSTRUCTIONS>\n" +
				"## 1. 核心原则\n" +
				"- Debug-First\n" +
				"- 事实优先\n" +
				"</INSTRUCTIONS>\n\n" +
				"<environment_context>\n" +
				"  <cwd>/Volumes/Work/code/firew2oai</cwd>\n" +
				"  <current_date>2026-04-20</current_date>\n" +
				"  <timezone>Asia/Shanghai</timezone>\n" +
				"</environment_context>",
		},
		{
			Role:    "user",
			Content: "你是资深 Go 工程师。请在当前仓库完成一个真实但边界清晰的测试补强任务：\n1) 阅读 internal/proxy/output_constraints.go 与现有 internal/proxy/*_test.go 风格。\n2) 新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`。\n3) 执行命令：go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'\n4) 执行命令：go test ./internal/proxy",
		},
		{Role: "user", Content: "继续推进"},
	}

	got := stableActionableUserTask(messages)
	if !strings.Contains(got, "internal/proxy/output_constraints_test.go") {
		t.Fatalf("stable actionable task should preserve concrete task instead of context blocks, got: %q", got)
	}
	if strings.Contains(got, "<environment_context>") || strings.Contains(got, "# AGENTS.md instructions") {
		t.Fatalf("stable actionable task should exclude AGENTS/environment context blocks, got: %q", got)
	}
}

func TestStableActionableUserTask_UsesDeveloperTaskWhenUserMessageIsOnlyEnvironmentContext(t *testing.T) {
	messages := []ChatMessage{
		{
			Role: "user",
			Content: "<environment_context>\n" +
				"  <cwd>/Volumes/Work/code/firew2oai</cwd>\n" +
				"  <current_date>2026-04-20</current_date>\n" +
				"</environment_context>",
		},
		{
			Role:    "developer",
			Content: "请在当前仓库完成一个真实但边界清晰的测试补强任务：1) 阅读 internal/proxy/output_constraints.go 与现有 internal/proxy/*_test.go 风格。 2) 新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`。 3) 执行命令：go test ./internal/proxy",
		},
		{Role: "user", Content: "继续推进"},
	}

	got := stableActionableUserTask(messages)
	if !strings.Contains(got, "internal/proxy/output_constraints_test.go") {
		t.Fatalf("stable actionable task should use developer task when user message is only environment context, got: %q", got)
	}
}

func TestChooseNextExecutionCommandWithStyles_PrioritizesMentionedSourceFileBeforeStyleInspection(t *testing.T) {
	sequenceFiles := []string{
		"internal/proxy/output_constraints.go",
		"internal/proxy/output_constraints_test.go",
	}
	writeTargets := []string{
		"internal/proxy/output_constraints_test.go",
	}
	styleCommands := []string{
		"find 'internal/proxy' -maxdepth 1 -name '*_test.go' | sort | head -n 5",
	}

	got := chooseNextExecutionCommandWithStyles(nil, sequenceFiles, styleCommands, executionHistorySignals{}, true, writeTargets)
	want := buildReadFileCommand("internal/proxy/output_constraints.go")
	if got != want {
		t.Fatalf("next command = %q, want source file read %q", got, want)
	}
}

func TestChooseNextExecutionCommandWithStyles_ReadsExistingStyleFileAfterListing(t *testing.T) {
	requiredFiles := []string{
		"internal/proxy/output_constraints.go",
		"internal/proxy/output_constraints_test.go",
	}
	styleCommand := "find 'internal/proxy' -maxdepth 1 -name '*_test.go' | sort | head -n 5"
	readSource := buildReadFileCommand("internal/proxy/output_constraints.go")
	signals := executionHistorySignals{
		Commands: []string{
			readSource,
			styleCommand,
		},
		SuccessfulCommands: []string{
			readSource,
			styleCommand,
		},
		CommandOutputs: map[string]string{
			styleCommand: "internal/proxy/execution_policy_style_test.go\ninternal/proxy/proxy_benchmark_test.go\ninternal/proxy/proxy_test.go\ninternal/proxy/responses_test.go\ninternal/proxy/output_constraints_test.go\n",
		},
	}

	got := chooseNextExecutionCommandWithStyles(nil, requiredFiles, []string{styleCommand}, signals, true, []string{"internal/proxy/output_constraints_test.go"})
	want := buildReadFileCommand("internal/proxy/proxy_test.go")
	if got != want {
		t.Fatalf("next command = %q, want %q", got, want)
	}
}

func TestMergeExecutionSequenceFiles_PreservesMentionOrderForWriteTargets(t *testing.T) {
	allMentioned := []string{
		"internal/codexfixture/feature/formatter.go",
		"internal/codexfixture/feature/formatter_test.go",
		"internal/codexfixture/feature/title.go",
	}
	writeTargets := []string{
		"internal/codexfixture/feature/title.go",
		"internal/codexfixture/feature/formatter.go",
		"internal/codexfixture/feature/formatter_test.go",
	}

	got := mergeExecutionSequenceFiles(allMentioned, writeTargets)
	want := []string{
		"internal/codexfixture/feature/formatter.go",
		"internal/codexfixture/feature/formatter_test.go",
		"internal/codexfixture/feature/title.go",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeExecutionSequenceFiles mismatch\n got: %#v\nwant: %#v", got, want)
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

	got := chooseNextExecutionCommandWithStyles(nil, requiredFiles, nil, signals, true, []string{"internal/proxy/output_constraints_test.go"})
	if got != "" {
		t.Fatalf("next command = %q, want empty after scaffolded target is already re-read as empty", got)
	}
}

func TestBuildExecutionPolicy_PrefersSeedWriteCommandForGoStringTransformTask(t *testing.T) {
	task := "你是资深 Go 工程师。请完成一个需要先搜索再修复的真实 Coding 任务：\n" +
		"1) 先执行命令：rg -n \"BuildTicketSummary|NormalizeTitle\" internal/codexfixture/searchfix。\n" +
		"2) 阅读 internal/codexfixture/searchfix/summary.go 与 internal/codexfixture/searchfix/summary_test.go。\n" +
		"3) 修改现有文件 internal/codexfixture/searchfix/summary.go，让 BuildTicketSummary 对 title 执行 strings.TrimSpace + strings.ToUpper，对 body 执行 strings.TrimSpace。\n" +
		"4) 不要新增文件。\n" +
		"5) 执行 go test ./internal/codexfixture/searchfix。"
	history := []json.RawMessage{
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "search_1", "arguments": `{"cmd":"rg -n \"BuildTicketSummary|NormalizeTitle\" internal/codexfixture/searchfix"}`}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "search_1", "output": map[string]any{"content": "internal/codexfixture/searchfix/summary.go:3:func BuildTicketSummary(title, body string) string", "success": true}}),
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "read_1", "arguments": `{"cmd":"sed -n '1,200p' 'internal/codexfixture/searchfix/summary.go'"}`}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "read_1", "output": map[string]any{"content": "package searchfix", "success": true}}),
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "read_2", "arguments": `{"cmd":"sed -n '1,200p' 'internal/codexfixture/searchfix/summary_test.go'"}`}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "read_2", "output": map[string]any{"content": "package searchfix", "success": true}}),
	}

	policy := buildExecutionPolicy("qwen3-vl-30b-a3b-instruct", task, history, true, false, true)
	if !policy.PendingWrite {
		t.Fatal("policy.PendingWrite = false, want true")
	}
	if !strings.HasPrefix(policy.NextCommand, "python3 -c ") {
		t.Fatalf("policy.NextCommand = %q, want single-line python seed write command", policy.NextCommand)
	}
	if strings.Contains(policy.NextCommand, "\n") {
		t.Fatalf("policy.NextCommand should stay single-line for Codex exec compatibility, got %q", policy.NextCommand)
	}
	for _, want := range []string{"base64.b64decode", "utf-8"} {
		if !strings.Contains(policy.NextCommand, want) {
			t.Fatalf("policy.NextCommand missing %q, got %q", want, policy.NextCommand)
		}
	}
}

func TestBuildSeedGoStringTransformCommand_DoesNotDuplicateExistingStringsImport(t *testing.T) {
	dir := t.TempDir()
	targetFile := dir + "/summary.go"
	if err := os.WriteFile(targetFile, []byte("package searchfix\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n)\n\nfunc BuildTicketSummary(title, body string) string {\n\treturn title + \": \" + body\n}\n"), 0o644); err != nil {
		t.Fatalf("write target file: %v", err)
	}

	task := "修改现有文件 " + targetFile + "，让 BuildTicketSummary 对 title 执行 strings.TrimSpace + strings.ToUpper，对 body 执行 strings.TrimSpace。"
	signals := executionHistorySignals{
		SuccessfulCommands: []string{
			"sed -n '1,200p' " + shellQuoteSingle(targetFile),
			"sed -n '1,200p' " + shellQuoteSingle(dir+"/summary_test.go"),
		},
	}
	cmd := buildSeedGoStringTransformCommand(task, []string{targetFile}, signals)
	if strings.TrimSpace(cmd) == "" {
		t.Fatal("buildSeedGoStringTransformCommand returned empty command")
	}
	run := exec.Command("sh", "-c", cmd)
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("run seed transform command: %v\n%s", err, string(output))
	}

	content, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("read target file: %v", err)
	}
	text := string(content)
	if strings.Count(text, "\"strings\"") != 1 {
		t.Fatalf("strings import count = %d, want 1\n%s", strings.Count(text, "\"strings\""), text)
	}
	if !strings.Contains(text, "return strings.ToUpper(strings.TrimSpace(title)) + \": \" + strings.TrimSpace(body)") {
		t.Fatalf("transformed return missing:\n%s", text)
	}
}

func TestBuildExecutionPolicy_PendingWriteWithoutConcreteNextCommandLeavesNextCommandEmpty(t *testing.T) {
	task := "你是资深 Go 工程师。请修改 internal/codexfixture/searchfix/summary.go，使逻辑与测试一致。\n" +
		"1) 阅读 internal/codexfixture/searchfix/summary.go。\n" +
		"2) 阅读 internal/codexfixture/searchfix/summary_test.go。\n" +
		"3) 不要新增文件。\n" +
		"4) 执行 go test ./internal/codexfixture/searchfix。"
	history := []json.RawMessage{
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "read_1", "arguments": `{"cmd":"sed -n '1,200p' 'internal/codexfixture/searchfix/summary.go'"}`}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "read_1", "output": map[string]any{"content": "package searchfix", "success": true}}),
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "read_2", "arguments": `{"cmd":"sed -n '1,200p' 'internal/codexfixture/searchfix/summary_test.go'"}`}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "read_2", "output": map[string]any{"content": "package searchfix", "success": true}}),
	}

	policy := buildExecutionPolicy("qwen3-vl-30b-a3b-instruct", task, history, true, false, true)
	if !policy.PendingWrite {
		t.Fatal("policy.PendingWrite = false, want true")
	}
	if policy.NextCommand != "" {
		t.Fatalf("policy.NextCommand = %q, want empty when no deterministic seed write can be synthesized", policy.NextCommand)
	}
}

func TestBuildExecutionPolicy_PrefersSeedWriteCommandForCrossFileFeatureTask(t *testing.T) {
	task := "你是资深 Go 工程师。请完成一个小型跨文件 Coding 任务：\n" +
		"1) 阅读 internal/codexfixture/feature/formatter.go 与 internal/codexfixture/feature/formatter_test.go。\n" +
		"2) 新增文件 internal/codexfixture/feature/title.go，提供 normalizeTitle 帮助函数。\n" +
		"3) 修改现有文件 internal/codexfixture/feature/formatter.go，让 BuildSummary 正确规范化 title 并裁剪 body。\n" +
		"4) 修改现有文件 internal/codexfixture/feature/formatter_test.go，追加一个空 body 场景。\n" +
		"5) 执行 go test ./internal/codexfixture/feature。"
	history := []json.RawMessage{
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "read_1", "arguments": `{"cmd":"sed -n '1,200p' 'internal/codexfixture/feature/formatter.go'"}`}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "read_1", "output": map[string]any{"content": "package feature\n\nfunc BuildSummary(title, body string) string {\n\treturn title + \": \" + body\n}\n", "success": true}}),
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "read_2", "arguments": `{"cmd":"sed -n '1,200p' 'internal/codexfixture/feature/formatter_test.go'"}`}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "read_2", "output": map[string]any{"content": "package feature\n\nimport \"testing\"\n", "success": true}}),
	}

	policy := buildExecutionPolicy("glm-5", task, history, true, false, true)
	if !policy.PendingWrite {
		t.Fatal("policy.PendingWrite = false, want true")
	}
	if strings.TrimSpace(policy.NextCommand) == "" {
		t.Fatal("policy.NextCommand = empty, want cross-file seed write command")
	}
	for _, want := range []string{"base64.b64decode", "utf-8"} {
		if !strings.Contains(policy.NextCommand, want) {
			t.Fatalf("policy.NextCommand missing %q: %q", want, policy.NextCommand)
		}
	}
}

func TestBuildSeedGoCrossFileFeatureCommand_WritesHelperAndUpdatesFiles(t *testing.T) {
	dir := t.TempDir()
	featureDir := dir + "/feature"
	if err := os.MkdirAll(featureDir, 0o755); err != nil {
		t.Fatalf("mkdir feature dir: %v", err)
	}
	helperFile := "feature/title.go"
	mainFile := "feature/formatter.go"
	testFile := "feature/formatter_test.go"
	if err := os.WriteFile(featureDir+"/formatter.go", []byte("package feature\n\nfunc BuildSummary(title, body string) string {\n\treturn title + \": \" + body\n}\n"), 0o644); err != nil {
		t.Fatalf("write main file: %v", err)
	}
	if err := os.WriteFile(featureDir+"/formatter_test.go", []byte("package feature\n\nimport \"testing\"\n\nfunc TestBuildSummary_NormalizesTitleAndBody(t *testing.T) {\n\tgot := BuildSummary(\"  firew2oai  \", \" adapter \")\n\twant := \"FIREW2OAI: adapter\"\n\tif got != want {\n\t\tt.Fatalf(\"BuildSummary() = %q, want %q\", got, want)\n\t}\n}\n"), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	task := "你是资深 Go 工程师。请完成一个小型跨文件 Coding 任务：\n" +
		"1) 阅读 " + mainFile + " 与 " + testFile + "。\n" +
		"2) 新增文件 " + helperFile + "，提供 normalizeTitle 帮助函数。\n" +
		"3) 修改现有文件 " + mainFile + "，让 BuildSummary 正确规范化 title 并裁剪 body。\n" +
		"4) 修改现有文件 " + testFile + "，追加一个空 body 场景。\n" +
		"5) 执行 go test ./internal/codexfixture/feature。"
	signals := executionHistorySignals{
		SuccessfulCommands: []string{
			"sed -n '1,200p' " + shellQuoteSingle(mainFile),
			"sed -n '1,200p' " + shellQuoteSingle(testFile),
		},
		Commands: []string{
			"sed -n '1,200p' " + shellQuoteSingle(mainFile),
			"sed -n '1,200p' " + shellQuoteSingle(testFile),
		},
		CommandOutputs: map[string]string{
			"sed -n '1,200p' " + shellQuoteSingle(mainFile): "package feature\n\nfunc BuildSummary(title, body string) string {\n\treturn title + \": \" + body\n}\n",
			"sed -n '1,200p' " + shellQuoteSingle(testFile): "package feature\n\nimport \"testing\"\n",
		},
	}

	cmd := buildSeedGoCrossFileFeatureCommand(task, []string{helperFile, mainFile, testFile}, signals)
	if strings.TrimSpace(cmd) == "" {
		t.Fatalf(
			"buildSeedGoCrossFileFeatureCommand returned empty command helper=%q main=%q package=%q mentions=%v",
			extractGoHelperFunctionName(task),
			extractGoPrimaryFunctionName(task, signals, mainFile),
			inferGoPackageNameForTarget(mainFile, signals),
			taskMentionsTitleAndBodyTransform(task),
		)
	}
	run := exec.Command("sh", "-c", cmd)
	run.Dir = dir
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("run cross-file seed command: %v\n%s", err, string(output))
	}

	helperContent, err := os.ReadFile(featureDir + "/title.go")
	if err != nil {
		t.Fatalf("read helper file: %v", err)
	}
	if !strings.Contains(string(helperContent), "func normalizeTitle(title string) string") {
		t.Fatalf("helper file missing normalizeTitle:\n%s", string(helperContent))
	}

	mainContent, err := os.ReadFile(featureDir + "/formatter.go")
	if err != nil {
		t.Fatalf("read main file: %v", err)
	}
	mainText := string(mainContent)
	if !strings.Contains(mainText, `trimmedBody := strings.TrimSpace(body)`) {
		t.Fatalf("main file missing trimmedBody branch:\n%s", mainText)
	}
	if !strings.Contains(mainText, `if trimmedBody == ""`) {
		t.Fatalf("main file missing empty body guard:\n%s", mainText)
	}
	if !strings.Contains(mainText, `return normalizeTitle(title) + ":"`) {
		t.Fatalf("main file missing empty body return:\n%s", mainText)
	}
	if !strings.Contains(mainText, `return normalizeTitle(title) + ": " + trimmedBody`) {
		t.Fatalf("main file missing non-empty body return:\n%s", mainText)
	}

	testContent, err := os.ReadFile(featureDir + "/formatter_test.go")
	if err != nil {
		t.Fatalf("read test file: %v", err)
	}
	if !strings.Contains(string(testContent), "TestBuildSummary_EmptyBody") {
		t.Fatalf("test file missing empty body scenario:\n%s", string(testContent))
	}
}

func TestBuildSeedMarkdownEnvTableCommand_AddsMissingEnvConstant(t *testing.T) {
	dir := t.TempDir()
	configPath := dir + "/internal/codexfixture/realdocs/config.go"
	docPath := dir + "/docs/codexfixture/realdocs.md"
	if err := os.MkdirAll(path.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.MkdirAll(path.Dir(docPath), 0o755); err != nil {
		t.Fatalf("mkdir docs dir: %v", err)
	}
	config := "package realdocs\n\nconst TimeoutEnv = \"REALDOCS_TIMEOUT\"\nconst RetriesEnv = \"REALDOCS_RETRIES\"\n"
	doc := "# Real Docs Fixture\n\n| 环境变量 | 说明 |\n|---|---|\n| REALDOCS_TIMEOUT | 请求超时时间，单位秒 |\n"
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(docPath, []byte(doc), 0o644); err != nil {
		t.Fatalf("write doc: %v", err)
	}
	task := "你是资深 Go 工程师。请模拟真实 Codex 文档同步任务：\n" +
		"1) 阅读 internal/codexfixture/realdocs/config.go。\n" +
		"2) 阅读 docs/codexfixture/realdocs.md。\n" +
		"3) 发现 config.go 中支持的环境变量，并更新 docs/codexfixture/realdocs.md 的配置表。"
	readConfig := "sed -n '1,200p' 'internal/codexfixture/realdocs/config.go'"
	readDoc := "sed -n '1,200p' 'docs/codexfixture/realdocs.md'"
	signals := executionHistorySignals{
		Commands:           []string{readConfig, readDoc},
		SuccessfulCommands: []string{readConfig, readDoc},
		CommandsWithResult: []string{readConfig, readDoc},
		CommandOutputs: map[string]string{
			readConfig: config,
			readDoc:    doc,
		},
	}

	cmd := buildSeedMarkdownEnvTableCommand(task, []string{"docs/codexfixture/realdocs.md"}, signals)
	if strings.TrimSpace(cmd) == "" {
		t.Fatal("buildSeedMarkdownEnvTableCommand returned empty command")
	}
	run := exec.Command("sh", "-c", cmd)
	run.Dir = dir
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("run markdown env seed command: %v\n%s", err, string(output))
	}
	updated, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read updated doc: %v", err)
	}
	text := string(updated)
	if !strings.Contains(text, "| REALDOCS_RETRIES | 待补充说明 |") {
		t.Fatalf("updated doc missing REALDOCS_RETRIES row:\n%s", text)
	}
	if strings.Count(text, "REALDOCS_TIMEOUT") != 1 {
		t.Fatalf("REALDOCS_TIMEOUT should not be duplicated:\n%s", text)
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
	wantNext := "sed -n '1,200p' 'internal/proxy/output_constraints.go'"
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

func TestBuildExecutionPolicy_PrefersSeedWriteCommandForDeterministicReplacementTask(t *testing.T) {
	task := "你是资深 Go 工程师。请修复一个真实但边界清晰的现有 bug：\n" +
		"1) 阅读 internal/codexfixture/bugfix/port.go 与 internal/codexfixture/bugfix/port_test.go。\n" +
		"2) 修复现有文件 internal/codexfixture/bugfix/port.go，把 `return port + 1` 改为 `return port`，使全部测试通过。\n" +
		"3) 执行 go test ./internal/codexfixture/bugfix。"
	readTargetArgs, _ := json.Marshal(map[string]any{"cmd": "sed -n '1,120p' 'internal/codexfixture/bugfix/port.go'"})
	readTestArgs, _ := json.Marshal(map[string]any{"cmd": "sed -n '1,120p' 'internal/codexfixture/bugfix/port_test.go'"})
	history := []json.RawMessage{
		mustMarshalRawJSON(map[string]any{
			"type":      "function_call",
			"name":      "exec_command",
			"call_id":   "read_target",
			"arguments": string(readTargetArgs),
		}),
		mustMarshalRawJSON(map[string]any{
			"type":    "function_call_output",
			"call_id": "read_target",
			"output":  "package bugfix\n\nfunc ClampPort(port int) int {\n\treturn port + 1\n}\n",
		}),
		mustMarshalRawJSON(map[string]any{
			"type":      "function_call",
			"name":      "exec_command",
			"call_id":   "read_test",
			"arguments": string(readTestArgs),
		}),
		mustMarshalRawJSON(map[string]any{
			"type":    "function_call_output",
			"call_id": "read_test",
			"output":  "package bugfix\n\nfunc TestClampPort(t *testing.T) {}\n",
		}),
	}

	policy := buildExecutionPolicy("glm-5", task, history, true, false, true)
	if !strings.Contains(policy.NextCommand, "text.replace") {
		t.Fatalf("policy.NextCommand = %q, want deterministic replacement command", policy.NextCommand)
	}
	if !strings.Contains(policy.NextCommand, "return port + 1") || !strings.Contains(policy.NextCommand, "return port") {
		t.Fatalf("policy.NextCommand should contain replacement pair, got %q", policy.NextCommand)
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

func TestApplyExecutionPolicyToParseResult_PendingWriteAfterFailedTestRewritesRetryToRepairRead(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	content := "继续跑测试。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	nextCommand := "sed -n '1,200p' 'internal/proxy/output_constraints_test.go'"
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
