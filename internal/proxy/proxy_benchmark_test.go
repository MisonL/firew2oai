package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

func BenchmarkMessagesToPrompt(b *testing.B) {
	msgs := []ChatMessage{
		{Role: "system", Content: "You are a precise assistant."},
		{Role: "user", Content: strings.Repeat("hello ", 32)},
		{Role: "assistant", Content: strings.Repeat("world ", 32)},
		{Role: "user", Content: strings.Repeat("follow-up ", 16)},
	}

	b.Run("optimized", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = messagesToPrompt(msgs)
		}
	})

	b.Run("legacy", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = legacyMessagesToPrompt(msgs)
		}
	})
}

func BenchmarkScanSSEEvents(b *testing.B) {
	sse := strings.Repeat("data: {\"type\":\"content\",\"content\":\"hello world\"}\n", 32) +
		"data: {\"type\":\"done\",\"content\":\"\"}\n"

	b.Run("optimized", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			reader := strings.NewReader(sse)
			if _, err := scanSSEEvents(reader, false, false, func(evt sseContentEvent) bool {
				return true
			}); err != nil {
				b.Fatalf("scanSSEEvents error: %v", err)
			}
		}
	})

	b.Run("legacy", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			reader := strings.NewReader(sse)
			if err := legacyScanSSEEvents(reader, false, false, func(evt sseContentEvent) bool {
				return true
			}); err != nil {
				b.Fatalf("legacyScanSSEEvents error: %v", err)
			}
		}
	})
}

func BenchmarkWriteSSEChunk(b *testing.B) {
	chunk := StreamChunk{
		ID:      "chatcmpl-bench",
		Object:  "chat.completion.chunk",
		Created: 1710000000,
		Model:   "deepseek-v3p2",
		Choices: []StreamChoice{
			{Index: 0, Delta: StreamDelta{Content: strings.Repeat("payload", 8)}},
		},
	}

	b.Run("optimized", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := writeSSEChunk(io.Discard, chunk); err != nil {
				b.Fatalf("writeSSEChunk error: %v", err)
			}
		}
	})

	b.Run("legacy", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := io.Discard.Write(legacySSEChunk(chunk)); err != nil {
				b.Fatalf("legacySSEChunk error: %v", err)
			}
		}
	})
}

func legacyMessagesToPrompt(messages []ChatMessage) string {
	var parts []string
	for _, msg := range messages {
		switch msg.Role {
		case "system":
			parts = append(parts, "System: "+msg.Content)
		case "user":
			parts = append(parts, "User: "+msg.Content)
		case "assistant":
			parts = append(parts, "Assistant: "+msg.Content)
		default:
			parts = append(parts, msg.Content)
		}
	}
	return strings.Join(parts, "\n")
}

func legacyScanSSEEvents(reader io.Reader, isThinking, showThinking bool, onEvent func(sseContentEvent) bool) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	inThinking := isThinking

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		jsonStr := strings.TrimSpace(line[5:])
		if jsonStr == "" {
			continue
		}

		var evt SSEEvent
		if err := json.Unmarshal([]byte(jsonStr), &evt); err != nil {
			continue
		}

		if evt.Type == "done" {
			if !onEvent(sseContentEvent{Type: "done"}) {
				return nil
			}
			break
		}

		if evt.Content == "" {
			continue
		}

		if evt.Content == thinkingSeparator {
			if isThinking {
				inThinking = false
				if showThinking && !onEvent(sseContentEvent{Type: "thinking_separator"}) {
					return nil
				}
			}
			continue
		}

		if isThinking && inThinking && !showThinking {
			continue
		}

		if !onEvent(sseContentEvent{Type: "content", Content: evt.Content}) {
			return nil
		}
	}

	return scanner.Err()
}

func legacySSEChunk(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		return []byte("data: {}\n\n")
	}
	return []byte(fmt.Sprintf("data: %s\n\n", data))
}
