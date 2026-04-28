package proxy

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
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

func TestBuildExecutionPolicy_ExplicitToolSequenceForcesSingleStepEvenForNonStrictModel(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"mcp__chrome_devtools__new_page":      {Name: "mcp__chrome_devtools__new_page", Type: "function", Structured: true, Namespace: "mcp__chrome_devtools__"},
		"mcp__chrome_devtools__take_snapshot": {Name: "mcp__chrome_devtools__take_snapshot", Type: "function", Structured: true, Namespace: "mcp__chrome_devtools__"},
	}

	policy := buildExecutionPolicyWithCatalog(
		"gpt-oss-20b",
		"必须使用 mcp__chrome_devtools__new_page 打开页面，然后使用 take_snapshot。",
		nil,
		toolCatalog,
		true,
		false,
		true,
	)

	if !policy.ForceSingleToolCall {
		t.Fatal("policy.ForceSingleToolCall = false, want true for explicit multi-step tool sequence")
	}
	if !policy.AllowTruncateToMax {
		t.Fatal("policy.AllowTruncateToMax = false, want true for explicit multi-step tool sequence")
	}
	if policy.NextRequiredTool != "mcp__chrome_devtools__new_page" {
		t.Fatalf("policy.NextRequiredTool = %q, want mcp__chrome_devtools__new_page", policy.NextRequiredTool)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesChromeNewPage(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"mcp__chrome_devtools__new_page": {Name: "mcp__chrome_devtools__new_page", Type: "function", Structured: true, Namespace: "mcp__chrome_devtools__"},
	}
	task := "必须使用 mcp__chrome_devtools__new_page 打开这个 data URL：" +
		"data:text/html,%3Cbutton%20id%3D%22go%22%3EGo%3C%2Fbutton%3E"

	policy := buildExecutionPolicyWithCatalog("gpt-oss-20b", task, nil, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "mcp__chrome_devtools__new_page" {
		t.Fatalf("policy.NextRequiredTool = %q, want mcp__chrome_devtools__new_page", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic new_page call")
	}
	if got := parsedCallName(t, *policy.SyntheticToolCall); got != "new_page" {
		t.Fatalf("synthetic tool name = %q, want new_page", got)
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "url"); got != "data:text/html,%3Cbutton%20id%3D%22go%22%3EGo%3C%2Fbutton%3E" {
		t.Fatalf("synthetic new_page url = %q", got)
	}
}

func TestApplyExecutionPolicyToParseResult_SynthesizesChromeNewPageOnFinalMode(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"mcp__chrome_devtools__new_page": {Name: "mcp__chrome_devtools__new_page", Type: "function", Structured: true, Namespace: "mcp__chrome_devtools__"},
	}
	task := "必须使用 mcp__chrome_devtools__new_page 打开这个 data URL：" +
		"data:text/html,%3Cbutton%20id%3D%22go%22%3EGo%3C%2Fbutton%3E"
	policy := buildExecutionPolicyWithCatalog("gpt-oss-20b", task, nil, toolCatalog, true, false, true)
	content := "RESULT: PASS\nFILES: none\nTEST: N/A\nNOTE: clicked\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"final\"}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{
		RequiredTool: "mcp__chrome_devtools__new_page",
		RequireTool:  true,
	})

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{
		RequiredTool: "mcp__chrome_devtools__new_page",
		RequireTool:  true,
	})
	if got.err != nil {
		t.Fatalf("got.err = %v, want nil", got.err)
	}
	if len(got.calls) != 1 {
		t.Fatalf("len(got.calls) = %d, want 1", len(got.calls))
	}
	if name := parsedCallName(t, got.calls[0]); name != "new_page" {
		t.Fatalf("tool name = %q, want new_page", name)
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

func TestApplyExecutionPolicyToParseResult_PendingWriteBlocksReadOnlyWithoutNextCommand(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	content := "继续检查。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"rg 结果\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	policy := executionPolicy{
		Enabled:              true,
		RequireTool:          true,
		Stage:                "execute",
		PendingWrite:         true,
		AllRequiredFilesSeen: true,
		RequiredFiles:        []string{"docs/codexfixture/realdocs.md"},
	}

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{})
	if got.err != nil {
		t.Fatalf("got.err = %v, want nil", got.err)
	}
	if len(got.calls) != 1 {
		t.Fatalf("len(got.calls) = %d, want 1", len(got.calls))
	}
	command := parsedCallCommand(t, got.calls[0])
	if !strings.Contains(command, "Codex adapter guard: pending write stage already inspected required context") {
		t.Fatalf("guard command missing pending-write message: %q", command)
	}
	if strings.Contains(command, "rg 结果") {
		t.Fatalf("guard command should not preserve unrelated read-only command: %q", command)
	}
}

func TestBuildExecutionPolicy_DocsSyncPendingWriteAfterContextRead(t *testing.T) {
	task := "你是资深 Go 工程师。请模拟真实 Codex 文档同步任务：\n" +
		"1) 阅读 internal/codexfixture/realdocs/config.go。\n" +
		"2) 阅读 docs/codexfixture/realdocs.md。\n" +
		"3) 发现 config.go 中支持的环境变量，并更新 docs/codexfixture/realdocs.md 的配置表。\n" +
		"4) 不要修改 Go 代码。\n" +
		"5) 执行 rg -n \"REALDOCS_TIMEOUT|REALDOCS_RETRIES\" docs/codexfixture/realdocs.md。\n" +
		"最后只输出四行：RESULT: PASS 或 FAIL；FILES: 你修改的文件；TEST: rg 结果；NOTE: 文档同步摘要。"
	history := append(
		historyExecCommandItems(
			"sed -n '1,200p' 'internal/codexfixture/realdocs/config.go'",
			"sed -n '1,200p' 'docs/codexfixture/realdocs.md'",
			"rg -n \"REALDOCS_TIMEOUT|REALDOCS_RETRIES\" docs/codexfixture/realdocs.md",
		),
		mustMarshalRawJSON(map[string]any{
			"type":    "function_call_output",
			"call_id": "call_rg",
			"output":  "5:| REALDOCS_TIMEOUT | 请求超时时间，单位秒 |\n",
		}),
	)

	policy := buildExecutionPolicy("gpt-oss-120b", task, history, true, false, true)
	if !policy.PendingWrite {
		t.Fatalf("policy.PendingWrite = false, want true: %+v", policy)
	}
	if !policy.AllRequiredFilesSeen {
		t.Fatalf("policy.AllRequiredFilesSeen = false, want true: %+v", policy)
	}
	if policy.NextCommand != "" {
		t.Fatalf("policy.NextCommand = %q, want empty after required reads/checks", policy.NextCommand)
	}
}

func TestBuildExecutionPolicy_GuardFailureFinalizesPendingWriteLoop(t *testing.T) {
	task := "你是资深 Go 工程师。请模拟真实 Codex 文档同步任务：\n" +
		"1) 阅读 internal/codexfixture/realdocs/config.go。\n" +
		"2) 阅读 docs/codexfixture/realdocs.md。\n" +
		"3) 发现 config.go 中支持的环境变量，并更新 docs/codexfixture/realdocs.md 的配置表。\n" +
		"4) 不要修改 Go 代码。\n" +
		"5) 执行 rg -n \"REALDOCS_TIMEOUT|REALDOCS_RETRIES\" docs/codexfixture/realdocs.md。\n" +
		"最后只输出四行：RESULT: PASS 或 FAIL；FILES: 你修改的文件；TEST: rg 结果；NOTE: 文档同步摘要。"
	guard := buildExecFailureCommand("Codex adapter guard: pending write stage already inspected required context; do not run more read-only commands; write non-empty content into target files now: docs/codexfixture/realdocs.md")
	guardArgs, _ := json.Marshal(map[string]any{"cmd": guard})
	history := append(
		historyExecCommandItems(
			"sed -n '1,200p' 'internal/codexfixture/realdocs/config.go'",
			"sed -n '1,200p' 'docs/codexfixture/realdocs.md'",
		),
		mustMarshalRawJSON(map[string]any{
			"type":      "function_call",
			"name":      "exec_command",
			"call_id":   "call_guard",
			"arguments": string(guardArgs),
		}),
		mustMarshalRawJSON(map[string]any{
			"type":    "function_call_output",
			"call_id": "call_guard",
			"output": map[string]any{
				"content": "Codex adapter guard: pending write stage already inspected required context\n",
				"success": false,
			},
		}),
	)

	policy := buildExecutionPolicy("gpt-oss-120b", task, history, true, false, true)
	if policy.Stage != "finalize" {
		t.Fatalf("policy.Stage = %q, want finalize after guard failure: %+v", policy.Stage, policy)
	}
	if policy.RequireTool {
		t.Fatalf("policy.RequireTool = true, want false after guard failure: %+v", policy)
	}
	if policy.PendingWrite {
		t.Fatalf("policy.PendingWrite = true, want false after guard failure: %+v", policy)
	}
	if policy.NextCommand != "" {
		t.Fatalf("policy.NextCommand = %q, want empty after guard failure", policy.NextCommand)
	}
}

func TestApplyExecutionPolicyToParseResult_PendingWriteRewritesMutationToDeterministicSeed(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	nextCommand := `python3 -c 'from pathlib import Path; Path("internal/proxy/output_constraints_test.go").write_text("package proxy\n\nimport \"testing\"\n", encoding='"'"'utf-8'"'"')'`
	content := "先创建测试文件。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"python3 -c 'from pathlib import Path; Path(\\\"internal/proxy/output_constraints_test.go\\\").write_text(\\\"broken\\\", encoding=\\\"utf-8\\\")'\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	policy := executionPolicy{
		Enabled:      true,
		RequireTool:  true,
		PendingWrite: true,
		Stage:        "execute",
		NextCommand:  nextCommand,
		RequiredFiles: []string{
			"internal/proxy/output_constraints_test.go",
		},
	}

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{})
	if got.err != nil {
		t.Fatalf("got.err = %v, want nil", got.err)
	}
	if len(got.calls) != 1 {
		t.Fatalf("len(got.calls) = %d, want 1", len(got.calls))
	}
	command := parsedCallCommand(t, got.calls[0])
	if command != nextCommand {
		t.Fatalf("command = %q, want rewritten deterministic seed command", command)
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

func TestApplyExecutionPolicyToParseResult_RewritesRepeatedRequiredFileReadBeforeRequiredTests(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	task := "你是资深 Go 工程师。请在当前仓库完成一个真实 Coding 只读核验任务：\n" +
		"1) 阅读 internal/proxy/output_constraints.go。\n" +
		"2) 阅读 internal/proxy/execution_evidence.go。\n" +
		"3) 执行 go test ./internal/proxy。\n" +
		"4) 执行 go test ./...。"
	history := historyExecCommandItems("sed -n '1,200p' 'internal/proxy/output_constraints.go'")
	policy := buildExecutionPolicyWithCatalog("qwen3-vl-30b-a3b-thinking", task, history, toolCatalog, true, false, true)
	if policy.NextCommand != "sed -n '1,200p' 'internal/proxy/execution_evidence.go'" {
		t.Fatalf("policy.NextCommand = %q, want second required file read", policy.NextCommand)
	}

	content := "重复读取第一份文件。\n<<<AI_ACTIONS_V1>>>\n" +
		"{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"sed -n '1,200p' 'internal/proxy/output_constraints.go'\"}}]}\n" +
		"<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{})
	if got.err != nil {
		t.Fatalf("got.err = %v, want nil", got.err)
	}
	if len(got.calls) != 1 {
		t.Fatalf("len(got.calls) = %d, want 1", len(got.calls))
	}
	command := parsedCallCommand(t, got.calls[0])
	if command != "sed -n '1,200p' 'internal/proxy/execution_evidence.go'" {
		t.Fatalf("command = %q, want rewritten to second required file read", command)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceRequiresNextTool(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"mcp__docfork__search_docs": {Name: "mcp__docfork__search_docs", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
		"mcp__docfork__fetch_doc":   {Name: "mcp__docfork__fetch_doc", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"search_docs","namespace":"mcp__docfork__","call_id":"call_docfork_1","arguments":"{\"library\":\"react\",\"query\":\"useEffectEvent\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_docfork_1","output":{"content":"Searched: react | 2 results\n\n[1] React docs\n    https://react.dev/reference/react/useEffectEvent\n\nUse fetch_doc on any URL above for full content.","success":true}}`),
	}

	policy := buildExecutionPolicyWithCatalog("glm-4p7", "必须使用 mcp__docfork__search_docs，然后必须使用 mcp__docfork__fetch_doc 获取文档内容。", history, toolCatalog, true, false, true)

	if !policy.RequireTool {
		t.Fatal("policy.RequireTool = false, want true")
	}
	if policy.NextRequiredTool != "mcp__docfork__fetch_doc" {
		t.Fatalf("policy.NextRequiredTool = %q, want mcp__docfork__fetch_doc", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic fetch_doc call")
	}
	if got := parsedCallName(t, *policy.SyntheticToolCall); got != "fetch_doc" {
		t.Fatalf("synthetic tool name = %q, want fetch_doc", got)
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "url"); got != "https://react.dev/reference/react/useEffectEvent" {
		t.Fatalf("synthetic fetch_doc url = %q", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceKeepsDocforkSearchAfterFailedSearch(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"mcp__docfork__search_docs": {Name: "mcp__docfork__search_docs", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
		"mcp__docfork__fetch_doc":   {Name: "mcp__docfork__fetch_doc", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"search_docs","namespace":"mcp__docfork__","call_id":"call_docfork_1","arguments":"{\"query\":\"useEffectEvent\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_docfork_1","output":{"content":"MCP error -32602: Input validation error: Invalid arguments for tool search_docs: [{\"path\":[\"library\"],\"message\":\"Invalid input: expected string, received undefined\"}]","success":false}}`),
	}

	policy := buildExecutionPolicyWithCatalog(
		"deepseek-v3p2",
		"你是测试代理。请验证 Docfork MCP：\n1) 必须使用 mcp__docfork__search_docs 搜索 react 文档中的 useEffectEvent。\n2) 必须再使用 mcp__docfork__fetch_doc 获取相关文档内容。\n3) 禁止使用 web_search 代替 Docfork。",
		history,
		toolCatalog,
		true,
		false,
		true,
	)

	if policy.NextRequiredTool != "mcp__docfork__search_docs" {
		t.Fatalf("policy.NextRequiredTool = %q, want mcp__docfork__search_docs after failed search", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want corrected synthetic search_docs call")
	}
	if got := parsedCallName(t, *policy.SyntheticToolCall); got != "search_docs" {
		t.Fatalf("synthetic tool name = %q, want search_docs", got)
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "library"); got != "react" {
		t.Fatalf("synthetic search_docs library = %q, want react", got)
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "query"); got != "useEffectEvent" {
		t.Fatalf("synthetic search_docs query = %q, want useEffectEvent", got)
	}
}

func TestApplyExecutionPolicyToParseResult_ExplicitToolSequenceSynthesizesDocforkFetchDoc(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"mcp__docfork__search_docs": {Name: "mcp__docfork__search_docs", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
		"mcp__docfork__fetch_doc":   {Name: "mcp__docfork__fetch_doc", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"search_docs","namespace":"mcp__docfork__","call_id":"call_docfork_1","arguments":"{\"library\":\"react\",\"query\":\"useEffectEvent\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_docfork_1","output":{"content":"Searched: react | 2 results\n\n[1] React docs\n    https://react.dev/reference/react/useEffectEvent\n\nUse fetch_doc on any URL above for full content.","success":true}}`),
	}
	parseResult := parseToolCallOutputsWithConstraints("我先总结一下。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"final\"}\n<<<END_AI_ACTIONS_V1>>>", toolCatalog, toolProtocolConstraints{
		RequiredTool: "mcp__docfork__fetch_doc",
		RequireTool:  true,
	})
	policy := buildExecutionPolicyWithCatalog("glm-4p7", "必须使用 mcp__docfork__search_docs，然后必须使用 mcp__docfork__fetch_doc 获取文档内容。", history, toolCatalog, true, false, true)

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{
		RequiredTool: "mcp__docfork__fetch_doc",
		RequireTool:  true,
	})
	if got.err != nil {
		t.Fatalf("got.err = %v, want nil", got.err)
	}
	if len(got.calls) != 1 {
		t.Fatalf("len(got.calls) = %d, want 1", len(got.calls))
	}
	if name := parsedCallName(t, got.calls[0]); name != "fetch_doc" {
		t.Fatalf("tool name = %q, want fetch_doc", name)
	}
	if url := parsedCallArgument(t, got.calls[0], "url"); url != "https://react.dev/reference/react/useEffectEvent" {
		t.Fatalf("fetch_doc url = %q", url)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesDocforkFetchDocFromEmbeddedMCPHistory(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"mcp__docfork__search_docs": {Name: "mcp__docfork__search_docs", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
		"mcp__docfork__fetch_doc":   {Name: "mcp__docfork__fetch_doc", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"id":"item_0","type":"mcp_tool_call","server":"docfork","tool":"search_docs","arguments":{"library":"react","query":"useEffectEvent"},"result":{"content":[{"type":"text","text":"Searched: react | 1 results\n\n[1] useEffectEvent\n    https://react.dev/reference/react/useEffectEvent\n\nUse fetch_doc on any URL above for full content."}],"structured_content":null},"error":null,"status":"completed"}`),
	}

	policy := buildExecutionPolicyWithCatalog(
		"glm-5",
		"你是测试代理。请验证 Docfork MCP：\n1) 必须使用 mcp__docfork__search_docs 搜索 react 文档中的 useEffectEvent。\n2) 必须再使用 mcp__docfork__fetch_doc 获取相关文档内容。",
		history,
		toolCatalog,
		true,
		false,
		true,
	)

	if policy.NextRequiredTool != "mcp__docfork__fetch_doc" {
		t.Fatalf("policy.NextRequiredTool = %q, want mcp__docfork__fetch_doc", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic fetch_doc call from embedded mcp_tool_call result")
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "url"); got != "https://react.dev/reference/react/useEffectEvent" {
		t.Fatalf("synthetic fetch_doc url = %q, want https://react.dev/reference/react/useEffectEvent", got)
	}
}

func TestBuildExecutionPolicy_MCPHistoryStillRequiresRemainingRead(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command":              {Name: "exec_command", Type: "function", Structured: true},
		"mcp__docfork__search_docs": {Name: "mcp__docfork__search_docs", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
		"mcp__docfork__fetch_doc":   {Name: "mcp__docfork__fetch_doc", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"id":"item_0","type":"mcp_tool_call","server":"docfork","tool":"search_docs","arguments":{"library":"react","query":"useEffectEvent"},"result":{"content":[{"type":"text","text":"Searched: react | 1 results\n\n[1] useEffectEvent\n    https://react.dev/reference/react/useEffectEvent\n\nUse fetch_doc on any URL above for full content."}],"structured_content":null},"error":null,"status":"completed"}`),
		json.RawMessage(`{"id":"item_1","type":"mcp_tool_call","server":"docfork","tool":"fetch_doc","arguments":{"url":"https://react.dev/reference/react/useEffectEvent"},"result":{"content":[{"type":"text","text":"Source: https://react.dev/reference/react/useEffectEvent\n\nuseEffectEvent lets you extract non-reactive logic into an Effect Event."}],"structured_content":null},"error":null,"status":"completed"}`),
	}

	policy := buildExecutionPolicyWithCatalog(
		"deepseek-v3p2",
		"你是资深 Go 工程师。请模拟真实 Codex 查文档任务：\n1) 必须使用 mcp__docfork__search_docs 搜索 react useEffectEvent。\n2) 必须使用 mcp__docfork__fetch_doc 获取相关页面。\n3) 阅读 README.md 的项目描述。\n4) 不要修改任何文件。",
		history,
		toolCatalog,
		true,
		false,
		true,
	)

	if policy.Stage != "verify" {
		t.Fatalf("policy.Stage = %q, want verify", policy.Stage)
	}
	if !policy.RequireTool {
		t.Fatal("policy.RequireTool = false, want true for remaining README read")
	}
	want := buildReadFileCommand("README.md")
	if policy.NextCommand != want {
		t.Fatalf("policy.NextCommand = %q, want %q", policy.NextCommand, want)
	}
}

func TestApplyExecutionPolicyToParseResult_RewritesRepeatedDocforkToRemainingRead(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command":              {Name: "exec_command", Type: "function", Structured: true},
		"mcp__docfork__search_docs": {Name: "mcp__docfork__search_docs", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
		"mcp__docfork__fetch_doc":   {Name: "mcp__docfork__fetch_doc", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
	}
	content := "重复查文档。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"mcp__docfork__search_docs\",\"arguments\":{\"library\":\"react\",\"query\":\"react useEffectEvent\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	policy := executionPolicy{
		Enabled:     true,
		Stage:       "verify",
		RequireTool: true,
		NextCommand: buildReadFileCommand("README.md"),
	}

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{})
	if got.err != nil {
		t.Fatalf("got.err = %v, want nil", got.err)
	}
	if len(got.calls) != 1 {
		t.Fatalf("len(got.calls) = %d, want 1", len(got.calls))
	}
	if command := parsedCallCommand(t, got.calls[0]); command != buildReadFileCommand("README.md") {
		t.Fatalf("command = %q, want README read", command)
	}
}

func TestApplyExecutionPolicyToParseResult_OverridesStaleDocforkToolChoiceForRemainingRead(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command":              {Name: "exec_command", Type: "function", Structured: true},
		"mcp__docfork__search_docs": {Name: "mcp__docfork__search_docs", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
		"mcp__docfork__fetch_doc":   {Name: "mcp__docfork__fetch_doc", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"id":"item_0","type":"mcp_tool_call","server":"docfork","tool":"search_docs","arguments":{"library":"react","query":"useEffectEvent"},"result":{"content":[{"type":"text","text":"Searched: react | 1 results\n\n[1] useEffectEvent\n    https://react.dev/reference/react/useEffectEvent\n\nUse fetch_doc on any URL above for full content."}]},"error":null,"status":"completed"}`),
		json.RawMessage(`{"id":"item_1","type":"mcp_tool_call","server":"docfork","tool":"fetch_doc","arguments":{"url":"https://react.dev/reference/react/useEffectEvent"},"result":{"content":[{"type":"text","text":"Source: https://react.dev/reference/react/useEffectEvent\n\nuseEffectEvent lets you extract non-reactive logic into an Effect Event."}]},"error":null,"status":"completed"}`),
	}
	policy := buildExecutionPolicyWithCatalog(
		"qwen3-vl-30b-a3b-thinking",
		"你是资深 Go 工程师。请模拟真实 Codex 查文档任务：\n1) 必须使用 mcp__docfork__search_docs 搜索 react useEffectEvent。\n2) 必须使用 mcp__docfork__fetch_doc 获取相关页面。\n3) 阅读 README.md 的项目描述。\n4) 不要修改任何文件。",
		history,
		toolCatalog,
		true,
		false,
		true,
	)
	if policy.NextRequiredTool != "" {
		t.Fatalf("policy.NextRequiredTool = %q, want empty after docfork tools completed", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want remaining README read call")
	}

	content := "继续查文档。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"mcp__docfork__search_docs\",\"arguments\":{\"library\":\"react\",\"query\":\"react useEffectEvent\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	constraints := toolProtocolConstraints{
		RequiredTool: "mcp__docfork__search_docs",
		RequireTool:  true,
	}
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, constraints)
	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, constraints)
	if got.err != nil {
		t.Fatalf("got.err = %v, want nil", got.err)
	}
	if len(got.calls) != 1 {
		t.Fatalf("len(got.calls) = %d, want 1", len(got.calls))
	}
	if command := parsedCallCommand(t, got.calls[0]); command != buildReadFileCommand("README.md") {
		t.Fatalf("command = %q, want README read", command)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSanitizesSyntheticDocforkURL(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"mcp__docfork__search_docs": {Name: "mcp__docfork__search_docs", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
		"mcp__docfork__fetch_doc":   {Name: "mcp__docfork__fetch_doc", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"search_docs","namespace":"mcp__docfork__","call_id":"call_docfork_1","arguments":"{\"library\":\"react\",\"query\":\"useEffectEvent\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_docfork_1","output":{"content":"Searched: react | 2 results\n\n[1] React docs\n    https://react.dev/reference/react/useEffectEvent#usage\\n\\n[2]\n\nUse fetch_doc on any URL above for full content.","success":true}}`),
	}

	policy := buildExecutionPolicyWithCatalog("glm-4p7", "必须使用 mcp__docfork__search_docs，然后必须使用 mcp__docfork__fetch_doc 获取文档内容。", history, toolCatalog, true, false, true)
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic fetch_doc call")
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "url"); got != "https://react.dev/reference/react/useEffectEvent#usage" {
		t.Fatalf("synthetic fetch_doc url = %q", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesDocforkSearchDocsArguments(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"mcp__docfork__search_docs": {Name: "mcp__docfork__search_docs", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
		"mcp__docfork__fetch_doc":   {Name: "mcp__docfork__fetch_doc", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
	}

	policy := buildExecutionPolicyWithCatalog(
		"deepseek-v3p1",
		"你是测试代理。请验证 Docfork MCP：\n1) 必须使用 mcp__docfork__search_docs 搜索 react 文档中的 useEffectEvent。\n2) 必须再使用 mcp__docfork__fetch_doc 获取相关文档内容。",
		nil,
		toolCatalog,
		true,
		false,
		true,
	)

	if policy.NextRequiredTool != "mcp__docfork__search_docs" {
		t.Fatalf("policy.NextRequiredTool = %q, want mcp__docfork__search_docs", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic search_docs call")
	}
	if got := parsedCallName(t, *policy.SyntheticToolCall); got != "search_docs" {
		t.Fatalf("synthetic tool name = %q, want search_docs", got)
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "library"); got != "react" {
		t.Fatalf("synthetic search_docs library = %q, want react", got)
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "query"); got != "useEffectEvent" {
		t.Fatalf("synthetic search_docs query = %q, want useEffectEvent", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesDocforkSearchDocsFromCompactPrompt(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"mcp__docfork__search_docs": {Name: "mcp__docfork__search_docs", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
		"mcp__docfork__fetch_doc":   {Name: "mcp__docfork__fetch_doc", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
	}

	policy := buildExecutionPolicyWithCatalog(
		"qwen3-vl-30b-a3b-thinking",
		"你是资深 Go 工程师。请模拟真实 Codex 查文档任务：\n1) 必须使用 mcp__docfork__search_docs 搜索 react useEffectEvent。\n2) 必须使用 mcp__docfork__fetch_doc 获取相关页面。\n3) 阅读 README.md 的项目描述。\n4) 不要修改任何文件。",
		nil,
		toolCatalog,
		true,
		false,
		true,
	)

	if policy.NextRequiredTool != "mcp__docfork__search_docs" {
		t.Fatalf("policy.NextRequiredTool = %q, want mcp__docfork__search_docs", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic search_docs call")
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "library"); got != "react" {
		t.Fatalf("synthetic search_docs library = %q, want react", got)
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "query"); got != "useEffectEvent" {
		t.Fatalf("synthetic search_docs query = %q, want useEffectEvent", got)
	}
}

func TestApplyExecutionPolicyToParseResult_RewritesDocforkSearchDocsArguments(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"mcp__docfork__search_docs": {Name: "mcp__docfork__search_docs", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
		"mcp__docfork__fetch_doc":   {Name: "mcp__docfork__fetch_doc", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
	}
	content := "先搜文档。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"mcp__docfork__search_docs\",\"arguments\":{\"query\":\"useEffectEvent\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	policy := buildExecutionPolicyWithCatalog(
		"deepseek-v3p1",
		"你是测试代理。请验证 Docfork MCP：\n1) 必须使用 mcp__docfork__search_docs 搜索 react 文档中的 useEffectEvent。\n2) 必须再使用 mcp__docfork__fetch_doc 获取相关文档内容。",
		nil,
		toolCatalog,
		true,
		false,
		true,
	)

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{})
	if got.err != nil {
		t.Fatalf("got.err = %v, want nil", got.err)
	}
	if len(got.calls) != 1 {
		t.Fatalf("len(got.calls) = %d, want 1", len(got.calls))
	}
	if name := parsedCallName(t, got.calls[0]); name != "search_docs" {
		t.Fatalf("tool name = %q, want search_docs", name)
	}
	if library := parsedCallArgument(t, got.calls[0], "library"); library != "react" {
		t.Fatalf("search_docs library = %q, want react", library)
	}
	if query := parsedCallArgument(t, got.calls[0], "query"); query != "useEffectEvent" {
		t.Fatalf("search_docs query = %q, want useEffectEvent", query)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesWebSearchCall(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"web_search": {Name: "web_search", Type: "web_search", Structured: true},
	}

	policy := buildExecutionPolicyWithCatalog(
		"gpt-oss-20b",
		"你是测试代理。请验证 web_search：\n1) 必须使用 web_search 查询 Go 官方最新稳定版本与发布日期。\n2) 禁止使用 exec_command、docfork 或其他工具代替 web_search。\n3) web_search 返回后，必须直接用四行格式收口，不要输出前言或解释工具行为。\n4) 不要修改任何文件。\n最后只输出四行：RESULT: PASS 或 FAIL；FILES: none；TEST: N/A；NOTE: 版本号与日期。",
		nil,
		toolCatalog,
		true,
		false,
		true,
	)

	if policy.NextRequiredTool != "web_search" {
		t.Fatalf("policy.NextRequiredTool = %q, want web_search", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic web_search call")
	}
	var item map[string]any
	if err := json.Unmarshal(policy.SyntheticToolCall.item, &item); err != nil {
		t.Fatalf("decode synthetic web_search item: %v", err)
	}
	if got, _ := item["type"].(string); got != "web_search_call" {
		t.Fatalf("synthetic item type = %q, want web_search_call", got)
	}
	action, _ := item["action"].(map[string]any)
	if got := strings.TrimSpace(asString(action["query"])); got != "latest Go release" {
		t.Fatalf("synthetic web_search query = %q, want latest Go release", got)
	}
}

func TestApplyExecutionPolicyToParseResult_RewritesNarrationOnlyToSyntheticWebSearch(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"web_search": {Name: "web_search", Type: "web_search", Structured: true},
	}
	parseResult := parseToolCallOutputsWithConstraints(
		"我会使用 web_search 查询 Go 官方最新稳定版本与发布日期。",
		toolCatalog,
		toolProtocolConstraints{RequireTool: true, RequiredTool: "web_search"},
	)
	policy := buildExecutionPolicyWithCatalog(
		"gpt-oss-20b",
		"你是测试代理。请验证 web_search：\n1) 必须使用 web_search 查询 Go 官方最新稳定版本与发布日期。\n2) 禁止使用 exec_command、docfork 或其他工具代替 web_search。\n3) web_search 返回后，必须直接用四行格式收口，不要输出前言或解释工具行为。\n4) 不要修改任何文件。\n最后只输出四行：RESULT: PASS 或 FAIL；FILES: none；TEST: N/A；NOTE: 版本号与日期。",
		nil,
		toolCatalog,
		true,
		false,
		true,
	)

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{RequireTool: true, RequiredTool: "web_search"})
	if got.err != nil {
		t.Fatalf("got.err = %v, want nil", got.err)
	}
	if len(got.calls) != 1 {
		t.Fatalf("len(got.calls) = %d, want 1", len(got.calls))
	}
	if got.mode != toolProtocolModeAIActionsTool {
		t.Fatalf("got.mode = %q, want %q", got.mode, toolProtocolModeAIActionsTool)
	}
	var item map[string]any
	if err := json.Unmarshal(got.calls[0].item, &item); err != nil {
		t.Fatalf("decode rewritten web_search item: %v", err)
	}
	if typ, _ := item["type"].(string); typ != "web_search_call" {
		t.Fatalf("tool item type = %q, want web_search_call", typ)
	}
	action, _ := item["action"].(map[string]any)
	if query := strings.TrimSpace(asString(action["query"])); query != "latest Go release" {
		t.Fatalf("web_search query = %q, want latest Go release", query)
	}
}

func TestExtractExplicitToolMentions_IgnoresNegatedWebSearchMention(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"web_search":                {Name: "web_search", Type: "web_search", Structured: true},
		"mcp__docfork__search_docs": {Name: "mcp__docfork__search_docs", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
		"mcp__docfork__fetch_doc":   {Name: "mcp__docfork__fetch_doc", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
	}

	got := extractExplicitToolMentions("必须使用 mcp__docfork__search_docs 搜索 react 文档中的 useEffectEvent，然后必须使用 mcp__docfork__fetch_doc 获取相关文档内容，禁止使用 web_search 代替 Docfork。", toolCatalog)
	want := []string{"mcp__docfork__search_docs", "mcp__docfork__fetch_doc"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit tools mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestExtractExplicitToolMentions_PreservesRepeatedJSReplMentions(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"js_repl":       {Name: "js_repl", Type: "custom", Structured: false},
		"js_repl_reset": {Name: "js_repl_reset", Type: "function", Structured: true},
	}

	got := extractExplicitToolMentions("必须先使用 js_repl 计算数组和，然后调用 js_repl_reset，再次使用 js_repl 计算 7 * 8。", toolCatalog)
	want := []string{"js_repl", "js_repl_reset", "js_repl"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit tools mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestExtractExplicitToolMentions_IgnoresDescriptiveJSReplHeadingAndNegatedAlias(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"js_repl":       {Name: "js_repl", Type: "custom", Structured: false},
		"js_repl_reset": {Name: "js_repl_reset", Type: "function", Structured: true},
		"exec_command":  {Name: "exec_command", Type: "function", Structured: true},
	}

	task := "你是测试代理。请验证 js_repl：\n" +
		"1) 必须先使用 js_repl 计算数组 [2,3,5] 的和。\n" +
		"2) 然后调用 js_repl_reset。\n" +
		"3) 再次使用 js_repl 计算 7 * 8。\n" +
		"4) 不要使用 exec_command 代替 js_repl。\n" +
		"5) 不要修改任何文件。"

	got := extractExplicitToolMentions(task, toolCatalog)
	want := []string{"js_repl", "js_repl_reset", "js_repl"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit tools mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestExtractExplicitToolMentions_IgnoresOutputLabelJSReplHint(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"js_repl":       {Name: "js_repl", Type: "custom", Structured: false},
		"js_repl_reset": {Name: "js_repl_reset", Type: "function", Structured: true},
		"exec_command":  {Name: "exec_command", Type: "function", Structured: true},
	}

	task := "你是测试代理。请验证 js_repl：\n" +
		"1) 必须先使用 js_repl 计算数组 [2,3,5] 的和。\n" +
		"2) 然后调用 js_repl_reset。\n" +
		"3) 再次使用 js_repl 计算 7 * 8。\n" +
		"4) 不要使用 exec_command 代替 js_repl。\n" +
		"5) 不要修改任何文件。\n" +
		"最后只输出四行：RESULT: PASS 或 FAIL；FILES: none；TEST: N/A；NOTE: js_repl 两次计算结果。"

	got := extractExplicitToolMentions(task, toolCatalog)
	want := []string{"js_repl", "js_repl_reset", "js_repl"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit tools mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesMCPResourceTemplates(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"list_mcp_resources":          {Name: "list_mcp_resources", Type: "function", Structured: true},
		"list_mcp_resource_templates": {Name: "list_mcp_resource_templates", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"list_mcp_resources","call_id":"call_mcp_1","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_mcp_1","output":{"content":"{\"resources\":[]}","success":true}}`),
	}

	policy := buildExecutionPolicyWithCatalog("glm-4p7", "必须调用 list_mcp_resources。必须调用 list_mcp_resource_templates。", history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "list_mcp_resource_templates" {
		t.Fatalf("policy.NextRequiredTool = %q, want list_mcp_resource_templates", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic list_mcp_resource_templates call")
	}
	if got := parsedCallName(t, *policy.SyntheticToolCall); got != "list_mcp_resource_templates" {
		t.Fatalf("synthetic tool name = %q, want list_mcp_resource_templates", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesJSReplReset(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"js_repl":       {Name: "js_repl", Type: "custom", Structured: false},
		"js_repl_reset": {Name: "js_repl_reset", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"custom_tool_call","name":"js_repl","call_id":"call_js_1","input":"[2,3,5].reduce((a,b)=>a+b,0)"}`),
		json.RawMessage(`{"type":"custom_tool_call_output","call_id":"call_js_1","output":{"content":"10","success":true}}`),
	}

	policy := buildExecutionPolicyWithCatalog("glm-4p7", "必须先使用 js_repl。然后调用 js_repl_reset。", history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "js_repl_reset" {
		t.Fatalf("policy.NextRequiredTool = %q, want js_repl_reset", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic js_repl_reset call")
	}
	if got := parsedCallName(t, *policy.SyntheticToolCall); got != "js_repl_reset" {
		t.Fatalf("synthetic tool name = %q, want js_repl_reset", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesInitialJSRepl(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"js_repl":       {Name: "js_repl", Type: "custom", Structured: false},
		"js_repl_reset": {Name: "js_repl_reset", Type: "function", Structured: true},
	}
	task := "你是测试代理。请验证 js_repl：\n" +
		"1) 必须先使用 js_repl 计算数组 [2,3,5] 的和。\n" +
		"2) 然后调用 js_repl_reset。\n" +
		"3) 再次使用 js_repl 计算 7 * 8。\n" +
		"4) 不要使用 exec_command 代替 js_repl。"

	policy := buildExecutionPolicyWithCatalog("gpt-oss-20b", task, nil, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "js_repl" {
		t.Fatalf("policy.NextRequiredTool = %q, want js_repl", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic initial js_repl call")
	}
	if got := parsedCallName(t, *policy.SyntheticToolCall); got != "js_repl" {
		t.Fatalf("synthetic tool name = %q, want js_repl", got)
	}
	if got := parsedCallInput(t, *policy.SyntheticToolCall); got != "[2,3,5].reduce((a,b)=>a+b,0)" {
		t.Fatalf("synthetic tool input = %q, want array sum expression", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesFollowupJSRepl(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"js_repl":       {Name: "js_repl", Type: "custom", Structured: false},
		"js_repl_reset": {Name: "js_repl_reset", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"custom_tool_call","name":"js_repl","call_id":"call_js_1","input":"[2,3,5].reduce((a,b)=>a+b,0)"}`),
		json.RawMessage(`{"type":"custom_tool_call_output","call_id":"call_js_1","output":{"content":"10","success":true}}`),
		json.RawMessage(`{"type":"function_call","name":"js_repl_reset","call_id":"call_js_2","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_js_2","output":{"content":"reset","success":true}}`),
	}

	policy := buildExecutionPolicyWithCatalog("glm-4p7", "必须先使用 js_repl 计算数组和，然后调用 js_repl_reset，再次使用 js_repl 计算 7 * 8。", history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "js_repl" {
		t.Fatalf("policy.NextRequiredTool = %q, want js_repl", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic js_repl call")
	}
	if got := parsedCallName(t, *policy.SyntheticToolCall); got != "js_repl" {
		t.Fatalf("synthetic tool name = %q, want js_repl", got)
	}
	if got := parsedCallInput(t, *policy.SyntheticToolCall); got != "7 * 8" {
		t.Fatalf("synthetic tool input = %q, want 7 * 8", got)
	}
}

func TestApplyExecutionPolicyToParseResult_RewritesMismatchedFollowupJSReplToSynthetic(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"js_repl":       {Name: "js_repl", Type: "custom", Structured: false},
		"js_repl_reset": {Name: "js_repl_reset", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"custom_tool_call","name":"js_repl","call_id":"call_js_1","input":"[2,3,5].reduce((a,b)=>a+b,0)"}`),
		json.RawMessage(`{"type":"custom_tool_call_output","call_id":"call_js_1","output":{"content":"10","success":true}}`),
		json.RawMessage(`{"type":"function_call","name":"js_repl_reset","call_id":"call_js_2","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_js_2","output":{"content":"reset","success":true}}`),
	}

	policy := buildExecutionPolicyWithCatalog("glm-4p7", "必须先使用 js_repl 计算数组和，然后调用 js_repl_reset，再次使用 js_repl 计算 7 * 8。", history, toolCatalog, true, false, true)
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic js_repl call")
	}

	content := "继续执行。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"js_repl\",\"input\":\"[2,3,5].reduce((a,b)=>a+b,0)\"}]}\n<<<END_AI_ACTIONS_V1>>>"
	constraints := toolProtocolConstraints{RequiredTool: "js_repl"}
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, constraints)

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, constraints)
	if got.err != nil {
		t.Fatalf("got.err = %v, want nil", got.err)
	}
	if len(got.calls) != 1 {
		t.Fatalf("len(got.calls) = %d, want 1", len(got.calls))
	}
	if gotName := parsedCallName(t, got.calls[0]); gotName != "js_repl" {
		t.Fatalf("tool name = %q, want js_repl", gotName)
	}
	if gotInput := parsedCallInput(t, got.calls[0]); gotInput != "7 * 8" {
		t.Fatalf("tool input = %q, want 7 * 8", gotInput)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceIgnoresDuplicateObservedCallIDs(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"js_repl":       {Name: "js_repl", Type: "custom", Structured: false},
		"js_repl_reset": {Name: "js_repl_reset", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"custom_tool_call","name":"js_repl","call_id":"call_js_1","input":"[2,3,5].reduce((a,b)=>a+b,0)"}`),
		json.RawMessage(`{"type":"custom_tool_call_output","call_id":"call_js_1","output":{"content":"10","success":true}}`),
		json.RawMessage(`{"type":"function_call","name":"js_repl_reset","call_id":"call_js_2","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_js_2","output":{"content":"reset","success":true}}`),
		json.RawMessage(`{"type":"custom_tool_call","name":"js_repl","call_id":"call_js_1","input":"[2,3,5].reduce((a,b)=>a+b,0)"}`),
	}

	policy := buildExecutionPolicyWithCatalog("glm-4p7", "必须先使用 js_repl 计算数组和，然后调用 js_repl_reset，再次使用 js_repl 计算 7 * 8。", history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "js_repl" {
		t.Fatalf("policy.NextRequiredTool = %q, want js_repl", policy.NextRequiredTool)
	}
	if policy.Stage != "execute" {
		t.Fatalf("policy.Stage = %q, want execute", policy.Stage)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic js_repl call")
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceRequiresMatchingFollowupJSReplInput(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"js_repl":       {Name: "js_repl", Type: "custom", Structured: false},
		"js_repl_reset": {Name: "js_repl_reset", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"custom_tool_call","name":"js_repl","call_id":"call_js_1","input":"[2,3,5].reduce((a,b)=>a+b,0)"}`),
		json.RawMessage(`{"type":"custom_tool_call_output","call_id":"call_js_1","output":{"content":"10","success":true}}`),
		json.RawMessage(`{"type":"function_call","name":"js_repl_reset","call_id":"call_js_2","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_js_2","output":{"content":"reset","success":true}}`),
		json.RawMessage(`{"type":"custom_tool_call","name":"js_repl","call_id":"call_js_3","input":"[2,3,5].reduce((a,b)=>a+b,0)"}`),
		json.RawMessage(`{"type":"custom_tool_call_output","call_id":"call_js_3","output":{"content":"10","success":true}}`),
	}

	policy := buildExecutionPolicyWithCatalog("glm-4p7", "必须先使用 js_repl 计算数组和，然后调用 js_repl_reset，再次使用 js_repl 计算 7 * 8。", history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "js_repl" {
		t.Fatalf("policy.NextRequiredTool = %q, want js_repl", policy.NextRequiredTool)
	}
	if policy.Stage != "execute" {
		t.Fatalf("policy.Stage = %q, want execute", policy.Stage)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic js_repl call")
	}
	if got := parsedCallInput(t, *policy.SyntheticToolCall); got != "7 * 8" {
		t.Fatalf("synthetic tool input = %q, want 7 * 8", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceCompletesAfterMatchingFollowupJSRepl(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"js_repl":       {Name: "js_repl", Type: "custom", Structured: false},
		"js_repl_reset": {Name: "js_repl_reset", Type: "function", Structured: true},
		"exec_command":  {Name: "exec_command", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"custom_tool_call","name":"js_repl","call_id":"call_js_1","input":"[2,3,5].reduce((a,b) => a + b, 0)"}`),
		json.RawMessage(`{"type":"custom_tool_call_output","call_id":"call_js_1","output":{"content":"10","success":true}}`),
		json.RawMessage(`{"type":"function_call","name":"js_repl_reset","call_id":"call_js_2","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_js_2","output":"js_repl kernel reset"}`),
		json.RawMessage(`{"type":"custom_tool_call","name":"js_repl","call_id":"call_js_3","input":"7 * 8"}`),
		json.RawMessage(`{"type":"custom_tool_call_output","call_id":"call_js_3","output":{"content":"56","success":true}}`),
	}
	task := "你是测试代理。请验证 js_repl：\n" +
		"1) 必须先使用 js_repl 计算数组 [2,3,5] 的和。\n" +
		"2) 然后调用 js_repl_reset。\n" +
		"3) 再次使用 js_repl 计算 7 * 8。\n" +
		"4) 不要使用 exec_command 代替 js_repl。\n" +
		"5) 不要修改任何文件。"

	policy := buildExecutionPolicyWithCatalog("glm-4p7", task, history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "" {
		t.Fatalf("policy.NextRequiredTool = %q, want empty", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall != nil {
		t.Fatalf("policy.SyntheticToolCall = %#v, want nil", policy.SyntheticToolCall)
	}
	if policy.Stage != "finalize" {
		t.Fatalf("policy.Stage = %q, want finalize", policy.Stage)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceCompletesAfterMatchingFollowupJSReplWithOutputLabels(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"js_repl":       {Name: "js_repl", Type: "custom", Structured: false},
		"js_repl_reset": {Name: "js_repl_reset", Type: "function", Structured: true},
		"exec_command":  {Name: "exec_command", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"custom_tool_call","name":"js_repl","call_id":"call_js_1","input":"[2,3,5].reduce((a,b) => a + b, 0)"}`),
		json.RawMessage(`{"type":"custom_tool_call_output","call_id":"call_js_1","output":{"content":"10","success":true}}`),
		json.RawMessage(`{"type":"function_call","name":"js_repl_reset","call_id":"call_js_2","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_js_2","output":"js_repl kernel reset"}`),
		json.RawMessage(`{"type":"custom_tool_call","name":"js_repl","call_id":"call_js_3","input":"7 * 8"}`),
		json.RawMessage(`{"type":"custom_tool_call_output","call_id":"call_js_3","output":{"content":"56","success":true}}`),
	}
	task := "你是测试代理。请验证 js_repl：\n" +
		"1) 必须先使用 js_repl 计算数组 [2,3,5] 的和。\n" +
		"2) 然后调用 js_repl_reset。\n" +
		"3) 再次使用 js_repl 计算 7 * 8。\n" +
		"4) 不要使用 exec_command 代替 js_repl。\n" +
		"5) 不要修改任何文件。\n" +
		"最后只输出四行：RESULT: PASS 或 FAIL；FILES: none；TEST: N/A；NOTE: js_repl 两次计算结果。"

	policy := buildExecutionPolicyWithCatalog("glm-4p7", task, history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "" {
		t.Fatalf("policy.NextRequiredTool = %q, want empty", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall != nil {
		t.Fatalf("policy.SyntheticToolCall = %#v, want nil", policy.SyntheticToolCall)
	}
	if policy.Stage != "finalize" {
		t.Fatalf("policy.Stage = %q, want finalize", policy.Stage)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesChromeTakeSnapshot(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"mcp__chrome_devtools__new_page":      {Name: "mcp__chrome_devtools__new_page", Type: "function", Structured: true, Namespace: "mcp__chrome_devtools__"},
		"mcp__chrome_devtools__take_snapshot": {Name: "mcp__chrome_devtools__take_snapshot", Type: "function", Structured: true, Namespace: "mcp__chrome_devtools__"},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"new_page","namespace":"mcp__chrome_devtools__","call_id":"call_chrome_1","arguments":"{\"url\":\"data:text/html,<button>Go</button>\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_chrome_1","output":{"content":"## Pages\n1: about:blank\n2: data:text/html,<button>Go</button> [selected]","success":true}}`),
	}

	policy := buildExecutionPolicyWithCatalog("glm-4p7", "必须使用 mcp__chrome_devtools__new_page 打开页面，然后使用 take_snapshot。", history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "mcp__chrome_devtools__take_snapshot" {
		t.Fatalf("policy.NextRequiredTool = %q, want mcp__chrome_devtools__take_snapshot", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic take_snapshot call")
	}
	if got := parsedCallName(t, *policy.SyntheticToolCall); got != "take_snapshot" {
		t.Fatalf("synthetic tool name = %q, want take_snapshot", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesExecCommand(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"update_plan":  {Name: "update_plan", Type: "function", Structured: true},
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"update_plan","call_id":"call_plan_1","arguments":"{\"explanation\":\"先做计划\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_plan_1","output":{"content":"Plan updated","success":true}}`),
	}
	task := "1) 必须先使用 update_plan。\n2) 然后必须使用 exec_command 执行 `sed -n '1,5p' README.md`。\n3) 最后再总结。"

	policy := buildExecutionPolicyWithCatalog("glm-4p7", task, history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "exec_command" {
		t.Fatalf("policy.NextRequiredTool = %q, want exec_command", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic exec_command call")
	}
	if got := parsedCallCommand(t, *policy.SyntheticToolCall); got != "sed -n '1,5p' README.md" {
		t.Fatalf("synthetic exec_command = %q", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesUpdatePlan(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"update_plan":  {Name: "update_plan", Type: "function", Structured: true},
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	task := "你是测试代理。请在当前仓库完成一个计划驱动的只读任务：\n" +
		"1) 必须先调用 update_plan。\n" +
		"2) update_plan 的 arguments 顶层字段必须叫 plan，不允许使用 steps。\n" +
		"3) plan 里只写两个步骤：Inspect README.md、Reply with summary。\n" +
		"4) 然后必须使用 exec_command 执行 `head -n 3 README.md`。\n" +
		"5) 不要修改任何文件。"

	policy := buildExecutionPolicyWithCatalog("gpt-oss-20b", task, nil, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "update_plan" {
		t.Fatalf("policy.NextRequiredTool = %q, want update_plan", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic update_plan call")
	}
	if got := parsedCallName(t, *policy.SyntheticToolCall); got != "update_plan" {
		t.Fatalf("synthetic tool name = %q, want update_plan", got)
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "explanation"); got == "" {
		t.Fatal("synthetic update_plan explanation is empty")
	}
	if got := parsedCallArgumentListOfObjects(t, *policy.SyntheticToolCall, "plan"); !reflect.DeepEqual(got, []map[string]string{
		{"step": "Inspect README.md", "status": "in_progress"},
		{"step": "Reply with summary", "status": "pending"},
	}) {
		t.Fatalf("synthetic update_plan plan = %#v", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesContext7ResolveAndGetDocs(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"mcp__context7__resolve-library-id": {Name: "mcp__context7__resolve-library-id", Type: "function", Structured: true, Namespace: "mcp__context7__"},
		"mcp__context7__get-library-docs":   {Name: "mcp__context7__get-library-docs", Type: "function", Structured: true, Namespace: "mcp__context7__"},
	}
	task := "你是测试代理。请验证 Context7 MCP：\n" +
		"1) 必须使用 mcp__context7__resolve-library-id 查找 react。\n" +
		"2) 必须再使用 mcp__context7__get-library-docs 获取 useEffectEvent 相关文档。\n" +
		"3) 禁止使用 exec_command、docfork 或其他工具代替 Context7。"

	resolvePolicy := buildExecutionPolicyWithCatalog("glm-4p7", task, nil, toolCatalog, true, false, true)
	if resolvePolicy.NextRequiredTool != "mcp__context7__resolve-library-id" {
		t.Fatalf("resolvePolicy.NextRequiredTool = %q, want mcp__context7__resolve-library-id", resolvePolicy.NextRequiredTool)
	}
	if resolvePolicy.SyntheticToolCall == nil {
		t.Fatal("resolvePolicy.SyntheticToolCall = nil, want synthetic context7 resolve call")
	}
	if got := parsedCallName(t, *resolvePolicy.SyntheticToolCall); got != "resolve-library-id" {
		t.Fatalf("synthetic tool name = %q, want resolve-library-id", got)
	}
	if got := parsedCallArgument(t, *resolvePolicy.SyntheticToolCall, "libraryName"); got != "react" {
		t.Fatalf("synthetic context7 libraryName = %q, want react", got)
	}

	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"resolve-library-id","namespace":"mcp__context7__","call_id":"call_ctx_1","arguments":"{\"libraryName\":\"react\",\"query\":\"react\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_ctx_1","output":{"content":"Available Libraries:\n- Title: React\n- Context7-compatible library ID: /reactjs/react.dev\n","success":true}}`),
	}
	docsPolicy := buildExecutionPolicyWithCatalog("glm-4p7", task, history, toolCatalog, true, false, true)
	if docsPolicy.NextRequiredTool != "mcp__context7__get-library-docs" {
		t.Fatalf("docsPolicy.NextRequiredTool = %q, want mcp__context7__get-library-docs", docsPolicy.NextRequiredTool)
	}
	if docsPolicy.SyntheticToolCall == nil {
		t.Fatal("docsPolicy.SyntheticToolCall = nil, want synthetic context7 get docs call")
	}
	if got := parsedCallName(t, *docsPolicy.SyntheticToolCall); got != "get-library-docs" {
		t.Fatalf("synthetic tool name = %q, want get-library-docs", got)
	}
	if got := parsedCallArgument(t, *docsPolicy.SyntheticToolCall, "context7CompatibleLibraryID"); got != "/reactjs/react.dev" {
		t.Fatalf("synthetic context7 library ID = %q, want /reactjs/react.dev", got)
	}
	if got := parsedCallArgument(t, *docsPolicy.SyntheticToolCall, "topic"); got != "useEffectEvent" {
		t.Fatalf("synthetic context7 topic = %q, want useEffectEvent", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesInteractiveExecCommandWithTTY(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		"write_stdin":  {Name: "write_stdin", Type: "function", Structured: true},
	}
	task := "你是测试代理。请验证交互式 shell 会话能力：\n" +
		"1) 必须使用 exec_command 启动一个交互式 python3 会话。\n" +
		"2) 必须使用 write_stdin 向同一 session 发送 print(2 + 3) 与 exit()。\n" +
		"3) 禁止使用 python3 -c、here-doc 或一次性命令替代 write_stdin。"

	policy := buildExecutionPolicyWithCatalog("glm-4p7", task, nil, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "exec_command" {
		t.Fatalf("policy.NextRequiredTool = %q, want exec_command", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic exec_command call")
	}
	if got := parsedCallName(t, *policy.SyntheticToolCall); got != "exec_command" {
		t.Fatalf("synthetic tool name = %q, want exec_command", got)
	}
	if got := parsedCallCommand(t, *policy.SyntheticToolCall); got != "python3" {
		t.Fatalf("synthetic exec_command cmd = %q, want python3", got)
	}
	if !parsedCallBoolArgument(t, *policy.SyntheticToolCall, "tty") {
		t.Fatal("synthetic exec_command tty = false, want true")
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "yield_time_ms"); got != "1000" {
		t.Fatalf("synthetic exec_command yield_time_ms = %q, want 1000", got)
	}
}

func TestApplyExecutionPolicyToParseResult_SynthesizesInteractiveExecCommandWhenTTYMissing(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		"write_stdin":  {Name: "write_stdin", Type: "function", Structured: true},
	}
	task := "1) 必须使用 exec_command 启动一个交互式 python3 会话。\n2) 必须使用 write_stdin 向同一 session 发送 print(2 + 3) 与 exit()。"

	policy := buildExecutionPolicyWithCatalog("glm-4p7", task, nil, toolCatalog, true, false, true)
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic exec_command call")
	}

	content := "<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"python3\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	constraints := toolProtocolConstraints{RequiredTool: "exec_command"}
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, constraints)
	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, constraints)

	if got.err != nil {
		t.Fatalf("got.err = %v, want nil", got.err)
	}
	if len(got.calls) != 1 {
		t.Fatalf("len(got.calls) = %d, want 1", len(got.calls))
	}
	if gotName := parsedCallName(t, got.calls[0]); gotName != "exec_command" {
		t.Fatalf("tool name = %q, want exec_command", gotName)
	}
	if gotCmd := parsedCallCommand(t, got.calls[0]); gotCmd != "python3" {
		t.Fatalf("tool cmd = %q, want python3", gotCmd)
	}
	if !parsedCallBoolArgument(t, got.calls[0], "tty") {
		t.Fatal("rewritten exec_command tty = false, want true")
	}
	if gotYield := parsedCallArgument(t, got.calls[0], "yield_time_ms"); gotYield != "1000" {
		t.Fatalf("rewritten exec_command yield_time_ms = %q, want 1000", gotYield)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesWriteStdin(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		"write_stdin":  {Name: "write_stdin", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"exec_command","call_id":"call_shell_1","arguments":"{\"cmd\":\"python3\",\"tty\":true}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_shell_1","output":{"content":"Process running with session ID 19834","session_id":19834,"success":true}}`),
	}
	task := "你是测试代理。请验证交互式 shell 会话能力：\n" +
		"1) 必须使用 exec_command 启动一个交互式 python3 会话。\n" +
		"2) 必须使用 write_stdin 向同一 session 发送 print(2 + 3) 与 exit()。\n" +
		"3) 禁止使用 python3 -c、here-doc 或一次性命令替代 write_stdin。\n" +
		"4) 不要修改任何文件。\n" +
		"最后只输出四行：RESULT: PASS 或 FAIL；FILES: none；TEST: N/A；NOTE: 交互式会话结果。"

	policy := buildExecutionPolicyWithCatalog("glm-4p7", task, history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "write_stdin" {
		t.Fatalf("policy.NextRequiredTool = %q, want write_stdin", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic write_stdin call")
	}
	if got := parsedCallName(t, *policy.SyntheticToolCall); got != "write_stdin" {
		t.Fatalf("synthetic tool name = %q, want write_stdin", got)
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "session_id"); got != "19834" {
		t.Fatalf("synthetic write_stdin session_id = %q, want 19834", got)
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "chars"); got != "print(2 + 3)\nexit()" {
		t.Fatalf("synthetic write_stdin chars = %q, want interactive follow-up", got)
	}
}

func TestApplyExecutionPolicyToParseResult_SynthesizesWriteStdinWhenRequiredToolIsMissed(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		"write_stdin":  {Name: "write_stdin", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"exec_command","call_id":"call_shell_1","arguments":"{\"cmd\":\"python3\",\"tty\":true}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_shell_1","output":{"content":"Process running with session ID 19834","session_id":19834,"success":true}}`),
	}
	task := "1) 必须使用 exec_command 启动一个交互式 python3 会话。\n2) 必须使用 write_stdin 向同一 session 发送 print(2 + 3) 与 exit()。"

	policy := buildExecutionPolicyWithCatalog("glm-4p7", task, history, toolCatalog, true, false, true)
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic write_stdin call")
	}

	content := "<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"python3\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	constraints := toolProtocolConstraints{RequiredTool: "write_stdin"}
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, constraints)
	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, constraints)

	if got.err != nil {
		t.Fatalf("got.err = %v, want nil", got.err)
	}
	if len(got.calls) != 1 {
		t.Fatalf("len(got.calls) = %d, want 1", len(got.calls))
	}
	if gotName := parsedCallName(t, got.calls[0]); gotName != "write_stdin" {
		t.Fatalf("tool name = %q, want write_stdin", gotName)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceKeepsWriteStdinRequiredAfterFailedAttempt(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		"write_stdin":  {Name: "write_stdin", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"exec_command","call_id":"call_shell_1","arguments":"{\"cmd\":\"python3\",\"tty\":true}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_shell_1","output":"Process running with session ID 88590"}`),
		json.RawMessage(`{"type":"function_call","name":"write_stdin","call_id":"call_stdin_1","arguments":"{\"input\":\"exit()\\n\",\"session_id\":\"0\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_stdin_1","output":"failed to parse function arguments: invalid type: string \"0\", expected i32 at line 1 column 36"}`),
	}
	task := "1) 必须使用 exec_command 启动一个交互式 python3 会话。\n2) 必须使用 write_stdin 向同一 session 发送 print(2 + 3) 与 exit()。"

	policy := buildExecutionPolicyWithCatalog("gpt-oss-20b", task, history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "write_stdin" {
		t.Fatalf("policy.NextRequiredTool = %q, want write_stdin after failed write_stdin output", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want repaired synthetic write_stdin call")
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "session_id"); got != "88590" {
		t.Fatalf("synthetic write_stdin session_id = %q, want 88590", got)
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "chars"); got != "print(2 + 3)\nexit()" {
		t.Fatalf("synthetic write_stdin chars = %q, want interactive follow-up", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceExtractsSessionIDFromWrappedExecOutput(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		"write_stdin":  {Name: "write_stdin", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"exec_command","call_id":"call_shell_1","arguments":"{\"cmd\":\"python3\",\"tty\":true}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_shell_1","output":"Chunk ID: abc123\nWall time: 0.0000 seconds\nProcess running with session ID 45776\nOutput:\nPython 3.12.0\n>>> "}`),
	}
	task := "1) 必须使用 exec_command 启动一个交互式 python3 会话。\n2) 必须使用 write_stdin 向同一 session 发送 print(2 + 3) 与 exit()。"

	policy := buildExecutionPolicyWithCatalog("gpt-oss-20b", task, history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "write_stdin" {
		t.Fatalf("policy.NextRequiredTool = %q, want write_stdin", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic write_stdin call")
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "session_id"); got != "45776" {
		t.Fatalf("synthetic write_stdin session_id = %q, want 45776", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceRepairsWriteStdinRuntimeSessionID(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		"write_stdin":  {Name: "write_stdin", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"exec_command","call_id":"call_shell_1","arguments":"{\"cmd\":\"python3\",\"tty\":true}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_shell_1","output":"Chunk ID: abc123\nWall time: 0.0000 seconds\nProcess running with session ID 45776\nOutput:\nPython 3.12.0\n>>> "}`),
		json.RawMessage(`{"type":"function_call","name":"write_stdin","call_id":"call_stdin_1","arguments":"{\"chars\":\"exit()\\n\",\"session_id\":1}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_stdin_1","output":"write_stdin failed: Unknown process id 1"}`),
	}
	task := "1) 必须使用 exec_command 启动一个交互式 python3 会话。\n2) 必须使用 write_stdin 向同一 session 发送 print(2 + 3) 与 exit()。"

	policy := buildExecutionPolicyWithCatalog("gpt-oss-20b", task, history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "write_stdin" {
		t.Fatalf("policy.NextRequiredTool = %q, want write_stdin after runtime failure", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want repaired synthetic write_stdin call")
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "session_id"); got != "45776" {
		t.Fatalf("synthetic write_stdin session_id = %q, want 45776", got)
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "chars"); got != "print(2 + 3)\nexit()" {
		t.Fatalf("synthetic write_stdin chars = %q, want interactive follow-up", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesApplyPatch(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		"apply_patch":  {Name: "apply_patch", Type: "custom", Structured: false},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"exec_command","call_id":"call_read_1","arguments":"{\"cmd\":\"sed -n '1,200p' 'tmp/apply_patch_probe.txt'\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_read_1","output":{"content":"alpha\n","success":true}}`),
	}
	task := "你是测试代理。请验证 apply_patch：\n" +
		"1) 先使用 exec_command 读取 tmp/apply_patch_probe.txt。\n" +
		"2) 必须使用 apply_patch，把文件中的 alpha 改为 beta。\n" +
		"3) 禁止使用 exec_command + python/sed/perl/cat 重写文件代替 apply_patch。\n" +
		"4) 不要执行测试，也不要修改其他文件。\n" +
		"最后只输出四行：RESULT: PASS 或 FAIL；FILES: tmp/apply_patch_probe.txt；TEST: N/A；NOTE: 是否真正使用了 apply_patch。"

	policy := buildExecutionPolicyWithCatalog("glm-4p7", task, history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "apply_patch" {
		t.Fatalf("policy.NextRequiredTool = %q, want apply_patch", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic apply_patch command")
	}
	if got := parsedCallName(t, *policy.SyntheticToolCall); got != "exec_command" {
		t.Fatalf("synthetic tool name = %q, want exec_command", got)
	}
	if got := parsedCallCommand(t, *policy.SyntheticToolCall); !strings.Contains(got, "| apply_patch") || !strings.Contains(got, "*** Update File: tmp/apply_patch_probe.txt") || !strings.Contains(got, "-alpha") || !strings.Contains(got, "+beta") {
		t.Fatalf("synthetic apply_patch command = %q, want minimal alpha->beta patch command", got)
	}
}

func TestApplyExecutionPolicyToParseResult_SynthesizesApplyPatchWhenRequiredToolIsMissed(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		"apply_patch":  {Name: "apply_patch", Type: "custom", Structured: false},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"exec_command","call_id":"call_read_1","arguments":"{\"cmd\":\"sed -n '1,20p' tmp/apply_patch_probe.txt\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_read_1","output":{"content":"alpha\n","success":true}}`),
	}
	task := "你是测试代理。请验证 apply_patch：\n" +
		"1) 先使用 exec_command 读取 tmp/apply_patch_probe.txt。\n" +
		"2) 必须使用 apply_patch，把文件中的 alpha 改为 beta。\n" +
		"3) 禁止使用 exec_command + python/sed/perl/cat 重写文件代替 apply_patch。\n" +
		"4) 不要执行测试，也不要修改其他文件。\n" +
		"最后只输出四行：RESULT: PASS 或 FAIL；FILES: tmp/apply_patch_probe.txt；TEST: N/A；NOTE: 是否真正使用了 apply_patch。"

	policy := buildExecutionPolicyWithCatalog("glm-4p7", task, history, toolCatalog, true, false, true)
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic apply_patch call")
	}

	content := "<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"cat tmp/apply_patch_probe.txt\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	constraints := toolProtocolConstraints{RequiredTool: "apply_patch"}
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, constraints)
	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, constraints)

	if got.err != nil {
		t.Fatalf("got.err = %v, want nil", got.err)
	}
	if len(got.calls) != 1 {
		t.Fatalf("len(got.calls) = %d, want 1", len(got.calls))
	}
	if gotName := parsedCallName(t, got.calls[0]); gotName != "exec_command" {
		t.Fatalf("tool name = %q, want exec_command", gotName)
	}
	if command := parsedCallCommand(t, got.calls[0]); !strings.Contains(command, "| apply_patch") {
		t.Fatalf("tool command = %q, want apply_patch command", command)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceDefersApplyPatchUntilReadSeeded(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		"apply_patch":  {Name: "apply_patch", Type: "custom", Structured: false},
	}
	task := "你是测试代理。请验证 apply_patch：\n" +
		"1) 先读取 internal/codexfixture/patchprobe/message.txt。\n" +
		"2) 必须使用 apply_patch，把文件中的 alpha 改为 beta。\n" +
		"3) 禁止使用 exec_command + python/sed/perl/cat 重写文件代替 apply_patch。"

	policy := buildExecutionPolicyWithCatalog("glm-4p7", task, nil, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "" {
		t.Fatalf("policy.NextRequiredTool = %q, want empty before initial read", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall != nil {
		t.Fatalf("policy.SyntheticToolCall = %#v, want nil before initial read", policy.SyntheticToolCall)
	}
	if got := policy.NextCommand; got != buildReadFileCommand("internal/codexfixture/patchprobe/message.txt") {
		t.Fatalf("policy.NextCommand = %q, want initial read command", got)
	}
}

func TestBuildExecutionPolicy_PromptDynamicApplyPatchSynthesizesAfterRead(t *testing.T) {
	rawTools := json.RawMessage(`[
		{"type":"function","name":"exec_command","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}
	]`)
	task := "你是测试代理。请验证 apply_patch：\n" +
		"1) 先读取 internal/codexfixture/patchprobe/message.txt。\n" +
		"2) 必须使用 apply_patch，把文件中的 alpha 改为 beta。\n" +
		"3) 再读取同一文件确认内容已经变成 beta。"
	toolCatalog := buildResponseToolCatalog(augmentResponseToolsForPromptDynamic(rawTools, task))
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"exec_command","call_id":"call_read_1","arguments":"{\"cmd\":\"sed -n '1,200p' 'internal/codexfixture/patchprobe/message.txt'\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_read_1","output":{"content":"alpha\n","success":true}}`),
	}

	policy := buildExecutionPolicyWithCatalog("glm-4p7", task, history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "apply_patch" {
		t.Fatalf("policy.NextRequiredTool = %q, want apply_patch", policy.NextRequiredTool)
	}
	if !policy.PendingWrite {
		t.Fatal("policy.PendingWrite = false, want true until apply_patch executes")
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic apply_patch command")
	}
	if got := parsedCallName(t, *policy.SyntheticToolCall); got != "exec_command" {
		t.Fatalf("synthetic tool name = %q, want exec_command", got)
	}
	if got := parsedCallCommand(t, *policy.SyntheticToolCall); !strings.Contains(got, "| apply_patch") || !strings.Contains(got, "*** Update File: internal/codexfixture/patchprobe/message.txt") || !strings.Contains(got, "-alpha") || !strings.Contains(got, "+beta") {
		t.Fatalf("synthetic apply_patch command = %q, want alpha to beta patch command", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesViewImageFromPWD(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		"view_image":   {Name: "view_image", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"exec_command","call_id":"call_pwd_1","arguments":"{\"cmd\":\"pwd\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_pwd_1","output":{"content":"/Volumes/Work/code/firew2oai\n","success":true}}`),
	}
	task := "1) 必须先使用 exec_command 执行 `pwd`，读取当前工作目录绝对路径。\n2) 然后必须使用 view_image 查看 internal/codexfixture/assets/red.png。"

	policy := buildExecutionPolicyWithCatalog("glm-4p7", task, history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "view_image" {
		t.Fatalf("policy.NextRequiredTool = %q, want view_image", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic view_image call")
	}
	if got := parsedCallName(t, *policy.SyntheticToolCall); got != "view_image" {
		t.Fatalf("synthetic tool name = %q, want view_image", got)
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "path"); got != "/Volumes/Work/code/firew2oai/internal/codexfixture/assets/red.png" {
		t.Fatalf("synthetic view_image path = %q", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceTreatsImageOutputAsSatisfied(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		"view_image":   {Name: "view_image", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"exec_command","call_id":"call_pwd_1","arguments":"{\"cmd\":\"pwd\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_pwd_1","output":{"content":"/Volumes/Work/code/firew2oai\n","success":true}}`),
		json.RawMessage(`{"type":"function_call","name":"view_image","call_id":"call_img_1","arguments":"{\"path\":\"/Volumes/Work/code/firew2oai/internal/codexfixture/assets/red.png\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_img_1","output":[{"type":"input_image","image_url":"data:image/png;base64,abc","detail":"high"}]}`),
	}
	task := "1) 必须先使用 exec_command 执行 `pwd`，读取当前工作目录绝对路径。\n2) 然后必须使用 view_image 查看 internal/codexfixture/assets/red.png。"

	policy := buildExecutionPolicyWithCatalog("glm-4p7", task, history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "" {
		t.Fatalf("policy.NextRequiredTool = %q, want empty after image output", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall != nil {
		t.Fatalf("policy.SyntheticToolCall = %#v, want nil", policy.SyntheticToolCall)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesChromeClick(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"mcp__chrome_devtools__new_page":      {Name: "mcp__chrome_devtools__new_page", Type: "function", Structured: true, Namespace: "mcp__chrome_devtools__"},
		"mcp__chrome_devtools__take_snapshot": {Name: "mcp__chrome_devtools__take_snapshot", Type: "function", Structured: true, Namespace: "mcp__chrome_devtools__"},
		"mcp__chrome_devtools__click":         {Name: "mcp__chrome_devtools__click", Type: "function", Structured: true, Namespace: "mcp__chrome_devtools__"},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"new_page","namespace":"mcp__chrome_devtools__","call_id":"call_chrome_1","arguments":"{\"url\":\"data:text/html,<button>Go</button>\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_chrome_1","output":{"content":"## Pages\n2: data:text/html,<button>Go</button> [selected]","success":true}}`),
		json.RawMessage(`{"type":"function_call","name":"take_snapshot","namespace":"mcp__chrome_devtools__","call_id":"call_chrome_2","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_chrome_2","output":{"content":"## Latest page snapshot\nuid=1_0 RootWebArea\n  uid=1_1 button \"Go\"\n  uid=1_2 StaticText \"idle\"\n","success":true}}`),
	}
	task := "必须使用 mcp__chrome_devtools__new_page，然后使用 take_snapshot，再必须使用 mcp__chrome_devtools__click 点击按钮。"

	policy := buildExecutionPolicyWithCatalog("glm-4p7", task, history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "mcp__chrome_devtools__click" {
		t.Fatalf("policy.NextRequiredTool = %q, want mcp__chrome_devtools__click", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic click call")
	}
	if got := parsedCallName(t, *policy.SyntheticToolCall); got != "click" {
		t.Fatalf("synthetic tool name = %q, want click", got)
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "uid"); got != "1_1" {
		t.Fatalf("synthetic click uid = %q", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesChromeWaitFor(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"mcp__chrome_devtools__wait_for": {Name: "mcp__chrome_devtools__wait_for", Type: "function", Structured: true, Namespace: "mcp__chrome_devtools__"},
	}
	task := "再必须使用 mcp__chrome_devtools__wait_for 等待页面出现 clicked。"

	policy := buildExecutionPolicyWithCatalog("glm-4p7", task, nil, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "mcp__chrome_devtools__wait_for" {
		t.Fatalf("policy.NextRequiredTool = %q, want mcp__chrome_devtools__wait_for", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic wait_for call")
	}
	if got := parsedCallName(t, *policy.SyntheticToolCall); got != "wait_for" {
		t.Fatalf("synthetic tool name = %q, want wait_for", got)
	}
	if got := parsedCallArgumentList(t, *policy.SyntheticToolCall, "text"); !reflect.DeepEqual(got, []string{"clicked"}) {
		t.Fatalf("synthetic wait_for text = %#v, want []string{\"clicked\"}", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesWaitAgentAndCloseAgent(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"spawn_agent": {Name: "spawn_agent", Type: "function", Structured: true},
		"wait_agent":  {Name: "wait_agent", Type: "function", Structured: true},
		"close_agent": {Name: "close_agent", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"spawn_agent","call_id":"call_spawn_1","arguments":"{\"message\":\"读取 README.md 第一行\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_spawn_1","output":{"content":"spawned","success":true}}`),
		json.RawMessage(`{"type":"collab_tool_call","tool":"spawn_agent","receiver_thread_ids":["agent_123"],"status":"completed"}`),
	}
	waitTask := "必须使用 spawn_agent，然后必须使用 wait_agent 等待结果。"
	closeTask := "必须使用 spawn_agent，然后必须使用 wait_agent，最后必须使用 close_agent 关闭子代理。"

	waitPolicy := buildExecutionPolicyWithCatalog("glm-4p7", waitTask, history, toolCatalog, true, false, true)
	if waitPolicy.NextRequiredTool != "wait_agent" {
		t.Fatalf("waitPolicy.NextRequiredTool = %q, want wait_agent", waitPolicy.NextRequiredTool)
	}
	if waitPolicy.SyntheticToolCall == nil {
		t.Fatal("waitPolicy.SyntheticToolCall = nil, want synthetic wait_agent call")
	}
	if got := parsedCallArgumentList(t, *waitPolicy.SyntheticToolCall, "targets"); !reflect.DeepEqual(got, []string{"agent_123"}) {
		t.Fatalf("synthetic wait_agent targets = %#v, want []string{\"agent_123\"}", got)
	}
	if got := parsedCallArgument(t, *waitPolicy.SyntheticToolCall, "timeout_ms"); got != strconv.Itoa(syntheticWaitAgentTimeoutMS) {
		t.Fatalf("synthetic wait_agent timeout_ms = %q, want %d", got, syntheticWaitAgentTimeoutMS)
	}

	closeHistory := append(append([]json.RawMessage(nil), history...),
		json.RawMessage(`{"type":"function_call","name":"wait_agent","call_id":"call_wait_1","arguments":"{\"targets\":[\"agent_123\"]}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_wait_1","output":{"content":"completed","success":true}}`),
	)
	closePolicy := buildExecutionPolicyWithCatalog("glm-4p7", closeTask, closeHistory, toolCatalog, true, false, true)
	if closePolicy.NextRequiredTool != "close_agent" {
		t.Fatalf("closePolicy.NextRequiredTool = %q, want close_agent", closePolicy.NextRequiredTool)
	}
	if closePolicy.SyntheticToolCall == nil {
		t.Fatal("closePolicy.SyntheticToolCall = nil, want synthetic close_agent call")
	}
	if got := parsedCallArgument(t, *closePolicy.SyntheticToolCall, "target"); got != "agent_123" {
		t.Fatalf("synthetic close_agent target = %q, want agent_123", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesSpawnAgent(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"spawn_agent": {Name: "spawn_agent", Type: "function", Structured: true},
	}
	task := "必须使用 spawn_agent 启动一个子代理。\n子代理任务是读取 README.md 第一行并返回结果。\n然后继续后续步骤。"

	policy := buildExecutionPolicyWithCatalog("glm-4p7", task, nil, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "spawn_agent" {
		t.Fatalf("policy.NextRequiredTool = %q, want spawn_agent", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic spawn_agent call")
	}
	if got := parsedCallName(t, *policy.SyntheticToolCall); got != "spawn_agent" {
		t.Fatalf("synthetic tool name = %q, want spawn_agent", got)
	}
	want := "必须使用 exec_command 执行 `head -n 1 README.md`，只返回 README.md 第一行内容，不要额外解释。"
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "message"); got != want {
		t.Fatalf("synthetic spawn_agent message = %q, want %q", got, want)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesWaitAgentFromSpawnOutputJSON(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"spawn_agent": {Name: "spawn_agent", Type: "function", Structured: true},
		"wait_agent":  {Name: "wait_agent", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"spawn_agent","call_id":"call_spawn_1","arguments":"{\"message\":\"读取 README.md 第一行\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_spawn_1","output":{"content":"{\"agent_id\":\"agent_456\",\"nickname\":\"Nash\"}","success":true}}`),
	}
	task := "必须使用 spawn_agent 启动一个子代理，然后必须使用 wait_agent 等待结果。"

	policy := buildExecutionPolicyWithCatalog("glm-4p7", task, history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "wait_agent" {
		t.Fatalf("policy.NextRequiredTool = %q, want wait_agent", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic wait_agent call")
	}
	if got := parsedCallArgumentList(t, *policy.SyntheticToolCall, "targets"); !reflect.DeepEqual(got, []string{"agent_456"}) {
		t.Fatalf("synthetic wait_agent targets = %#v, want []string{\"agent_456\"}", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceAdvancesFromCompletedCollabSpawnAgent(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"spawn_agent": {Name: "spawn_agent", Type: "function", Structured: true},
		"wait_agent":  {Name: "wait_agent", Type: "function", Structured: true},
		"close_agent": {Name: "close_agent", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"collab_tool_call","tool":"spawn_agent","receiver_thread_ids":["agent_789"],"status":"completed"}`),
	}
	task := "必须使用 spawn_agent 启动一个子代理，然后必须使用 wait_agent 等待结果，最后必须使用 close_agent 关闭子代理。"

	policy := buildExecutionPolicyWithCatalog("qwen3-vl-30b-a3b-thinking", task, history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "wait_agent" {
		t.Fatalf("policy.NextRequiredTool = %q, want wait_agent", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic wait_agent call")
	}
	if got := parsedCallArgumentList(t, *policy.SyntheticToolCall, "targets"); !reflect.DeepEqual(got, []string{"agent_789"}) {
		t.Fatalf("synthetic wait_agent targets = %#v, want []string{\"agent_789\"}", got)
	}
	if !policy.RequireTool {
		t.Fatal("policy.RequireTool = false, want true")
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceAdvancesFromCompletedSpawnFunctionCall(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"spawn_agent": {Name: "spawn_agent", Type: "function", Structured: true},
		"wait_agent":  {Name: "wait_agent", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"spawn_agent","call_id":"call_spawn_1","arguments":"{\"message\":\"读取 README.md 第一行\"}","status":"completed"}`),
	}
	task := "必须使用 spawn_agent 启动一个子代理，然后必须使用 wait_agent 等待结果。"

	policy := buildExecutionPolicyWithCatalog("qwen3-vl-30b-a3b-thinking", task, history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "wait_agent" {
		t.Fatalf("policy.NextRequiredTool = %q, want wait_agent", policy.NextRequiredTool)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceAdvancesFromCompletedCollabWaitAlias(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"spawn_agent": {Name: "spawn_agent", Type: "function", Structured: true},
		"wait_agent":  {Name: "wait_agent", Type: "function", Structured: true},
		"close_agent": {Name: "close_agent", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"collab_tool_call","tool":"spawn_agent","receiver_thread_ids":["agent_789"],"status":"completed"}`),
		json.RawMessage(`{"type":"collab_tool_call","tool":"wait","receiver_thread_ids":["agent_789"],"agents_states":{"agent_789":{"status":"completed","message":"Hello World"}},"status":"completed"}`),
	}
	task := "必须使用 spawn_agent 启动一个子代理，然后必须使用 wait_agent 等待结果，最后必须使用 close_agent 关闭子代理。"

	policy := buildExecutionPolicyWithCatalog("qwen3-vl-30b-a3b-thinking", task, history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "close_agent" {
		t.Fatalf("policy.NextRequiredTool = %q, want close_agent", policy.NextRequiredTool)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceAdvancesFromFailedWaitAgentOutput(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"spawn_agent": {Name: "spawn_agent", Type: "function", Structured: true},
		"wait_agent":  {Name: "wait_agent", Type: "function", Structured: true},
		"close_agent": {Name: "close_agent", Type: "function", Structured: true},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"spawn_agent","call_id":"call_spawn_1","arguments":"{\"message\":\"读取 README.md 第一行\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_spawn_1","output":{"content":"{\"agent_id\":\"agent_123\",\"nickname\":\"Planck\"}","success":true}}`),
		json.RawMessage(`{"type":"function_call","name":"wait_agent","call_id":"call_wait_1","arguments":"{\"targets\":[\"agent_123\"]}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_wait_1","output":{"content":"{\"status\":{\"agent_123\":{\"completed\":\"Codex adapter error: upstream stream failed before content\"}},\"timed_out\":false}","success":false}}`),
	}
	task := "必须使用 spawn_agent 启动一个子代理，然后必须使用 wait_agent 等待结果，最后必须使用 close_agent 关闭子代理。"

	policy := buildExecutionPolicyWithCatalog("minimax-m2p5", task, history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "close_agent" {
		t.Fatalf("policy.NextRequiredTool = %q, want close_agent", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want synthetic close_agent call")
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "target"); got != "agent_123" {
		t.Fatalf("synthetic close_agent target = %q, want agent_123", got)
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

func TestExtractRequiredCommands_SplitsCompoundInlineCommands(t *testing.T) {
	task := "读取 internal/proxy/output_constraints.go 和 internal/proxy/execution_evidence.go，运行 go test ./internal/proxy 和 go test ./...，最终只输出四行：RESULT: PASS 或 FAIL；CONSTRAINT: 说明；EVIDENCE: 说明；TEST: 说明。"

	got := dedupePreserveOrder(extractRequiredCommands(task))
	want := []string{
		"go test ./internal/proxy",
		"go test ./...",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("required commands mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestExtractRequiredCommands_StopsAtChineseCommaAfterBacktickCommand(t *testing.T) {
	task := "1) 必须先使用 exec_command 执行 `pwd`，读取当前工作目录绝对路径。\n2) 然后继续。"

	got := dedupePreserveOrder(extractRequiredCommands(task))
	want := []string{"pwd"}
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

	got := chooseNextExecutionCommandWithStyles(requiredCommands, requiredFiles, nil, signals, true, []string{"internal/proxy/output_constraints_test.go"})
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

	got := chooseNextExecutionCommandWithStyles(nil, requiredFiles, nil, signals, true, []string{"internal/proxy/output_constraints_test.go"})
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

func TestInferToolOutputSuccess_RecognizesWriteStdinParseError(t *testing.T) {
	got := inferToolOutputSuccess("failed to parse function arguments: invalid type: string \"0\", expected i32 at line 1 column 36")
	if got == nil || *got {
		t.Fatalf("inferToolOutputSuccess should return false for write_stdin parse error, got %#v", got)
	}
}

func TestInferToolOutputSuccess_RecognizesWriteStdinRuntimeError(t *testing.T) {
	got := inferToolOutputSuccess("write_stdin failed: Unknown process id 1")
	if got == nil || *got {
		t.Fatalf("inferToolOutputSuccess should return false for write_stdin runtime error, got %#v", got)
	}
}

func TestInferToolOutputSuccess_RecognizesRateLimitError(t *testing.T) {
	got := inferToolOutputSuccess(`429 Too Many Requests: {"error":"Too Many Requests","message":"Monthly rate limit exceeded."}`)
	if got == nil || *got {
		t.Fatalf("inferToolOutputSuccess should return false for rate limit error, got %#v", got)
	}
}

func TestInferToolOutputSuccess_RecognizesGatewayTimeout(t *testing.T) {
	got := inferToolOutputSuccess("504 Gateway Timeout: upstream request timeout")
	if got == nil || *got {
		t.Fatalf("inferToolOutputSuccess should return false for gateway timeout, got %#v", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceKeepsDocforkSearchAfterGatewayTimeout(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"mcp__docfork__search_docs": {Name: "mcp__docfork__search_docs", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
		"mcp__docfork__fetch_doc":   {Name: "mcp__docfork__fetch_doc", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"search_docs","namespace":"mcp__docfork__","call_id":"call_docfork_1","arguments":"{\"library\":\"react\",\"query\":\"useEffectEvent\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_docfork_1","output":{"content":"504 Gateway Timeout: upstream request timeout"}}`),
	}

	policy := buildExecutionPolicyWithCatalog(
		"glm-4p7",
		"你是测试代理。请验证 Docfork MCP：\n1) 必须使用 mcp__docfork__search_docs 搜索 react 文档中的 useEffectEvent。\n2) 必须再使用 mcp__docfork__fetch_doc 获取相关文档内容。",
		history,
		toolCatalog,
		true,
		false,
		true,
	)

	if policy.NextRequiredTool != "mcp__docfork__search_docs" {
		t.Fatalf("policy.NextRequiredTool = %q, want mcp__docfork__search_docs after gateway timeout", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want retryable synthetic search_docs call")
	}
	if got := parsedCallName(t, *policy.SyntheticToolCall); got != "search_docs" {
		t.Fatalf("synthetic tool name = %q, want search_docs", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceKeepsDocforkSearchAfterRateLimit(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"mcp__docfork__search_docs": {Name: "mcp__docfork__search_docs", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
		"mcp__docfork__fetch_doc":   {Name: "mcp__docfork__fetch_doc", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"search_docs","namespace":"mcp__docfork__","call_id":"call_docfork_1","arguments":"{\"library\":\"react\",\"query\":\"useEffectEvent\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_docfork_1","output":{"content":"429 Too Many Requests: {\"error\":\"Too Many Requests\",\"message\":\"Monthly rate limit exceeded.\"}"}}`),
	}

	policy := buildExecutionPolicyWithCatalog(
		"llama-v3p3-70b-instruct",
		"你是测试代理。请验证 Docfork MCP：\n1) 必须使用 mcp__docfork__search_docs 搜索 react 文档中的 useEffectEvent。\n2) 必须再使用 mcp__docfork__fetch_doc 获取相关文档内容。",
		history,
		toolCatalog,
		true,
		false,
		true,
	)

	if policy.NextRequiredTool != "mcp__docfork__search_docs" {
		t.Fatalf("policy.NextRequiredTool = %q, want mcp__docfork__search_docs after rate limit", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want retryable synthetic search_docs call")
	}
	if got := parsedCallName(t, *policy.SyntheticToolCall); got != "search_docs" {
		t.Fatalf("synthetic tool name = %q, want search_docs", got)
	}
}

func TestBuildExecutionPolicy_ExplicitToolSequenceStopsAfterRepeatedDocforkRateLimit(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"mcp__docfork__search_docs": {Name: "mcp__docfork__search_docs", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
		"mcp__docfork__fetch_doc":   {Name: "mcp__docfork__fetch_doc", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
	}
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"search_docs","namespace":"mcp__docfork__","call_id":"call_docfork_1","arguments":"{\"library\":\"react\",\"query\":\"useEffectEvent\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_docfork_1","output":{"content":"429 Too Many Requests: {\"message\":\"Monthly rate limit exceeded.\"}"}}`),
		json.RawMessage(`{"type":"function_call","name":"search_docs","namespace":"mcp__docfork__","call_id":"call_docfork_2","arguments":"{\"library\":\"react\",\"query\":\"useEffectEvent\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_docfork_2","output":{"content":"429 Too Many Requests: {\"message\":\"Monthly rate limit exceeded.\"}"}}`),
	}

	policy := buildExecutionPolicyWithCatalog(
		"llama-v3p3-70b-instruct",
		"你是测试代理。请验证 Docfork MCP：\n1) 必须使用 mcp__docfork__search_docs 搜索 react 文档中的 useEffectEvent。\n2) 必须再使用 mcp__docfork__fetch_doc 获取相关文档内容。",
		history,
		toolCatalog,
		true,
		false,
		true,
	)

	if policy.NextRequiredTool != "" {
		t.Fatalf("policy.NextRequiredTool = %q, want empty after repeated rate limit", policy.NextRequiredTool)
	}
	if policy.RequireTool {
		t.Fatal("policy.RequireTool = true, want false after repeated external failure")
	}
	if policy.Stage != "finalize" {
		t.Fatalf("policy.Stage = %q, want finalize", policy.Stage)
	}
	if policy.SyntheticToolCall != nil {
		t.Fatalf("policy.SyntheticToolCall = %#v, want nil", policy.SyntheticToolCall)
	}
}

func TestInferTestCommandOutputSuccess_RecognizesSingleLineGoTestOK(t *testing.T) {
	got := inferTestCommandOutputSuccess("ok\tgithub.com/mison/firew2oai/internal/proxy\t0.321s")
	if got == nil || !*got {
		t.Fatalf("inferTestCommandOutputSuccess should return true for single-line go test ok, got %#v", got)
	}
}

func TestInferTestCommandOutputSuccess_RecognizesWrappedGoTestOK(t *testing.T) {
	text := "Command: /bin/zsh -lc 'go test ./internal/proxy -run TestStableActionableUserTask_PrefersEarlierSpecificPromptOverWeakContinuation'\n" +
		"Chunk ID: abc123\n" +
		"Wall time: 0.0000 seconds\n" +
		"Process exited with code 0\n" +
		"Original token count: 14\n" +
		"Output:\n" +
		"ok  \tgithub.com/mison/firew2oai/internal/proxy\t(cached)\n"

	got := inferTestCommandOutputSuccess(text)
	if got == nil || !*got {
		t.Fatalf("inferTestCommandOutputSuccess should return true for wrapped go test ok, got %#v", got)
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

func TestCollectExecutionHistorySignals_MarksWrappedGoTestOutputAsSuccessful(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"cmd": "go test ./internal/proxy -run TestStableActionableUserTask_PrefersEarlierSpecificPromptOverWeakContinuation"})
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
			"output": "Command: /bin/zsh -lc 'go test ./internal/proxy -run TestStableActionableUserTask_PrefersEarlierSpecificPromptOverWeakContinuation'\n" +
				"Chunk ID: abc123\n" +
				"Wall time: 0.0000 seconds\n" +
				"Process exited with code 0\n" +
				"Original token count: 14\n" +
				"Output:\n" +
				"ok  \tgithub.com/mison/firew2oai/internal/proxy\t(cached)\n",
		}),
	}

	signals := collectExecutionHistorySignals(history)
	want := "go test ./internal/proxy -run TestStableActionableUserTask_PrefersEarlierSpecificPromptOverWeakContinuation"
	if !reflect.DeepEqual(signals.SuccessfulCommands, []string{want}) {
		t.Fatalf("SuccessfulCommands = %#v, want %#v", signals.SuccessfulCommands, []string{want})
	}
}

func TestCollectExecutionHistorySignals_CountsWebSearchAliasAsToolCall(t *testing.T) {
	history := []json.RawMessage{
		mustMarshalRawJSON(map[string]any{
			"type":    "web_search",
			"call_id": "call_ws_1",
			"query":   "latest Go release",
		}),
	}

	signals := collectExecutionHistorySignals(history)
	if signals.ToolCalls != 1 {
		t.Fatalf("ToolCalls = %d, want 1", signals.ToolCalls)
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

func TestBuildSeedGoTestFunction_ReturnsEmptyWhenTaskLacksConcreteAssertion(t *testing.T) {
	task := "新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`。"

	got := buildSeedGoTestFunction(task, "TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise")
	if got != "" {
		t.Fatalf("seed test body = %q, want empty when no concrete assertion can be inferred", got)
	}
}

func TestBuildSeedWriteCommand_ReturnsEmptyWhenTaskLacksConcreteAssertion(t *testing.T) {
	task := "请在当前仓库完成一个真实但边界清晰的测试补强任务：\n" +
		"1) 阅读 internal/proxy/output_constraints.go 与现有 internal/proxy/*_test.go 风格。\n" +
		"2) 新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`。\n" +
		"3) 执行命令：go test ./internal/proxy"

	signals := executionHistorySignals{
		Commands: []string{
			"sed -n '1,120p' 'internal/proxy/output_constraints.go'",
		},
		CommandOutputs: map[string]string{
			"sed -n '1,120p' 'internal/proxy/output_constraints.go'": "package proxy\n",
		},
	}

	got := buildSeedWriteCommand(task, []string{"internal/proxy/output_constraints_test.go"}, signals)
	if got != "" {
		t.Fatalf("seed write command = %q, want empty when no concrete assertion can be inferred", got)
	}
}

func TestBuildExecutionPolicy_RealDebugUsesDeterministicSeedAfterRead(t *testing.T) {
	task := "你是资深 Go 工程师。请模拟真实 Codex 调试任务：\n" +
		"1) 先执行 go test ./internal/codexfixture/realdebug，观察失败。\n" +
		"2) 阅读 internal/codexfixture/realdebug/*.go。\n" +
		"3) 定位根因并修复 internal/codexfixture/realdebug/parser.go，使测试通过。\n" +
		"4) 不要新增无关文件。\n" +
		"5) 再执行 go test ./internal/codexfixture/realdebug。\n" +
		"最后只输出四行：RESULT: PASS 或 FAIL；FILES: 你修改的文件；TEST: 测试结果；NOTE: 根因和修复摘要。"
	history := []json.RawMessage{
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "test_1", "arguments": `{"cmd":"go test ./internal/codexfixture/realdebug"}`}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "test_1", "output": map[string]any{"content": "--- FAIL: TestParsePortKeepsConfiguredValue\n    parser_test.go:10: ParsePort() = 39528, want 39527", "success": false}}),
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "read_1", "arguments": `{"cmd":"sed -n '1,200p' 'internal/codexfixture/realdebug/parser.go'"}`}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "read_1", "output": map[string]any{"content": "package realdebug\n\nimport \"strconv\"\n\nfunc ParsePort(value string) (int, error) {\n\tport, err := strconv.Atoi(value)\n\tif err != nil {\n\t\treturn 0, err\n\t}\n\treturn port + 1, nil\n}\n", "success": true}}),
	}

	policy := buildExecutionPolicy("qwen3-vl-30b-a3b-instruct", task, history, true, false, true)
	if !policy.PendingWrite {
		t.Fatalf("policy.PendingWrite = false, want true before deterministic seed runs: %+v", policy)
	}
	seed := buildSeedWriteCommand(task, policy.RequiredFiles, collectExecutionHistorySignals(history))
	if seed == "" || !strings.Contains(seed, "base64.b64decode") {
		t.Fatalf("buildSeedWriteCommand = %q, want deterministic seed command", seed)
	}
	if !strings.Contains(seed, "python3 -c") {
		t.Fatalf("buildSeedWriteCommand = %q, want python seed command", seed)
	}
}

func TestApplyExecutionPolicyToParseResult_RealDebugRewritesFailedTestRetryToSeed(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	content := "再跑测试。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"go test ./internal/codexfixture/realdebug\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	seed := buildPythonExecCommand([]string{
		"from pathlib import Path",
		"path = Path(\"internal/codexfixture/realdebug/parser.go\")",
		"text = path.read_text(encoding='utf-8')",
		"path.write_text(text.replace('return port + 1, nil', 'return port, nil', 1), encoding='utf-8')",
	})
	policy := executionPolicy{
		Enabled:       true,
		RequireTool:   true,
		PendingWrite:  true,
		RequiredFiles: []string{"internal/codexfixture/realdebug/parser.go"},
		NextCommand:   seed,
	}

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{})
	command := parsedCallCommand(t, got.calls[0])
	if command != seed {
		t.Fatalf("rewritten command = %q, want seed %q", command, seed)
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
	if got != "" {
		t.Fatalf("next command = %q, want empty for read-only failed test after all commands were attempted", got)
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

func TestLatestActionableUserTask_SkipsSubagentNotification(t *testing.T) {
	messages := []ChatMessage{
		{Role: "user", Content: "必须使用 spawn_agent 启动一个子代理，然后必须使用 wait_agent，最后必须使用 close_agent。"},
		{Role: "assistant", Content: "Assistant requested tool: spawn_agent"},
		{Role: "user", Content: "<subagent_notification>\n{\"agent_path\":\"agent_123\",\"status\":{\"completed\":\"# OpenAI Codex CLI\"}}\n</subagent_notification>"},
	}

	got := latestActionableUserTask(messages)
	want := "必须使用 spawn_agent 启动一个子代理，然后必须使用 wait_agent，最后必须使用 close_agent。"
	if got != want {
		t.Fatalf("latest actionable task mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestStableActionableUserTask_PrefersEarlierSpecificPromptOverWeakContinuation(t *testing.T) {
	messages := []ChatMessage{
		{Role: "user", Content: "你是资深 Go 工程师。请在当前仓库完成一个真实但边界清晰的测试补强任务：\n1) 阅读 internal/proxy/output_constraints.go 与现有 internal/proxy/*_test.go 风格。\n2) 新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`。\n3) 执行命令：go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'\n4) 执行命令：go test ./internal/proxy"},
		{Role: "assistant", Content: "Assistant requested tool: exec_command"},
		{Role: "user", Content: "继续推进"},
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

func TestBuildExecutionPolicy_ReadOnlyTaskPollsRunningRequiredCommand(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		"write_stdin":  {Name: "write_stdin", Type: "function", Structured: true},
	}
	task := "只读审计：\n1) 执行 `go test ./internal/proxy`\n2) 执行 `go test ./...`"
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"exec_command","call_id":"call_test_1","arguments":"{\"cmd\":\"go test ./internal/proxy\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_test_1","output":"Chunk ID: b2d050\nWall time: 10.0009 seconds\nProcess running with session ID 32760\nOriginal token count: 0\nOutput:\n"}`),
	}

	policy := buildExecutionPolicyWithCatalog("qwen3-vl-30b-a3b-thinking", task, history, toolCatalog, true, false, true)
	if policy.NextRequiredTool != "write_stdin" {
		t.Fatalf("policy.NextRequiredTool = %q, want write_stdin", policy.NextRequiredTool)
	}
	if policy.SyntheticToolCall == nil {
		t.Fatal("policy.SyntheticToolCall = nil, want polling write_stdin call")
	}
	if got := parsedCallName(t, *policy.SyntheticToolCall); got != "write_stdin" {
		t.Fatalf("synthetic tool name = %q, want write_stdin", got)
	}
	if got := parsedCallArgument(t, *policy.SyntheticToolCall, "session_id"); got != "32760" {
		t.Fatalf("synthetic write_stdin session_id = %q, want 32760", got)
	}
}

func TestBuildExecutionPolicy_ReadOnlyTaskAdvancesAfterPolledCommandExit(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		"write_stdin":  {Name: "write_stdin", Type: "function", Structured: true},
	}
	task := "只读审计：\n1) 执行 `go test ./internal/proxy`\n2) 执行 `go test ./...`"
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"exec_command","call_id":"call_test_1","arguments":"{\"cmd\":\"go test ./internal/proxy\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_test_1","output":"Chunk ID: b2d050\nWall time: 10.0009 seconds\nProcess running with session ID 32760\nOriginal token count: 0\nOutput:\n"}`),
		json.RawMessage(`{"type":"function_call","name":"write_stdin","call_id":"call_poll_1","arguments":"{\"session_id\":32760,\"chars\":\"\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_poll_1","output":"Chunk ID: b2d050\nWall time: 18.0000 seconds\nProcess exited with code 0\nOutput:\nok  \tgithub.com/mison/firew2oai/internal/proxy\t18.000s"}`),
	}

	policy := buildExecutionPolicyWithCatalog("qwen3-vl-30b-a3b-thinking", task, history, toolCatalog, true, false, true)
	if policy.NextRequiredTool == "write_stdin" {
		t.Fatalf("policy.NextRequiredTool = write_stdin, want next required command after process exit: %+v", policy)
	}
	if policy.NextCommand != "go test ./..." {
		t.Fatalf("policy.NextCommand = %q, want go test ./...", policy.NextCommand)
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

func TestBuildExecutionPolicy_FinalizesWhenToolHistoryOnlyExistsAsSummaryMessages(t *testing.T) {
	task := "你是资深 Go 工程师。请在当前仓库完成一个真实但边界清晰的测试补强任务：\n" +
		"1) 阅读 internal/proxy/output_constraints.go 与现有 internal/proxy/*_test.go 风格。\n" +
		"2) 新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`。\n" +
		"3) 执行命令：go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'\n" +
		"4) 执行命令：go test ./internal/proxy"

	rawItems := []any{
		map[string]any{
			"type":    "message",
			"role":    "assistant",
			"content": "Assistant requested tool: exec_command (call_id=call_write)\nTool payload:\n{\"cmd\":\"python3 -c 'from pathlib import Path; Path(\\\"internal/proxy/output_constraints_test.go\\\").write_text(\\\"package proxy\\\", encoding=\\\"utf-8\\\")'\"}",
		},
		map[string]any{
			"type":    "message",
			"role":    "user",
			"content": "Tool result (call_id=call_write)\nSuccess: true\nOutput:\n",
		},
		map[string]any{
			"type":    "message",
			"role":    "assistant",
			"content": "Assistant requested tool: exec_command (call_id=call_test_one)\nTool payload:\n{\"cmd\":\"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'\"}",
		},
		map[string]any{
			"type":    "message",
			"role":    "user",
			"content": "Tool result (call_id=call_test_one)\nSuccess: true\nOutput:\nok  \tgithub.com/mison/firew2oai/internal/proxy\t0.015s",
		},
		map[string]any{
			"type":    "message",
			"role":    "assistant",
			"content": "Assistant requested tool: exec_command (call_id=call_test_all)\nTool payload:\n{\"cmd\":\"go test ./internal/proxy\"}",
		},
		map[string]any{
			"type":    "message",
			"role":    "user",
			"content": "Tool result (call_id=call_test_all)\nSuccess: true\nOutput:\nok  \tgithub.com/mison/firew2oai/internal/proxy\t12.124s",
		},
	}

	history := make([]json.RawMessage, 0, len(rawItems))
	for _, item := range rawItems {
		raw, ok := normalizeRawResponseInputItem(item)
		if !ok {
			t.Fatalf("normalizeRawResponseInputItem returned ok=false for %#v", item)
		}
		history = append(history, raw)
	}

	policy := buildExecutionPolicy("glm-5", task, history, true, false, true)
	if policy.Stage != "finalize" {
		t.Fatalf("stage = %q, want finalize; policy=%+v", policy.Stage, policy)
	}
	if policy.PendingWrite {
		t.Fatalf("policy.PendingWrite = true, want false after summarized write and tests completed: %+v", policy)
	}
}

func TestBuildExecutionPolicy_AdvancesFromCommandStyleToolResultsWithoutAssistantSummary(t *testing.T) {
	task := "你是资深 Go 工程师。请完成一个需要先搜索再修复的真实 Coding 任务：\n" +
		"1) 先执行命令：rg -n \"BuildTicketSummary|NormalizeTitle\" internal/codexfixture/searchfix。\n" +
		"2) 阅读 internal/codexfixture/searchfix/summary.go 与 internal/codexfixture/searchfix/summary_test.go。\n" +
		"3) 修改现有文件 internal/codexfixture/searchfix/summary.go，让 BuildTicketSummary 对 title 执行 strings.TrimSpace + strings.ToUpper，对 body 执行 strings.TrimSpace。\n" +
		"4) 不要新增文件。\n" +
		"5) 执行 go test ./internal/codexfixture/searchfix。"

	rawItems := []any{
		"Tool result (call_id=call_search)\nCommand: rg -n \"BuildTicketSummary|NormalizeTitle\" internal/codexfixture/searchfix\nExit code: 0\nOutput:\ninternal/codexfixture/searchfix/summary.go:3:func BuildTicketSummary(title, body string) string",
		"Tool result (call_id=call_read_main)\nCommand: sed -n '1,200p' 'internal/codexfixture/searchfix/summary.go'\nExit code: 0\nOutput:\npackage searchfix\n\nfunc BuildTicketSummary(title, body string) string {\n\treturn title + \": \" + body\n}",
		"Tool result (call_id=call_read_test)\nCommand: sed -n '1,200p' 'internal/codexfixture/searchfix/summary_test.go'\nExit code: 0\nOutput:\npackage searchfix\n\nimport \"testing\"\n\nfunc TestBuildTicketSummary_TrimsAndUppercases(t *testing.T) {\n\tgot := BuildTicketSummary(\"  firew2oai  \", \" adapter \")\n\twant := \"FIREW2OAI: adapter\"\n\tif got != want {\n\t\tt.Fatalf(\"BuildTicketSummary() = %q, want %q\", got, want)\n\t}\n}",
	}

	history := make([]json.RawMessage, 0, len(rawItems)*2)
	for _, item := range rawItems {
		raws := normalizeRawResponseInputItems(item)
		if len(raws) == 0 {
			t.Fatalf("normalizeRawResponseInputItems returned empty for %#v", item)
		}
		history = append(history, raws...)
	}

	policy := buildExecutionPolicy("glm-5", task, history, true, false, true)
	if !policy.PendingWrite {
		t.Fatalf("policy.PendingWrite = false, want true")
	}
	for _, want := range []string{"base64.b64decode", "utf-8"} {
		if !strings.Contains(policy.NextCommand, want) {
			t.Fatalf("policy.NextCommand = %q, want encoded deterministic replacement command containing %q", policy.NextCommand, want)
		}
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

func TestBuildExecutionPolicy_ReadOnlyCodingTaskAdvancesAfterSeenReadCommandWithoutSuccessFlag(t *testing.T) {
	task := "你是资深 Go 工程师。请执行一个真实编码排障任务（只读分析，不修改文件）：\n1) 执行 `sed -n '1,220p' internal/proxy/output_constraints.go`\n2) 执行 `sed -n '1,220p' internal/proxy/execution_evidence.go`\n3) 执行 `go test ./internal/proxy`\n4) 执行 `go test ./...`\n完成后只输出四行。"
	readOneArgs, _ := json.Marshal(map[string]any{"cmd": "sed -n '1,220p' internal/proxy/output_constraints.go"})
	readTwoArgs, _ := json.Marshal(map[string]any{"cmd": "sed -n '1,220p' internal/proxy/execution_evidence.go"})
	history := []json.RawMessage{
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "read_1", "arguments": string(readOneArgs)}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "read_1", "output": map[string]any{"content": "package proxy", "success": true}}),
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "read_2", "arguments": string(readTwoArgs)}),
	}

	policy := buildExecutionPolicy("deepseek-v3p2", task, history, true, false, true)
	if policy.NextCommand != "go test ./internal/proxy" {
		t.Fatalf("next command = %q, want go test ./internal/proxy; policy=%+v", policy.NextCommand, policy)
	}
}

func TestBuildExecutionPolicy_ReadOnlyInlinePromptFinalizesWithMentionedFiles(t *testing.T) {
	task := "在当前仓库执行只读核验任务：1) 运行 `sed -n '1,80p' internal/proxy/task_intent.go`；2) 运行 `go test ./internal/proxy -run TestStableActionableUserTask_PrefersEarlierSpecificPromptOverWeakContinuation`；3) 不要修改文件；4) 最后只输出两行。"
	history := historyExecCommandItems(
		"sed -n '1,80p' internal/proxy/task_intent.go",
		"go test ./internal/proxy -run TestStableActionableUserTask_PrefersEarlierSpecificPromptOverWeakContinuation",
	)

	policy := buildExecutionPolicy("deepseek-v3p2", task, history, true, false, true)
	if policy.Stage != "finalize" {
		t.Fatalf("stage = %q, want finalize; policy=%+v", policy.Stage, policy)
	}
	if policy.RequireTool {
		t.Fatalf("policy.RequireTool = true, want false after read-only required commands are done: %+v", policy)
	}
	wantFiles := []string{"internal/proxy/task_intent.go"}
	if !reflect.DeepEqual(policy.RequiredFiles, wantFiles) {
		t.Fatalf("policy.RequiredFiles = %#v, want %#v for read-only inline prompt", policy.RequiredFiles, wantFiles)
	}
}

func TestBuildExecutionPolicy_ReadOnlyNaturalPromptRequiresMentionedFilesFirst(t *testing.T) {
	task := "只读分析 internal/proxy/output_constraints.go 和 internal/proxy/execution_evidence.go，执行 go test ./internal/proxy 和 go test ./...。最后只输出四行：RESULT、CONSTRAINT、EVIDENCE、TEST。不要修改文件。"

	policy := buildExecutionPolicy("glm-5", task, nil, true, false, true)
	if policy.Stage != "explore" {
		t.Fatalf("stage = %q, want explore; policy=%+v", policy.Stage, policy)
	}
	wantFiles := []string{
		"internal/proxy/output_constraints.go",
		"internal/proxy/execution_evidence.go",
	}
	if !reflect.DeepEqual(policy.RequiredFiles, wantFiles) {
		t.Fatalf("policy.RequiredFiles = %#v, want %#v", policy.RequiredFiles, wantFiles)
	}
	wantNext := buildReadFileCommand("internal/proxy/output_constraints.go")
	if policy.NextCommand != wantNext {
		t.Fatalf("policy.NextCommand = %q, want %q; policy=%+v", policy.NextCommand, wantNext, policy)
	}
}

func TestBuildExecutionPolicy_ClearsMissingFileAfterSuccessfulMutation(t *testing.T) {
	task := "你是资深 Go 工程师。请在当前仓库完成一个真实但边界清晰的测试补强任务：\n" +
		"1) 阅读 internal/proxy/output_constraints.go 与现有 internal/proxy/*_test.go 风格。\n" +
		"2) 新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`。\n" +
		"3) 执行命令：go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'\n" +
		"4) 执行命令：go test ./internal/proxy\n"
	readMissingArgs, _ := json.Marshal(map[string]any{"cmd": "sed -n '1,200p' 'internal/proxy/output_constraints_test.go'"})
	readRefArgs, _ := json.Marshal(map[string]any{"cmd": "sed -n '1,200p' 'internal/proxy/output_constraints.go'"})
	writeArgs, _ := json.Marshal(map[string]any{"cmd": "python3 -c 'from pathlib import Path; Path(\"internal/proxy/output_constraints_test.go\").write_text(\"package proxy\\n\", encoding='\"'\"'utf-8'\"'\"')'"})
	testArgs, _ := json.Marshal(map[string]any{"cmd": "go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'"})

	history := []json.RawMessage{
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "read_missing", "arguments": string(readMissingArgs)}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "read_missing", "output": map[string]any{"content": "sed: internal/proxy/output_constraints_test.go: No such file or directory", "success": false}}),
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "read_ref", "arguments": string(readRefArgs)}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "read_ref", "output": map[string]any{"content": "package proxy", "success": true}}),
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "write_target", "arguments": string(writeArgs)}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "write_target", "output": map[string]any{"content": "", "success": true}}),
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "test_target", "arguments": string(testArgs)}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "test_target", "output": map[string]any{"content": "--- FAIL: TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise", "success": false}}),
	}

	policy := buildExecutionPolicy("minimax-m2p5", task, history, true, false, true)
	if len(policy.MissingFiles) != 0 {
		t.Fatalf("policy.MissingFiles = %#v, want empty after successful mutation", policy.MissingFiles)
	}
	if !policy.PendingWrite {
		t.Fatalf("policy.PendingWrite = false, want true after failed test following mutation: %+v", policy)
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

func TestApplyExecutionPolicyToParseResult_FinalizeStageIgnoresUnexpectedToolCall(t *testing.T) {
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}
	content := "RESULT: PASS\nTEST: 所需验证已完成。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"go test ./...\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	parseResult := parseToolCallOutputsWithConstraints(content, toolCatalog, toolProtocolConstraints{})
	policy := executionPolicy{
		Enabled: true,
		Stage:   "finalize",
	}

	got := applyExecutionPolicyToParseResult(parseResult, policy, toolCatalog, toolProtocolConstraints{})
	if got.err != nil {
		t.Fatalf("got.err = %v, want nil", got.err)
	}
	if len(got.calls) != 0 {
		t.Fatalf("len(got.calls) = %d, want 0 in finalize stage", len(got.calls))
	}
	if got.visibleText != "RESULT: PASS\nTEST: 所需验证已完成。" {
		t.Fatalf("visibleText = %q", got.visibleText)
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

func parsedCallName(t *testing.T, call parsedToolCall) string {
	t.Helper()

	var item map[string]any
	if err := json.Unmarshal(call.item, &item); err != nil {
		t.Fatalf("unmarshal parsed call: %v", err)
	}
	name, _ := item["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		t.Fatalf("missing name in parsed call item: %s", string(call.item))
	}
	return name
}

func parsedCallArgument(t *testing.T, call parsedToolCall, key string) string {
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
	value := ""
	switch raw := args[key].(type) {
	case string:
		value = strings.TrimSpace(raw)
	case float64:
		value = strconv.Itoa(int(raw))
	}
	if value == "" {
		t.Fatalf("missing %s in parsed call arguments: %s", key, argsText)
	}
	return value
}

func parsedCallBoolArgument(t *testing.T, call parsedToolCall, key string) bool {
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
	value, ok := args[key].(bool)
	if !ok {
		t.Fatalf("missing bool %s in parsed call arguments: %s", key, argsText)
	}
	return value
}

func parsedCallArgumentList(t *testing.T, call parsedToolCall, key string) []string {
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
	rawList, _ := args[key].([]any)
	if len(rawList) == 0 {
		t.Fatalf("missing %s list in parsed call arguments: %s", key, argsText)
	}

	out := make([]string, 0, len(rawList))
	for _, raw := range rawList {
		value, _ := raw.(string)
		value = strings.TrimSpace(value)
		if value == "" {
			t.Fatalf("empty value in %s list: %s", key, argsText)
		}
		out = append(out, value)
	}
	return out
}

func parsedCallArgumentListOfObjects(t *testing.T, call parsedToolCall, key string) []map[string]string {
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
	rawList, ok := args[key].([]any)
	if !ok || len(rawList) == 0 {
		t.Fatalf("missing %s object list in parsed call arguments: %s", key, argsText)
	}

	out := make([]map[string]string, 0, len(rawList))
	for _, raw := range rawList {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("non-object entry in %s list: %#v", key, raw)
		}
		normalized := make(map[string]string, len(entry))
		for field, value := range entry {
			normalized[field] = strings.TrimSpace(asString(value))
		}
		out = append(out, normalized)
	}
	return out
}

func parsedCallInput(t *testing.T, call parsedToolCall) string {
	t.Helper()

	var item map[string]any
	if err := json.Unmarshal(call.item, &item); err != nil {
		t.Fatalf("unmarshal parsed call: %v", err)
	}
	input, _ := item["input"].(string)
	input = strings.TrimSpace(input)
	if input == "" {
		t.Fatalf("missing input in parsed call item: %s", string(call.item))
	}
	return input
}

func TestBuildExecutionPolicy_RealRefactorIncludesSourceAndTestTargets(t *testing.T) {
	task := "你是资深 Go 工程师。请模拟真实 Codex 小重构任务：\n" +
		"1) 阅读 internal/codexfixture/realrefactor/formatter.go 与 formatter_test.go。\n" +
		"2) 将 BuildUserLine 中的清洗逻辑拆到新增文件 internal/codexfixture/realrefactor/normalize.go。\n" +
		"3) 让 name 执行 strings.TrimSpace，role 执行 strings.TrimSpace + strings.ToLower。\n" +
		"4) 在 formatter_test.go 追加 role 大小写混合的测试。\n" +
		"5) 执行 go test ./internal/codexfixture/realrefactor。"

	policy := buildExecutionPolicy("qwen3-vl-30b-a3b-instruct", task, nil, true, false, true)
	want := []string{
		"internal/codexfixture/realrefactor/normalize.go",
		"internal/codexfixture/realrefactor/formatter.go",
		"internal/codexfixture/realrefactor/formatter_test.go",
	}
	if !reflect.DeepEqual(policy.RequiredFiles, want) {
		t.Fatalf("policy.RequiredFiles = %#v, want %#v", policy.RequiredFiles, want)
	}
}

func TestBuildExecutionPolicy_RealRefactorPrefersDeterministicSeedAfterScaffold(t *testing.T) {
	task := "你是资深 Go 工程师。请模拟真实 Codex 小重构任务：\n" +
		"1) 阅读 internal/codexfixture/realrefactor/formatter.go 与 formatter_test.go。\n" +
		"2) 将 BuildUserLine 中的清洗逻辑拆到新增文件 internal/codexfixture/realrefactor/normalize.go。\n" +
		"3) 让 name 执行 strings.TrimSpace，role 执行 strings.TrimSpace + strings.ToLower。\n" +
		"4) 在 formatter_test.go 追加 role 大小写混合的测试。\n" +
		"5) 执行 go test ./internal/codexfixture/realrefactor。"

	readFormatterArgs, _ := json.Marshal(map[string]any{"cmd": "sed -n '1,200p' 'internal/codexfixture/realrefactor/formatter.go'"})
	readTestArgs, _ := json.Marshal(map[string]any{"cmd": "sed -n '1,200p' 'internal/codexfixture/realrefactor/formatter_test.go'"})
	touchArgs, _ := json.Marshal(map[string]any{"cmd": "mkdir -p -- 'internal/codexfixture/realrefactor' && touch 'internal/codexfixture/realrefactor/normalize.go'"})
	readEmptyArgs, _ := json.Marshal(map[string]any{"cmd": "sed -n '1,200p' 'internal/codexfixture/realrefactor/normalize.go'"})
	history := []json.RawMessage{
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "read_formatter", "arguments": string(readFormatterArgs)}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "read_formatter", "output": map[string]any{"content": "package realrefactor\n\nfunc BuildUserLine(name, role string) string {\n\treturn name + \" (\" + role + \")\"\n}\n", "success": true}}),
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "touch", "arguments": string(touchArgs)}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "touch", "output": map[string]any{"content": "", "success": true}}),
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "read_empty", "arguments": string(readEmptyArgs)}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "read_empty", "output": map[string]any{"content": "", "success": true}}),
		mustMarshalRawJSON(map[string]any{"type": "function_call", "name": "exec_command", "call_id": "read_test", "arguments": string(readTestArgs)}),
		mustMarshalRawJSON(map[string]any{"type": "function_call_output", "call_id": "read_test", "output": map[string]any{"content": "package realrefactor\n\nimport \"testing\"\n", "success": true}}),
	}

	policy := buildExecutionPolicy("qwen3-vl-30b-a3b-instruct", task, history, true, false, true)
	if policy.NextCommand == "" {
		t.Fatalf("policy.NextCommand is empty; policy=%+v", policy)
	}
	for _, want := range []string{"base64.b64decode", "utf-8"} {
		if !strings.Contains(policy.NextCommand, want) {
			t.Fatalf("policy.NextCommand missing %q: %s", want, policy.NextCommand)
		}
	}
}

func TestBuildSeedGoRealRefactorCommandRunsOnFixture(t *testing.T) {
	dir := t.TempDir()
	fixtureDir := filepath.Join(dir, "internal/codexfixture/realrefactor")
	if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	formatterPath := filepath.Join(fixtureDir, "formatter.go")
	testPath := filepath.Join(fixtureDir, "formatter_test.go")
	formatterText := "package realrefactor\n\n" +
		"func BuildUserLine(name, role string) string {\n" +
		"\treturn name + \" (\" + role + \")\"\n" +
		"}\n"
	testText := "package realrefactor\n\n" +
		"import \"testing\"\n\n" +
		"func TestBuildUserLineNormalizesWhitespace(t *testing.T) {\n" +
		"\tgot := BuildUserLine(\"  Mison  \", \" ADMIN \")\n" +
		"\twant := \"Mison (admin)\"\n" +
		"\tif got != want {\n" +
		"\t\tt.Fatalf(\"BuildUserLine() = %q, want %q\", got, want)\n" +
		"\t}\n" +
		"}\n"
	if err := os.WriteFile(formatterPath, []byte(formatterText), 0o644); err != nil {
		t.Fatalf("write formatter: %v", err)
	}
	if err := os.WriteFile(testPath, []byte(testText), 0o644); err != nil {
		t.Fatalf("write test: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	task := "2) 将 BuildUserLine 中的清洗逻辑拆到新增文件 internal/codexfixture/realrefactor/normalize.go。\n" +
		"3) 让 name 执行 strings.TrimSpace，role 执行 strings.TrimSpace + strings.ToLower。\n" +
		"4) 在 formatter_test.go 追加 role 大小写混合的测试。"
	readFormatter := "sed -n '1,200p' 'internal/codexfixture/realrefactor/formatter.go'"
	readTest := "sed -n '1,200p' 'internal/codexfixture/realrefactor/formatter_test.go'"
	signals := executionHistorySignals{
		Commands:           []string{readFormatter, readTest},
		CommandsWithResult: []string{readFormatter, readTest},
		SuccessfulCommands: []string{readFormatter, readTest},
		CommandOutputs: map[string]string{
			readFormatter: formatterText,
			readTest:      testText,
		},
	}
	cmd := buildSeedGoRealRefactorCommand(task, []string{
		"internal/codexfixture/realrefactor/normalize.go",
		"internal/codexfixture/realrefactor/formatter.go",
		"internal/codexfixture/realrefactor/formatter_test.go",
	}, signals)
	if cmd == "" {
		t.Fatal("buildSeedGoRealRefactorCommand returned empty command")
	}
	run := exec.Command("sh", "-c", cmd)
	run.Dir = dir
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("run refactor seed command: %v\n%s", err, string(output))
	}
	testRun := exec.Command("go", "test", "./internal/codexfixture/realrefactor")
	testRun.Dir = dir
	if output, err := testRun.CombinedOutput(); err != nil {
		t.Fatalf("go test fixture: %v\n%s", err, string(output))
	}
}
