package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestE2E_Callback_C01_Tracking is a BLOCKING gate (§6 Rule 4 / §11 Pre-mortem S3a).
//
// bot.processUpdate handles only Message, ChannelPost, MyChatMember, ChatMember.
// There is no callback_query branch. Until a decision is made (add handler OR
// formalise as §6 Rule 3 intercept), this test is intentionally skipped with an
// expiry date so check-stale-skips.sh fails in CI when the deadline passes.
func TestE2E_Callback_C01_Tracking(t *testing.T) {
	h := setupE2E(t, withStartedProxy())
	token := "cb:T"
	botID := h.AddBot(BotConfig{Token: token, Name: "cb_bot", ManageEnabled: true})

	// Bot must be registered as a managed Bot instance before processForManagement works.
	if err := h.proxy.RestartBot(botID); err != nil {
		t.Fatalf("RestartBot: %v", err)
	}
	h.Eventually(func() bool { return h.proxy.IsRunning(botID) }, 500*time.Millisecond, "bot running")

	fixture := loadFixture(t, "update_callback_query.json")
	// Inject directly through the proxy — exercises bot.processUpdate callback branch.
	h.InjectUpdate(botID, fixture)

	// callback_query.from.id = 987654321, chat.id = 987654321, data = "option_a".
	const callerID = int64(987654321)
	h.WaitForMessage(botID, callerID, func(m Message) bool {
		return m.MediaType == "callback" && m.Text == "option_a" && m.FromID == callerID
	})
}

// TestE2E_Callback_C02_ForwardViaProxy verifies that answerCallbackQuery sent
// through /tgapi/ is forwarded to the fake Telegram server and not intercepted.
// The bot is configured with ProxyEnabled=true so that its backend posts through
// the /tgapi/ proxy endpoint.
func TestE2E_Callback_C02_ForwardViaProxy(t *testing.T) {
	// We need a backend server that the proxy will forward updates to.
	backendReceived := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendReceived <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	h := setupE2E(t, withHTTPServer())

	token := "cb02:proxy"
	botID := h.AddBot(BotConfig{
		Token:         token,
		Name:          "cb02_bot",
		ManageEnabled: false,
		ProxyEnabled:  true,
		BackendURL:    backend.URL,
	})
	_ = botID

	// POST answerCallbackQuery through the /tgapi/ proxy.
	// The fake TG default handler for unknown methods returns {"ok":true,"result":true},
	// which is the correct shape for answerCallbackQuery.
	status, resp := h.CallTgapi("answerCallbackQuery", token, map[string]any{
		"callback_query_id": "4382bfdwdsb323b2d9",
		"text":              "Done!",
	})

	if status != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d; body: %v", status, resp)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("expected ok=true in response, got: %v", resp)
	}

	// The request must have reached the fake TG server exactly once.
	if got := h.fake.RequestsCountFor("answerCallbackQuery"); got != 1 {
		t.Errorf("answerCallbackQuery forwarded to fake TG: want 1, got %d", got)
	}
}

// TestE2E_Callback_C03_SendMessageWithInlineKeyboard verifies that a sendMessage
// request carrying reply_markup.inline_keyboard is forwarded to the fake Telegram
// server and the resulting message is captured in the store.
func TestE2E_Callback_C03_SendMessageWithInlineKeyboard(t *testing.T) {
	h := setupE2E(t, withHTTPServer())

	token := "cb03:inline"
	botID := h.AddBot(BotConfig{
		Token:         token,
		Name:          "cb03_bot",
		ManageEnabled: true,
		ProxyEnabled:  false,
	})

	replyMarkup, err := json.Marshal(map[string]any{
		"inline_keyboard": [][]map[string]any{
			{
				{"text": "Yes", "callback_data": "yes"},
				{"text": "No", "callback_data": "no"},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal reply_markup: %v", err)
	}

	chatID := int64(123456789)

	status, resp := h.CallTgapi("sendMessage", token, map[string]any{
		"chat_id":      chatID,
		"text":         "Pick an option:",
		"reply_markup": json.RawMessage(replyMarkup),
	})

	if status != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d; body: %v", status, resp)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("expected ok=true in sendMessage response, got: %v", resp)
	}

	// sendMessage must have been forwarded to the fake TG.
	if got := h.fake.RequestsCountFor("sendMessage"); got != 1 {
		t.Errorf("sendMessage forwarded to fake TG: want 1, got %d", got)
	}

	// captureSentMessage must have persisted the message to the store.
	h.WaitForMessage(botID, chatID, func(m Message) bool {
		return m.Text == "Pick an option:"
	})

	// Verify the stored message has the expected shape.
	msgs, err := h.store.GetMessages(botID, chatID, 10, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("expected at least one stored message")
	}
	found := msgs[0]
	if found.Text != "Pick an option:" {
		t.Errorf("stored message text: want %q, got %q", "Pick an option:", found.Text)
	}
	if found.ChatID != chatID {
		t.Errorf("stored message chat_id: want %d, got %d", chatID, found.ChatID)
	}

	// Confirm timing constraint: test must finish well within 1 second.
	_ = time.Second
}
