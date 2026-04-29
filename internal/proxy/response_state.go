package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

const defaultResponseStoreEntries = 1024

type storedResponse struct {
	owner        string
	response     ResponsesResponse
	requestItems []json.RawMessage
	historyItems []json.RawMessage
}

type responseStore struct {
	mu         sync.RWMutex
	entries    map[string]storedResponse
	insertions []string
	maxEntries int
}

func newResponseStore(maxEntries int) *responseStore {
	if maxEntries <= 0 {
		maxEntries = defaultResponseStoreEntries
	}
	return &responseStore{
		entries:    make(map[string]storedResponse),
		insertions: make([]string, 0, maxEntries),
		maxEntries: maxEntries,
	}
}

func (s *responseStore) put(owner string, response ResponsesResponse, requestItems []json.RawMessage, historyItems []json.RawMessage) {
	if s == nil || response.ID == "" || owner == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, exists := s.entries[response.ID]; exists {
		if existing.owner != owner {
			return
		}
	} else {
		s.insertions = append(s.insertions, response.ID)
	}
	s.entries[response.ID] = storedResponse{
		owner:        owner,
		response:     response,
		requestItems: cloneRawItems(requestItems),
		historyItems: cloneRawItems(historyItems),
	}
	s.evictLocked()
}

func (s *responseStore) get(owner string, id string) (storedResponse, bool) {
	if s == nil || owner == "" || id == "" {
		return storedResponse{}, false
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.entries[id]
	if !ok {
		return storedResponse{}, false
	}
	if entry.owner != owner {
		return storedResponse{}, false
	}
	entry.requestItems = cloneRawItems(entry.requestItems)
	entry.historyItems = cloneRawItems(entry.historyItems)
	return entry, true
}

func responseOwnerFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	return responseOwnerFromToken(auth[7:])
}

func responseOwnerFromToken(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *responseStore) evictLocked() {
	for len(s.entries) > s.maxEntries && len(s.insertions) > 0 {
		oldest := s.insertions[0]
		copy(s.insertions, s.insertions[1:])
		s.insertions = s.insertions[:len(s.insertions)-1]
		delete(s.entries, oldest)
	}
}

func cloneMessages(messages []ChatMessage) []ChatMessage {
	if len(messages) == 0 {
		return nil
	}
	cloned := make([]ChatMessage, len(messages))
	copy(cloned, messages)
	return cloned
}

func cloneRawItems(items []json.RawMessage) []json.RawMessage {
	if len(items) == 0 {
		return nil
	}
	cloned := make([]json.RawMessage, len(items))
	for i, item := range items {
		if len(item) == 0 {
			cloned[i] = append(json.RawMessage{}, item...)
			continue
		}
		cloned[i] = append(json.RawMessage(nil), item...)
	}
	return cloned
}
