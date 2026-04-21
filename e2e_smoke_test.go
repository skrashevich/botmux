package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/skrashevich/botmux/internal/models"
)

// TestE2E_Smoke is the final Phase 0 sanity test.
// Verifies the end-to-end path: registered bot -> fake TG getUpdates polling ->
// ProxyManager.pollLoop -> processUpdate -> store.SaveMessage.
//
// Flow: fake TG preloaded with one message update -> bot starts polling ->
// message appears in store within 1 second.
func TestE2E_Smoke(t *testing.T) {
	h := setupE2E(t, withFastBackoff(), withStartedProxy())

	token := "smoke:1234567890"
	botID := h.AddBot(models.BotConfig{
		Token:         token,
		Name:          "smokebot",
		BotUsername:   "smokebot",
		ManageEnabled: true,
	})

	// Load fixture update
	fixturePath := filepath.Join("testdata", "tg", "update_message_text.json")
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var update map[string]any
	if err := json.Unmarshal(raw, &update); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	// Enqueue update in fake TG before starting the bot's pollLoop
	h.fake.EnqueueUpdate(token, update)

	// RestartBot creates the managed Bot instance and starts pollLoop for this bot.
	// proxy.Start() was called before AddBot, so the bot has no runner yet.
	if err := h.proxy.RestartBot(botID); err != nil {
		t.Fatalf("RestartBot: %v", err)
	}

	// Extract chat.id from the fixture to query the store
	msg := update["message"].(map[string]any)
	chat := msg["chat"].(map[string]any)
	chatID := int64(chat["id"].(float64))

	// Wait for the message to appear in the store
	h.WaitForMessage(botID, chatID, func(m models.Message) bool {
		return m.Text != ""
	})
}
