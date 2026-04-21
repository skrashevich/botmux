package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/bot"
	"github.com/skrashevich/botmux/internal/bridge"
	"github.com/skrashevich/botmux/internal/models"
)

const testSlackSigningSecret = "test_signing_secret_32byteslong!!"

// signSlackRequest computes the X-Slack-Signature header value for a given body and timestamp.
func signSlackRequest(secret, timestamp string, body []byte) string {
	baseString := fmt.Sprintf("v0:%s:%s", timestamp, string(body))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(baseString))
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

// slackEventBody builds a minimal JSON body for a Slack event_callback with a message event.
func slackEventBody(channel, userID, text, ts string) []byte {
	inner := map[string]any{
		"type":    "message",
		"subtype": "",
		"channel": channel,
		"user":    userID,
		"text":    text,
		"ts":      ts,
	}
	innerBytes, _ := json.Marshal(inner)
	outer := map[string]any{
		"type":    "event_callback",
		"team_id": "T12345",
		"event":   json.RawMessage(innerBytes),
	}
	b, _ := json.Marshal(outer)
	return b
}

// postBridgeIncoming POSTs to /bridge/{id}/incoming with optional Slack signature headers.
func postBridgeIncoming(t *testing.T, serverURL string, bridgeID int64, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	url := fmt.Sprintf("%s/bridge/%d/incoming", serverURL, bridgeID)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("postBridgeIncoming: NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("postBridgeIncoming: Do: %v", err)
	}
	return resp
}

// addSlackBridge creates a Slack bridge in the store and loads it into the bridge manager.
// Returns the assigned bridge ID.
func addSlackBridge(t *testing.T, h *e2eHarness, botID int64, slackAPIBaseURL, signingSecret string) int64 {
	t.Helper()
	cfg := bridge.SlackConfig{
		BotToken:      "xoxb-test-token",
		SigningSecret: signingSecret,
		APIBaseURL:    slackAPIBaseURL,
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("addSlackBridge: marshal config: %v", err)
	}
	bridgeID, err := h.store.AddBridge(models.BridgeConfig{
		Name:        "test-slack-bridge",
		Protocol:    "slack",
		LinkedBotID: botID,
		Config:      string(cfgJSON),
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("addSlackBridge: AddBridge: %v", err)
	}
	// Load into the bridge manager in-memory cache
	h.bridge.Reload(bridgeID)
	return bridgeID
}

// TestE2E_BridgeSlack runs all Slack bridge E2E scenarios.
func TestE2E_BridgeSlack(t *testing.T) {
	t.Run("B-01_URLVerification", testSlackB01URLVerification)
	t.Run("B-02_HMACSignatureValidation", testSlackB02HMACSignature)
	t.Run("B-05_IncomingSlackEventFlow", testSlackB05IncomingEventFlow)
}

// B-01: POST /bridge/{id}/incoming with url_verification challenge.
// Slack sends this when first configuring the Events API URL.
// Expected: 200, response body contains the challenge value.
func testSlackB01URLVerification(t *testing.T) {
	fs := newFakeSlack(t)
	h := setupE2E(t, withHTTPServer(), withBridge(), withFakeSlackReal(fs))

	token := "b01bot:1234567890"
	botID := h.AddBot(models.BotConfig{
		Token:         token,
		Name:          "b01bot",
		BotUsername:   "b01bot",
		ManageEnabled: true,
	})
	h.fake.RegisterBot(token, "b01bot", 201)

	bridgeID := addSlackBridge(t, h, botID, fs.URL(), testSlackSigningSecret)

	// url_verification payloads have no signing requirement — but we still sign it
	// to make the test realistic. The server skips sig check for url_verification
	// ... actually HandleSlackEvent verifies signature before parsing type.
	// So we must provide a valid signature.
	body := []byte(`{"type":"url_verification","challenge":"abc123xyz"}`)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signSlackRequest(testSlackSigningSecret, ts, body)

	resp := postBridgeIncoming(t, h.ts.URL, bridgeID, body, map[string]string{
		"X-Slack-Request-Timestamp": ts,
		"X-Slack-Signature":         sig,
	})
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("B-01: expected status 200, got %d", resp.StatusCode)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("B-01: read body: %v", err)
	}

	// Response should contain the challenge value
	if !strings.Contains(string(respBody), "abc123xyz") {
		t.Errorf("B-01: response body does not contain challenge 'abc123xyz': %s", respBody)
	}
}

// B-02: Signature validation — invalid sig returns 401/403, valid sig returns 200.
func testSlackB02HMACSignature(t *testing.T) {
	fs := newFakeSlack(t)
	h := setupE2E(t, withHTTPServer(), withBridge(), withFakeSlackReal(fs))

	token := "b02bot:1234567890"
	botID := h.AddBot(models.BotConfig{
		Token:         token,
		Name:          "b02bot",
		BotUsername:   "b02bot",
		ManageEnabled: true,
	})
	h.fake.RegisterBot(token, "b02bot", 202)

	bridgeID := addSlackBridge(t, h, botID, fs.URL(), testSlackSigningSecret)

	body := slackEventBody("C9999", "U001", "hello", "111.222")
	ts := fmt.Sprintf("%d", time.Now().Unix())

	// --- Invalid signature ---
	respInvalid := postBridgeIncoming(t, h.ts.URL, bridgeID, body, map[string]string{
		"X-Slack-Request-Timestamp": ts,
		"X-Slack-Signature":         "v0=invalidsignaturehex",
	})
	defer respInvalid.Body.Close()
	_, _ = io.ReadAll(respInvalid.Body)

	if respInvalid.StatusCode != 401 && respInvalid.StatusCode != 403 {
		t.Errorf("B-02: expected 401 or 403 for invalid signature, got %d", respInvalid.StatusCode)
	}

	// --- Valid signature ---
	validSig := signSlackRequest(testSlackSigningSecret, ts, body)
	respValid := postBridgeIncoming(t, h.ts.URL, bridgeID, body, map[string]string{
		"X-Slack-Request-Timestamp": ts,
		"X-Slack-Signature":         validSig,
	})
	defer respValid.Body.Close()
	_, _ = io.ReadAll(respValid.Body)

	if respValid.StatusCode != 200 {
		t.Errorf("B-02: expected 200 for valid signature, got %d", respValid.StatusCode)
	}
}

// B-05: Full flow: Slack event -> resolveSlackUsername -> inject update -> store -> outgoing via fake Slack.
//
// Setup:
//  1. Slack bridge with APIBaseURL = fakeSlack.URL() and signing_secret = testSlackSigningSecret
//  2. Bot with ManageEnabled=true and onMessageSent hook from bridge
//
// Flow:
//  1. POST Slack event (message) with valid HMAC -> HandleSlackEvent
//  2. resolveSlackUsername calls fake Slack /users.info
//  3. Synthetic update injected via processUpdate -> store.SaveMessage
//  4. onMessageSent hook fires -> NotifyOutgoing -> notifySlackOutgoing -> fake Slack /chat.postMessage
func testSlackB05IncomingEventFlow(t *testing.T) {
	fs := newFakeSlack(t)
	fs.SetUsername("U_alice", "Alice Smith")

	h := setupE2E(t, withHTTPServer(), withBridge(), withFakeSlackReal(fs))

	token := "b05bot:1234567890"
	botID := h.AddBot(models.BotConfig{
		Token:         token,
		Name:          "b05bot",
		BotUsername:   "b05bot",
		ManageEnabled: true,
	})
	h.fake.RegisterBot(token, "b05bot", 205)

	bridgeID := addSlackBridge(t, h, botID, fs.URL(), testSlackSigningSecret)

	// Ensure the bridge manager has a managed bot with the outgoing hook installed.
	managedBot, err := bot.NewBot(token, h.store, botID, h.fake.URL())
	if err != nil {
		t.Fatalf("B-05: NewBot: %v", err)
	}
	h.bridge.InstallHookOnBot(managedBot)
	h.proxy.RegisterManagedBot(botID, managedBot)

	// --- Send Slack event ---
	const slackChannel = "C_general"
	const msgText = "hello from slack"
	const msgTS = "999.111"

	body := slackEventBody(slackChannel, "U_alice", msgText, msgTS)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signSlackRequest(testSlackSigningSecret, ts, body)

	resp := postBridgeIncoming(t, h.ts.URL, bridgeID, body, map[string]string{
		"X-Slack-Request-Timestamp": ts,
		"X-Slack-Signature":         sig,
	})
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		t.Fatalf("B-05: expected 200, got %d", resp.StatusCode)
	}

	// --- Verify users.info was called ---
	h.Eventually(func() bool {
		return fs.RequestsCountFor("users.info") >= 1
	}, 2*time.Second, "users.info call to fake Slack")

	usersInfoReqs := fs.RequestsFor("users.info")
	if len(usersInfoReqs) == 0 {
		t.Fatal("B-05: expected at least one users.info request to fake Slack")
	}

	// Verify the user ID is present in the query
	gotUserID := usersInfoReqs[0].query.Get("user")
	if gotUserID != "U_alice" {
		t.Errorf("B-05: users.info called with user=%q, want U_alice", gotUserID)
	}

	// --- Verify the message was stored with the resolved username ---
	// Determine the synthetic chat ID the bridge would have assigned
	tgChatID, err := h.store.GetBridgeChatMapping(bridgeID, slackChannel)
	if err != nil || tgChatID == 0 {
		t.Fatalf("B-05: bridge chat mapping not found: err=%v chatID=%d", err, tgChatID)
	}

	var stored models.Message
	h.Eventually(func() bool {
		msgs, err := h.store.GetMessages(botID, tgChatID, 10, 0)
		if err != nil {
			return false
		}
		for _, m := range msgs {
			if m.Text == msgText {
				stored = m
				return true
			}
		}
		return false
	}, 2*time.Second, "incoming Slack message stored")

	if stored.Text != msgText {
		t.Errorf("B-05: stored message text=%q, want %q", stored.Text, msgText)
	}

	// --- Bot reply: call sendMessage on the bot to trigger onMessageSent -> fake Slack ---
	// Use the store's synthetic chat ID to send a reply
	const replyText = "bot reply to slack"
	if err := managedBot.SendMessage(tgChatID, replyText); err != nil {
		t.Fatalf("B-05: SendMessage: %v", err)
	}

	// --- Verify chat.postMessage was called on fake Slack ---
	h.Eventually(func() bool {
		return fs.RequestsCountFor("chat.postMessage") >= 1
	}, 2*time.Second, "chat.postMessage call to fake Slack")

	postMsgReqs := fs.RequestsFor("chat.postMessage")
	if len(postMsgReqs) == 0 {
		t.Fatal("B-05: expected at least one chat.postMessage request to fake Slack")
	}

	// Verify the body contains the reply text
	var postMsgBody struct {
		Channel string `json:"channel"`
		Text    string `json:"text"`
	}
	if err := json.Unmarshal(postMsgReqs[0].body, &postMsgBody); err != nil {
		t.Fatalf("B-05: unmarshal chat.postMessage body: %v", err)
	}
	if postMsgBody.Text != replyText {
		t.Errorf("B-05: chat.postMessage text=%q, want %q", postMsgBody.Text, replyText)
	}
	if postMsgBody.Channel != slackChannel {
		t.Errorf("B-05: chat.postMessage channel=%q, want %q", postMsgBody.Channel, slackChannel)
	}
}
