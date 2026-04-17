package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
)

const (
	initialSSEScanBufferSize = 4 << 10
	maxSSEScanTokenSize      = 1 << 20
	pooledJSONBufferLimit    = 64 << 10
)

var (
	sseDataPrefix = []byte("data:")

	sseScanBufferPool = sync.Pool{
		New: func() any {
			return make([]byte, initialSSEScanBufferSize)
		},
	}

	jsonBufferPool = sync.Pool{
		New: func() any {
			return bytes.NewBuffer(make([]byte, 0, 1024))
		},
	}
)

// sseContentEvent represents a parsed, filtered SSE content event ready for processing.
type sseContentEvent struct {
	Type    string // "content", "done", "thinking_separator"
	Content string // only set for "content" events
}

type thinkingScanState struct {
	inThinking     bool
	separatorSeen  bool
	tagSeen        bool
	visibleEmitted bool
	inThinkTag     bool
	hiddenBuffer   strings.Builder
}

const (
	thinkingTagOpen  = "<think>"
	thinkingTagClose = "</think>"
)

func emitVisibleSSEContent(content string, state *thinkingScanState, onEvent func(sseContentEvent) bool) bool {
	if content == "" {
		return true
	}
	state.visibleEmitted = true
	return onEvent(sseContentEvent{Type: "content", Content: content})
}

func handleTaggedThinkingContent(content string, showThinking bool, state *thinkingScanState, onEvent func(sseContentEvent) bool) bool {
	state.tagSeen = true
	remaining := content

	for {
		if state.inThinkTag {
			idx := strings.Index(remaining, thinkingTagClose)
			if idx < 0 {
				if showThinking {
					return emitVisibleSSEContent(remaining, state, onEvent)
				}
				return true
			}

			thinkingPart := remaining[:idx]
			if showThinking && !emitVisibleSSEContent(thinkingPart, state, onEvent) {
				return false
			}
			remaining = remaining[idx+len(thinkingTagClose):]
			state.inThinkTag = false
			state.inThinking = false
			state.separatorSeen = true
			if showThinking && !onEvent(sseContentEvent{Type: "thinking_separator"}) {
				return false
			}
			continue
		}

		openIdx := strings.Index(remaining, thinkingTagOpen)
		closeIdx := strings.Index(remaining, thinkingTagClose)
		if closeIdx >= 0 && (openIdx < 0 || closeIdx < openIdx) {
			beforeClose := remaining[:closeIdx]
			if state.inThinking {
				if showThinking && !emitVisibleSSEContent(beforeClose, state, onEvent) {
					return false
				}
				state.inThinking = false
				state.separatorSeen = true
				if showThinking && !onEvent(sseContentEvent{Type: "thinking_separator"}) {
					return false
				}
			} else if beforeClose != "" && !emitVisibleSSEContent(beforeClose, state, onEvent) {
				return false
			}
			remaining = remaining[closeIdx+len(thinkingTagClose):]
			continue
		}

		if openIdx < 0 {
			return emitVisibleSSEContent(remaining, state, onEvent)
		}

		before := remaining[:openIdx]
		if before != "" && !emitVisibleSSEContent(before, state, onEvent) {
			return false
		}

		remaining = remaining[openIdx+len(thinkingTagOpen):]
		state.inThinkTag = true
		state.inThinking = true
	}
}

// messagesToPrompt converts OpenAI multi-turn messages into a single prompt
// string since Fireworks chat/single processes a flat message list.
func messagesToPrompt(messages []ChatMessage) string {
	if len(messages) == 0 {
		return ""
	}

	total := 0
	for _, msg := range messages {
		total += len(msg.Content) + len(rolePrefix(msg.Role)) + 1
	}

	var builder strings.Builder
	builder.Grow(total)
	for i, msg := range messages {
		if i > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(rolePrefix(msg.Role))
		builder.WriteString(msg.Content)
	}
	return builder.String()
}

func rolePrefix(role string) string {
	switch role {
	case "system":
		return "System: "
	case "developer":
		return "Developer: "
	case "user":
		return "User: "
	case "assistant":
		return "Assistant: "
	default:
		return ""
	}
}

// scanSSEEvents reads SSE events from reader and calls onEvent for each filtered event.
// Returning false from onEvent stops scanning early.
// Returns (hasContent, error) where hasContent indicates if any content was emitted.
func scanSSEEvents(reader io.Reader, isThinking, showThinking bool, onEvent func(sseContentEvent) bool) (bool, error) {
	scanner := bufio.NewScanner(reader)
	scanBuffer := sseScanBufferPool.Get().([]byte)
	scanner.Buffer(scanBuffer[:initialSSEScanBufferSize], maxSSEScanTokenSize)
	defer func() {
		sseScanBufferPool.Put(scanBuffer[:initialSSEScanBufferSize])
	}()

	state := thinkingScanState{inThinking: isThinking}
	hasContent := false

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.HasPrefix(line, sseDataPrefix) {
			slog.Debug("skipping non-data line", "line", string(line))
			continue
		}

		payload := bytes.TrimSpace(line[len(sseDataPrefix):])
		if len(payload) == 0 {
			continue
		}

		var evt SSEEvent
		if err := json.Unmarshal(payload, &evt); err != nil {
			slog.Debug("failed to parse SSE event", "raw", string(payload), "error", err)
			continue
		}
		slog.Debug("parsed SSE event", "type", evt.Type, "content_preview", truncateString(evt.Content, 100))

		// Handle error events from Fireworks
		if evt.Type == "error" {
			message := evt.Error
			if message == "" {
				message = evt.Content
			}
			slog.Error("received error event from upstream", "error", message)
			if message == "" {
				message = "unknown upstream error"
			}
			return hasContent, fmt.Errorf("upstream error: %s", message)
		}

		if evt.Type == "done" {
			if isThinking && !showThinking && !state.separatorSeen && !state.tagSeen && !state.visibleEmitted && state.hiddenBuffer.Len() > 0 {
				if !emitVisibleSSEContent(state.hiddenBuffer.String(), &state, onEvent) {
					return hasContent, nil
				}
			}
			if !onEvent(sseContentEvent{Type: "done"}) {
				return hasContent, nil
			}
			break
		}

		if evt.Content == "" {
			continue
		}

		if evt.Content == thinkingSeparator {
			if isThinking {
				state.inThinking = false
				state.separatorSeen = true
				state.hiddenBuffer.Reset()
				if showThinking && !onEvent(sseContentEvent{Type: "thinking_separator"}) {
					return hasContent, nil
				}
			}
			continue
		}

		if isThinking && (state.inThinkTag || strings.Contains(evt.Content, thinkingTagOpen) || strings.Contains(evt.Content, thinkingTagClose)) {
			if !handleTaggedThinkingContent(evt.Content, showThinking, &state, onEvent) {
				return hasContent, nil
			}
			hasContent = hasContent || state.visibleEmitted
			continue
		}

		if isThinking && state.inThinking {
			if showThinking {
				if !emitVisibleSSEContent(evt.Content, &state, onEvent) {
					return hasContent, nil
				}
				hasContent = true
			} else {
				state.hiddenBuffer.WriteString(evt.Content)
			}
			continue
		}

		if !emitVisibleSSEContent(evt.Content, &state, onEvent) {
			return hasContent, nil
		}
		hasContent = true
	}

	return hasContent, scanner.Err()
}

// truncateString truncates a string to maxLen and adds "..." if truncated
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func getPooledJSONBuffer() *bytes.Buffer {
	buf := jsonBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

func putPooledJSONBuffer(buf *bytes.Buffer) {
	if buf.Cap() > pooledJSONBufferLimit {
		return
	}
	jsonBufferPool.Put(buf)
}

func writeSSEChunk(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}

	buf := getPooledJSONBuffer()
	defer putPooledJSONBuffer(buf)

	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")

	_, err = w.Write(buf.Bytes())
	return err
}
