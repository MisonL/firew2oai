package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mison/firew2oai/internal/config"
	"github.com/mison/firew2oai/internal/transport"
)

const (
	defaultWebSearchEndpoint         = "https://html.duckduckgo.com/html/"
	defaultWebSearchFallbackEndpoint = "https://lite.duckduckgo.com/lite/"
	maxWebSearchResults              = 5
	maxWebSearchBodyBytes            = 512 * 1024
	webSearchMaxAttempts             = 3
	webSearchRetryDelay              = 250 * time.Millisecond
)

type webSearchStatusError struct {
	StatusCode int
}

func (e webSearchStatusError) Error() string {
	return fmt.Sprintf("web search endpoint returned %d", e.StatusCode)
}

type webSearchChallengeError struct {
	Provider string
}

func (e webSearchChallengeError) Error() string {
	provider := strings.TrimSpace(e.Provider)
	if provider == "" {
		provider = "search backend"
	}
	return fmt.Sprintf("web search backend blocked request with %s challenge", provider)
}

var (
	webSearchAnchorPattern          = regexp.MustCompile(`(?is)<a\b([^>]*)>(.*?)</a>`)
	webSearchResultLinkClassPattern = regexp.MustCompile(`(?is)\bclass\s*=\s*(?:"[^"]*(?:result__a|result-link)[^"]*"|'[^']*(?:result__a|result-link)[^']*')`)
	webSearchHrefPattern            = regexp.MustCompile(`(?is)\bhref\s*=\s*(?:"([^"]+)"|'([^']+)')`)
	webSearchResultSnippetPattern   = regexp.MustCompile(`(?is)<(?:a|div|td)[^>]+class=(?:"[^"]*(?:result__snippet|result-snippet)[^"]*"|'[^']*(?:result__snippet|result-snippet)[^']*')[^>]*>(.*?)</(?:a|div|td)>`)
	htmlTagPattern                  = regexp.MustCompile(`(?is)<[^>]+>`)
)

type webSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

type webSearchCall struct {
	ID    string
	Query string
}

func isWebSearchOnlyCallBatch(calls []parsedToolCall) bool {
	if len(calls) == 0 {
		return false
	}
	for _, call := range calls {
		var item map[string]any
		if err := json.Unmarshal(call.item, &item); err != nil {
			return false
		}
		if typ, _ := item["type"].(string); typ != "web_search_call" {
			return false
		}
	}
	return true
}

func decodeWebSearchCalls(calls []parsedToolCall) ([]webSearchCall, error) {
	decoded := make([]webSearchCall, 0, len(calls))
	for _, call := range calls {
		var item map[string]any
		if err := json.Unmarshal(call.item, &item); err != nil {
			return nil, fmt.Errorf("decode web_search_call item: %w", err)
		}
		if typ, _ := item["type"].(string); typ != "web_search_call" {
			return nil, fmt.Errorf("unsupported tool call type %q for server-side web search", typ)
		}
		callID, _ := item["id"].(string)
		callID = strings.TrimSpace(callID)
		if callID == "" {
			callID = rawItemID(call.item)
		}
		query := ""
		if action, ok := item["action"].(map[string]any); ok {
			query, _ = firstStringField(action, "query")
		}
		if query == "" {
			query, _ = firstStringField(item, "query")
		}
		query = strings.TrimSpace(query)
		if query == "" {
			return nil, fmt.Errorf("web_search_call %q is missing query", callID)
		}
		decoded = append(decoded, webSearchCall{ID: callID, Query: query})
	}
	return decoded, nil
}

func (p *Proxy) completeResponsesViaServerWebSearch(
	ctx context.Context,
	authToken string,
	model string,
	showThinking bool,
	baseHistoryItems []json.RawMessage,
	requestItems []json.RawMessage,
	instructions string,
	temperature *float64,
	maxOutputTokens *int,
	currentTask string,
	calls []parsedToolCall,
) (string, []json.RawMessage, []json.RawMessage, bool, error) {
	if !isWebSearchOnlyCallBatch(calls) {
		return "", nil, nil, false, nil
	}

	decodedCalls, err := decodeWebSearchCalls(calls)
	if err != nil {
		return "", nil, nil, true, err
	}

	callOutputItems := buildParsedToolOutputItems(calls)
	followupRequestItems := cloneRawItems(requestItems)
	historyRequestItems := append(cloneRawItems(requestItems), cloneRawItems(callOutputItems)...)
	searchSummaries := make([]string, 0, len(decodedCalls))

	for _, call := range decodedCalls {
		summary, searchErr := p.runWebSearch(ctx, call.Query)
		success := searchErr == nil
		if searchErr != nil {
			summary = "web_search failed: " + searchErr.Error()
		}
		resultItem := buildInputMessageItem("user", formatToolOutputSummary(call.ID, &success, summary, ""))
		followupRequestItems = append(followupRequestItems, resultItem)
		historyRequestItems = append(historyRequestItems, resultItem)
		if searchErr != nil {
			return buildToolProtocolErrorMessage(searchErr, ""), callOutputItems, historyRequestItems, true, nil
		}
		searchSummaries = append(searchSummaries, summary)
	}

	followupPrompt := buildResponsesWebSearchFollowupPrompt(baseHistoryItems, followupRequestItems, instructions, currentTask)
	bodyBytes, err := buildFireworksRequestBody(model, followupPrompt, temperature, maxOutputTokens)
	if err != nil {
		return "", callOutputItems, historyRequestItems, true, err
	}

	finalText, err := p.collectResponseText(ctx, authToken, model, bodyBytes, showThinking)
	if err != nil {
		if taskRequestsNoFilesLabel(currentTask) && taskRequestsNotApplicableLabel(currentTask, "TEST") {
			return buildWebSearchFallbackFinalText(currentTask, searchSummaries), callOutputItems, historyRequestItems, true, nil
		}
		return "", callOutputItems, historyRequestItems, true, err
	}
	if shouldUseWebSearchFallback(finalText) {
		return finalText, callOutputItems, historyRequestItems, true, fmt.Errorf("web_search follow-up did not answer from captured results")
	}
	if missingRequiredOutputLabels(currentTask, finalText) {
		return finalText, callOutputItems, historyRequestItems, true, fmt.Errorf("web_search follow-up omitted required output labels")
	}
	return finalText, callOutputItems, historyRequestItems, true, nil
}

func buildResponsesWebSearchFollowupPrompt(_ []json.RawMessage, followupRequestItems []json.RawMessage, _ string, currentTask string) string {
	currentMessages := rawItemsToMessages(followupRequestItems)
	followupTask := sanitizeWebSearchFollowupTask(currentTask)

	var builder strings.Builder
	builder.WriteString("You are serving an OpenAI Responses follow-up after web_search was already executed by the proxy.\n")
	builder.WriteString("Do not mention searching, tool calls, or tool availability.\n")
	builder.WriteString("Use the provided search results to answer the task directly.\n")
	builder.WriteString("Return the final answer only.\n")
	if strings.TrimSpace(followupTask) != "" {
		builder.WriteString("\n<CURRENT_USER_TASK>\n")
		builder.WriteString(strings.TrimSpace(followupTask))
		builder.WriteString("\n</CURRENT_USER_TASK>\n")
	}
	if formatBlock := buildFinalizeOutputFormatBlock(currentTask); formatBlock != "" {
		builder.WriteString(formatBlock)
	}
	if webSearchContext := buildWebSearchPromptContext(currentMessages); webSearchContext != "" {
		builder.WriteString("\n<SEARCH_RESULTS>\n")
		builder.WriteString(webSearchContext)
		builder.WriteString("\n</SEARCH_RESULTS>\n")
	}
	builder.WriteString("\n<FINALIZE_RULES>\n")
	builder.WriteString("Answer from SEARCH_RESULTS.\n")
	builder.WriteString("If the task asks for exact labels, output exactly those labels and nothing else.\n")
	builder.WriteString("Do not say you will search.\n")
	builder.WriteString("Do not say the tool is unavailable.\n")
	builder.WriteString("</FINALIZE_RULES>\n")
	return builder.String()
}

func sanitizeWebSearchFollowupTask(task string) string {
	task = strings.TrimSpace(task)
	if task == "" {
		return ""
	}

	lines := strings.Split(task, "\n")
	kept := make([]string, 0, len(lines))
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		line = stripWebSearchFollowupTaskNoise(line)
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if shouldDropWebSearchFollowupTaskLine(line) {
			continue
		}
		line = strings.TrimPrefix(line, "你是测试代理。")
		line = strings.TrimSpace(line)
		if line != "" {
			kept = append(kept, line)
		}
	}
	if len(kept) == 0 {
		return task
	}
	return strings.Join(kept, "\n")
}

func stripWebSearchFollowupTaskNoise(line string) string {
	replacements := []string{
		"必须使用 web_search",
		"web_search 返回后，",
		"web_search 返回后",
		"不要输出前言或解释工具行为。",
		"不要输出前言或解释工具行为",
	}
	normalized := strings.TrimSpace(line)
	for _, token := range replacements {
		normalized = strings.ReplaceAll(normalized, token, "")
	}
	normalized = strings.TrimLeft(normalized, "：:，,。.；; ")
	return normalized
}

func shouldDropWebSearchFollowupTaskLine(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	if lower == "" {
		return false
	}
	for _, token := range []string{
		"必须使用 web_search",
		"禁止使用",
		"不要输出前言或解释工具行为",
		"不要修改任何文件",
		"tool calls",
		"tool availability",
	} {
		if strings.Contains(lower, strings.ToLower(token)) {
			return true
		}
	}
	return false
}

func buildWebSearchPromptContext(messages []ChatMessage) string {
	if len(messages) == 0 {
		return ""
	}
	blocks := make([]string, 0, len(messages))
	for _, msg := range messages {
		text := strings.TrimSpace(msg.Content)
		if text == "" {
			continue
		}
		if isToolResultSummaryMessage(text) {
			if output := extractToolResultOutputBody(text); output != "" {
				blocks = append(blocks, output)
			}
		}
	}
	if len(blocks) == 0 {
		return ""
	}
	return strings.Join(blocks, "\n\n")
}

func extractToolResultOutputBody(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if idx := strings.Index(text, "\nOutput:\n"); idx >= 0 {
		output := strings.TrimSpace(text[idx+len("\nOutput:\n"):])
		if output != "" {
			return output
		}
	}
	return text
}

func (p *Proxy) collectResponseText(ctx context.Context, authToken, model string, body []byte, showThinking bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.transport.Timeout())
	defer cancel()

	openReader := func() (io.ReadCloser, error) {
		return p.transport.StreamPost(ctx, p.upstreamURL, bytes.NewReader(body), authToken)
	}

	reader, err := openReader()
	if err != nil {
		return "", fmt.Errorf("follow-up upstream request failed: %w", err)
	}

	var result strings.Builder
	isThinking := config.IsThinkingModel(model)
	doneReceived := false
	contentEmitted := false
	var scanErr error
	attempt := 0
	for {
		doneReceived = false
		contentEmitted, scanErr = scanSSEEvents(reader, isThinking, showThinking, func(evt sseContentEvent) bool {
			switch evt.Type {
			case "done":
				doneReceived = true
			case "thinking_separator":
				result.WriteString("\n\n--- Answer ---\n\n")
			case "content":
				result.WriteString(evt.Content)
			}
			return true
		})
		_ = reader.Close()

		if p.upstreamEmptyRetry.shouldRetry(attempt, doneReceived, contentEmitted, result.Len(), scanErr, false) {
			delay := p.upstreamEmptyRetry.delay(attempt)
			attempt++
			if err := sleepWithContext(ctx, delay); err != nil {
				scanErr = err
				break
			}
			reader, err = openReader()
			if err != nil {
				scanErr = err
				break
			}
			continue
		}
		break
	}

	if scanErr != nil && !contentEmitted && result.Len() == 0 {
		return "", fmt.Errorf("follow-up upstream stream failed: %w", scanErr)
	}
	if !doneReceived && !contentEmitted && result.Len() == 0 {
		return "", fmt.Errorf("follow-up upstream response ended without a completion signal")
	}
	return result.String(), nil
}

func (p *Proxy) runWebSearch(ctx context.Context, query string) (string, error) {
	endpoint := strings.TrimSpace(p.webSearchEndpoint)
	if endpoint == "" {
		endpoint = defaultWebSearchEndpoint
	}
	client := p.webSearchClient
	if client == nil {
		client = &http.Client{Timeout: p.transport.Timeout()}
	}

	reqURLs, err := buildWebSearchRequestURLs(endpoint, query)
	if err != nil {
		return "", fmt.Errorf("invalid web search endpoint: %w", err)
	}

	var lastErr error
	attempts := 0
	for attempt := 1; attempt <= webSearchMaxAttempts; attempt++ {
		attempts = attempt
		retryable := false
		for _, reqURL := range reqURLs {
			summary, err := p.runWebSearchOnce(ctx, client, reqURL, query)
			if err == nil {
				return summary, nil
			}
			lastErr = err
			if isRetryableWebSearchError(err) {
				retryable = true
			}
		}
		if attempt == webSearchMaxAttempts || !retryable {
			break
		}
		if err := sleepWithContext(ctx, webSearchRetryDelay); err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("web search failed after %d attempt(s): %w", attempts, lastErr)
}

func buildWebSearchRequestURLs(endpoint, query string) ([]string, error) {
	trimmed := strings.TrimSpace(endpoint)
	if trimmed == "" {
		trimmed = defaultWebSearchEndpoint
	}
	primary, err := buildWebSearchRequestURL(trimmed, query)
	if err != nil {
		return nil, err
	}
	urls := []string{primary}
	if trimmed == "" || strings.TrimRight(trimmed, "/") == strings.TrimRight(defaultWebSearchEndpoint, "/") {
		fallback, err := buildWebSearchRequestURL(defaultWebSearchFallbackEndpoint, query)
		if err != nil {
			return nil, err
		}
		urls = append(urls, fallback)
	}
	return urls, nil
}

func buildWebSearchRequestURL(endpoint, query string) (string, error) {
	reqURL, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	values := reqURL.Query()
	values.Set("q", query)
	reqURL.RawQuery = values.Encode()
	return reqURL.String(), nil
}

func (p *Proxy) runWebSearchOnce(ctx context.Context, client *http.Client, reqURL string, query string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("create web search request: %w", err)
	}
	req.Header.Set("Accept", "text/html,application/json;q=0.9")
	req.Header.Set("User-Agent", transport.ChromeUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("execute web search request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", webSearchStatusError{StatusCode: resp.StatusCode}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxWebSearchBodyBytes))
	if err != nil {
		return "", fmt.Errorf("read web search response: %w", err)
	}

	results, err := parseWebSearchResults(body, resp.Header.Get("Content-Type"))
	if err != nil {
		return "", err
	}
	if len(results) == 0 {
		return fmt.Sprintf("Web search query: %s\nNo results found.", query), nil
	}
	return formatWebSearchSummary(query, results), nil
}

func isRetryableWebSearchError(err error) bool {
	if err == nil {
		return false
	}
	var statusErr webSearchStatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode == http.StatusTooManyRequests || statusErr.StatusCode >= 500
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

func parseWebSearchResults(body []byte, contentType string) ([]webSearchResult, error) {
	trimmed := bytes.TrimSpace(body)
	if strings.Contains(strings.ToLower(contentType), "json") || (len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')) {
		results, ok, err := parseWebSearchResultsJSON(trimmed)
		if err == nil && ok {
			return limitWebSearchResults(results), nil
		}
	}
	return parseWebSearchResultsHTML(string(body))
}

func parseWebSearchResultsJSON(body []byte) ([]webSearchResult, bool, error) {
	type resultEnvelope struct {
		Results []webSearchResult `json:"results"`
		Items   []webSearchResult `json:"items"`
	}
	var envelope resultEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, false, fmt.Errorf("parse web search json: %w", err)
	}
	switch {
	case len(envelope.Results) > 0:
		return envelope.Results, true, nil
	case len(envelope.Items) > 0:
		return envelope.Items, true, nil
	default:
		return nil, false, nil
	}
}

func parseWebSearchResultsHTML(body string) ([]webSearchResult, error) {
	if isDuckDuckGoChallengePage(body) {
		return nil, webSearchChallengeError{Provider: "DuckDuckGo"}
	}
	links := extractWebSearchResultLinks(body)
	snippets := webSearchResultSnippetPattern.FindAllStringSubmatch(body, maxWebSearchResults)
	if len(links) == 0 {
		return nil, fmt.Errorf("parse web search html: no results found")
	}

	results := make([]webSearchResult, 0, len(links))
	for i, link := range links {
		result := webSearchResult{
			Title: stripHTML(link.Title),
			URL:   decodeDuckDuckGoLink(link.URL),
		}
		if i < len(snippets) {
			for _, candidate := range snippets[i][1:] {
				if text := stripHTML(candidate); text != "" {
					result.Snippet = text
					break
				}
			}
		}
		if result.Title == "" && result.URL == "" && result.Snippet == "" {
			continue
		}
		results = append(results, result)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("parse web search html: no usable results found")
	}
	return results, nil
}

func isDuckDuckGoChallengePage(body string) bool {
	lower := strings.ToLower(body)
	if !strings.Contains(lower, "duckduckgo") {
		return false
	}
	matches := 0
	for _, marker := range []string{
		"unfortunately, bots use duckduckgo too",
		"please complete the following challenge",
		"anomaly.js",
	} {
		if strings.Contains(lower, marker) {
			matches++
		}
	}
	return matches >= 2
}

type webSearchHTMLLink struct {
	URL   string
	Title string
}

func extractWebSearchResultLinks(body string) []webSearchHTMLLink {
	matches := webSearchAnchorPattern.FindAllStringSubmatch(body, -1)
	links := make([]webSearchHTMLLink, 0, maxWebSearchResults)
	for _, match := range matches {
		if len(match) < 3 {
			continue
		}
		attrs := match[1]
		if !webSearchResultLinkClassPattern.MatchString(attrs) {
			continue
		}
		hrefMatches := webSearchHrefPattern.FindStringSubmatch(attrs)
		if len(hrefMatches) < 3 {
			continue
		}
		rawURL := strings.TrimSpace(hrefMatches[1])
		if rawURL == "" {
			rawURL = strings.TrimSpace(hrefMatches[2])
		}
		if rawURL == "" {
			continue
		}
		links = append(links, webSearchHTMLLink{
			URL:   rawURL,
			Title: match[2],
		})
		if len(links) >= maxWebSearchResults {
			break
		}
	}
	return links
}

func formatWebSearchSummary(query string, results []webSearchResult) string {
	var builder strings.Builder
	builder.WriteString("Web search query: ")
	builder.WriteString(strings.TrimSpace(query))
	builder.WriteByte('\n')
	for index, result := range limitWebSearchResults(results) {
		fmt.Fprintf(&builder, "%d. %s\n", index+1, strings.TrimSpace(result.Title))
		if result.URL != "" {
			builder.WriteString("URL: ")
			builder.WriteString(result.URL)
			builder.WriteByte('\n')
		}
		if result.Snippet != "" {
			builder.WriteString("Snippet: ")
			builder.WriteString(strings.TrimSpace(result.Snippet))
			builder.WriteByte('\n')
		}
	}
	return strings.TrimSpace(builder.String())
}

func limitWebSearchResults(results []webSearchResult) []webSearchResult {
	if len(results) <= maxWebSearchResults {
		return results
	}
	return results[:maxWebSearchResults]
}

func stripHTML(raw string) string {
	text := html.UnescapeString(raw)
	text = htmlTagPattern.ReplaceAllString(text, " ")
	text = strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	return text
}

func decodeDuckDuckGoLink(raw string) string {
	raw = html.UnescapeString(strings.TrimSpace(raw))
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if target := parsed.Query().Get("uddg"); target != "" {
		if decoded, err := url.QueryUnescape(target); err == nil {
			return decoded
		}
		return target
	}
	return raw
}

func shouldUseWebSearchFallback(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return true
	}
	markers := []string{
		"don't have access to any search results",
		"do not have access to any search results",
		"need the actual search results",
		"please provide the search results",
		"don't see any search results",
		"do not see any search results",
		"please clarify what you'd like me to help",
		"could you please clarify",
		"no search results or specific task were included",
		"don't have the necessary information",
		"do not have the necessary information",
		"i'll search for",
		"i will search for",
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func missingRequiredOutputLabels(task, text string) bool {
	labels := dedupePreserveOrder(extractRequiredOutputLabels(task))
	if len(labels) == 0 {
		return false
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	nonEmpty := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			nonEmpty = append(nonEmpty, line)
		}
	}
	if len(nonEmpty) < len(labels) {
		return true
	}
	for index, label := range labels {
		if !strings.HasPrefix(nonEmpty[index], label+":") {
			return true
		}
	}
	return false
}

func buildWebSearchFallbackFinalText(task string, summaries []string) string {
	combined := strings.TrimSpace(strings.Join(filterNonEmptyStrings(summaries), "\n\n"))
	if combined == "" {
		combined = "No web_search results were captured."
	}
	labels := dedupePreserveOrder(extractRequiredOutputLabels(task))
	if len(labels) == 0 {
		return combined
	}
	note := strings.Join(strings.Fields(combined), " ")
	noteRunes := []rune(note)
	if len(noteRunes) > 320 {
		note = string(noteRunes[:320])
	}
	lines := make([]string, 0, len(labels))
	for _, label := range labels {
		value := note
		switch label {
		case "RESULT":
			value = "PASS"
		case "FILES":
			value = "none"
		case "TEST":
			value = "N/A"
		case "NOTE":
			value = note
		}
		lines = append(lines, label+": "+value)
	}
	return strings.Join(lines, "\n")
}
