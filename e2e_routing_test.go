package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// makeTextUpdate builds a minimal raw Telegram message update for injection.
func makeTextUpdate(updateID int, chatID, userID int64, username, text string) map[string]any {
	return map[string]any{
		"update_id": float64(updateID),
		"message": map[string]any{
			"message_id": float64(updateID * 10),
			"date":       float64(1700000000 + updateID),
			"chat": map[string]any{
				"id":   float64(chatID),
				"type": "private",
			},
			"from": map[string]any{
				"id":         float64(userID),
				"is_bot":     false,
				"username":   username,
				"first_name": username,
			},
			"text": text,
		},
	}
}

// registerAndManage registers a bot in the store and fake TG, creates its managed Bot
// instance, and registers it with the proxy so applyRoutes/applyLLMRoutes can call it.
// It temporarily sets the package-level telegramAPIURL to point at the fake TG server.
func registerAndManage(h *e2eHarness, token, username string) int64 {
	h.t.Helper()
	botID := h.AddBot(BotConfig{
		Token:         token,
		Name:          username,
		BotUsername:   username,
		ManageEnabled: true,
	})
	managedBot, err := NewBot(token, h.store, botID, h.fake.URL())
	if err != nil {
		h.t.Fatalf("registerAndManage: NewBot(%s): %v", username, err)
	}
	h.proxy.RegisterManagedBot(botID, managedBot)
	return botID
}

// TestE2E_Routing runs the four routing scenarios as sub-tests.
func TestE2E_Routing(t *testing.T) {

	// R-01: rule-based text-match forward
	t.Run("R01_TextMatch_Forward", func(t *testing.T) {
		h := setupE2E(t)

		srcBot := registerAndManage(h, "src01:token", "srcbot01")
		tgtBot := registerAndManage(h, "tgt01:token", "tgtbot01")

		// Create route: source_bot=srcBot, source_chat=100, target_bot=tgtBot,
		// target_chat=200, condition_type="text", condition_value="hello", action="forward"
		_, err := h.store.AddRoute(Route{
			SourceBotID:    srcBot,
			TargetBotID:    tgtBot,
			SourceChatID:   100,
			ConditionType:  "text",
			ConditionValue: "hello",
			Action:         "forward",
			TargetChatID:   200,
			Enabled:        true,
			CreatedAt:      time.Now().Format(time.RFC3339),
		})
		if err != nil {
			t.Fatalf("AddRoute: %v", err)
		}

		update := makeTextUpdate(1, 100, 7, "alice", "hello world")
		h.InjectUpdate(srcBot, update)

		// The target bot should have sent exactly one sendMessage to chat 200.
		sends := h.fake.RequestsFor("sendMessage")
		if len(sends) != 1 {
			t.Fatalf("R01: expected 1 sendMessage, got %d", len(sends))
		}
		chatID := parseChatID(nil, sends[0].body)
		if chatID != 200 {
			t.Errorf("R01: expected sendMessage to chat_id=200, got %d", chatID)
		}
	})

	// R-02: rule-based user_id filter — only routes when from.id matches
	t.Run("R02_UserID_Filter", func(t *testing.T) {
		h := setupE2E(t)

		srcBot := registerAndManage(h, "src02:token", "srcbot02")
		tgtBot := registerAndManage(h, "tgt02:token", "tgtbot02")

		_, err := h.store.AddRoute(Route{
			SourceBotID:    srcBot,
			TargetBotID:    tgtBot,
			ConditionType:  "user_id",
			ConditionValue: "42",
			Action:         "forward",
			TargetChatID:   300,
			Enabled:        true,
			CreatedAt:      time.Now().Format(time.RFC3339),
		})
		if err != nil {
			t.Fatalf("AddRoute: %v", err)
		}

		// Message from a different user — must NOT be routed.
		h.InjectUpdate(srcBot, makeTextUpdate(1, 50, 99, "other", "hello"))
		if cnt := h.fake.RequestsCountFor("sendMessage"); cnt != 0 {
			t.Fatalf("R02: expected 0 sendMessage for non-matching user, got %d", cnt)
		}

		// Message from user_id=42 — must be routed.
		h.InjectUpdate(srcBot, makeTextUpdate(2, 50, 42, "alice", "hello"))
		sends := h.fake.RequestsFor("sendMessage")
		if len(sends) != 1 {
			t.Fatalf("R02: expected 1 sendMessage for matching user, got %d", len(sends))
		}
		chatID := parseChatID(nil, sends[0].body)
		if chatID != 300 {
			t.Errorf("R02: expected sendMessage to chat_id=300, got %d", chatID)
		}
	})

	// R-03: reverse-NAT — reply in target chat is forwarded back to source chat
	t.Run("R03_ReverseNAT", func(t *testing.T) {
		h := setupE2E(t)

		srcBot := registerAndManage(h, "src03:token", "srcbot03")
		tgtBot := registerAndManage(h, "tgt03:token", "tgtbot03")

		routeID, err := h.store.AddRoute(Route{
			SourceBotID:    srcBot,
			TargetBotID:    tgtBot,
			SourceChatID:   100,
			ConditionType:  "text",
			ConditionValue: "route_me",
			Action:         "forward",
			TargetChatID:   200,
			Enabled:        true,
			CreatedAt:      time.Now().Format(time.RFC3339),
		})
		if err != nil {
			t.Fatalf("AddRoute: %v", err)
		}

		// Step 1: inject original message into source bot → applyRoutes → sends to target chat 200.
		h.InjectUpdate(srcBot, makeTextUpdate(1, 100, 7, "alice", "route_me"))

		// Verify forward happened.
		if cnt := h.fake.RequestsCountFor("sendMessage"); cnt == 0 {
			t.Fatal("R03: expected forward sendMessage, got 0")
		}

		// Step 2: the route_mapping must have been recorded.
		mapping, err := h.store.FindReverseMapping(tgtBot, 200)
		if err != nil {
			t.Fatalf("R03: FindReverseMapping: %v — route mapping was not saved", err)
		}
		if mapping.SourceBotID != srcBot {
			t.Errorf("R03: mapping.SourceBotID=%d, want %d", mapping.SourceBotID, srcBot)
		}
		if mapping.SourceChatID != 100 {
			t.Errorf("R03: mapping.SourceChatID=%d, want 100", mapping.SourceChatID)
		}
		if mapping.RouteID != routeID {
			t.Errorf("R03: mapping.RouteID=%d, want %d", mapping.RouteID, routeID)
		}

		// Step 3: inject a human reply in the target chat → applyReverseRoutes should
		// deliver the reply back to the source chat via the source bot.
		sendsBefore := h.fake.RequestsCountFor("sendMessage")

		replyUpdate := map[string]any{
			"update_id": float64(2),
			"message": map[string]any{
				"message_id": float64(999),
				"date":       float64(1700000100),
				"chat": map[string]any{
					"id":   float64(200),
					"type": "private",
				},
				"from": map[string]any{
					"id":       float64(55),
					"is_bot":   false,
					"username": "operator",
				},
				"text": "I got your message",
				"reply_to_message": map[string]any{
					"message_id": float64(mapping.TargetMsgID),
				},
			},
		}
		h.InjectUpdate(tgtBot, replyUpdate)

		sendsAfter := h.fake.RequestsCountFor("sendMessage")
		if sendsAfter <= sendsBefore {
			t.Fatalf("R03: expected reverse sendMessage, count did not increase (%d → %d)",
				sendsBefore, sendsAfter)
		}
	})

	// R-04: LLM-based routing via fakeLLM
	t.Run("R04_LLM_Routing", func(t *testing.T) {
		h := setupE2E(t)
		llm := newFakeLLM(t)

		srcBot := registerAndManage(h, "src04:token", "srcbot04")
		tgtBot := registerAndManage(h, "tgt04:token", "tgtbot04")

		// Configure the fake LLM to route to tgtBot chat 200.
		llm.SetNextRoute(map[string]any{
			"target_bot_id":  float64(tgtBot),
			"target_chat_id": float64(200),
			"action":         "forward",
			"reason":         "test",
		})

		// Persist LLM config pointing at the fake server.
		if err := h.store.SaveLLMConfig(LLMConfig{
			APIURL:  llm.URL(),
			APIKey:  "test-key",
			Model:   "test-model",
			Enabled: true,
		}); err != nil {
			t.Fatalf("SaveLLMConfig: %v", err)
		}

		// Reload the LLM router so it picks up the saved config.
		h.proxy.llmRouter = NewLLMRouter(h.store)

		// Inject a message — applyLLMRoutes should call the fake LLM.
		h.InjectUpdate(srcBot, makeTextUpdate(1, 100, 7, "alice", "please route this"))

		// Verify the fake LLM received exactly one /chat/completions call.
		if cnt := llm.RequestsCountFor("/chat/completions"); cnt != 1 {
			t.Errorf("R04: expected 1 LLM /chat/completions call, got %d", cnt)
		}

		// Verify the LLM request body referenced the message text.
		reqs := llm.Requests()
		if len(reqs) == 0 {
			t.Fatal("R04: no LLM requests recorded")
		}
		bodyBytes, _ := json.Marshal(reqs[0].Body)
		bodyStr := string(bodyBytes)
		if !strings.Contains(bodyStr, "please route this") {
			t.Errorf("R04: LLM request body does not contain message text: %s", bodyStr)
		}

		// Verify a sendMessage was sent to chat 200.
		sends := h.fake.RequestsFor("sendMessage")
		if len(sends) == 0 {
			t.Fatal("R04: expected sendMessage from LLM route, got 0")
		}
		chatID := parseChatID(nil, sends[len(sends)-1].body)
		if chatID != 200 {
			t.Errorf("R04: expected sendMessage to chat_id=200, got %d", chatID)
		}
	})
}
