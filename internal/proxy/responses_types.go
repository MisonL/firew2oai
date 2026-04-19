package proxy

import "encoding/json"

// ResponsesRequest is a minimal OpenAI Responses API subset.
type ResponsesRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Instructions       string          `json:"instructions,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Tools              json.RawMessage `json:"tools,omitempty"`
	ToolChoice         json.RawMessage `json:"tool_choice,omitempty"`
	Reasoning          json.RawMessage `json:"reasoning,omitempty"`
	Text               json.RawMessage `json:"text,omitempty"`
	Include            []string        `json:"include,omitempty"`
	PromptCacheKey     string          `json:"prompt_cache_key,omitempty"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	MaxOutputTokens    *int            `json:"max_output_tokens,omitempty"`
	ShowThinking       *bool           `json:"show_thinking,omitempty"`
}

type ResponseOutputText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ResponseOutputMessage struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Type    string               `json:"type"`
	Role    string               `json:"role"`
	Content []ResponseOutputText `json:"content"`
}

type ResponsesResponse struct {
	ID        string            `json:"id"`
	Object    string            `json:"object"`
	CreatedAt int64             `json:"created_at"`
	Status    string            `json:"status"`
	Model     string            `json:"model"`
	Output    []json.RawMessage `json:"output,omitempty"`
	Usage     *ResponseUsage    `json:"usage,omitempty"`
}

type ResponseUsage struct {
	InputTokens         int                          `json:"input_tokens"`
	InputTokensDetails  *ResponseInputTokensDetails  `json:"input_tokens_details,omitempty"`
	OutputTokens        int                          `json:"output_tokens"`
	OutputTokensDetails *ResponseOutputTokensDetails `json:"output_tokens_details,omitempty"`
	TotalTokens         int                          `json:"total_tokens"`
}

type ResponseInputTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type ResponseOutputTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type ResponseLifecycleEvent struct {
	Type     string            `json:"type"`
	Response ResponsesResponse `json:"response"`
}

type ResponseOutputTextDeltaEvent struct {
	Type         string `json:"type"`
	ResponseID   string `json:"response_id"`
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Delta        string `json:"delta"`
}

type ResponseOutputTextDoneEvent struct {
	Type         string `json:"type"`
	ResponseID   string `json:"response_id"`
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Text         string `json:"text"`
}

type ResponseOutputItemAddedEvent struct {
	Type        string          `json:"type"`
	ResponseID  string          `json:"response_id"`
	OutputIndex int             `json:"output_index"`
	Item        json.RawMessage `json:"item"`
}

type ResponseContentPartAddedEvent struct {
	Type         string             `json:"type"`
	ResponseID   string             `json:"response_id"`
	ItemID       string             `json:"item_id"`
	OutputIndex  int                `json:"output_index"`
	ContentIndex int                `json:"content_index"`
	Part         ResponseOutputText `json:"part"`
}

type ResponseOutputItemDoneEvent struct {
	Type        string          `json:"type"`
	ResponseID  string          `json:"response_id"`
	OutputIndex int             `json:"output_index"`
	Item        json.RawMessage `json:"item"`
}

type ResponseInputItemList struct {
	Object  string            `json:"object"`
	Data    []json.RawMessage `json:"data"`
	FirstID string            `json:"first_id,omitempty"`
	LastID  string            `json:"last_id,omitempty"`
	HasMore bool              `json:"has_more"`
}
