package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mison/firew2oai/internal/transport"
)

func recordWebSearchTestError(errCh chan error, format string, args ...any) {
	select {
	case errCh <- fmt.Errorf(format, args...):
	default:
	}
}

func firstWebSearchTestError(errCh chan error) error {
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

func TestHandleResponses_NonStreamServerSideWebSearchFollowup(t *testing.T) {
	var mu sync.Mutex
	requests := make([]FireworksRequest, 0, 2)
	errCh := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			_ = r.Body.Close()
		}()
		var fwReq FireworksRequest
		if err := json.NewDecoder(r.Body).Decode(&fwReq); err != nil {
			recordWebSearchTestError(errCh, "decode upstream request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, fwReq)
		requestCount := len(requests)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if requestCount == 1 {
			content := "<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"web_search\",\"arguments\":{\"query\":\"latest Go release\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
			_, _ = w.Write([]byte(marshalSSEContent(t, content)))
		} else {
			_, _ = w.Write([]byte(marshalSSEContent(t, "RESULT: PASS\nVERSION: Go 1.25.3\nDATE: 2025-09-29")))
		}
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"content\":\"\"}\n\n"))
	}))
	defer upstream.Close()

	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"Go 1.25.3 is released","url":"https://go.dev/doc/devel/release#go1.25.3","snippet":"Go 1.25.3 was released on September 29, 2025."}]}`))
	}))
	defer search.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	p.webSearchEndpoint = search.URL
	p.webSearchClient = search.Client()
	mux := newTestMux(t, p, "*")

	body := `{"model":"deepseek-v3p2","stream":false,"input":"必须使用 web_search 查询 Go 官方最新稳定版本与发布日期，并最终输出三行：RESULT、VERSION、DATE。","tools":[{"type":"web_search","external_web_access":true}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if err := firstWebSearchTestError(errCh); err != nil {
		t.Fatal(err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	gotRequests := append([]FireworksRequest(nil), requests...)
	mu.Unlock()
	if len(gotRequests) != 2 {
		t.Fatalf("upstream requests = %d, want 2", len(gotRequests))
	}
	if strings.Contains(gotRequests[1].Messages[0].Content, "<AVAILABLE_TOOLS>") {
		t.Fatalf("follow-up prompt should disable tools after server-side web_search:\n%s", gotRequests[1].Messages[0].Content)
	}
	for _, want := range []string{
		"Web search query: latest Go release",
		"Go 1.25.3 is released",
		"https://go.dev/doc/devel/release#go1.25.3",
	} {
		if !strings.Contains(gotRequests[1].Messages[0].Content, want) {
			t.Fatalf("follow-up prompt missing %q:\n%s", want, gotRequests[1].Messages[0].Content)
		}
	}

	var resp ResponsesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Output) != 2 {
		t.Fatalf("output len = %d, want 2", len(resp.Output))
	}

	var callItem map[string]any
	if err := json.Unmarshal(resp.Output[0], &callItem); err != nil {
		t.Fatalf("decode call item: %v", err)
	}
	if callItem["type"] != "web_search_call" {
		t.Fatalf("first output type = %v, want web_search_call", callItem["type"])
	}

	var messageItem ResponseOutputMessage
	if err := json.Unmarshal(resp.Output[1], &messageItem); err != nil {
		t.Fatalf("decode message item: %v", err)
	}
	if len(messageItem.Content) == 0 {
		t.Fatal("expected at least one content item in ResponseOutputMessage, got 0")
	}
	got := messageItem.Content[0].Text
	for _, want := range []string{
		"RESULT: PASS",
		"VERSION: Go 1.25.3",
		"DATE: 2025-09-29",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("final text missing %q:\n%s", want, got)
		}
	}
}

func TestHandleResponses_StreamServerSideWebSearchFollowup(t *testing.T) {
	var mu sync.Mutex
	requests := make([]FireworksRequest, 0, 2)
	errCh := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			_ = r.Body.Close()
		}()
		var fwReq FireworksRequest
		if err := json.NewDecoder(r.Body).Decode(&fwReq); err != nil {
			recordWebSearchTestError(errCh, "decode upstream request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, fwReq)
		requestCount := len(requests)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if requestCount == 1 {
			content := "<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"web_search\",\"arguments\":{\"query\":\"latest Go release\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
			_, _ = w.Write([]byte(marshalSSEContent(t, content)))
		} else {
			_, _ = w.Write([]byte(marshalSSEContent(t, "RESULT: PASS\nVERSION: Go 1.25.3\nDATE: 2025-09-29")))
		}
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"content\":\"\"}\n\n"))
	}))
	defer upstream.Close()

	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"Go 1.25.3 is released","url":"https://go.dev/doc/devel/release#go1.25.3","snippet":"Go 1.25.3 was released on September 29, 2025."}]}`))
	}))
	defer search.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	p.webSearchEndpoint = search.URL
	p.webSearchClient = search.Client()
	mux := newTestMux(t, p, "*")

	body := `{"model":"deepseek-v3p2","stream":true,"input":"必须使用 web_search 查询 Go 官方最新稳定版本与发布日期，并最终输出三行：RESULT、VERSION、DATE。","tools":[{"type":"web_search","external_web_access":true}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if err := firstWebSearchTestError(errCh); err != nil {
		t.Fatal(err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	gotRequests := append([]FireworksRequest(nil), requests...)
	mu.Unlock()
	if len(gotRequests) != 2 {
		t.Fatalf("upstream requests = %d, want 2", len(gotRequests))
	}

	bodyText := rec.Body.String()
	for _, want := range []string{
		`"type":"web_search_call"`,
		`"text":"RESULT: PASS\nVERSION: Go 1.25.3\nDATE: 2025-09-29"`,
		"event: response.completed",
	} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("stream body missing %q:\n%s", want, bodyText)
		}
	}
}

func TestBuildResponsesWebSearchFollowupPrompt_OmitsBaseInstructionsAndKeepsTaskAndResults(t *testing.T) {
	followupItems := []json.RawMessage{
		buildInputMessageItem("user", "Tool output for call call_ws_1:\nSuccess: true\nOutput:\nWeb search query: latest Go release\n1. Go 1.25.3 is released\nURL: https://go.dev/doc/devel/release#go1.25.3\nSnippet: Go 1.25.3 was released on September 29, 2025."),
	}

	prompt := buildResponsesWebSearchFollowupPrompt(
		nil,
		followupItems,
		"你是 Codex 代理。先阅读仓库约束，再等待用户指定任务。",
		"必须使用 web_search 查询 Go 官方最新稳定版本与发布日期，并最终输出三行：RESULT、VERSION、DATE。",
	)

	if strings.Contains(prompt, "<BASE_INSTRUCTIONS>") {
		t.Fatalf("prompt should omit BASE_INSTRUCTIONS for web_search follow-up:\n%s", prompt)
	}
	for _, want := range []string{
		"<CURRENT_USER_TASK>",
		"<SEARCH_RESULTS>",
		"Web search query: latest Go release",
		"RESULT、VERSION、DATE",
		"Go 官方最新稳定版本与发布日期",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	for _, unwanted := range []string{
		"User: Tool result",
		"User: Tool output",
		"必须使用 web_search",
		"禁止使用 exec_command",
	} {
		if strings.Contains(prompt, unwanted) {
			t.Fatalf("prompt should not keep message-role wrapper %q:\n%s", unwanted, prompt)
		}
	}
}

func TestHandleResponses_NonStreamServerSideWebSearchFollowupMissingLabelsReturnsAdapterError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		defer func() {
			_ = r.Body.Close()
		}()

		var fwReq FireworksRequest
		if err := json.NewDecoder(r.Body).Decode(&fwReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		content := "<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"web_search\",\"arguments\":{\"query\":\"latest Go release\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
		if strings.Contains(fwReq.Messages[0].Content, "Web search query: latest Go release") {
			content = "Go 1.25.3 was released on 2025-09-29."
		}
		_, _ = w.Write([]byte(marshalSSEContent(t, content)))
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"content\":\"\"}\n\n"))
	}))
	defer upstream.Close()

	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"Go 1.25.3 is released","url":"https://go.dev/doc/devel/release#go1.25.3","snippet":"Go 1.25.3 was released on September 29, 2025."}]}`))
	}))
	defer search.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	p.webSearchEndpoint = search.URL
	p.webSearchClient = search.Client()
	mux := newTestMux(t, p, "*")

	body := `{"model":"deepseek-v3p2","stream":false,"input":"必须使用 web_search 查询 Go 官方最新稳定版本与发布日期，并最终输出三行：RESULT、VERSION、DATE。","tools":[{"type":"web_search","external_web_access":true}]}`
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
	if len(resp.Output) != 2 {
		t.Fatalf("output len = %d, want 2", len(resp.Output))
	}

	var messageItem ResponseOutputMessage
	if err := json.Unmarshal(resp.Output[1], &messageItem); err != nil {
		t.Fatalf("decode message item: %v", err)
	}
	if len(messageItem.Content) == 0 {
		t.Fatal("expected at least one content item in ResponseOutputMessage, got 0")
	}
	got := messageItem.Content[0].Text
	if !strings.Contains(got, "Codex adapter error: web_search follow-up omitted required output labels") {
		t.Fatalf("final text = %q, want explicit adapter error", got)
	}
	if strings.Contains(got, "RESULT: PASS") {
		t.Fatalf("final text = %q, should not silently claim PASS", got)
	}
}

func TestHandleResponses_NonStreamServerSideWebSearchFollowupUpstream500UsesFallbackFinal(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			_ = r.Body.Close()
		}()
		var fwReq FireworksRequest
		if err := json.NewDecoder(r.Body).Decode(&fwReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if strings.Contains(fwReq.Messages[0].Content, "Web search query: latest Go release") {
			http.Error(w, "upstream exploded", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		content := "<<<AI_ACTIONS_V1>>>\n{\"mode\":\"tool\",\"calls\":[{\"name\":\"web_search\",\"arguments\":{\"query\":\"latest Go release\"}}]}\n<<<END_AI_ACTIONS_V1>>>"
		_, _ = w.Write([]byte(marshalSSEContent(t, content)))
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"content\":\"\"}\n\n"))
	}))
	defer upstream.Close()

	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"Go 1.25.3 is released","url":"https://go.dev/doc/devel/release#go1.25.3","snippet":"Go 1.25.3 was released on September 29, 2025."}]}`))
	}))
	defer search.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, upstream.URL)
	p.webSearchEndpoint = search.URL
	p.webSearchClient = search.Client()
	mux := newTestMux(t, p, "*")

	body := `{"model":"gpt-oss-20b","stream":false,"input":"你是测试代理。请验证 web_search：\n1) 必须使用 web_search 查询 Go 官方最新稳定版本与发布日期。\n2) 禁止使用 exec_command、docfork 或其他工具代替 web_search。\n3) web_search 返回后，必须直接用四行格式收口，不要输出前言或解释工具行为。\n4) 不要修改任何文件。\n最后只输出四行：RESULT: PASS 或 FAIL；FILES: none；TEST: N/A；NOTE: 版本号与日期。","tools":[{"type":"web_search","external_web_access":true}]}`
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
	if len(resp.Output) != 2 {
		t.Fatalf("output len = %d, want 2", len(resp.Output))
	}

	var messageItem ResponseOutputMessage
	if err := json.Unmarshal(resp.Output[1], &messageItem); err != nil {
		t.Fatalf("decode message item: %v", err)
	}
	if len(messageItem.Content) == 0 {
		t.Fatal("expected at least one content item in ResponseOutputMessage, got 0")
	}
	got := messageItem.Content[0].Text
	for _, want := range []string{
		"RESULT: PASS",
		"FILES: none",
		"TEST: N/A",
		"NOTE:",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("final text missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Codex adapter error:") {
		t.Fatalf("final text = %q, should not expose adapter error", got)
	}
}

func TestParseWebSearchResults_FallsBackToHTMLWhenJSONDecodeFails(t *testing.T) {
	body := []byte(`<html><body><a class="result__a" href="https://go.dev/doc/devel/release#go1.25.3">Go 1.25.3 is released</a><div class="result__snippet">Go 1.25.3 was released on September 29, 2025.</div></body></html>`)

	results, err := parseWebSearchResults(body, "application/json")
	if err != nil {
		t.Fatalf("parseWebSearchResults error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].URL != "https://go.dev/doc/devel/release#go1.25.3" {
		t.Fatalf("URL = %q", results[0].URL)
	}
}

func TestParseWebSearchResultsHTML_ParsesDuckDuckGoLiteMarkup(t *testing.T) {
	body := `<html><body>
		<a rel="nofollow" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2Fdevel%2Frelease&amp;rut=abc" class='result-link'>Release History - The Go Programming Language</a>
		<td class='result-snippet'>Go 1.25.3 was released on September 29, 2025.</td>
	</body></html>`

	results, err := parseWebSearchResultsHTML(body)
	if err != nil {
		t.Fatalf("parseWebSearchResultsHTML error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].URL != "https://go.dev/doc/devel/release" {
		t.Fatalf("URL = %q", results[0].URL)
	}
	if results[0].Title != "Release History - The Go Programming Language" {
		t.Fatalf("Title = %q", results[0].Title)
	}
	if !strings.Contains(results[0].Snippet, "Go 1.25.3") {
		t.Fatalf("Snippet = %q", results[0].Snippet)
	}
}

func TestBuildWebSearchRequestURLs_AddsLiteFallbackForDefaultEndpoint(t *testing.T) {
	urls, err := buildWebSearchRequestURLs("", "latest Go release")
	if err != nil {
		t.Fatalf("buildWebSearchRequestURLs error = %v", err)
	}
	if len(urls) != 2 {
		t.Fatalf("len(urls) = %d, want 2: %#v", len(urls), urls)
	}
	if !strings.Contains(urls[0], "html.duckduckgo.com") || !strings.Contains(urls[1], "lite.duckduckgo.com") {
		t.Fatalf("unexpected fallback urls: %#v", urls)
	}
}

func TestBuildWebSearchRequestURLs_DoesNotAddFallbackForCustomEndpoint(t *testing.T) {
	urls, err := buildWebSearchRequestURLs("http://example.test/search", "latest Go release")
	if err != nil {
		t.Fatalf("buildWebSearchRequestURLs error = %v", err)
	}
	if len(urls) != 1 {
		t.Fatalf("len(urls) = %d, want 1: %#v", len(urls), urls)
	}
}

func TestBuildWebSearchFallbackFinalText_PreservesUTF8Boundary(t *testing.T) {
	task := "最后只输出两行：RESULT: PASS 或 FAIL；NOTE: 说明。"
	summary := strings.Repeat("你", 400)

	got := buildWebSearchFallbackFinalText(task, []string{summary})
	if !utf8.ValidString(got) {
		t.Fatalf("fallback text is not valid UTF-8: %q", got)
	}
}

func TestRunWebSearch_RetriesTemporaryStatus(t *testing.T) {
	var attempts int
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"Go 1.25.3 is released","url":"https://go.dev/doc/devel/release#go1.25.3","snippet":"Go 1.25.3 was released on September 29, 2025."}]}`))
	}))
	defer search.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, "http://example.invalid")
	p.webSearchEndpoint = search.URL
	p.webSearchClient = search.Client()

	summary, err := p.runWebSearch(context.Background(), "latest Go release")
	if err != nil {
		t.Fatalf("runWebSearch error = %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if !strings.Contains(summary, "Go 1.25.3 is released") {
		t.Fatalf("summary missing search result:\n%s", summary)
	}
}

func TestRunWebSearch_DoesNotRetryParseFailure(t *testing.T) {
	var attempts int
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body>no results markup</body></html>`))
	}))
	defer search.Close()

	p := NewWithUpstream(transport.New(30*time.Second), "test", false, "http://example.invalid")
	p.webSearchEndpoint = search.URL
	p.webSearchClient = search.Client()

	_, err := p.runWebSearch(context.Background(), "latest Go release")
	if err == nil {
		t.Fatal("runWebSearch error = nil, want parse failure")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if !strings.Contains(err.Error(), "after 1 attempt") {
		t.Fatalf("error does not report actual attempt count: %v", err)
	}
}
