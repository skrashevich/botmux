package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// slackRequest records a single request received by the fake Slack server.
type slackRequest struct {
	method    string // "chat.postMessage", "users.info", etc.
	body      []byte
	query     url.Values
	timestamp time.Time
}

// fakeSlack is an httptest-based fake Slack API server.
type fakeSlack struct {
	t         *testing.T
	server    *httptest.Server
	mu        sync.Mutex
	requests  []slackRequest
	usernames map[string]string // userID -> display_name
}

// newFakeSlack creates and starts a fake Slack HTTP server.
// Handles:
//   - POST /chat.postMessage -> {"ok":true,"channel":"C1","ts":"123.456","message":{}}
//   - GET /users.info?user=X -> {"ok":true,"user":{"id":X,"name":"username","real_name":"Real Name"}}
//   - everything else -> {"ok":false,"error":"unknown_method"}
func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	s := &fakeSlack{
		t:         t,
		usernames: make(map[string]string),
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.route))
	t.Cleanup(s.server.Close)
	return s
}

// URL returns the base URL of the fake Slack server.
func (s *fakeSlack) URL() string { return s.server.URL }

// SetUsername registers a deterministic display name for a Slack user ID.
func (s *fakeSlack) SetUsername(userID, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usernames[userID] = name
}

// Requests returns a snapshot of all recorded requests.
func (s *fakeSlack) Requests() []slackRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]slackRequest, len(s.requests))
	copy(out, s.requests)
	return out
}

// RequestsFor returns all recorded requests matching a method name.
func (s *fakeSlack) RequestsFor(method string) []slackRequest {
	var out []slackRequest
	for _, r := range s.Requests() {
		if r.method == method {
			out = append(out, r)
		}
	}
	return out
}

// RequestsCountFor returns the number of recorded requests for a given method.
func (s *fakeSlack) RequestsCountFor(method string) int {
	return len(s.RequestsFor(method))
}

// route is the main HTTP handler for the fake Slack server.
// The Slack API uses paths like /chat.postMessage and /users.info.
func (s *fakeSlack) route(w http.ResponseWriter, r *http.Request) {
	// Strip leading slash to get method name, e.g. "/chat.postMessage" -> "chat.postMessage"
	method := r.URL.Path
	if len(method) > 0 && method[0] == '/' {
		method = method[1:]
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Errorf("fakeSlack: read body: %v", err)
	}

	rec := slackRequest{
		method:    method,
		body:      body,
		query:     r.URL.Query(),
		timestamp: time.Now(),
	}

	s.mu.Lock()
	s.requests = append(s.requests, rec)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")

	switch method {
	case "chat.postMessage":
		s.handlePostMessage(w, r, body)
	case "users.info":
		s.handleUsersInfo(w, r)
	default:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":false,"error":"unknown_method"}`))
	}
}

func (s *fakeSlack) handlePostMessage(w http.ResponseWriter, _ *http.Request, body []byte) {
	// Parse channel from JSON body
	var req struct {
		Channel string `json:"channel"`
		Text    string `json:"text"`
	}
	_ = json.Unmarshal(body, &req)
	channel := req.Channel
	if channel == "" {
		channel = "C_unknown"
	}

	resp := map[string]any{
		"ok":      true,
		"channel": channel,
		"ts":      "123.456",
		"message": map[string]any{
			"text": req.Text,
			"ts":   "123.456",
		},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *fakeSlack) handleUsersInfo(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user")

	s.mu.Lock()
	displayName := s.usernames[userID]
	s.mu.Unlock()

	if displayName == "" {
		displayName = "user_" + userID
	}

	resp := map[string]any{
		"ok": true,
		"user": map[string]any{
			"id":        userID,
			"name":      displayName,
			"real_name": displayName,
			"profile": map[string]any{
				"display_name": displayName,
				"real_name":    displayName,
			},
		},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// withFakeSlackReal is a functional option that attaches a full fakeSlack
// to the harness and stores it in h.fakeSlk.Server field via the underlying server.
// It does NOT replace withFakeSlack() to avoid conflicts with Phase 4-B.
func withFakeSlackReal(fs *fakeSlack) e2eOpt {
	return func(h *e2eHarness) {
		// The fakeSlack server is already started; store its underlying httptest.Server
		// in the harness so existing helpers that use h.fakeSlk work.
		h.fakeSlk = fs.server
	}
}
