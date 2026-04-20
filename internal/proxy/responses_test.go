package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mison/firew2oai/internal/transport"
)

func TestResponseInputToMessages_String(t *testing.T) {
	msgs, err := responseInputToMessages(json.RawMessage(`"hello"`))
	if err != nil {
		t.Fatalf("responseInputToMessages error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "hello" {
		t.Fatalf("user message = %+v", msgs[0])
	}
}

func TestResponseInputToMessages_ArrayContentParts(t *testing.T) {
	input := json.RawMessage(`[
		{"role":"user","content":[{"type":"input_text","text":"hello "},{"type":"input_text","text":"world"}]},
		{"type":"input_text","text":"again"}
	]`)
	msgs, err := responseInputToMessages(input)
	if err != nil {
		t.Fatalf("responseInputToMessages error: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if msgs[0].Content != "hello world" {
		t.Errorf("msgs[0].Content = %q, want hello world", msgs[0].Content)
	}
	if msgs[1].Content != "again" {
		t.Errorf("msgs[1].Content = %q, want again", msgs[1].Content)
	}
}

func TestResponseInputToMessages_Invalid(t *testing.T) {
	_, err := responseInputToMessages(json.RawMessage(`[]`))
	if err == nil {
		t.Fatal("expected error for empty input array")
	}
}

func TestResponsesPromptMessages_InstructionsNotStored(t *testing.T) {
	base := []ChatMessage{{Role: "user", Content: "first"}, {Role: "assistant", Content: "answer"}}
	current := []ChatMessage{
		{Role: "developer", Content: "use tools carefully"},
		{Role: "user", Content: "repo rules"},
		{Role: "user", Content: "second"},
	}
	tools := json.RawMessage(`[{"type":"function","name":"exec_command","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]`)
	prompt := buildResponsesPrompt(base, "be concise", current, tools, 0, responsesPromptOptions{})

	for _, want := range []string{
		"<BASE_INSTRUCTIONS>",
		"be concise",
		"<PREVIOUS_CONVERSATION>",
		"User: first",
		"Assistant: answer",
		"<CURRENT_TURN_CONTEXT>",
		"Developer: use tools carefully",
		"User: repo rules",
		"<CURRENT_USER_TASK>",
		"second",
		"<AVAILABLE_TOOLS>",
		"exec_command",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildResponsesPrompt_FinalizeCompactsMetaContext(t *testing.T) {
	base := []ChatMessage{
		{Role: "assistant", Content: "I've received the context handoff. What would you like me to work on?"},
		{Role: "user", Content: "Tool result (call_id=abc)\nSuccess: true\nOutput:\n/Volumes/Work/code/firew2oai"},
	}
	current := []ChatMessage{
		{Role: "developer", Content: "checkpoint handoff summary"},
		{Role: "user", Content: "只输出四行：RESULT: PASS；CONSTRAINT: ...；EVIDENCE: ...；TEST: ..."},
	}
	tools := json.RawMessage(`[{"type":"function","name":"exec_command","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]`)

	prompt := buildResponsesPrompt(base, "be concise", current, tools, 0, responsesPromptOptions{
		CompactForFinalize:  true,
		SuppressMetaContext: true,
	})

	for _, want := range []string{
		"<CURRENT_USER_TASK>",
		"只输出四行",
		"Finalize stage reached.",
		"Use CURRENT_USER_TASK and EXECUTION_EVIDENCE to produce the final answer.",
		"<FINAL_OUTPUT_FORMAT>",
		"RESULT: <value>",
		"CONSTRAINT: <value>",
		"EVIDENCE: <value>",
		"TEST: <value>",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("compact finalize prompt missing %q:\n%s", want, prompt)
		}
	}
	for _, unwanted := range []string{
		"<PREVIOUS_CONVERSATION>",
		"<CURRENT_TURN_CONTEXT>",
		"context handoff",
		"checkpoint handoff summary",
		"What would you like me to work on",
	} {
		if strings.Contains(prompt, unwanted) {
			t.Fatalf("compact finalize prompt should omit %q:\n%s", unwanted, prompt)
		}
	}
}

func TestBuildPendingWriteMutationHint_ListsMissingTargetsAndMutationTools(t *testing.T) {
	policy := executionPolicy{
		PendingWrite: true,
		MissingFiles: []string{"internal/proxy/output_constraints_test.go"},
	}
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		"write_file":   {Name: "write_file", Type: "function", Structured: true},
		"apply_patch":  {Name: "apply_patch", Type: "custom", Structured: false},
	}

	hint := buildPendingWriteMutationHint(policy, toolCatalog)
	for _, want := range []string{
		"<WRITE_MUTATION_HINT>",
		"internal/proxy/output_constraints_test.go",
		"write_file",
		"apply_patch",
		"Emit exactly one mutation tool call now.",
	} {
		if !strings.Contains(hint, want) {
			t.Fatalf("write mutation hint missing %q:\n%s", want, hint)
		}
	}
	if strings.Contains(hint, "exec_command") {
		t.Fatalf("write mutation hint should not list non-mutation tools:\n%s", hint)
	}
}

func TestBuildPendingWriteMutationHint_FallsBackToExecCommandWriteGuidance(t *testing.T) {
	policy := executionPolicy{
		PendingWrite:         true,
		AllRequiredFilesSeen: true,
		RequiredFiles:        []string{"internal/proxy/output_constraints_test.go"},
		EmptyFiles:           []string{"internal/proxy/output_constraints_test.go"},
		RepeatedScaffold:     []string{"internal/proxy/output_constraints_test.go"},
	}
	toolCatalog := map[string]responseToolDescriptor{
		"exec_command": {Name: "exec_command", Type: "function", Structured: true},
	}

	hint := buildPendingWriteMutationHint(policy, toolCatalog)
	for _, want := range []string{
		"All required files have already been inspected.",
		"These target files already exist but are still empty.",
		"Repeated scaffold-only commands were already observed",
		"Use exec_command with a shell write/edit command now.",
		"Invalid now: pwd, ls, cat, sed -n, head, tail, rg, grep, or any other read-only command.",
		"Emit exactly one exec_command call whose cmd mutates the target file now.",
	} {
		if !strings.Contains(hint, want) {
			t.Fatalf("exec_command fallback write hint missing %q:\n%s", want, hint)
		}
	}
}

func TestPreferredPendingWriteTool_PrefersConcreteFileMutationBeforeApplyPatch(t *testing.T) {
	got := preferredPendingWriteTool([]string{"apply_patch", "write_file", "append_file"})
	if got != "write_file" {
		t.Fatalf("preferredPendingWriteTool = %q, want write_file", got)
	}
}

func TestBuildResponsesPrompt_UsesDeclaredFileToolNamesWhenAvailable(t *testing.T) {
	tools := json.RawMessage(`[
		{"type":"function","name":"read_file","description":"read file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}},
		{"type":"function","name":"write_file","description":"write file","parameters":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}}}}
	]`)
	prompt := buildResponsesPrompt(nil, "", []ChatMessage{{Role: "user", Content: "修改 internal/proxy/output_constraints.go"}}, tools, 1, responsesPromptOptions{})

	if !strings.Contains(prompt, "If file tools are listed in AVAILABLE_TOOLS, use those exact names for file reads and writes.") {
		t.Fatalf("prompt missing declared file tool guidance:\n%s", prompt)
	}
	if strings.Contains(prompt, "Do not emit read_file/cat/list_files aliases; use exec_command with cmd instead.") {
		t.Fatalf("prompt should not force exec_command when file tools are declared:\n%s", prompt)
	}
}

func TestConstrainFinalText_MetaHandoffSynthesizesKnownLabels(t *testing.T) {
	task := "读取 internal/proxy/output_constraints.go 和 internal/proxy/execution_evidence.go，运行 go test ./internal/proxy 和 go test ./...，最终只输出四行：RESULT: PASS 或 FAIL；CONSTRAINT: 说明；EVIDENCE: 说明；TEST: 说明。"
	text := "I've reviewed the handoff context. Ready to assist when you provide the specific task."
	evidence := executionEvidence{
		Commands: []string{
			"sed -n '1,220p' internal/proxy/output_constraints.go",
			"sed -n '1,220p' internal/proxy/execution_evidence.go",
			"go test ./internal/proxy",
			"go test ./...",
		},
		Outputs: []string{
			"sed -n '1,220p' internal/proxy/output_constraints.go => success=true package proxy func enforceTaskOutputConstraints(task, text string, evidence executionEvidence, checkControlMarkup bool)",
			"sed -n '1,220p' internal/proxy/execution_evidence.go => success=true package proxy func buildExecutionEvidence(historyItems []json.RawMessage) executionEvidence",
			"go test ./internal/proxy => success=true ok",
			"go test ./... => success=true ok",
		},
	}

	got := constrainFinalText(task, text, evidence, true)
	for _, want := range []string{
		"RESULT: PASS",
		"CONSTRAINT:",
		"EVIDENCE:",
		"TEST:",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("constrained text missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(strings.ToLower(got), "handoff") {
		t.Fatalf("constrained text should not leak handoff meta text:\n%s", got)
	}
}

func TestResponseInputToMessages_ToolOutput(t *testing.T) {
	input := json.RawMessage(`[
		{"type":"function_call","name":"exec_command","call_id":"call_1","arguments":"{\"cmd\":\"pwd\"}"},
		{"type":"function_call_output","call_id":"call_1","output":{"content":"ok","success":true}}
	]`)
	msgs, err := responseInputToMessages(input)
	if err != nil {
		t.Fatalf("responseInputToMessages error: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if msgs[0].Role != "assistant" || !strings.Contains(msgs[0].Content, "exec_command") {
		t.Fatalf("assistant tool summary = %+v", msgs[0])
	}
	if msgs[1].Role != "user" || !strings.Contains(msgs[1].Content, "Tool result") || !strings.Contains(msgs[1].Content, "ok") {
		t.Fatalf("tool output summary = %+v", msgs[1])
	}
}

func TestNormalizeToolSummaryMessageItem_ParsesExitCodeStyleToolResult(t *testing.T) {
	item := map[string]any{
		"role":    "user",
		"content": "Tool result (call_id=call_test_all)\nCommand: go test ./...\nExit code: 0\nOutput:\nok  \tgithub.com/mison/firew2oai/internal/proxy\t0.011s",
	}

	raw, ok := normalizeToolSummaryMessageItem(item)
	if !ok {
		t.Fatal("normalizeToolSummaryMessageItem = false, want true")
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode normalized raw: %v", err)
	}
	if decoded["type"] != "function_call_output" {
		t.Fatalf("type = %v, want function_call_output", decoded["type"])
	}
	if decoded["call_id"] != "call_test_all" {
		t.Fatalf("call_id = %v, want call_test_all", decoded["call_id"])
	}
	output, _ := decoded["output"].(map[string]any)
	if output == nil {
		t.Fatalf("output = %#v, want object", decoded["output"])
	}
	if output["success"] != true {
		t.Fatalf("success = %#v, want true", output["success"])
	}
	if !strings.Contains(output["content"].(string), "ok") {
		t.Fatalf("content = %#v, want go test output", output["content"])
	}
}

func TestNormalizeRawResponseInputItem_StringToolSummariesBecomeRawHistoryItems(t *testing.T) {
	callRaw, ok := normalizeRawResponseInputItem("Assistant requested tool: exec_command (call_id=call_test_all)\nTool payload:\n{\"cmd\":\"go test ./...\"}")
	if !ok {
		t.Fatal("assistant summary normalize ok = false, want true")
	}
	var call map[string]any
	if err := json.Unmarshal(callRaw, &call); err != nil {
		t.Fatalf("decode call raw: %v", err)
	}
	if call["type"] != "function_call" {
		t.Fatalf("call type = %v, want function_call", call["type"])
	}

	resultRaw, ok := normalizeRawResponseInputItem("Tool result (call_id=call_test_all)\nExit code: 0\nOutput:\nok  \tgithub.com/mison/firew2oai/internal/proxy\t0.011s")
	if !ok {
		t.Fatal("tool result normalize ok = false, want true")
	}
	var result map[string]any
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		t.Fatalf("decode result raw: %v", err)
	}
	if result["type"] != "function_call_output" {
		t.Fatalf("result type = %v, want function_call_output", result["type"])
	}
	output, _ := result["output"].(map[string]any)
	if output == nil || output["success"] != true {
		t.Fatalf("output = %#v, want success=true", result["output"])
	}
}

func TestBuildExecutionEvidence_InfersSuccessFromGoTestOutputWithoutExplicitFlag(t *testing.T) {
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"exec_command","call_id":"call_test","arguments":"{\"cmd\":\"go test ./internal/proxy\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_test","output":"ok  \tgithub.com/mison/firew2oai/internal/proxy\t0.011s"}`),
	}

	evidence := buildExecutionEvidence(history)
	if len(evidence.Outputs) != 1 {
		t.Fatalf("len(evidence.Outputs) = %d, want 1", len(evidence.Outputs))
	}
	if !strings.Contains(evidence.Outputs[0], "success=true") {
		t.Fatalf("evidence output = %q, want inferred success=true", evidence.Outputs[0])
	}
}

func TestBuildExecutionEvidence_InfersSuccessFromWrappedMultiLineGoTestOutput(t *testing.T) {
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"exec_command","call_id":"call_test_all","arguments":"{\"cmd\":\"go test ./...\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_test_all","output":"Chunk ID: 81328a\nWall time: 0.1076 seconds\nProcess exited with code 0\nOriginal token count: 88\nOutput:\n?   \tgithub.com/mison/firew2oai/cmd/server\t[no test files]\nok  \tgithub.com/mison/firew2oai/internal/config\t(cached)\nok  \tgithub.com/mison/firew2oai/internal/proxy\t(cached)\nok  \tgithub.com/mison/firew2oai/internal/tokenauth\t(cached)\nok  \tgithub.com/mison/firew2oai/internal/transport\t(cached)\nok  \tgithub.com/mison/firew2oai/internal/whitelist\t(cached)"}`),
	}

	evidence := buildExecutionEvidence(history)
	if len(evidence.Outputs) != 1 {
		t.Fatalf("len(evidence.Outputs) = %d, want 1", len(evidence.Outputs))
	}
	if !strings.Contains(evidence.Outputs[0], "success=true") {
		t.Fatalf("evidence output = %q, want inferred success=true", evidence.Outputs[0])
	}
}

func TestParseToolCallOutput_Function(t *testing.T) {
	result := parseToolCallOutput(
		"```json\n{\"type\":\"function_call\",\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"pwd\"}}\n```",
		map[string]responseToolDescriptor{
			"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		},
		"",
	)
	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if result.call == nil {
		t.Fatal("expected function tool call")
	}
	if !strings.Contains(result.call.conversation.Content, "exec_command") {
		t.Fatalf("conversation = %+v", result.call.conversation)
	}
	if !strings.Contains(string(result.call.item), `"type":"function_call"`) || !strings.Contains(string(result.call.item), `"name":"exec_command"`) {
		t.Fatalf("item = %s", string(result.call.item))
	}
	if !strings.Contains(string(result.call.item), `\"cmd\":\"pwd\"`) {
		t.Fatalf("item arguments missing cmd: %s", string(result.call.item))
	}
}

func TestParseToolCallOutput_ExtractsMixedTextAndNormalizesAlias(t *testing.T) {
	result := parseToolCallOutput(
		"I will inspect first.\n{\"type\":\"function_call\",\"name\":\"run_terminal\",\"arguments\":{\"cmd\":\"pwd\"}}",
		map[string]responseToolDescriptor{
			"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		},
		"",
	)
	if result.err != nil {
		t.Fatalf("unexpected parse error: %v", result.err)
	}
	if result.call == nil {
		t.Fatal("expected function tool call from mixed text")
	}
	if !strings.Contains(string(result.call.item), `"name":"exec_command"`) {
		t.Fatalf("item did not normalize tool name: %s", string(result.call.item))
	}
}

func TestParseToolCallOutput_RejectsUndeclaredTool(t *testing.T) {
	result := parseToolCallOutput(
		"{\"type\":\"function_call\",\"name\":\"unknown_tool\",\"arguments\":{\"cmd\":\"pwd\"}}",
		map[string]responseToolDescriptor{
			"exec_command": {Name: "exec_command", Type: "function", Structured: true},
		},
		"",
	)
	if result.call != nil {
		t.Fatalf("expected no parsed tool call, got %+v", result.call)
	}
	if result.err == nil || !strings.Contains(result.err.Error(), "not declared") {
		t.Fatalf("expected undeclared tool error, got %v", result.err)
	}
}

func TestHandleResponses_MethodNotAllowed(t *testing.T) {
	p := newTestProxy()
	mux := newTestMux(t, p, "*")
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHandleResponses_PreviousResponseID(t *testing.T) {
	requests := make([]FireworksRequest, 0, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var fwReq FireworksRequest
		if err := json.NewDecoder(r.Body).Decode(&fwReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		requests = append(requests, fwReq)

		w.Header().Set("Content-Type", "text/event-stream")
		if len(requests) == 1 {
			_, _ = w.Write([]byte("data: {\"type\":\"content\",\"content\":\"ok\"}\n\n"))
		} else {
			_, _ = w.Write([]byte("data: {\"type\":\"content\",\"content\":\"blue-raven\"}\n\n"))
		}
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"content\":\"\"}\n\n"))
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")

	firstBody := `{"model":"deepseek-v3p2","instructions":"do not carry this","input":"请记住暗号是 blue-raven。只回复 ok。"}`
	firstReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(firstBody))
	firstReq.Header.Set("Authorization", "Bearer test-key")
	firstReq.Header.Set("Content-Type", "application/json")
	firstRec := httptest.NewRecorder()
	mux.ServeHTTP(firstRec, firstReq)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("first status = %d, body=%s", firstRec.Code, firstRec.Body.String())
	}

	var firstResp ResponsesResponse
	if err := json.Unmarshal(firstRec.Body.Bytes(), &firstResp); err != nil {
		t.Fatalf("decode first response: %v", err)
	}

	secondBody := `{"model":"deepseek-v3p2","previous_response_id":"` + firstResp.ID + `","input":"刚才的暗号是什么？只回复暗号。"}`
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(secondBody))
	secondReq.Header.Set("Authorization", "Bearer test-key")
	secondReq.Header.Set("Content-Type", "application/json")
	secondRec := httptest.NewRecorder()
	mux.ServeHTTP(secondRec, secondReq)
	if secondRec.Code != http.StatusOK {
		t.Fatalf("second status = %d, body=%s", secondRec.Code, secondRec.Body.String())
	}

	if len(requests) != 2 {
		t.Fatalf("upstream requests = %d, want 2", len(requests))
	}
	prompt := requests[1].Messages[0].Content
	for _, want := range []string{
		"User: 请记住暗号是 blue-raven。只回复 ok。",
		"Assistant: ok",
		"<CURRENT_USER_TASK>\n刚才的暗号是什么？只回复暗号。\n</CURRENT_USER_TASK>",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("second prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "do not carry this") {
		t.Fatalf("instructions were carried into previous response history:\n%s", prompt)
	}
}

func TestHandleResponseByIDAndInputItems(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"content\",\"content\":\"ok\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"content\":\"\"}\n\n"))
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")

	body := `{"model":"deepseek-v3p2","input":"say ok"}`
	createReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	createReq.Header.Set("Authorization", "Bearer test-key")
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create status = %d, body=%s", createRec.Code, createRec.Body.String())
	}

	var created ResponsesResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created response: %v", err)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/v1/responses/"+created.ID, nil)
	getReq.Header.Set("Authorization", "Bearer test-key")
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status = %d, body=%s", getRec.Code, getRec.Body.String())
	}
	if !strings.Contains(getRec.Body.String(), created.ID) {
		t.Fatalf("get body missing response id: %s", getRec.Body.String())
	}

	itemsReq := httptest.NewRequest(http.MethodGet, "/v1/responses/"+created.ID+"/input_items", nil)
	itemsReq.Header.Set("Authorization", "Bearer test-key")
	itemsRec := httptest.NewRecorder()
	mux.ServeHTTP(itemsRec, itemsReq)
	if itemsRec.Code != http.StatusOK {
		t.Fatalf("input_items status = %d, body=%s", itemsRec.Code, itemsRec.Body.String())
	}
	if !strings.Contains(itemsRec.Body.String(), `"text":"say ok"`) {
		t.Fatalf("input_items body missing input text: %s", itemsRec.Body.String())
	}
}

func TestHandleResponses_PreviousResponseIDNotFound(t *testing.T) {
	p := newTestProxy()
	mux := newTestMux(t, p, "*")
	body := `{"model":"deepseek-v3p2","previous_response_id":"resp_missing","input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "previous_response_not_found") {
		t.Fatalf("body = %s, want previous_response_not_found", rec.Body.String())
	}
}

func TestHandleResponses_InvalidInput(t *testing.T) {
	p := newTestProxy()
	mux := newTestMux(t, p, "*")
	body := `{"model":"deepseek-v3p2","input":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"invalid_input"`) {
		t.Fatalf("body = %s, want invalid_input", rec.Body.String())
	}
}

func TestHandleResponses_NonStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var fwReq FireworksRequest
		if err := json.NewDecoder(r.Body).Decode(&fwReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if fwReq.ModelKey != "deepseek-v3p2" {
			t.Fatalf("ModelKey = %q, want deepseek-v3p2", fwReq.ModelKey)
		}
		if fwReq.MaxTokens == nil || *fwReq.MaxTokens != 64 {
			t.Fatalf("MaxTokens = %v, want 64", fwReq.MaxTokens)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"content\",\"content\":\"ok\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"content\":\"\"}\n\n"))
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")
	body := `{"model":"deepseek-v3p2","input":"say ok","stream":false,"max_output_tokens":64}`
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
		t.Fatalf("json.Unmarshal error: %v", err)
	}
	if resp.Object != "response" {
		t.Fatalf("object = %q, want response", resp.Object)
	}
	if resp.Status != "completed" {
		t.Fatalf("status = %q, want completed", resp.Status)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("output = %+v, want one assistant text item", resp.Output)
	}
	if resp.Usage == nil {
		t.Fatal("usage is nil")
	}
	if resp.Usage.InputTokens <= 0 || resp.Usage.OutputTokens <= 0 || resp.Usage.TotalTokens <= 0 {
		t.Fatalf("usage = %+v, want positive token counts", resp.Usage)
	}
	var item ResponseOutputMessage
	if err := json.Unmarshal(resp.Output[0], &item); err != nil {
		t.Fatalf("decode output item: %v", err)
	}
	if len(item.Content) != 1 || item.Content[0].Type != "output_text" {
		t.Fatalf("content = %+v, want one output_text item", item.Content)
	}
	if item.Content[0].Text != "ok" {
		t.Fatalf("text = %q, want ok", item.Content[0].Text)
	}
}

func TestHandleResponses_Stream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream server doesn't support flushing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		_, _ = w.Write([]byte("data: {\"type\":\"content\",\"content\":\"he\"}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: {\"type\":\"content\",\"content\":\"llo\"}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"content\":\"\"}\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")
	body := `{"model":"deepseek-v3p2","input":"hello","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	bodyText := rec.Body.String()
	for _, want := range []string{
		"event: response.created",
		`"type":"response.created"`,
		"event: response.output_item.added",
		"event: response.content_part.added",
		"event: response.output_text.delta",
		`"delta":"he"`,
		`"delta":"llo"`,
		"event: response.output_text.done",
		`"text":"hello"`,
		"event: response.output_item.done",
		"event: response.completed",
		`"type":"response.completed"`,
		`"status":"completed"`,
		`"usage":{"input_tokens":`,
		`"output_tokens":`,
	} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("stream body missing %q:\n%s", want, bodyText)
		}
	}
	if strings.Contains(bodyText, "[DONE]") {
		t.Fatalf("responses stream should not emit chat-style [DONE]:\n%s", bodyText)
	}
}

func TestHandleResponses_StreamEmptyUpstreamEmitsCompletedError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream server doesn't support flushing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")
	body := `{"model":"deepseek-v3p2","input":"hello","stream":true}`
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
		"event: response.output_text.done",
		"event: response.output_item.done",
		"event: response.completed",
		"Codex adapter error: upstream response ended without a completion signal",
	} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("stream body missing %q:\n%s", want, bodyText)
		}
	}
}

func TestHandleResponses_StreamFunctionToolCall(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"content\",\"content\":\"{\\\"type\\\":\\\"function_call\\\",\\\"name\\\":\\\"exec_command\\\",\\\"arguments\\\":{\\\"cmd\\\":\\\"pwd\\\"}}\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"content\":\"\"}\n\n"))
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")
	body := `{"model":"deepseek-v3p2","input":"read file","stream":true,"tools":[{"type":"function","name":"exec_command","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
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
		`"type":"function_call"`,
		`"name":"exec_command"`,
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

func TestHandleResponses_StreamToolFinalOutputIsConstrained(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream server doesn't support flushing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		_, _ = w.Write([]byte("data: {\"type\":\"content\",\"content\":\"ID: I'll start by reading the relevant files to understand the code structure and testing style.\\nRESULT: PASS\\nREADME: # firew2oai\\nTOOLP: go test ./internal/proxy ok\\n<<<AI_ACTIONS_V1>>>\\n{\\\"mode\\\":\\\"final\\\"}\\n<<<END_AI_ACTIONS_V1>>>\"}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"content\":\"\"}\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")
	body := `{"model":"glm-5","stream":true,"input":"你在真实 Go 项目中执行任务。最终只输出三行：RESULT: PASS 或 FAIL；README: 简述；TOOLP: 工具策略。","tools":[{"type":"function","name":"exec_command","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	bodyText := rec.Body.String()
	want := "\"text\":\"RESULT: PASS\\nREADME: # firew2oai\\nTOOLP: go test ./internal/proxy ok\""
	if !strings.Contains(bodyText, want) {
		t.Fatalf("stream body missing constrained final text %q:\n%s", want, bodyText)
	}
	if strings.Contains(bodyText, "ID: I'll start by reading the relevant files") {
		t.Fatalf("stream body should not leak raw prefix chatter:\n%s", bodyText)
	}
}

func TestHandleResponses_PreviousResponseIDWithToolOutput(t *testing.T) {
	requests := make([]FireworksRequest, 0, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var fwReq FireworksRequest
		if err := json.NewDecoder(r.Body).Decode(&fwReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		requests = append(requests, fwReq)

		w.Header().Set("Content-Type", "text/event-stream")
		if len(requests) == 1 {
			_, _ = w.Write([]byte("data: {\"type\":\"content\",\"content\":\"{\\\"type\\\":\\\"function_call\\\",\\\"name\\\":\\\"exec_command\\\",\\\"arguments\\\":{\\\"cmd\\\":\\\"pwd\\\"}}\"}\n\n"))
		} else {
			_, _ = w.Write([]byte("data: {\"type\":\"content\",\"content\":\"工作目录已确认\"}\n\n"))
		}
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"content\":\"\"}\n\n"))
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")

	firstBody := `{"model":"deepseek-v3p2","input":"读取当前目录","tools":[{"type":"function","name":"exec_command","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
	firstReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(firstBody))
	firstReq.Header.Set("Authorization", "Bearer test-key")
	firstReq.Header.Set("Content-Type", "application/json")
	firstRec := httptest.NewRecorder()
	mux.ServeHTTP(firstRec, firstReq)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("first status = %d, body=%s", firstRec.Code, firstRec.Body.String())
	}

	var firstResp ResponsesResponse
	if err := json.Unmarshal(firstRec.Body.Bytes(), &firstResp); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if len(firstResp.Output) != 1 {
		t.Fatalf("first output len = %d, want 1", len(firstResp.Output))
	}
	var firstOutput map[string]any
	if err := json.Unmarshal(firstResp.Output[0], &firstOutput); err != nil {
		t.Fatalf("decode first output item: %v", err)
	}
	callID, _ := firstOutput["call_id"].(string)
	if callID == "" {
		t.Fatalf("missing call_id in first output: %s", string(firstResp.Output[0]))
	}

	secondBody := `{"model":"deepseek-v3p2","previous_response_id":"` + firstResp.ID + `","input":[{"type":"function_call_output","call_id":"` + callID + `","output":"` + "`/Volumes/Work/code/firew2oai`" + `"}]}`
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(secondBody))
	secondReq.Header.Set("Authorization", "Bearer test-key")
	secondReq.Header.Set("Content-Type", "application/json")
	secondRec := httptest.NewRecorder()
	mux.ServeHTTP(secondRec, secondReq)
	if secondRec.Code != http.StatusOK {
		t.Fatalf("second status = %d, body=%s", secondRec.Code, secondRec.Body.String())
	}

	if len(requests) != 2 {
		t.Fatalf("upstream requests = %d, want 2", len(requests))
	}
	prompt := requests[1].Messages[0].Content
	for _, want := range []string{
		"Assistant requested tool: exec_command",
		"Tool result",
		"/Volumes/Work/code/firew2oai",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("second prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildExecutionEvidencePromptBlock_FromHistoryItems(t *testing.T) {
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"exec_command","call_id":"call_1","arguments":"{\"cmd\":\"go test ./internal/proxy\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_1","output":{"content":"ok","success":true}}`),
	}

	evidence := buildExecutionEvidence(history)
	block := buildExecutionEvidencePromptBlock(evidence)
	for _, want := range []string{
		"<EXECUTION_EVIDENCE>",
		"go test ./internal/proxy",
		"success=true ok",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("evidence block missing %q:\n%s", want, block)
		}
	}
}

func TestHandleResponses_StreamEmptyUpstreamRetriesOnce(t *testing.T) {
	var attempts int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := atomic.AddInt32(&attempts, 1)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream server doesn't support flushing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		if current == 1 {
			return
		}
		_, _ = w.Write([]byte("data: {\"type\":\"content\",\"content\":\"retry-ok\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"content\":\"\"}\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")
	body := `{"model":"deepseek-v3p2","input":"hello","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if attempts != 2 {
		t.Fatalf("upstream attempts = %d, want 2", attempts)
	}
	bodyText := rec.Body.String()
	for _, want := range []string{
		`"delta":"retry-ok"`,
		`"text":"retry-ok"`,
		"event: response.completed",
	} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("stream body missing %q:\n%s", want, bodyText)
		}
	}
	if strings.Contains(bodyText, "upstream response ended without a completion signal") {
		t.Fatalf("stream body should not return empty-stream terminal error after retry:\n%s", bodyText)
	}
}

func TestHandleResponses_NonStreamEmptyUpstreamRetriesOnce(t *testing.T) {
	var attempts int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if current == 1 {
			return
		}
		_, _ = w.Write([]byte("data: {\"type\":\"content\",\"content\":\"retry-ok\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"content\":\"\"}\n\n"))
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")
	body := `{"model":"deepseek-v3p2","input":"say ok","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if attempts != 2 {
		t.Fatalf("upstream attempts = %d, want 2", attempts)
	}
	var resp ResponsesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var item ResponseOutputMessage
	if err := json.Unmarshal(resp.Output[0], &item); err != nil {
		t.Fatalf("decode output item: %v", err)
	}
	if got := item.Content[0].Text; got != "retry-ok" {
		t.Fatalf("final text = %q, want retry-ok", got)
	}
}

func TestHandleResponses_NonStreamRequiredLabelsNormalized(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"content\",\"content\":\"好的，结果如下。\\n- RESULT: PASS\\nREADME: # firew2oai\\nTOOLP: go test ./internal/proxy ok\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"content\":\"\"}\n\n"))
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")
	body := `{"model":"deepseek-v3p2","stream":false,"input":"你在真实 Go 项目中执行任务。最终只输出三行：RESULT: PASS 或 FAIL；README: 简述；TOOLP: 工具策略。"}`
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
	want := "RESULT: PASS\nREADME: # firew2oai\nTOOLP: go test ./internal/proxy ok"
	if item.Content[0].Text != want {
		t.Fatalf("final text = %q, want %q", item.Content[0].Text, want)
	}
}

func TestHandleResponses_NonStreamRequiredLabelsMissingSynthesizesLabels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"content\",\"content\":\"RESULT: PASS\\nREADME: # firew2oai\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"content\":\"\"}\n\n"))
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")
	body := `{"model":"deepseek-v3p2","stream":false,"input":"执行任务后最终只输出三行：RESULT: PASS 或 FAIL；README: 简述；TOOLP: 工具策略。"}`
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
	got := item.Content[0].Text
	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("final text lines = %d, want 3, text=%q", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], "RESULT: ") {
		t.Fatalf("line1 = %q, want RESULT label", lines[0])
	}
	if !strings.HasPrefix(lines[1], "README: ") {
		t.Fatalf("line2 = %q, want README label", lines[1])
	}
	if !strings.HasPrefix(lines[2], "TOOLP: ") {
		t.Fatalf("line3 = %q, want TOOLP label", lines[2])
	}
}

func TestHandleResponses_NonStreamNoisyRequiredLabelsAreRewritten(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"content\",\"content\":\"RESULT: PASS\\nCONSTRAINT: Chunk ID: 123abc Wall time: 0.0000 seconds Process exited with code 0 Output: package proxy ...\\nEVIDENCE: Chunk ID: 456def Wall time: 0.0000 seconds Process exited with code 0 Output: package proxy ...\\nTEST: 全部测试通过\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"content\":\"\"}\n\n"))
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")
	body := `{"model":"glm-5","stream":false,"input":"你是资深 Go 工程师。请执行一个真实编码排障任务（只读分析，不修改文件）：最终只输出四行：RESULT: PASS 或 FAIL；CONSTRAINT: 一句话说明 output_constraints 这一层的核心职责；EVIDENCE: 一句话说明 execution_evidence 这一层的核心职责；TEST: 一句话给出测试是否通过与关键结果。"}`
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
	got := item.Content[0].Text
	lines := strings.Split(got, "\n")
	if len(lines) != 4 {
		t.Fatalf("final text lines = %d, want 4, text=%q", len(lines), got)
	}
	if got != "RESULT: PASS\nCONSTRAINT: 负责对最终输出文本执行标签约束校验、控制标记清理与严格门禁拦截。\nEVIDENCE: 负责从历史消息中提取已执行命令与工具输出摘要，构建可追溯的执行证据块。\nTEST: 全部测试通过" {
		t.Fatalf("final text = %q", got)
	}
}

func TestHandleResponses_NonStreamRequiredLabelsEmptySynthesizesLabels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"content\":\"\"}\n\n"))
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")
	body := `{"model":"deepseek-v3p2","stream":false,"input":"执行任务后最终只输出三行：RESULT: PASS 或 FAIL；README: 简述；TOOLP: 工具策略。"}`
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
	got := item.Content[0].Text
	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("final text lines = %d, want 3, text=%q", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], "RESULT: ") {
		t.Fatalf("line1 = %q, want RESULT label", lines[0])
	}
	if !strings.HasPrefix(lines[1], "README: ") {
		t.Fatalf("line2 = %q, want README label", lines[1])
	}
	if !strings.HasPrefix(lines[2], "TOOLP: ") {
		t.Fatalf("line3 = %q, want TOOLP label", lines[2])
	}
}

func TestHandleResponses_NonStreamLeakedControlMarkupIsSanitized(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"content\",\"content\":\"分析完成。\\n<function_calls><call name=\\\"exec_command\\\" /></function_calls>\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"content\":\"\"}\n\n"))
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")
	body := `{"model":"deepseek-v3p2","stream":false,"input":"只回答结果","tools":[{"type":"function","name":"exec_command","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
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
	text := item.Content[0].Text
	if strings.Contains(text, "Codex adapter error:") {
		t.Fatalf("expected sanitized text, got %q", text)
	}
	if strings.Contains(text, "<function_calls>") {
		t.Fatalf("final text should not leak control markup, got %q", text)
	}
	if text != "分析完成。" {
		t.Fatalf("final text = %q, want %q", text, "分析完成。")
	}
}

func TestHandleResponses_NonStreamRequiredLabelsMissingReturnsAdapterErrorWhenStrictGateEnabled(t *testing.T) {
	t.Setenv("FIREW2OAI_STRICT_OUTPUT_GATE", "1")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"content\",\"content\":\"RESULT: PASS\\nREADME: # firew2oai\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"content\":\"\"}\n\n"))
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")
	body := `{"model":"deepseek-v3p2","stream":false,"input":"执行任务后最终只输出三行：RESULT: PASS 或 FAIL；README: 简述；TOOLP: 工具策略。"}`
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
	if !strings.Contains(item.Content[0].Text, "Codex adapter error: final output missing required labels: TOOLP") {
		t.Fatalf("expected strict missing-label adapter error, got %q", item.Content[0].Text)
	}
}

func TestHandleResponses_NonStreamLeakedControlMarkupReturnsAdapterErrorWhenStrictGateEnabled(t *testing.T) {
	t.Setenv("FIREW2OAI_STRICT_OUTPUT_GATE", "1")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"content\",\"content\":\"分析完成。\\n<function_calls><call name=\\\"exec_command\\\" /></function_calls>\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"content\":\"\"}\n\n"))
	}))
	defer upstream.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	mux := newTestMux(t, p, "*")
	body := `{"model":"deepseek-v3p2","stream":false,"input":"只回答结果","tools":[{"type":"function","name":"exec_command","description":"run shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
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
	text := item.Content[0].Text
	if !strings.Contains(text, "Codex adapter error: model leaked unsupported tool-control markup") {
		t.Fatalf("expected strict control-markup error, got %q", text)
	}
}

func TestSynthesizeRequiredLabelOutput_FromEvidence(t *testing.T) {
	text := "已完成。"
	labels := []string{"RESULT", "README", "TOOLP"}
	evidence := executionEvidence{
		Commands: []string{
			"head -n 5 README.md",
			"sed -n '170,260p' internal/proxy/tool_protocol.go",
			"go test ./internal/proxy",
		},
		Outputs: []string{
			"head -n 5 README.md => success=true # firew2oai Fireworks.ai Chat API 转换代理",
			"sed -n '170,260p' internal/proxy/tool_protocol.go => success=true 该段处理 AI_ACTIONS 解析与约束校验",
			"go test ./internal/proxy => success=true ok",
		},
	}
	got, ok := synthesizeRequiredLabelOutput("", text, labels, evidence)
	if !ok {
		t.Fatal("synthesizeRequiredLabelOutput returned ok=false, want true")
	}
	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("line count = %d, want 3, text=%q", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], "RESULT: PASS") {
		t.Fatalf("line1 = %q, want RESULT: PASS...", lines[0])
	}
	if !strings.HasPrefix(lines[1], "README: ") {
		t.Fatalf("line2 = %q, want README label", lines[1])
	}
	if !strings.HasPrefix(lines[2], "TOOLP: ") {
		t.Fatalf("line3 = %q, want TOOLP label", lines[2])
	}
}

func TestConstrainFinalText_RepairsMalformedFilesAndNoteLabelsFromTask(t *testing.T) {
	task := "你是资深 Go 工程师。请在当前仓库完成一个真实但边界清晰的测试补强任务：\n" +
		"1) 阅读 internal/proxy/output_constraints.go 与现有 internal/proxy/*_test.go 风格。\n" +
		"2) 新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`。\n" +
		"3) 执行命令：go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'\n" +
		"4) 执行命令：go test ./internal/proxy\n\n" +
		"最后只输出四行，不要有任何额外内容：\n" +
		"RESULT: <PASS 或 FAIL>\n" +
		"FILES: <修改的文件路径，若只改一个就写一个>\n" +
		"TEST: <一句话说明测试结果>\n" +
		"NOTE: <一句话说明是否只新增测试且未改业务逻辑>"
	text := "ID: I'll start by reading the existing files to understand the code structure and testing style.\n" +
		"RESULT: PASS\n" +
		"FILES: I'll start by reading the existing files to understand the code structure and testing style.\n" +
		"TEST: 已完成相关验证命令，未观察到明确失败信号。\n" +
		"NOTE: I'll start by reading the existing files to understand the code structure and testing style."
	evidence := executionEvidence{
		Commands: []string{
			"find 'internal/proxy' -maxdepth 1 -name '*_test.go' | sort | head -n 5",
			"sed -n '1,200p' 'internal/proxy/output_constraints.go'",
			"python3 -c 'from pathlib import Path; Path(\"internal/proxy/output_constraints_test.go\").write_text(\"package proxy\\n\", encoding='\"'\"'utf-8'\"'\"')'",
			"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'",
			"go test ./internal/proxy",
		},
		Outputs: []string{
			"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise' => success=true ok",
			"go test ./internal/proxy => success=true ok",
		},
	}

	got := constrainFinalText(task, text, evidence, true)
	lines := strings.Split(got, "\n")
	if len(lines) != 4 {
		t.Fatalf("line count = %d, want 4, text=%q", len(lines), got)
	}
	if lines[1] != "FILES: internal/proxy/output_constraints_test.go" {
		t.Fatalf("FILES line = %q", lines[1])
	}
	if lines[3] != "NOTE: 只新增测试文件，未修改业务逻辑。" {
		t.Fatalf("NOTE line = %q", lines[3])
	}
}

func TestConstrainFinalText_CanonicalizesDeterministicLabelsEvenWhenPresent(t *testing.T) {
	task := "你是资深 Go 工程师。请在当前仓库完成一个真实但边界清晰的测试补强任务：\n" +
		"1) 阅读 internal/proxy/output_constraints.go 与现有 internal/proxy/*_test.go 风格。\n" +
		"2) 新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`。\n" +
		"3) 执行命令：go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'\n" +
		"4) 执行命令：go test ./internal/proxy\n\n" +
		"最后只输出四行，不要有任何额外内容：\n" +
		"RESULT: <PASS 或 FAIL>\n" +
		"FILES: <修改的文件路径，若只改一个就写一个>\n" +
		"TEST: <一句话说明测试结果>\n" +
		"NOTE: <一句话说明是否只新增测试且未改业务逻辑>"
	text := "RESULT: PASS\n" +
		"FILES: internal/proxy/output_constraints_test.go\n" +
		"TEST: Done.\n" +
		"NOTE: Completed the requested change."
	evidence := executionEvidence{
		Commands: []string{
			"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'",
			"go test ./internal/proxy",
		},
		Outputs: []string{
			"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise' => success=true ok",
			"go test ./internal/proxy => success=true ok",
		},
	}

	got := constrainFinalText(task, text, evidence, true)
	want := "RESULT: PASS\n" +
		"FILES: internal/proxy/output_constraints_test.go\n" +
		"TEST: 已完成相关验证命令，未观察到明确失败信号。\n" +
		"NOTE: 只新增测试文件，未修改业务逻辑。"
	if got != want {
		t.Fatalf("constrained text = %q, want %q", got, want)
	}
}

func TestConstrainFinalText_OverridesModelFailLabelWhenEvidencePassed(t *testing.T) {
	task := "你是资深 Go 工程师。请在当前仓库完成一个真实但边界清晰的测试补强任务：\n" +
		"1) 阅读 internal/proxy/output_constraints.go 与现有 internal/proxy/*_test.go 风格。\n" +
		"2) 新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`。\n" +
		"3) 执行命令：go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'\n" +
		"4) 执行命令：go test ./internal/proxy\n\n" +
		"最后只输出四行，不要有任何额外内容：\n" +
		"RESULT: <PASS 或 FAIL>\n" +
		"FILES: <修改的文件路径，若只改一个就写一个>\n" +
		"TEST: <一句话说明测试结果>\n" +
		"NOTE: <一句话说明是否只新增测试且未改业务逻辑>"
	text := "RESULT: FAIL\n" +
		"FILES: internal/proxy/output_constraints_test.go\n" +
		"TEST: 测试未全部通过，至少一个验证命令返回失败。\n" +
		"NOTE: 只新增测试文件，未修改业务逻辑。"
	evidence := executionEvidence{
		Commands: []string{
			"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'",
			"go test ./internal/proxy",
		},
		Outputs: []string{
			"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise' => success=true ok",
			"go test ./internal/proxy => success=true ok",
		},
	}

	got := constrainFinalText(task, text, evidence, true)
	want := "RESULT: PASS\n" +
		"FILES: internal/proxy/output_constraints_test.go\n" +
		"TEST: 已完成相关验证命令，未观察到明确失败信号。\n" +
		"NOTE: 只新增测试文件，未修改业务逻辑。"
	if got != want {
		t.Fatalf("constrained text = %q, want %q", got, want)
	}
}

func TestConstrainFinalText_IgnoresExploratoryFailureAfterRequiredCommandsPass(t *testing.T) {
	task := "你是资深 Go 工程师。请在当前仓库完成一个真实但边界清晰的测试补强任务：\n" +
		"1) 阅读 internal/proxy/output_constraints.go 与现有 internal/proxy/*_test.go 风格。\n" +
		"2) 新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`。\n" +
		"3) 执行命令：go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'\n" +
		"4) 执行命令：go test ./internal/proxy\n\n" +
		"最后只输出四行，不要有任何额外内容：\n" +
		"RESULT: <PASS 或 FAIL>\n" +
		"FILES: <修改的文件路径，若只改一个就写一个>\n" +
		"TEST: <一句话说明测试结果>\n" +
		"NOTE: <一句话说明是否只新增测试且未改业务逻辑>"
	text := "ID: 001\n" +
		"RESULT: FAIL\n" +
		"FILES: internal/proxy/output_constraints_test.go\n" +
		"TEST: 测试未全部通过，至少一个验证命令返回失败。\n" +
		"NOTE: 只新增测试文件，未修改业务逻辑。"
	evidence := executionEvidence{
		Commands: []string{
			"find 'internal/proxy' -maxdepth 1 -name '*_test.go' | sort | head -n 5",
			"sed -n '1,200p' 'internal/proxy/output_constraints_test.go'",
			"python3 -c 'from pathlib import Path; Path(\"internal/proxy/output_constraints_test.go\").write_text(\"package proxy\\n\", encoding='\"'\"'utf-8'\"'\"')'",
			"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'",
			"go test ./internal/proxy",
		},
		Outputs: []string{
			"find 'internal/proxy' -maxdepth 1 -name '*_test.go' | sort | head -n 5 => success=true internal/proxy/execution_policy_test.go",
			"sed -n '1,200p' 'internal/proxy/output_constraints_test.go' => success=false sed: internal/proxy/output_constraints_test.go: No such file or directory",
			"python3 -c 'from pathlib import Path; Path(\"internal/proxy/output_constraints_test.go\").write_text(\"package proxy\\n\", encoding='\"'\"'utf-8'\"'\"')' => success=true",
			"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise' => success=true ok",
			"go test ./internal/proxy => success=true ok",
		},
	}

	got := constrainFinalText(task, text, evidence, true)
	want := "RESULT: PASS\n" +
		"FILES: internal/proxy/output_constraints_test.go\n" +
		"TEST: 已完成相关验证命令，未观察到明确失败信号。\n" +
		"NOTE: 只新增测试文件，未修改业务逻辑。"
	if got != want {
		t.Fatalf("constrained text = %q, want %q", got, want)
	}
}

func TestConstrainFinalText_DoesNotMarkIncompleteWriteTaskAsPass(t *testing.T) {
	task := "你是资深 Go 工程师。请在当前仓库完成一个真实但边界清晰的测试补强任务：\n" +
		"1) 阅读 internal/proxy/output_constraints.go 与现有 internal/proxy/*_test.go 风格。\n" +
		"2) 新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`。\n" +
		"3) 执行命令：go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'\n" +
		"4) 执行命令：go test ./internal/proxy\n\n" +
		"最后只输出四行，不要有任何额外内容：\n" +
		"RESULT: <PASS 或 FAIL>\n" +
		"FILES: <修改的文件路径，若只改一个就写一个>\n" +
		"TEST: <一句话说明测试结果>\n" +
		"NOTE: <一句话说明是否只新增测试且未改业务逻辑>"
	text := "ID: <tool_code> <tool name=\"exec_command\"> <parameter name=\"cmd\">cat internal/proxy/output_constraints.go</parameter> </tool> </tool_code>\n" +
		"RESULT: PASS\n" +
		"FILES: internal/proxy/output_constraints_test.go\n" +
		"TEST: 已完成相关验证命令，未观察到明确失败信号。\n" +
		"NOTE: 只新增测试文件，未修改业务逻辑。"
	evidence := executionEvidence{
		Commands: []string{
			"find 'internal/proxy' -maxdepth 1 -name '*_test.go' | sort | head -n 5",
		},
		Outputs: []string{
			"find 'internal/proxy' -maxdepth 1 -name '*_test.go' | sort | head -n 5 => success=true internal/proxy/execution_policy_test.go",
		},
	}

	got := constrainFinalText(task, text, evidence, true)
	want := "RESULT: FAIL\n" +
		"FILES: internal/proxy/output_constraints_test.go\n" +
		"TEST: 未完成任务要求的验证命令，当前不能判定测试通过。\n" +
		"NOTE: 任务尚未完成，仍缺少所需修改或验证步骤。"
	if got != want {
		t.Fatalf("constrained text = %q, want %q", got, want)
	}
}

func TestNormalizeRequiredLabelOutput_PrefersLastCompleteBlock(t *testing.T) {
	text := "RESULT: FAIL\n" +
		"FILES: internal/proxy/bad.go\n" +
		"TEST: 第一个块是脏数据。\n" +
		"NOTE: 第一个块不应被采用。\n" +
		"ID: RESULT: PASS FILES: internal/proxy/output_constraints_test.go TEST: 第二个块才是最终答案 NOTE: 只新增测试文件，未修改业务逻辑。"

	got, missing := normalizeRequiredLabelOutput(text, []string{"RESULT", "FILES", "TEST", "NOTE"})
	if len(missing) != 0 {
		t.Fatalf("missing = %v, want none", missing)
	}
	want := "RESULT: PASS\n" +
		"FILES: internal/proxy/output_constraints_test.go\n" +
		"TEST: 第二个块才是最终答案\n" +
		"NOTE: 只新增测试文件，未修改业务逻辑。"
	if got != want {
		t.Fatalf("normalized = %q, want %q", got, want)
	}
}

func TestConstrainFinalText_RepairsInlineFourLabelSingleLine(t *testing.T) {
	task := "你是资深 Go 工程师。请在当前仓库完成一个真实但边界清晰的测试补强任务：\n" +
		"1) 阅读 internal/proxy/output_constraints.go 与现有 internal/proxy/*_test.go 风格。\n" +
		"2) 新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`。\n" +
		"3) 执行命令：go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'\n" +
		"4) 执行命令：go test ./internal/proxy\n\n" +
		"最后只输出四行，不要有任何额外内容：\n" +
		"RESULT: <PASS 或 FAIL>\n" +
		"FILES: <修改的文件路径，若只改一个就写一个>\n" +
		"TEST: <一句话说明测试结果>\n" +
		"NOTE: <一句话说明是否只新增测试且未改业务逻辑>"
	text := "ID: RESULT: PASS FILES: internal/proxy/output_constraints_test.go TEST: TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise passed successfully NOTE: Only added test file without modifying business logic"
	evidence := executionEvidence{
		Commands: []string{
			"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'",
			"go test ./internal/proxy",
		},
		Outputs: []string{
			"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise' => success=true ok",
			"go test ./internal/proxy => success=true ok",
		},
	}

	got := constrainFinalText(task, text, evidence, true)
	want := "RESULT: PASS\n" +
		"FILES: internal/proxy/output_constraints_test.go\n" +
		"TEST: 已完成相关验证命令，未观察到明确失败信号。\n" +
		"NOTE: 只新增测试文件，未修改业务逻辑。"
	if got != want {
		t.Fatalf("constrained text = %q, want %q", got, want)
	}
}

func TestConstrainFinalText_RepairsMarkdownWrappedFinalAnswer(t *testing.T) {
	task := "你是资深 Go 工程师。请在当前仓库完成一个真实但边界清晰的测试补强任务：\n" +
		"1) 阅读 internal/proxy/output_constraints.go 与现有 internal/proxy/*_test.go 风格。\n" +
		"2) 新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`。\n" +
		"3) 执行命令：go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'\n" +
		"4) 执行命令：go test ./internal/proxy\n\n" +
		"最后只输出四行，不要有任何额外内容：\n" +
		"RESULT: <PASS 或 FAIL>\n" +
		"FILES: <修改的文件路径，若只改一个就写一个>\n" +
		"TEST: <一句话说明测试结果>\n" +
		"NOTE: <一句话说明是否只新增测试且未改业务逻辑>"
	text := "ID: ### Final Answer #### RESULT: PASS #### FILES: internal/proxy/output_constraints_test.go #### TEST: 测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise` 通过。 #### NOTE: 只新增了测试，未修改业务逻辑。"
	evidence := executionEvidence{
		Commands: []string{
			"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'",
			"go test ./internal/proxy",
		},
		Outputs: []string{
			"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise' => success=true ok",
			"go test ./internal/proxy => success=true ok",
		},
	}

	got := constrainFinalText(task, text, evidence, true)
	want := "RESULT: PASS\n" +
		"FILES: internal/proxy/output_constraints_test.go\n" +
		"TEST: 已完成相关验证命令，未观察到明确失败信号。\n" +
		"NOTE: 只新增测试文件，未修改业务逻辑。"
	if got != want {
		t.Fatalf("constrained text = %q, want %q", got, want)
	}
}

func TestConstrainFinalText_EmptyTextSynthesizesFilesAndNote(t *testing.T) {
	task := "新增文件 internal/proxy/output_constraints_test.go，添加测试 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`，最后只输出四行：RESULT: PASS 或 FAIL；FILES: 路径；TEST: 说明；NOTE: 是否只新增测试。"
	evidence := executionEvidence{
		Commands: []string{
			"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'",
			"go test ./internal/proxy",
		},
		Outputs: []string{
			"go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise' => success=true ok",
			"go test ./internal/proxy => success=true ok",
		},
	}

	got := constrainFinalText(task, "", evidence, true)
	for _, want := range []string{
		"RESULT: PASS",
		"FILES: internal/proxy/output_constraints_test.go",
		"TEST: 已完成相关验证命令，未观察到明确失败信号。",
		"NOTE: 只新增测试文件，未修改业务逻辑。",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("constrained text missing %q:\n%s", want, got)
		}
	}
}
