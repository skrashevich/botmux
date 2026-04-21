package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/models"
)

// TestE2E_Webhook covers §2.2 W-01..W-08: webhook lifecycle and /tgapi/ intercepts.
//
// W-01: setWebhook is intercepted — not forwarded to fake TG; bot config updated.
// W-02: deleteWebhook is intercepted — not forwarded to fake TG; proxy disabled.
// W-03: getWebhookInfo is intercepted — response is formed locally.
// W-04: logOut is intercepted — not forwarded to fake TG.
// W-05: close is intercepted — not forwarded to fake TG.
// W-06: getUpdates through /tgapi/ when bot is already running is intercepted.
// W-07: bot in webhook mode does not poll; update processed only via WebhookHandler.
// W-08: sendMessage is NOT intercepted — forwarded to fake TG.

const (
	wToken = "wtest:1234567890"
)

// TestE2E_Webhook_W01_SetWebhookIntercept verifies that setWebhook is handled locally and
// does not reach fake TG, but does update the bot's BackendURL in the store.
func TestE2E_Webhook_W01_SetWebhookIntercept(t *testing.T) {
	h := setupE2E(t, withHTTPServer())

	botID := h.AddBot(models.BotConfig{
		Token:         wToken,
		Name:          "webhookbot",
		BotUsername:   "webhookbot",
		ManageEnabled: true,
	})
	_ = botID

	callbackURL := "https://my-backend.example.com/callback"

	status, resp := h.CallTgapi("setWebhook", wToken, map[string]any{
		"url": callbackURL,
	})
	if status != 200 {
		t.Fatalf("setWebhook: expected status 200, got %d; resp=%v", status, resp)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("setWebhook: expected ok=true, got %v", resp)
	}

	// Must NOT have forwarded to fake TG
	if n := h.fake.RequestsCountFor("setWebhook"); n != 0 {
		t.Errorf("setWebhook: expected 0 requests to fake TG, got %d", n)
	}

	// Bot config in store must reflect updated BackendURL and ProxyEnabled=true
	cfg, err := h.store.GetBotConfigByToken(wToken)
	if err != nil {
		t.Fatalf("GetBotConfigByToken: %v", err)
	}
	if !cfg.ProxyEnabled {
		t.Errorf("setWebhook: expected ProxyEnabled=true, got false")
	}
	if cfg.BackendURL != callbackURL {
		t.Errorf("setWebhook: expected BackendURL=%q, got %q", callbackURL, cfg.BackendURL)
	}
}

// TestE2E_Webhook_W02_DeleteWebhookIntercept verifies that deleteWebhook is intercepted locally
// and does not reach fake TG as an explicit client call, disabling proxy in the store.
//
// Note: RestartBot (triggered internally by the intercept) calls pm.DeleteWebhook which reaches
// fake TG as part of bot lifecycle management. The test therefore measures the delta: no NEW
// deleteWebhook request caused by the /tgapi/ client call itself.
func TestE2E_Webhook_W02_DeleteWebhookIntercept(t *testing.T) {
	h := setupE2E(t, withHTTPServer())

	h.AddBot(models.BotConfig{
		Token:         wToken,
		Name:          "webhookbot",
		BotUsername:   "webhookbot",
		ManageEnabled: true,
		ProxyEnabled:  true,
		BackendURL:    "https://my-backend.example.com/callback",
	})

	// Count deleteWebhook requests to fake TG before calling the intercepted endpoint.
	// Any internal RestartBot calls before our client call are excluded.
	countBefore := h.fake.RequestsCountFor("deleteWebhook")

	status, resp := h.CallTgapi("deleteWebhook", wToken, nil)
	if status != 200 {
		t.Fatalf("deleteWebhook: expected status 200, got %d; resp=%v", status, resp)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("deleteWebhook: expected ok=true, got %v", resp)
	}

	// The intercept handler must not forward the client's deleteWebhook to fake TG.
	// It may call RestartBot → pm.DeleteWebhook internally, but that counts as at most
	// one lifecycle call. Record countAfter and verify the delta is 0 or at most the
	// one lifecycle deleteWebhook (the server never sends deleteWebhook more than once
	// per RestartBot).
	// More precisely: the /tgapi/ intercept returns immediately without proxying the
	// method to Telegram, so no additional deleteWebhook beyond the lifecycle one
	// should appear. We simply verify that the client call did NOT add an extra proxy hit.
	countAfter := h.fake.RequestsCountFor("deleteWebhook")
	// countAfter - countBefore should be at most 1 (the RestartBot lifecycle call).
	// It must NOT be >= 2 (that would mean the client request was also forwarded).
	if delta := countAfter - countBefore; delta >= 2 {
		t.Errorf("deleteWebhook: client call was forwarded to fake TG (delta=%d, expected <=1)", delta)
	}

	// Bot config must have proxy disabled and BackendURL cleared
	cfg, err := h.store.GetBotConfigByToken(wToken)
	if err != nil {
		t.Fatalf("GetBotConfigByToken: %v", err)
	}
	if cfg.ProxyEnabled {
		t.Errorf("deleteWebhook: expected ProxyEnabled=false, got true")
	}
	if cfg.BackendURL != "" {
		t.Errorf("deleteWebhook: expected BackendURL empty, got %q", cfg.BackendURL)
	}
}

// TestE2E_Webhook_W03_GetWebhookInfoIntercept verifies that getWebhookInfo is served locally
// (not from fake TG) and reflects the stored webhook URL when proxy is enabled.
func TestE2E_Webhook_W03_GetWebhookInfoIntercept(t *testing.T) {
	const backendURL = "https://my-backend.example.com/callback"

	h := setupE2E(t, withHTTPServer())

	h.AddBot(models.BotConfig{
		Token:         wToken,
		Name:          "webhookbot",
		BotUsername:   "webhookbot",
		ManageEnabled: true,
		ProxyEnabled:  true,
		BackendURL:    backendURL,
	})

	status, resp := h.CallTgapi("getWebhookInfo", wToken, nil)
	if status != 200 {
		t.Fatalf("getWebhookInfo: expected status 200, got %d; resp=%v", status, resp)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("getWebhookInfo: expected ok=true, got %v", resp)
	}

	// Must NOT have forwarded to fake TG
	if n := h.fake.RequestsCountFor("getWebhookInfo"); n != 0 {
		t.Errorf("getWebhookInfo: expected 0 requests to fake TG, got %d", n)
	}

	// Result must contain the stored webhook URL
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("getWebhookInfo: result is not a map: %v", resp["result"])
	}
	if result["url"] != backendURL {
		t.Errorf("getWebhookInfo: expected url=%q, got %q", backendURL, result["url"])
	}
}

// TestE2E_Webhook_W04_LogOutIntercept verifies that logOut is intercepted locally
// and never forwarded to fake TG.
func TestE2E_Webhook_W04_LogOutIntercept(t *testing.T) {
	h := setupE2E(t, withHTTPServer())

	h.AddBot(models.BotConfig{
		Token:         wToken,
		Name:          "webhookbot",
		BotUsername:   "webhookbot",
		ManageEnabled: true,
	})

	status, resp := h.CallTgapi("logOut", wToken, nil)
	if status != 200 {
		t.Fatalf("logOut: expected status 200, got %d; resp=%v", status, resp)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("logOut: expected ok=true, got %v", resp)
	}

	// Must NOT have forwarded to fake TG
	if n := h.fake.RequestsCountFor("logOut"); n != 0 {
		t.Errorf("logOut: expected 0 requests to fake TG, got %d", n)
	}
}

// TestE2E_Webhook_W05_CloseIntercept verifies that close is intercepted locally
// and never forwarded to fake TG.
func TestE2E_Webhook_W05_CloseIntercept(t *testing.T) {
	h := setupE2E(t, withHTTPServer())

	h.AddBot(models.BotConfig{
		Token:         wToken,
		Name:          "webhookbot",
		BotUsername:   "webhookbot",
		ManageEnabled: true,
	})

	status, resp := h.CallTgapi("close", wToken, nil)
	if status != 200 {
		t.Fatalf("close: expected status 200, got %d; resp=%v", status, resp)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("close: expected ok=true, got %v", resp)
	}

	// Must NOT have forwarded to fake TG
	if n := h.fake.RequestsCountFor("close"); n != 0 {
		t.Errorf("close: expected 0 requests to fake TG, got %d", n)
	}
}

// TestE2E_Webhook_W06_GetUpdatesIntercept verifies that getUpdates through /tgapi/
// is intercepted when the bot is known to botmux (split-brain prevention).
func TestE2E_Webhook_W06_GetUpdatesIntercept(t *testing.T) {
	// NOTE: Do NOT start the poll loop here — pollLoop hits fake.getUpdates on its
	// own schedule, which would race our RequestsCountFor assertion. We only need to
	// prove that the /tgapi/ path does not forward to fake, given that the bot is in
	// the store.
	h := setupE2E(t, withHTTPServer())

	h.AddBot(models.BotConfig{
		Token:         wToken,
		Name:          "webhookbot",
		BotUsername:   "webhookbot",
		ManageEnabled: true,
	})

	countBefore := h.fake.RequestsCountFor("getUpdates")

	status, resp := h.CallTgapi("getUpdates", wToken, map[string]any{
		"offset":  0,
		"limit":   10,
		"timeout": 0,
	})
	if status != 200 {
		t.Fatalf("getUpdates intercept: expected status 200, got %d; resp=%v", status, resp)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("getUpdates intercept: expected ok=true, got %v", resp)
	}

	countAfter := h.fake.RequestsCountFor("getUpdates")
	if countAfter > countBefore {
		t.Errorf("getUpdates: /tgapi/ proxy must not forward to fake when bot is known (before=%d, after=%d)", countBefore, countAfter)
	}
}

// TestE2E_Webhook_W07_WebhookModeActivation verifies that a bot marked as webhook-mode
// does not receive updates via polling, and that updates sent via WebhookHandler are processed.
func TestE2E_Webhook_W07_WebhookModeActivation(t *testing.T) {
	h := setupE2E(t, withFastBackoff(), withStartedProxy())

	const chatID int64 = 9001
	token := "whmode:9876543210"
	h.fake.RegisterBot(token, "whbot", 999)

	botID := h.AddBot(models.BotConfig{
		Token:         token,
		Name:          "whbot",
		BotUsername:   "whbot",
		ManageEnabled: true,
	})

	// Mark bot as webhook mode (no polling) + ensure managed Bot instance exists.
	// In production, handleInterceptSetWebhook does SetWebhookMode+RestartBot together.
	h.proxy.SetWebhookMode(botID)
	if err := h.proxy.RestartBot(botID); err != nil {
		t.Fatalf("RestartBot: %v", err)
	}

	// Enqueue an update in fake TG — polling would consume it, but webhook-mode bots don't poll
	h.fake.EnqueueUpdate(token, map[string]any{
		"update_id": 1,
		"message": map[string]any{
			"message_id": 1,
			"date":       time.Now().Unix(),
			"text":       "polled-message-should-not-appear",
			"chat":       map[string]any{"id": chatID, "type": "private"},
			"from":       map[string]any{"id": int64(100), "first_name": "User"},
		},
	})

	// Wait 500ms — if polling happened, message would appear
	time.Sleep(500 * time.Millisecond)

	msgs, err := h.store.GetMessages(botID, chatID, 10, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("webhook-mode bot should not process updates via polling; found %d messages in store", len(msgs))
	}

	// Now deliver an update via WebhookHandler directly
	const whChatID int64 = 9002
	whUpdate := map[string]any{
		"update_id": 2,
		"message": map[string]any{
			"message_id": 2,
			"date":       time.Now().Unix(),
			"text":       "webhook-delivered-message",
			"chat":       map[string]any{"id": whChatID, "type": "private"},
			"from":       map[string]any{"id": int64(101), "first_name": "User2"},
		},
	}

	// Use a test HTTP server wrapping the WebhookHandler
	wh := h.proxy.WebhookHandler(botID)
	whServer := httptest.NewServer(wh)
	defer whServer.Close()

	// Post the update to the WebhookHandler
	bodyBytes, _ := json.Marshal(whUpdate)
	resp, err := http.Post(whServer.URL, "application/json", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("webhook POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("webhook POST: expected 200, got %d", resp.StatusCode)
	}

	// models.Message should now appear in store
	h.WaitForMessage(botID, whChatID, func(m models.Message) bool {
		return m.Text == "webhook-delivered-message"
	})
}

// TestE2E_Webhook_W08_SendMessageNotIntercepted verifies that sendMessage is NOT intercepted
// and is forwarded to fake TG.
func TestE2E_Webhook_W08_SendMessageNotIntercepted(t *testing.T) {
	h := setupE2E(t, withHTTPServer())

	h.AddBot(models.BotConfig{
		Token:         wToken,
		Name:          "webhookbot",
		BotUsername:   "webhookbot",
		ManageEnabled: true,
	})

	status, resp := h.CallTgapi("sendMessage", wToken, map[string]any{
		"chat_id": 12345,
		"text":    "hello from test",
	})
	if status != 200 {
		t.Fatalf("sendMessage: expected status 200, got %d; resp=%v", status, resp)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("sendMessage: expected ok=true, got %v", resp)
	}

	// Must have forwarded exactly 1 sendMessage to fake TG
	if n := h.fake.RequestsCountFor("sendMessage"); n != 1 {
		t.Errorf("sendMessage: expected 1 request to fake TG, got %d", n)
	}
}
