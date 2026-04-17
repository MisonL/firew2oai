package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	prompt := buildResponsesPrompt(base, "be concise", current, tools)

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

func TestParseToolCallOutput_Function(t *testing.T) {
	call, ok := parseToolCallOutput("```json\n{\"type\":\"function_call\",\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"pwd\"}}\n```")
	if !ok {
		t.Fatal("expected function tool call")
	}
	if !strings.Contains(call.conversation.Content, "exec_command") {
		t.Fatalf("conversation = %+v", call.conversation)
	}
	if !strings.Contains(string(call.item), `"type":"function_call"`) || !strings.Contains(string(call.item), `"name":"exec_command"`) {
		t.Fatalf("item = %s", string(call.item))
	}
	if !strings.Contains(string(call.item), `\"cmd\":\"pwd\"`) {
		t.Fatalf("item arguments missing cmd: %s", string(call.item))
	}
}

func TestParseToolCallOutput_ExtractsMixedTextAndNormalizesAlias(t *testing.T) {
	call, ok := parseToolCallOutput("I will inspect first.\n{\"type\":\"function_call\",\"name\":\"run_terminal\",\"arguments\":{\"cmd\":\"pwd\"}}")
	if !ok {
		t.Fatal("expected function tool call from mixed text")
	}
	if !strings.Contains(string(call.item), `"name":"exec_command"`) {
		t.Fatalf("item did not normalize tool name: %s", string(call.item))
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
	} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("stream body missing %q:\n%s", want, bodyText)
		}
	}
	if strings.Contains(bodyText, "[DONE]") {
		t.Fatalf("responses stream should not emit chat-style [DONE]:\n%s", bodyText)
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
