package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mison/firew2oai/internal/transport"
)

func marshalSSEContent(t *testing.T, content string) string {
	t.Helper()

	data, err := json.Marshal(map[string]string{
		"type":    "content",
		"content": content,
	})
	if err != nil {
		t.Fatalf("marshal SSE content: %v", err)
	}
	return "data: " + string(data) + "\n\n"
}

func TestBuildChatPrompt_UsesAIActionsProtocol(t *testing.T) {
	prompt := buildChatPrompt(
		[]ChatMessage{{Role: "user", Content: "读取 README.md"}},
		json.RawMessage(`[{"type":"function","name":"Read","description":"read file","parameters":{"type":"object","properties":{"file_path":{"type":"string"}}}}]`),
		nil,
		0,
	)

	for _, want := range []string{
		"<<<AI_ACTIONS_V1>>>",
		"<<<END_AI_ACTIONS_V1>>>",
		`{"mode":"tool","calls":[`,
		`{"mode":"final"}`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "reply with exactly one JSON object") {
		t.Fatalf("prompt still contains legacy single-JSON instruction:\n%s", prompt)
	}
}

func TestBuildResponsesPrompt_UsesAIActionsProtocol(t *testing.T) {
	prompt := buildResponsesPrompt(
		nil,
		"be concise",
		[]ChatMessage{{Role: "user", Content: "列出目录并读取 README.md"}},
		json.RawMessage(`[{"type":"function","name":"exec_command","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]`),
		0,
		responsesPromptOptions{},
	)

	for _, want := range []string{
		"<<<AI_ACTIONS_V1>>>",
		"<<<END_AI_ACTIONS_V1>>>",
		`{"mode":"tool","calls":[`,
		`{"mode":"final"}`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "reply with exactly one JSON object") {
		t.Fatalf("prompt still contains legacy single-JSON instruction:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Emit exactly one AI_ACTIONS block per reply.") {
		t.Fatalf("prompt missing single-block guidance:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Use mode final only when the task is fully complete") {
		t.Fatalf("prompt missing final-mode completion guidance:\n%s", prompt)
	}
}

func TestBuildResponsesPrompt_MaxCallsOneIncludesSingleStepGuidance(t *testing.T) {
	prompt := buildResponsesPrompt(
		nil,
		"be concise",
		[]ChatMessage{{Role: "user", Content: "先读 README 再读 tool_protocol"}},
		json.RawMessage(`[{"type":"function","name":"exec_command","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]`),
		1,
		responsesPromptOptions{},
	)

	if !strings.Contains(prompt, "calls array must contain exactly one item") {
		t.Fatalf("prompt missing maxCalls=1 one-call constraint:\n%s", prompt)
	}
	if !strings.Contains(prompt, "emit only the next single tool call now") {
		t.Fatalf("prompt missing single-step guidance for maxCalls=1:\n%s", prompt)
	}
}

func TestBuildResponsesPrompt_UpdatePlanIncludesExplicitPlanShape(t *testing.T) {
	prompt := buildResponsesPrompt(
		nil,
		"be concise",
		[]ChatMessage{{Role: "user", Content: "先更新计划再读取 README.md"}},
		json.RawMessage(`[{"type":"function","name":"update_plan","description":"update task plan","parameters":{"type":"object","properties":{"explanation":{"type":"string"},"plan":{"type":"array","items":{"type":"object","properties":{"step":{"type":"string"},"status":{"type":"string"}}}}},"required":["plan"]}}]`),
		1,
		responsesPromptOptions{},
	)

	for _, want := range []string{
		"For update_plan, arguments must be an object with key plan, not steps.",
		`{"plan":[{"step":"Inspect README.md","status":"in_progress"}`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildResponsesPrompt_IncludesSpecialHintsForWebSearchJsReplAndNamespacedTools(t *testing.T) {
	prompt := buildResponsesPrompt(
		nil,
		"be concise",
		[]ChatMessage{{Role: "user", Content: "先搜索资料，再用 js 计算，然后调用 docfork"}},
		json.RawMessage(`[
			{"type":"custom","name":"js_repl","description":"run js"},
			{"type":"web_search","external_web_access":true},
			{"type":"namespace","name":"mcp__docfork__","tools":[
				{"type":"function","name":"search_docs","description":"search docs","parameters":{"type":"object","properties":{"library":{"type":"string"},"query":{"type":"string"}}}}
			]}
		]`),
		0,
		responsesPromptOptions{},
	)

	for _, want := range []string{
		"For web_search, use the exact tool name web_search and pass arguments as {\"query\":\"...\"}. The proxy will convert that into a web_search_call item.",
		"For js_repl, send raw JavaScript source in input.",
		"For namespaced MCP tools, use the full declared name exactly as shown, including the namespace prefix and double-underscore separator.",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildToolChoiceInstructions_RequiredNamedToolIsMandatory(t *testing.T) {
	instructions := buildToolChoiceInstructions(mustMarshalRawJSON(map[string]any{"name": "exec_command"}))

	if !strings.Contains(instructions, `must`) {
		t.Fatalf("tool choice instructions should be mandatory, got %q", instructions)
	}
	if strings.Contains(instructions, "If you emit") {
		t.Fatalf("tool choice instructions should not be conditional, got %q", instructions)
	}
	if !strings.Contains(instructions, `"exec_command"`) {
		t.Fatalf("tool choice instructions missing required tool name: %q", instructions)
	}
}

func TestBuildChatPrompt_ToolChoiceNoneDoesNotExposeTools(t *testing.T) {
	prompt := buildChatPrompt(
		[]ChatMessage{{Role: "user", Content: "只回答结果"}},
		json.RawMessage(`[{"type":"function","name":"Read","description":"read file","parameters":{"type":"object","properties":{"file_path":{"type":"string"}}}}]`),
		mustMarshalRawJSON("none"),
		0,
	)

	for _, unwanted := range []string{
		"<AVAILABLE_TOOLS>",
		"Read",
		"<<<AI_ACTIONS_V1>>>",
		`{"mode":"tool","calls":[`,
	} {
		if strings.Contains(prompt, unwanted) {
			t.Fatalf("prompt should not expose tools when tool_choice=none, found %q:\n%s", unwanted, prompt)
		}
	}
	if !strings.Contains(prompt, "Do not call any tools. Answer with plain text only.") {
		t.Fatalf("prompt missing tool_choice=none guidance:\n%s", prompt)
	}
}

func TestBuildResponsesPrompt_ToolChoiceNoneDoesNotExposeTools(t *testing.T) {
	prompt := buildResponsesPrompt(
		nil,
		"",
		[]ChatMessage{{Role: "user", Content: "只回答结果"}},
		nil,
		0,
		responsesPromptOptions{},
	)
	if strings.Contains(prompt, "<AVAILABLE_TOOLS>") || strings.Contains(prompt, "<<<AI_ACTIONS_V1>>>") {
		t.Fatalf("prompt without tools should not expose tool protocol:\n%s", prompt)
	}
}

func TestTaskLikelyNeedsTools_ActionTask(t *testing.T) {
	task := "修改 internal/proxy/responses.go 并运行 go test ./internal/proxy"
	if !taskLikelyNeedsTools(task) {
		t.Fatalf("expected action task to require tools, task=%q", task)
	}
}

func TestTaskLikelyNeedsTools_PlainQuestion(t *testing.T) {
	task := "请总结这个仓库的用途"
	if taskLikelyNeedsTools(task) {
		t.Fatalf("plain question should not require tools, task=%q", task)
	}
}

func TestTaskLikelyNeedsTools_ExplicitToolName(t *testing.T) {
	task := "必须使用 update_plan，然后读取 README.md 前三行"
	if !taskLikelyNeedsTools(task) {
		t.Fatalf("explicit tool task should require tools, task=%q", task)
	}
}

func TestTaskLikelyNeedsTools_NamespacedTool(t *testing.T) {
	task := "必须使用 mcp__docfork__search_docs 查询 react useEffectEvent 文档"
	if !taskLikelyNeedsTools(task) {
		t.Fatalf("namespaced MCP tool task should require tools, task=%q", task)
	}
}

func TestBuildTaskCompletionGate_ExtractsRequirements(t *testing.T) {
	task := `你在一个真实 Go 项目里做一次小型编码任务。严格执行：
1) 修改 internal/proxy/responses.go：新增别名。
2) 运行并通过：
   - go test ./internal/proxy
最终只输出三行：
RESULT: PASS 或 FAIL
CHANGED: 逗号分隔文件列表
TEST: 最后一条 go test 摘要`

	gate := buildTaskCompletionGate(task)
	for _, want := range []string{
		"<TASK_COMPLETION_GATE>",
		"internal/proxy/responses.go",
		"go test ./internal/proxy",
		"RESULT:",
		"CHANGED:",
		"TEST:",
	} {
		if !strings.Contains(gate, want) {
			t.Fatalf("gate missing %q:\n%s", want, gate)
		}
	}
}

func TestBuildTaskCompletionGate_PlainQuestionReturnsEmpty(t *testing.T) {
	task := "请总结这个仓库的用途"
	if gate := buildTaskCompletionGate(task); gate != "" {
		t.Fatalf("plain question should not produce completion gate, got:\n%s", gate)
	}
}

func TestBuildExplicitToolUseBlock_ExtractsDeclaredNames(t *testing.T) {
	task := "必须使用 mcp__docfork__search_docs 搜索，再调用 fetch_doc 获取正文"
	block := buildExplicitToolUseBlock(task, map[string]responseToolDescriptor{
		"mcp__docfork__search_docs": {Name: "mcp__docfork__search_docs", Type: "function", Structured: true},
		"mcp__docfork__fetch_doc":   {Name: "mcp__docfork__fetch_doc", Type: "function", Structured: true},
	})

	for _, want := range []string{
		"<EXPLICIT_TOOL_REQUIREMENTS>",
		"mcp__docfork__search_docs",
		"mcp__docfork__fetch_doc",
		"do not describe intended tool use in prose",
	} {
		if !strings.Contains(strings.ToLower(block), strings.ToLower(want)) {
			t.Fatalf("block missing %q:\n%s", want, block)
		}
	}
}

func TestBuildResponsesPrompt_ExplicitToolTaskAddsNoNarrationRule(t *testing.T) {
	tools := mustMarshalRawJSON([]map[string]any{
		{
			"type":        "function",
			"name":        "web_search",
			"description": "search",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string"},
				},
			},
		},
	})

	prompt := buildResponsesPrompt(
		nil,
		"",
		[]ChatMessage{{Role: "user", Content: "必须使用 web_search 查询 Go 最新稳定版本"}},
		tools,
		0,
		responsesPromptOptions{},
	)

	for _, want := range []string{
		"<EXPLICIT_TOOL_REQUIREMENTS>",
		"Narration like \"I'll use web_search\" without a real tool call is invalid.",
		"emit AI_ACTIONS mode tool immediately with no visible text before the block",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestParseToolCallOutputs_AIActionsBlockMultipleCalls(t *testing.T) {
	text := "先读取 README.md 和当前目录。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"Read\",\"arguments\":{\"file_path\":\"README.md\"}},{\"name\":\"Bash\",\"arguments\":{\"command\":\"pwd\",\"description\":\"show cwd\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	result := parseToolCallOutputs(text, map[string]responseToolDescriptor{
		"Read": {Name: "Read", Type: "function", Structured: true},
		"Bash": {Name: "Bash", Type: "function", Structured: true},
	}, "")

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 2 {
		t.Fatalf("tool call count = %d, want 2", len(result.calls))
	}
	if result.visibleText != "先读取 README.md 和当前目录。" {
		t.Fatalf("visible text = %q", result.visibleText)
	}
	if !strings.Contains(string(result.calls[0].item), `"name":"Read"`) {
		t.Fatalf("first item = %s", string(result.calls[0].item))
	}
	if !strings.Contains(string(result.calls[1].item), `"name":"Bash"`) {
		t.Fatalf("second item = %s", string(result.calls[1].item))
	}
}

func TestParseToolCallOutputs_LegacySingleToolJSONWithoutType(t *testing.T) {
	text := "AI_ACTIONS:\n```json\n{\"tool\":\"spawn_agent\",\"arguments\":{\"task\":\"Read README.md first line\"}}\n```"
	result := parseToolCallOutputs(text, map[string]responseToolDescriptor{
		"spawn_agent": {Name: "spawn_agent", Type: "function", Structured: true},
	}, "")

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
	if !strings.Contains(string(result.calls[0].item), `"name":"spawn_agent"`) {
		t.Fatalf("call item = %s", string(result.calls[0].item))
	}
	if !strings.Contains(string(result.calls[0].item), `\"message\":\"Read README.md first line\"`) {
		t.Fatalf("spawn_agent call should normalize task->message, item=%s", string(result.calls[0].item))
	}
}

func TestParseToolCallOutputs_ResolvesMCPToolNameHyphenUnderscoreAlias(t *testing.T) {
	text := "<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"mcp__context7__resolve_library_id\",\"arguments\":{\"library_name\":\"react\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	result := parseToolCallOutputs(text, map[string]responseToolDescriptor{
		"mcp__context7__resolve-library-id": {Name: "mcp__context7__resolve-library-id", Type: "function", Structured: true, Namespace: "mcp__context7__"},
	}, "")

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
	if !strings.Contains(string(result.calls[0].item), `"name":"resolve-library-id"`) {
		t.Fatalf("call item = %s", string(result.calls[0].item))
	}
	if !strings.Contains(string(result.calls[0].item), `"namespace":"mcp__context7__"`) {
		t.Fatalf("call item = %s", string(result.calls[0].item))
	}
}

func TestParseToolCallOutputs_ParsesInlineAIActionsShorthand(t *testing.T) {
	text := `I'll use a sub-agent.

AI_ACTIONS: spawn_agent {"task":"Read README.md first line"}`
	result := parseToolCallOutputs(text, map[string]responseToolDescriptor{
		"spawn_agent": {Name: "spawn_agent", Type: "function", Structured: true},
	}, "")

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
	if result.visibleText != "I'll use a sub-agent." {
		t.Fatalf("visible text = %q", result.visibleText)
	}
	item := string(result.calls[0].item)
	if !strings.Contains(item, `"name":"spawn_agent"`) || !strings.Contains(item, `\"message\":\"Read README.md first line\"`) {
		t.Fatalf("unexpected shorthand parse item: %s", item)
	}
}

func TestParseToolCallOutputs_NormalizesSpawnAgentPromptWithoutLeavingAlias(t *testing.T) {
	text := "<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"spawn_agent\",\"arguments\":{\"prompt\":\"Read README.md first line\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	result := parseToolCallOutputs(text, map[string]responseToolDescriptor{
		"spawn_agent": {Name: "spawn_agent", Type: "function", Structured: true},
	}, "")

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
	item := string(result.calls[0].item)
	if !strings.Contains(item, `\"message\":\"Read README.md first line\"`) {
		t.Fatalf("normalized spawn_agent item = %s, want message field", item)
	}
	if strings.Contains(item, `\"prompt\":`) {
		t.Fatalf("normalized spawn_agent item = %s, should not keep prompt alias", item)
	}
}

func TestParseToolCallOutputs_AIActionsBlockFinalMode(t *testing.T) {
	text := "这是最终答案。\n\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"final\"}\n<<<END_AI_ACTIONS_V1>>>"
	result := parseToolCallOutputs(text, map[string]responseToolDescriptor{
		"Read": {Name: "Read", Type: "function", Structured: true},
	}, "")

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 0 {
		t.Fatalf("tool call count = %d, want 0", len(result.calls))
	}
	if result.visibleText != "这是最终答案。" {
		t.Fatalf("visible text = %q", result.visibleText)
	}
}

func TestParseToolCallOutputs_AIActionsBlockWithFencedJSON(t *testing.T) {
	text := "先读取 README。\n<<<AI_ACTIONS_V1>>>\n```json\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"sed -n '1,5p' README.md\"}}]}\n```\n<<<END_AI_ACTIONS_V1>>>"
	result := parseToolCallOutputs(text, map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}, "")

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
	if result.visibleText != "先读取 README。" {
		t.Fatalf("visible text = %q", result.visibleText)
	}
}

func TestParseToolCallOutputs_AIActionsBlockRepairsMissingCallsArrayCloser(t *testing.T) {
	text := "先更新计划。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"update_plan\",\"arguments\":{\"plan\":[{\"step\":\"读取 README\",\"status\":\"pending\"},{\"step\":\"回复\",\"status\":\"pending\"}]}}}\n<<<END_AI_ACTIONS_V1>>>"
	result := parseToolCallOutputs(text, map[string]responseToolDescriptor{
		"update_plan": {Name: "update_plan", Type: "function", Structured: true},
	}, "")

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
	if !strings.Contains(string(result.calls[0].item), `"name":"update_plan"`) {
		t.Fatalf("unexpected item: %s", string(result.calls[0].item))
	}
}

func TestParseToolCallOutputs_AIActionsBlockWithCompatStartMarker(t *testing.T) {
	text := "先读取 README。\n<<<AI_ACTIONS_V1>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"sed -n '1,5p' README.md\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	result := parseToolCallOutputs(text, map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}, "")

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
}

func TestParseToolCallOutputs_FallsBackToLegacyJSON(t *testing.T) {
	result := parseToolCallOutputs(
		"I will inspect first.\n{\"type\":\"function_call\",\"name\":\"run_terminal\",\"arguments\":{\"cmd\":\"pwd\"}}",
		map[string]responseToolDescriptor{
			"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		},
		"",
	)

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
	if !strings.Contains(string(result.calls[0].item), `"name":"exec_command"`) {
		t.Fatalf("item did not normalize alias: %s", string(result.calls[0].item))
	}
}

func TestParseToolCallOutputs_NormalizesTerminalCommandAliases(t *testing.T) {
	for _, alias := range []string{"run_terminal_cmd", "run_command", "shell_command", "execute_command", "terminal_exec"} {
		t.Run(alias, func(t *testing.T) {
			result := parseToolCallOutputs(
				fmt.Sprintf(`{"type":"function_call","name":%q,"arguments":{"cmd":"ls -la"}}`, alias),
				map[string]responseToolDescriptor{
					"exec_command": {Name: "exec_command", Type: "function", Structured: true},
				},
				"",
			)

			if result.err != nil {
				t.Fatalf("unexpected parse error: %v", result.err)
			}
			if len(result.calls) != 1 {
				t.Fatalf("tool call count = %d, want 1", len(result.calls))
			}
			if !strings.Contains(string(result.calls[0].item), `"name":"exec_command"`) {
				t.Fatalf("item did not normalize alias: %s", string(result.calls[0].item))
			}
		})
	}
}

func TestParseToolCallOutputs_AIActionsBlock_RecoversFromTrailingNarration(t *testing.T) {
	text := "先执行。\n<<<AI_ACTIONS_V1>>>\n```json\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"pwd\"}}]}\n```\n补充说明\n<<<END_AI_ACTIONS_V1>>>"
	result := parseToolCallOutputs(text, map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}, "")

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
	if result.visibleText != "先执行。" {
		t.Fatalf("visible text = %q", result.visibleText)
	}
	if !strings.Contains(string(result.calls[0].item), `"name":"exec_command"`) {
		t.Fatalf("item tool name mismatch: %s", string(result.calls[0].item))
	}
}

func TestParseToolCallOutputs_RecoversValidToolBlockFromMultipleAIActionsBlocks(t *testing.T) {
	text := "先读源码。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"internal/proxy/output_constraints.go\"}}]}\n<<<END_AI_ACTIONS_V1>>>\n再创建测试。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"write_file\",\"arguments\":{\"path\":\"internal/proxy/output_constraints_test.go\",\"content\":\"package proxy\"}}]}\n<<<END_AI_ACTIONS_V1>>>\nRESULT: PASS"
	result := parseToolCallOutputs(
		text,
		map[string]responseToolDescriptor{
			"exec_command": {Name: "exec_command", Type: "function", Structured: true},
			"write_file":   {Name: "write_file", Type: "function", Structured: true},
		},
		"",
	)

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
	item := string(result.calls[0].item)
	if !strings.Contains(item, `"name":"write_file"`) {
		t.Fatalf("item tool name mismatch: %s", item)
	}
	if !strings.Contains(item, `\"path\":\"internal/proxy/output_constraints_test.go\"`) {
		t.Fatalf("item missing write target path: %s", item)
	}
}

func TestParseToolCallOutputsWithConstraints_PrefersMutationBlockBeforeTrailingFinal(t *testing.T) {
	text := "先读源码。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"internal/proxy/output_constraints.go\"}}]}\n<<<END_AI_ACTIONS_V1>>>\n再创建测试。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"write_file\",\"arguments\":{\"path\":\"internal/proxy/output_constraints_test.go\",\"content\":\"package proxy\"}}]}\n<<<END_AI_ACTIONS_V1>>>\n运行验证。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"go test ./internal/proxy\"}}]}\n<<<END_AI_ACTIONS_V1>>>\n完成。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"final\"}\n<<<END_AI_ACTIONS_V1>>>"
	result := parseToolCallOutputsWithConstraints(
		text,
		map[string]responseToolDescriptor{
			"exec_command": {Name: "exec_command", Type: "function", Structured: true},
			"write_file":   {Name: "write_file", Type: "function", Structured: true},
		},
		toolProtocolConstraints{
			RequireTool:        true,
			PreferredToolNames: []string{"write_file", "apply_patch"},
		},
	)

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
	item := string(result.calls[0].item)
	if !strings.Contains(item, `"name":"write_file"`) {
		t.Fatalf("expected recovered mutation tool call, got: %s", item)
	}
}

func TestParseToolCallOutputsWithConstraints_PrefersApplyPatchBlockWhenItIsRequiredMutationTool(t *testing.T) {
	text := "先查看文件。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"sed -n '1,200p' internal/proxy/output_constraints.go\"}}]}\n<<<END_AI_ACTIONS_V1>>>\n再修改测试。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"apply_patch\",\"input\":\"*** Begin Patch\\n*** Add File: internal/proxy/output_constraints_test.go\\n+package proxy\\n*** End Patch\"}]}\n<<<END_AI_ACTIONS_V1>>>\n完成。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"final\"}\n<<<END_AI_ACTIONS_V1>>>"
	result := parseToolCallOutputsWithConstraints(
		text,
		map[string]responseToolDescriptor{
			"exec_command": {Name: "exec_command", Type: "function", Structured: true},
			"apply_patch":  {Name: "apply_patch", Type: "custom", Structured: false},
		},
		toolProtocolConstraints{
			RequireTool:        true,
			RequiredTool:       "apply_patch",
			PreferredToolNames: []string{"apply_patch"},
		},
	)

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
	item := string(result.calls[0].item)
	if !strings.Contains(item, `"name":"apply_patch"`) {
		t.Fatalf("expected recovered apply_patch call, got: %s", item)
	}
}

func TestParseToolCallOutputsWithConstraints_RecoversLastToolBlockWhenTailIsFinal(t *testing.T) {
	text := "先读 README。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"head -n 5 README.md\"}}]}\n<<<END_AI_ACTIONS_V1>>>\n再跑测试。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"go test ./internal/proxy\"}}]}\n<<<END_AI_ACTIONS_V1>>>\n完成。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"final\"}\n<<<END_AI_ACTIONS_V1>>>"
	result := parseToolCallOutputsWithConstraints(
		text,
		map[string]responseToolDescriptor{
			"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		},
		toolProtocolConstraints{RequireTool: true},
	)

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
	if got := parsedCallCommand(t, result.calls[0]); got != "go test ./internal/proxy" {
		t.Fatalf("expected last valid tool block, got command %q", got)
	}
}

func TestParseToolCallOutputs_NormalizesExecCommandInputField(t *testing.T) {
	result := parseToolCallOutputs(
		"<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"shell\",\"input\":\"ls -la\"}]}\n<<<END_AI_ACTIONS_V1>>>",
		map[string]responseToolDescriptor{
			"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		},
		"",
	)

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
	item := string(result.calls[0].item)
	if !strings.Contains(item, `"name":"exec_command"`) {
		t.Fatalf("item tool name mismatch: %s", item)
	}
	if !strings.Contains(item, `\"cmd\":\"ls -la\"`) {
		t.Fatalf("item missing normalized cmd argument: %s", item)
	}
}

func TestParseToolCallOutputs_NormalizesExecCommandCommandField(t *testing.T) {
	result := parseToolCallOutputs(
		"<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"command\":\"pwd\"}}]}\n<<<END_AI_ACTIONS_V1>>>",
		map[string]responseToolDescriptor{
			"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		},
		"",
	)

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
	item := string(result.calls[0].item)
	if !strings.Contains(item, `"name":"exec_command"`) {
		t.Fatalf("item tool name mismatch: %s", item)
	}
	if !strings.Contains(item, `\"cmd\":\"pwd\"`) {
		t.Fatalf("item missing normalized cmd argument: %s", item)
	}
}

func TestParseToolCallOutputs_NormalizesReadFileAliasToExecCommand(t *testing.T) {
	result := parseToolCallOutputs(
		"<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"README.md\"}}]}\n<<<END_AI_ACTIONS_V1>>>",
		map[string]responseToolDescriptor{
			"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		},
		"",
	)

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
	item := string(result.calls[0].item)
	if !strings.Contains(item, `"name":"exec_command"`) {
		t.Fatalf("item tool name mismatch: %s", item)
	}
	if !strings.Contains(item, `\"cmd\":\"sed -n '1,200p' 'README.md'\"`) {
		t.Fatalf("item missing normalized read command: %s", item)
	}
}

func TestParseToolCallOutputs_AcceptsWebSearchPseudoTool(t *testing.T) {
	result := parseToolCallOutputs(
		"<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"web_search\",\"arguments\":{\"query\":\"latest Go release\"}}]}\n<<<END_AI_ACTIONS_V1>>>",
		map[string]responseToolDescriptor{
			"web_search": {Name: "web_search", Type: "web_search", Structured: true},
		},
		"",
	)

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
	item := string(result.calls[0].item)
	if !strings.Contains(item, `"id":"ws_`) {
		t.Fatalf("item missing web_search id: %s", item)
	}
	if !strings.Contains(item, `"type":"web_search_call"`) {
		t.Fatalf("item tool type mismatch: %s", item)
	}
	if !strings.Contains(item, `"query":"latest Go release"`) {
		t.Fatalf("item missing query argument: %s", item)
	}
}

func TestParseToolCallOutputs_AcceptsNamespacedFunctionTool(t *testing.T) {
	result := parseToolCallOutputs(
		"<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"mcp__docfork__search_docs\",\"arguments\":{\"library\":\"react\",\"query\":\"server components\"}}]}\n<<<END_AI_ACTIONS_V1>>>",
		map[string]responseToolDescriptor{
			"mcp__docfork__search_docs": {Name: "mcp__docfork__search_docs", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
		},
		"",
	)

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
	item := string(result.calls[0].item)
	if !strings.Contains(item, `"id":"fc_`) {
		t.Fatalf("item missing function-call id: %s", item)
	}
	if !strings.Contains(item, `"name":"search_docs"`) {
		t.Fatalf("item tool name mismatch: %s", item)
	}
	if !strings.Contains(item, `"namespace":"mcp__docfork__"`) {
		t.Fatalf("item missing namespace: %s", item)
	}
}

func TestParseToolCallOutputs_NormalizesDotSeparatedNamespacedToolAlias(t *testing.T) {
	result := parseToolCallOutputs(
		"<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"mcp__docfork__.search_docs\",\"arguments\":{\"library\":\"react\",\"query\":\"server components\"}}]}\n<<<END_AI_ACTIONS_V1>>>",
		map[string]responseToolDescriptor{
			"mcp__docfork__search_docs": {Name: "mcp__docfork__search_docs", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
		},
		"",
	)

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
	item := string(result.calls[0].item)
	if !strings.Contains(item, `"name":"search_docs"`) {
		t.Fatalf("item tool name mismatch: %s", item)
	}
	if !strings.Contains(item, `"namespace":"mcp__docfork__"`) {
		t.Fatalf("item missing namespace: %s", item)
	}
}

func TestParseToolCallOutputs_NormalizesDocforkSearchDocsSourceAlias(t *testing.T) {
	result := parseToolCallOutputs(
		"<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"mcp__docfork__search_docs\",\"arguments\":{\"query\":\"useEffectEvent\",\"source\":\"react.dev\"}}]}\n<<<END_AI_ACTIONS_V1>>>",
		map[string]responseToolDescriptor{
			"mcp__docfork__search_docs": {Name: "mcp__docfork__search_docs", Type: "function", Structured: true, Namespace: "mcp__docfork__"},
		},
		"",
	)

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
	item := string(result.calls[0].item)
	if !strings.Contains(item, `"name":"search_docs"`) {
		t.Fatalf("item tool name mismatch: %s", item)
	}
	var decoded map[string]any
	if err := json.Unmarshal(result.calls[0].item, &decoded); err != nil {
		t.Fatalf("decode tool call: %v", err)
	}
	argumentsText, _ := decoded["arguments"].(string)
	var arguments map[string]any
	if err := json.Unmarshal([]byte(argumentsText), &arguments); err != nil {
		t.Fatalf("decode arguments: %v", err)
	}
	if arguments["library"] != "react.dev" {
		t.Fatalf("library = %v, want react.dev", arguments["library"])
	}
	if _, ok := arguments["source"]; ok {
		t.Fatalf("source alias should be removed, arguments=%#v", arguments)
	}
}

func TestParseToolCallOutputs_LegacyJSONSequence(t *testing.T) {
	result := parseToolCallOutputs(
		"{\"type\":\"function_call\",\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"sed -n '1,5p' README.md\"}}\n{\"type\":\"function_call\",\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"sed -n '170,260p' internal/proxy/tool_protocol.go\"}}",
		map[string]responseToolDescriptor{
			"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		},
		"",
	)

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 2 {
		t.Fatalf("tool call count = %d, want 2", len(result.calls))
	}
	if !strings.Contains(string(result.calls[0].item), `\"cmd\":\"sed -n '1,5p' README.md\"`) {
		t.Fatalf("first item missing cmd: %s", string(result.calls[0].item))
	}
	if !strings.Contains(string(result.calls[1].item), `\"cmd\":\"sed -n '170,260p' internal/proxy/tool_protocol.go\"`) {
		t.Fatalf("second item missing cmd: %s", string(result.calls[1].item))
	}
}

func TestParseToolCallOutputs_LegacyJSONSequence_AllowsFunctionCallClosingTagTail(t *testing.T) {
	result := parseToolCallOutputs(
		"{\"type\":\"function_call\",\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"sed -n '1,5p' README.md\"}}\n{\"type\":\"function_call\",\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"sed -n '170,260p' internal/proxy/tool_protocol.go\"}}\n</function_call>",
		map[string]responseToolDescriptor{
			"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		},
		"",
	)

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 2 {
		t.Fatalf("tool call count = %d, want 2", len(result.calls))
	}
}

func TestParseToolCallOutputs_LegacyJSONSequence_AllowsAIActionsClosingTagTail(t *testing.T) {
	result := parseToolCallOutputs(
		"{\"type\":\"function_call\",\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"pwd\"}}\n</ai_actions>",
		map[string]responseToolDescriptor{
			"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		},
		"",
	)

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
}

func TestParseToolCallOutputs_LegacyJSONSequenceWithPrefixText(t *testing.T) {
	result := parseToolCallOutputs(
		"我先读取两个位置。\n{\"type\":\"function_call\",\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"sed -n '1,5p' README.md\"}}\n{\"type\":\"function_call\",\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"sed -n '170,260p' internal/proxy/tool_protocol.go\"}}",
		map[string]responseToolDescriptor{
			"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		},
		"",
	)

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 2 {
		t.Fatalf("tool call count = %d, want 2", len(result.calls))
	}
	if result.visibleText != "我先读取两个位置。\n{\"type\":\"function_call\",\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"sed -n '1,5p' README.md\"}}\n{\"type\":\"function_call\",\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"sed -n '170,260p' internal/proxy/tool_protocol.go\"}}" {
		t.Fatalf("visible text = %q", result.visibleText)
	}
}

func TestParseToolCallOutputs_LegacyJSON_LongProseWithBracesTreatedAsPlainText(t *testing.T) {
	text := strings.Repeat("这是普通分析文本，不是工具调用。", 8) + "\n```go\nfunc demo() { return }\n```\n"
	result := parseToolCallOutputs(
		text,
		map[string]responseToolDescriptor{
			"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		},
		"",
	)

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 0 {
		t.Fatalf("tool call count = %d, want 0", len(result.calls))
	}
	if result.mode != toolProtocolModePlainText {
		t.Fatalf("mode = %v, want plain_text", result.mode)
	}
}

func TestParseToolCallOutputs_LegacyJSONSequenceRejectsMarkdownBetweenCalls(t *testing.T) {
	result := parseToolCallOutputs(
		"```json\n{\"type\":\"function_call\",\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"pwd\"}}\n```\n```json\n{\"type\":\"function_call\",\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"ls\"}}\n```",
		map[string]responseToolDescriptor{
			"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		},
		"",
	)

	if result.err == nil || !strings.Contains(result.err.Error(), "tool call JSON decode failed") {
		t.Fatalf("expected decode error for markdown-separated legacy JSON, got %v", result.err)
	}
}

func TestParseToolCallOutputs_LegacyJSONWithoutTypeStaysPlainText(t *testing.T) {
	text := `{"name":"exec_command","arguments":{"cmd":"pwd"}}`
	result := parseToolCallOutputs(
		text,
		map[string]responseToolDescriptor{
			"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		},
		"",
	)

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 0 {
		t.Fatalf("tool call count = %d, want 0", len(result.calls))
	}
	if result.visibleText != text {
		t.Fatalf("visible text = %q, want %q", result.visibleText, text)
	}
}

func TestParseToolCallOutputs_LegacyJSONWithoutTypeWithFenceTailStaysPlainText(t *testing.T) {
	text := "{\"cmd\":\"pwd\"}\n```"
	result := parseToolCallOutputs(
		text,
		map[string]responseToolDescriptor{
			"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		},
		"",
	)

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 0 {
		t.Fatalf("tool call count = %d, want 0", len(result.calls))
	}
	if result.visibleText != text {
		t.Fatalf("visible text = %q, want %q", result.visibleText, text)
	}
}

func TestParseToolCallOutputsWithConstraints_RequireToolRejectsFinalMode(t *testing.T) {
	result := parseToolCallOutputsWithConstraints(
		"这是最终答案。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"final\"}\n<<<END_AI_ACTIONS_V1>>>",
		map[string]responseToolDescriptor{
			"Read": {Name: "Read", Type: "function", Structured: true},
		},
		toolProtocolConstraints{RequireTool: true},
	)

	if result.err == nil || !strings.Contains(result.err.Error(), "requires a tool call") {
		t.Fatalf("expected required-tool error, got %v", result.err)
	}
}

func TestParseToolCallOutputsWithConstraints_MaxCallsRejectsMultipleCalls(t *testing.T) {
	result := parseToolCallOutputsWithConstraints(
		"先做两步。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"Read\",\"arguments\":{\"file_path\":\"README.md\"}},{\"name\":\"Read\",\"arguments\":{\"file_path\":\"AGENTS.md\"}}]}\n<<<END_AI_ACTIONS_V1>>>",
		map[string]responseToolDescriptor{
			"Read": {Name: "Read", Type: "function", Structured: true},
		},
		toolProtocolConstraints{MaxCalls: 1},
	)

	if result.err == nil || !strings.Contains(result.err.Error(), "at most 1 call") {
		t.Fatalf("expected max-calls error, got %v", result.err)
	}
}

func TestParseToolCallOutputs_RejectsStructuredToolUsingInputField(t *testing.T) {
	result := parseToolCallOutputs(
		"<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"Read\",\"input\":\"README.md\"}]}\n<<<END_AI_ACTIONS_V1>>>",
		map[string]responseToolDescriptor{
			"Read": {Name: "Read", Type: "function", Structured: true},
		},
		"",
	)

	if result.err == nil || !strings.Contains(result.err.Error(), "must use arguments") {
		t.Fatalf("expected structured-field mismatch error, got %v", result.err)
	}
}

func TestParseToolCallOutputs_NormalizesCustomToolArgumentsInputAlias(t *testing.T) {
	result := parseToolCallOutputs(
		"<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"js_repl\",\"arguments\":{\"input\":\"[2,3,5].reduce((a,b) => a + b, 0)\"}}]}\n<<<END_AI_ACTIONS_V1>>>",
		map[string]responseToolDescriptor{
			"js_repl": {Name: "js_repl", Type: "custom", Structured: false},
		},
		"",
	)

	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(result.calls))
	}
	item := string(result.calls[0].item)
	if !strings.Contains(item, `"type":"custom_tool_call"`) {
		t.Fatalf("item tool type mismatch: %s", item)
	}
	if !strings.Contains(item, `"name":"js_repl"`) {
		t.Fatalf("item tool name mismatch: %s", item)
	}
	var decoded map[string]any
	if err := json.Unmarshal(result.calls[0].item, &decoded); err != nil {
		t.Fatalf("decode tool call: %v", err)
	}
	if decoded["input"] != "[2,3,5].reduce((a,b) => a + b, 0)" {
		t.Fatalf("input = %v, want raw js source", decoded["input"])
	}
}

func TestHandleChatCompletions_NonStreamAIActionsBlock_MultipleToolCalls(t *testing.T) {
	content := "先检查文件和目录。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"Read\",\"arguments\":{\"file_path\":\"README.md\"}},{\"name\":\"Bash\",\"arguments\":{\"command\":\"pwd\",\"description\":\"show cwd\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(marshalSSEContent(t, content)))
		_, _ = w.Write([]byte("data: {\"type\":\"done\"}\n\n"))
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")

	body := `{
		"model":"deepseek-v3p2",
		"stream":false,
		"messages":[{"role":"user","content":"读取 README.md 并查看当前目录"}],
		"tools":[
			{"type":"function","function":{"name":"Read","description":"read file","parameters":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}},
			{"type":"function","function":{"name":"Bash","description":"run shell","parameters":{"type":"object","properties":{"command":{"type":"string"},"description":{"type":"string"}},"required":["command"]}}}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var resp ChatCompletionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := resp.Choices[0].FinishReason; got != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", got)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 2 {
		t.Fatalf("tool_calls len = %d, want 2", len(resp.Choices[0].Message.ToolCalls))
	}
	if resp.Choices[0].Message.ToolCalls[0].Function.Name != "Read" {
		t.Fatalf("first tool = %q, want Read", resp.Choices[0].Message.ToolCalls[0].Function.Name)
	}
	if resp.Choices[0].Message.ToolCalls[1].Function.Name != "Bash" {
		t.Fatalf("second tool = %q, want Bash", resp.Choices[0].Message.ToolCalls[1].Function.Name)
	}
}

func TestHandleResponses_StreamAIActionsBlock_MultipleToolCalls(t *testing.T) {
	content := "先检查目录和 README。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"pwd\"}},{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"ls\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(marshalSSEContent(t, content)))
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"content\":\"\"}\n\n"))
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")
	body := `{"model":"deepseek-v3p2","input":"查看目录并列出文件","stream":true,"tools":[{"type":"function","name":"exec_command","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	bodyText := rec.Body.String()
	if strings.Count(bodyText, "event: response.output_item.added") != 2 {
		t.Fatalf("response.output_item.added count = %d, want 2:\n%s", strings.Count(bodyText, "event: response.output_item.added"), bodyText)
	}
	if strings.Count(bodyText, "event: response.output_item.done") != 2 {
		t.Fatalf("response.output_item.done count = %d, want 2:\n%s", strings.Count(bodyText, "event: response.output_item.done"), bodyText)
	}
	if strings.Index(bodyText, "event: response.output_item.added") > strings.Index(bodyText, "event: response.output_item.done") {
		t.Fatalf("response.output_item.added should appear before done:\n%s", bodyText)
	}
	for _, want := range []string{
		`"type":"function_call"`,
		`"name":"exec_command"`,
		`\"cmd\":\"pwd\"`,
		`\"cmd\":\"ls\"`,
		"event: response.completed",
	} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("stream body missing %q:\n%s", want, bodyText)
		}
	}
	if strings.Contains(bodyText, "response.output_text.delta") {
		t.Fatalf("tool-call stream should not emit text deltas:\n%s", bodyText)
	}
}

func TestHandleResponses_ToolChoiceNoneDoesNotExposeToolsToUpstream(t *testing.T) {
	var capturedPrompt string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var req FireworksRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if len(req.Messages) != 1 {
			t.Fatalf("upstream messages len = %d, want 1", len(req.Messages))
		}
		capturedPrompt = req.Messages[0].Content
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, marshalSSEContent(t, "纯文本结果"))
		fmt.Fprint(w, "data: {\"type\":\"done\",\"content\":\"\"}\n\n")
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")

	body := `{"model":"deepseek-v3p2","input":"只回答结果","stream":false,"tool_choice":"none","tools":[{"type":"function","name":"exec_command","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	for _, unwanted := range []string{
		"<AVAILABLE_TOOLS>",
		"exec_command",
		"<<<AI_ACTIONS_V1>>>",
		`{"mode":"tool","calls":[`,
	} {
		if strings.Contains(capturedPrompt, unwanted) {
			t.Fatalf("responses upstream prompt should not expose tools when tool_choice=none, found %q:\n%s", unwanted, capturedPrompt)
		}
	}
	if !strings.Contains(capturedPrompt, "Do not call any tools. Answer with plain text only.") {
		t.Fatalf("responses upstream prompt missing tool_choice=none guidance:\n%s", capturedPrompt)
	}
}

func TestShouldDisableToolsForExecutionFinalize(t *testing.T) {
	policy := executionPolicy{
		Enabled: true,
		Stage:   "finalize",
	}

	if !shouldDisableToolsForExecutionFinalize(policy, resolvedToolChoice{}) {
		t.Fatal("finalize stage should disable tools for implicit tool choice")
	}
	if shouldDisableToolsForExecutionFinalize(policy, resolvedToolChoice{RequireTool: true}) {
		t.Fatal("required tool choice should keep tools enabled")
	}
	if shouldDisableToolsForExecutionFinalize(policy, resolvedToolChoice{RequiredTool: "exec_command", RequireTool: true}) {
		t.Fatal("named required tool choice should keep tools enabled")
	}
}

func TestHandleResponses_ExecutionFinalizeDoesNotExposeToolsToUpstream(t *testing.T) {
	var capturedPrompt string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var req FireworksRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if len(req.Messages) != 1 {
			t.Fatalf("upstream messages len = %d, want 1", len(req.Messages))
		}
		capturedPrompt = req.Messages[0].Content
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, marshalSSEContent(t, "RESULT: PASS\nREADME: # firew2oai\nTOOLP: tool_choice requires tool call when enabled"))
		fmt.Fprint(w, "data: {\"type\":\"done\",\"content\":\"\"}\n\n")
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")

	body := `{"model":"deepseek-v3p2","stream":false,"input":[{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"head -n 5 README.md\"}"},{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"sed -n '170,260p' internal/proxy/tool_protocol.go\"}"},{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"go test ./internal/proxy\"}"},{"role":"user","content":"只读审计任务：1) 执行 head -n 5 README.md 2) 执行 sed -n '170,260p' internal/proxy/tool_protocol.go 3) 执行 go test ./internal/proxy 最终只输出三行。"}],"tools":[{"type":"function","name":"exec_command","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	for _, unwanted := range []string{
		"<AVAILABLE_TOOLS>",
		"<<<AI_ACTIONS_V1>>>",
		`{"mode":"tool","calls":[`,
	} {
		if strings.Contains(capturedPrompt, unwanted) {
			t.Fatalf("responses upstream prompt should not expose tools in finalize stage, found %q:\n%s", unwanted, capturedPrompt)
		}
	}
	if !strings.Contains(capturedPrompt, "Execution policy reached finalize stage. Do not call any tools.") {
		t.Fatalf("responses upstream prompt missing finalize no-tool guidance:\n%s", capturedPrompt)
	}
}

func TestExtractAIActionsBlock_RejectsTrailingContentAfterMarker(t *testing.T) {
	_, found := extractAIActionsBlock("ok\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"final\"}\n<<<END_AI_ACTIONS_V1>>>\nextra")
	if found {
		t.Fatal("expected trailing content after end marker to disable block parsing")
	}
}

func TestExtractAIActionsBlock_AcceptsCompatStartMarker(t *testing.T) {
	block, found := extractAIActionsBlock("ok\n<<<AI_ACTIONS_V1>>\n{\"mode\":\"final\"}\n<<<END_AI_ACTIONS_V1>>>")
	if !found {
		t.Fatal("expected compat start marker to be accepted")
	}
	if block.VisibleText != "ok" {
		t.Fatalf("visible text = %q, want %q", block.VisibleText, "ok")
	}
	if block.JSONText != "{\"mode\":\"final\"}" {
		t.Fatalf("json text = %q", block.JSONText)
	}
}

func TestParseToolCallOutputs_RepairsMissingCallBraceInAIActions(t *testing.T) {
	text := "开始执行。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"pwd\"}]}\n<<<END_AI_ACTIONS_V1>>>"
	allowedTools := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}

	result := parseToolCallOutputs(text, allowedTools, "")
	if result.err != nil {
		t.Fatalf("parseToolCallOutputs error = %v", result.err)
	}
	if len(result.calls) != 1 {
		t.Fatalf("len(result.calls) = %d, want 1", len(result.calls))
	}
	if cmd := parsedCallCommand(t, result.calls[0]); cmd != "pwd" {
		t.Fatalf("cmd = %q, want pwd", cmd)
	}
}

func TestHandleResponses_NonStreamAIActionsBlock_FinalModeStripsControlBlock(t *testing.T) {
	content := "这是最终文本。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"final\"}\n<<<END_AI_ACTIONS_V1>>>"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, marshalSSEContent(t, content))
		fmt.Fprint(w, "data: {\"type\":\"done\",\"content\":\"\"}\n\n")
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")

	body := `{"model":"deepseek-v3p2","input":"只回答结果","stream":false,"tools":[{"type":"function","name":"exec_command","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var resp ResponsesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("output len = %d, want 1", len(resp.Output))
	}
	var item ResponseOutputMessage
	if err := json.Unmarshal(resp.Output[0], &item); err != nil {
		t.Fatalf("decode output item: %v", err)
	}
	if got := item.Content[0].Text; got != "这是最终文本。" {
		t.Fatalf("final text = %q, want stripped text", got)
	}
}

func TestHandleResponses_NonStreamAIActionsBlock_RequiredToolRejectsFinalMode(t *testing.T) {
	content := "这是最终文本。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"final\"}\n<<<END_AI_ACTIONS_V1>>>"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, marshalSSEContent(t, content))
		fmt.Fprint(w, "data: {\"type\":\"done\",\"content\":\"\"}\n\n")
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")

	body := `{"model":"deepseek-v3p2","input":"必须用工具","stream":false,"tool_choice":"required","tools":[{"type":"function","name":"exec_command","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var resp ResponsesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var item ResponseOutputMessage
	if err := json.Unmarshal(resp.Output[0], &item); err != nil {
		t.Fatalf("decode output item: %v", err)
	}
	if !strings.Contains(item.Content[0].Text, "Codex adapter error: tool_choice requires a tool call") {
		t.Fatalf("expected explicit required-tool error, got %q", item.Content[0].Text)
	}
}

func TestHandleResponses_NonStreamAIActionsBlock_ActionTaskSynthesizesNextToolCall(t *testing.T) {
	content := "这是最终文本。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"final\"}\n<<<END_AI_ACTIONS_V1>>>"
	var capturedPrompt string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var req FireworksRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if len(req.Messages) != 1 {
			t.Fatalf("upstream messages len = %d, want 1", len(req.Messages))
		}
		capturedPrompt = req.Messages[0].Content
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, marshalSSEContent(t, content))
		fmt.Fprint(w, "data: {\"type\":\"done\",\"content\":\"\"}\n\n")
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")

	body := `{"model":"deepseek-v3p2","input":"修改 internal/proxy/responses.go 并运行 go test ./internal/proxy","stream":false,"tools":[{"type":"function","name":"exec_command","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var resp ResponsesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("output len = %d, want 1", len(resp.Output))
	}
	var item map[string]any
	if err := json.Unmarshal(resp.Output[0], &item); err != nil {
		t.Fatalf("decode output item map: %v", err)
	}
	if typ, _ := item["type"].(string); typ != "function_call" {
		t.Fatalf("output type = %q, want function_call; raw=%s", typ, string(resp.Output[0]))
	}
	if name, _ := item["name"].(string); name != "exec_command" {
		t.Fatalf("tool name = %q, want exec_command; raw=%s", name, string(resp.Output[0]))
	}
	argsText, _ := item["arguments"].(string)
	if !strings.Contains(argsText, "internal/proxy/responses.go") {
		t.Fatalf("synthetic command should prioritize reading the target file before verification, got arguments=%q", argsText)
	}
	if !strings.Contains(capturedPrompt, "<TASK_COMPLETION_GATE>") {
		t.Fatalf("action task prompt missing completion gate:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "<EXECUTION_POLICY>") {
		t.Fatalf("action task prompt missing execution policy block:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "internal/proxy/responses.go") {
		t.Fatalf("action task prompt missing soft tool guidance:\n%s", capturedPrompt)
	}
}

func TestHandleResponses_NonStreamAIActionsBlock_PlainQuestionDoesNotAutoRequireTool(t *testing.T) {
	content := "这是仓库简介。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"final\"}\n<<<END_AI_ACTIONS_V1>>>"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, marshalSSEContent(t, content))
		fmt.Fprint(w, "data: {\"type\":\"done\",\"content\":\"\"}\n\n")
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")

	body := `{"model":"deepseek-v3p2","input":"请总结这个仓库的用途","stream":false,"tools":[{"type":"function","name":"exec_command","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var resp ResponsesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var item ResponseOutputMessage
	if err := json.Unmarshal(resp.Output[0], &item); err != nil {
		t.Fatalf("decode output item: %v", err)
	}
	if strings.Contains(item.Content[0].Text, "Codex adapter error: tool_choice requires a tool call") {
		t.Fatalf("plain question should not auto-require tools, got %q", item.Content[0].Text)
	}
	if item.Content[0].Text != "这是仓库简介。" {
		t.Fatalf("final text = %q, want stripped visible text", item.Content[0].Text)
	}
}

func TestHandleResponses_NonStreamAIActionsBlock_ParallelToolCallsFalseRejectsMultipleCalls(t *testing.T) {
	content := "先做两步。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"pwd\"}},{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"ls\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, marshalSSEContent(t, content))
		fmt.Fprint(w, "data: {\"type\":\"done\",\"content\":\"\"}\n\n")
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")

	body := `{"model":"deepseek-v3p2","input":"最多一个工具","stream":false,"parallel_tool_calls":false,"tools":[{"type":"function","name":"exec_command","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var resp ResponsesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var item ResponseOutputMessage
	if err := json.Unmarshal(resp.Output[0], &item); err != nil {
		t.Fatalf("decode output item: %v", err)
	}
	if !strings.Contains(item.Content[0].Text, "at most 1 call") {
		t.Fatalf("expected parallel limit error, got %q", item.Content[0].Text)
	}
}

func TestHandleChatCompletions_NonStreamAIActionsBlock_RequiredToolRejectsFinalMode(t *testing.T) {
	content := "这是最终文本。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"final\"}\n<<<END_AI_ACTIONS_V1>>>"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(marshalSSEContent(t, content)))
		_, _ = w.Write([]byte("data: {\"type\":\"done\"}\n\n"))
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")

	body := `{
		"model":"deepseek-v3p2",
		"stream":false,
		"tool_choice":"required",
		"messages":[{"role":"user","content":"必须调用工具"}],
		"tools":[{"type":"function","function":{"name":"Read","description":"read file","parameters":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var resp ChatCompletionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := resp.Choices[0].FinishReason; got != "stop" {
		t.Fatalf("finish_reason = %q, want stop", got)
	}
	if !strings.Contains(resp.Choices[0].Message.Content, "Codex adapter error: tool_choice requires a tool call") {
		t.Fatalf("expected explicit required-tool error, got %q", resp.Choices[0].Message.Content)
	}
}

func TestHandleChatCompletions_NonStreamAIActionsBlock_ActionTaskUsesSoftToolGuidance(t *testing.T) {
	content := "这是最终文本。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"final\"}\n<<<END_AI_ACTIONS_V1>>>"
	var capturedPrompt string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var req FireworksRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if len(req.Messages) != 1 {
			t.Fatalf("upstream messages len = %d, want 1", len(req.Messages))
		}
		capturedPrompt = req.Messages[0].Content
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(marshalSSEContent(t, content)))
		_, _ = w.Write([]byte("data: {\"type\":\"done\"}\n\n"))
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")

	body := `{
		"model":"deepseek-v3p2",
		"stream":false,
		"messages":[{"role":"user","content":"修改 internal/proxy/tool_protocol.go 并运行 go test ./internal/proxy"}],
		"tools":[{"type":"function","function":{"name":"Read","description":"read file","parameters":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var resp ChatCompletionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if strings.Contains(resp.Choices[0].Message.Content, "Codex adapter error: tool_choice requires a tool call") {
		t.Fatalf("action task soft guidance should not force tool-call error, got %q", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].Message.Content != "这是最终文本。" {
		t.Fatalf("final text = %q, want stripped visible text", resp.Choices[0].Message.Content)
	}
	if !strings.Contains(capturedPrompt, "requires workspace execution. Emit tool calls before any final answer text.") {
		t.Fatalf("action task prompt missing soft tool guidance:\n%s", capturedPrompt)
	}
}

func TestHandleChatCompletions_ToolChoiceRequiredWithoutToolsRejected(t *testing.T) {
	p := NewWithUpstream(transport.New(30*time.Second), "test", false, "http://127.0.0.1:1")
	mux := newTestMux(t, p, "*")

	body := `{
		"model":"deepseek-v3p2",
		"stream":false,
		"tool_choice":"required",
		"messages":[{"role":"user","content":"必须调用工具"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "tool_choice requires at least one declared tool") {
		t.Fatalf("expected explicit tool-choice validation error, got %s", rec.Body.String())
	}
}

func TestHandleResponses_StreamAIActionsBlock_RequiredToolRejectsFinalMode(t *testing.T) {
	content := "这是最终文本。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"final\"}\n<<<END_AI_ACTIONS_V1>>>"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, marshalSSEContent(t, content))
		fmt.Fprint(w, "data: {\"type\":\"done\",\"content\":\"\"}\n\n")
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")

	body := `{"model":"deepseek-v3p2","input":"必须调用工具","stream":true,"tool_choice":"required","tools":[{"type":"function","name":"exec_command","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	bodyText := rec.Body.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.output_item.done",
		"event: response.completed",
		"Codex adapter error: tool_choice requires a tool call",
	} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("stream body missing %q:\n%s", want, bodyText)
		}
	}
	if strings.Index(bodyText, "event: response.output_item.added") > strings.Index(bodyText, "event: response.output_item.done") {
		t.Fatalf("response.output_item.added should appear before done on fallback path:\n%s", bodyText)
	}
	if strings.Contains(bodyText, `"type":"function_call"`) {
		t.Fatalf("required-tool error path should not emit function_call item:\n%s", bodyText)
	}
}

func TestHandleResponses_ToolChoiceRequiredWithoutToolsRejected(t *testing.T) {
	p := NewWithUpstream(transport.New(30*time.Second), "test", false, "http://127.0.0.1:1")
	mux := newTestMux(t, p, "*")

	body := `{"model":"deepseek-v3p2","input":"必须调用工具","stream":false,"tool_choice":"required"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "tool_choice requires at least one declared tool") {
		t.Fatalf("expected explicit tool-choice validation error, got %s", rec.Body.String())
	}
}

func TestHandleResponses_StreamAIActionsBlock_ParallelToolCallsFalseRejectsMultipleCalls(t *testing.T) {
	content := "先做两步。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"pwd\"}},{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"ls\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, marshalSSEContent(t, content))
		fmt.Fprint(w, "data: {\"type\":\"done\",\"content\":\"\"}\n\n")
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")

	body := `{"model":"deepseek-v3p2","input":"最多一个工具","stream":true,"parallel_tool_calls":false,"tools":[{"type":"function","name":"exec_command","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	bodyText := rec.Body.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.output_item.done",
		"event: response.completed",
		"Codex adapter error: tool protocol allows at most 1 call",
	} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("stream body missing %q:\n%s", want, bodyText)
		}
	}
	if strings.Contains(bodyText, `"type":"function_call"`) {
		t.Fatalf("parallel-tool limit error path should not emit function_call item:\n%s", bodyText)
	}
}

func TestHandleResponses_StreamAIActionsBlock_RequiredToolRejectsFinalModeWithoutDoneStillEmitsAdded(t *testing.T) {
	content := "这是最终文本。\n<<<AI_ACTIONS_V1>>>\n{\"mode\":\"final\"}\n<<<END_AI_ACTIONS_V1>>>"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, marshalSSEContent(t, content))
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")

	body := `{"model":"deepseek-v3p2","input":"必须调用工具","stream":true,"tool_choice":"required","tools":[{"type":"function","name":"exec_command","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	bodyText := rec.Body.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.output_text.done",
		"event: response.output_item.done",
		"event: response.completed",
		"Codex adapter error: tool_choice requires a tool call",
	} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("stream body missing %q:\n%s", want, bodyText)
		}
	}
	if strings.Index(bodyText, "event: response.output_item.added") > strings.Index(bodyText, "event: response.output_item.done") {
		t.Fatalf("response.output_item.added should appear before done on no-done fallback path:\n%s", bodyText)
	}
}
