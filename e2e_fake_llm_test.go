package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// llmRequest captures one incoming POST /chat/completions call.
type llmRequest struct {
	Path string
	Body map[string]any
}

// fakeLLM is a minimal OpenAI-compatible chat-completions server for tests.
// It responds to POST /chat/completions with a configurable routing decision.
type fakeLLM struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	requests []llmRequest
	// nextRoute is the JSON object placed inside choices[0].message.content.
	// Defaults to {"target_bot_id":0,"target_chat_id":0,"action":"ignore","reason":"default"}
	nextRoute map[string]any
}

func newFakeLLM(t *testing.T) *fakeLLM {
	t.Helper()
	f := &fakeLLM{
		t: t,
		nextRoute: map[string]any{
			"target_bot_id":  float64(0),
			"target_chat_id": float64(0),
			"action":         "ignore",
			"reason":         "default",
		},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

// URL returns the base URL of the fake server.
func (f *fakeLLM) URL() string { return f.server.URL }

// SetNextRoute configures what routing decision the next /chat/completions call returns.
func (f *fakeLLM) SetNextRoute(r map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextRoute = r
}

// Requests returns a snapshot of all recorded requests.
func (f *fakeLLM) Requests() []llmRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]llmRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

// RequestsCountFor returns the count of requests whose path ends with the given suffix.
// Use "/chat/completions" to count LLM calls.
func (f *fakeLLM) RequestsCountFor(pathSuffix string) int {
	count := 0
	for _, r := range f.Requests() {
		if len(r.Path) >= len(pathSuffix) && r.Path[len(r.Path)-len(pathSuffix):] == pathSuffix {
			count++
		}
	}
	return count
}

func (f *fakeLLM) handle(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("fakeLLM: read body: %v", err)
	}

	var bodyMap map[string]any
	_ = json.Unmarshal(bodyBytes, &bodyMap)

	f.mu.Lock()
	f.requests = append(f.requests, llmRequest{Path: r.URL.Path, Body: bodyMap})
	routeDecision := f.nextRoute
	f.mu.Unlock()

	contentBytes, err := json.Marshal(routeDecision)
	if err != nil {
		f.t.Errorf("fakeLLM: marshal nextRoute: %v", err)
	}

	resp := map[string]any{
		"choices": []map[string]any{
			{
				"message": map[string]any{
					"role":    "assistant",
					"content": string(contentBytes),
				},
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		f.t.Errorf("fakeLLM: encode response: %v", err)
	}
}
