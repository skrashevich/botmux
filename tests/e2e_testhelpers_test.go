package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/bridge"
	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/proxy"
	"github.com/skrashevich/botmux/internal/server"
	"github.com/skrashevich/botmux/internal/store"
)

// e2eHarness wires up a complete in-process test environment.
type e2eHarness struct {
	t       *testing.T
	fake    *fakeTG          // always present — fake Telegram HTTP server
	fakeSlk *httptest.Server // only when withFakeSlack() opt is applied
	fakeLLM *httptest.Server // only when withFakeLLM() opt is applied
	store   *store.Store
	proxy   *proxy.Manager
	server  *server.Server
	bridge  *bridge.Manager  // always created; Start() only when withBridge() is applied
	ts      *httptest.Server // httptest.NewServer(server.BuildMux()); only with withHTTPServer()
	session string           // admin session cookie for /api/* requests
}

// e2eOpt is a functional option applied after the baseline harness is assembled.
type e2eOpt func(*e2eHarness)

// withStartedProxy starts the ProxyManager (launches pollLoop goroutines).
func withStartedProxy() e2eOpt {
	return func(h *e2eHarness) {
		h.proxy.Start()
	}
}

// withHTTPServer wraps server.BuildMux() in an httptest.Server for full round-trip tests.
func withHTTPServer() e2eOpt {
	return func(h *e2eHarness) {
		h.ts = httptest.NewServer(h.server.BuildMux())
		h.t.Cleanup(h.ts.Close)
	}
}

// withFastBackoff sets the ProxyManager retry delays to near-zero for fast error-path tests.
func withFastBackoff() e2eOpt {
	return func(h *e2eHarness) {
		h.proxy.SetRetryDelays(1*time.Millisecond, 10*time.Millisecond)
	}
}

// withFakeSlack attaches a stub httptest.Server representing the Slack API.
// The server returns 200 OK with an empty JSON body by default.
// Tests that need richer behaviour should call h.fakeSlk.Config.Handler = ... afterwards.
func withFakeSlack() e2eOpt {
	return func(h *e2eHarness) {
		h.fakeSlk = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		h.t.Cleanup(h.fakeSlk.Close)
	}
}

// withFakeLLM attaches a stub httptest.Server representing an OpenAI-compatible LLM API.
// Returns a minimal chat-completion response with an empty routing decision by default.
func withFakeLLM() e2eOpt {
	return func(h *e2eHarness) {
		h.fakeLLM = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"choices":[{"message":{"role":"assistant","content":"{\"action\":\"ignore\"}"}}]
			}`))
		}))
		h.t.Cleanup(h.fakeLLM.Close)
	}
}

// withBridge starts the BridgeManager (loads bridges from store).
func withBridge() e2eOpt {
	return func(h *e2eHarness) {
		h.bridge.Start()
	}
}

// setupE2E creates a fresh e2eHarness and applies all provided options.
func setupE2E(t *testing.T, opts ...e2eOpt) *e2eHarness {
	t.Helper()

	h := &e2eHarness{t: t}

	// 1. Fake Telegram server
	h.fake = newFakeTG(t)

	// 2. Store — reuse the newTestStore helper from server_capture_test.go
	dbPath := filepath.Join(t.TempDir(), "e2e.db")
	store, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("setupE2E: NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	h.store = store

	// 3. ProxyManager — point at fake TG
	h.proxy = proxy.NewManager(store, h.fake.URL())
	t.Cleanup(func() { h.proxy.StopAll() })

	// 4. Server — point at fake TG
	h.server = server.NewServer(store, h.proxy)
	h.server.TgAPIBaseURL = h.fake.URL()

	// 5. BridgeManager — always created; Start() only via withBridge()
	h.bridge = bridge.NewManager(store, h.proxy, h.fake.URL())
	h.server.SetBridgeManager(h.bridge)

	// 6. Admin session for API calls — reuse createTestAuth from longpoll_test.go
	h.session = createTestAuth(t, store)

	// 7. Apply options
	for _, opt := range opts {
		opt(h)
	}

	return h
}

// AddBot registers a bot in the fake TG server and adds it to the store.
// Returns the assigned bot ID.
func (h *e2eHarness) AddBot(cfg models.BotConfig) int64 {
	h.t.Helper()

	username := cfg.BotUsername
	if username == "" {
		username = "testbot"
	}

	// Register with fake TG first so getMe works if proxy calls NewBot.
	// Use a deterministic synthetic bot ID derived from the sequential counter.
	h.fake.RegisterBot(cfg.Token, username, 100+int64(len(h.fake.Requests())))

	id, err := h.store.AddBotConfig(cfg)
	if err != nil {
		h.t.Fatalf("AddBot: AddBotConfig: %v", err)
	}
	return id
}

// CallTgapi performs a POST to /tgapi/bot{token}/{method}.
// Requires withHTTPServer() to have been applied.
func (h *e2eHarness) CallTgapi(method, token string, body any) (int, map[string]any) {
	h.t.Helper()
	if h.ts == nil {
		h.t.Fatal("CallTgapi requires withHTTPServer()")
	}

	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("CallTgapi: marshal body: %v", err)
		}
		reqBody = bytes.NewReader(data)
	}

	url := h.ts.URL + "/tgapi/bot" + token + "/" + method
	req, err := http.NewRequest(http.MethodPost, url, reqBody)
	if err != nil {
		h.t.Fatalf("CallTgapi: NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: h.session})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("CallTgapi: Do: %v", err)
	}
	defer resp.Body.Close()

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		h.t.Fatalf("CallTgapi: decode response: %v", err)
	}
	return resp.StatusCode, result
}

// CallMedia performs a GET on /api/media?file_id=&bot_id=.
// Requires withHTTPServer() to have been applied.
func (h *e2eHarness) CallMedia(botID int64, fileID string) (*http.Response, []byte) {
	h.t.Helper()
	if h.ts == nil {
		h.t.Fatal("CallMedia requires withHTTPServer()")
	}

	url := h.ts.URL + fmt.Sprintf("/api/media?bot_id=%d&file_id=%s", botID, fileID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		h.t.Fatalf("CallMedia: NewRequest: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: h.session})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("CallMedia: Do: %v", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("CallMedia: ReadAll: %v", err)
	}
	return resp, data
}

// InjectUpdate calls processUpdate directly — for integration tests that don't need pollLoop.
func (h *e2eHarness) InjectUpdate(botID int64, update map[string]any) {
	h.t.Helper()
	h.proxy.ProcessUpdate(botID, update)
}

// Eventually polls cond until it returns true or timeout elapses, then calls t.Fatalf.
func (h *e2eHarness) Eventually(cond func() bool, timeout time.Duration, msg string) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("Eventually timeout (%s): %s", timeout, msg)
}

// WaitForMessage polls the store until a message matching pred appears, or times out.
func (h *e2eHarness) WaitForMessage(botID, chatID int64, pred func(models.Message) bool) models.Message {
	h.t.Helper()
	var found models.Message
	h.Eventually(func() bool {
		msgs, err := h.store.GetMessages(botID, chatID, 50, 0)
		if err != nil {
			return false
		}
		for _, m := range msgs {
			if pred(m) {
				found = m
				return true
			}
		}
		return false
	}, 1*time.Second, "message matching predicate")
	return found
}

// loadFixture reads a JSON fixture from testdata/tg/ and returns it as map[string]any.
func loadFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "tg", name))
	if err != nil {
		t.Fatalf("loadFixture(%q): %v", name, err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("loadFixture(%q): unmarshal: %v", name, err)
	}
	return out
}

// --- Smoke test ---

func TestE2EHarness_Smoke(t *testing.T) {
	h := setupE2E(t)

	if h.fake == nil {
		t.Fatal("fake nil")
	}
	if h.store == nil {
		t.Fatal("store nil")
	}
	if h.proxy == nil {
		t.Fatal("proxy nil")
	}
	if h.server == nil {
		t.Fatal("server nil")
	}
	if h.bridge == nil {
		t.Fatal("bridge nil")
	}
	if h.server.TgAPIBaseURL != h.fake.URL() {
		t.Errorf("server.TgAPIBaseURL=%q, want %q", h.server.TgAPIBaseURL, h.fake.URL())
	}
}
