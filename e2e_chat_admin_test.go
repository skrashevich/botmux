package main

import (
	"net/http"
	"testing"
	"time"
)

// setupChatAdminBot registers a manage-enabled bot with all necessary fake TG
// handlers so that trackChat (called by processForManagement) succeeds without
// errors. Returns (botID, token).
func setupChatAdminBot(h *e2eHarness, token string) int64 {
	const botTGID = int64(111222333)
	h.fake.RegisterBot(token, "mybot", botTGID)

	// getChatMember — called by trackChat to determine admin status
	h.fake.SetHandler("getChatMember", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"status":"administrator","user":{"id":111222333,"is_bot":true,"first_name":"MyBot","username":"mybot"}}}`))
	})

	// getChatMembersCount — called by trackChat
	h.fake.SetHandler("getChatMembersCount", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":42}`))
	})

	// getChat — called by trackChat to get description
	h.fake.SetHandler("getChat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"id":-1001234567890,"type":"supergroup","title":"Test Group","username":"testgroup","description":""}}`))
	})

	botID := h.AddBot(BotConfig{
		Token:         token,
		Name:          "chatadminbot",
		BotUsername:   "mybot",
		ManageEnabled: true,
	})
	return botID
}

// findChatInStore returns the first Chat matching chatID in GetChats result, or nil.
func findChatInStore(h *e2eHarness, botID, chatID int64) *Chat {
	chats, err := h.store.GetChats(botID)
	if err != nil {
		return nil
	}
	for i := range chats {
		if chats[i].ID == chatID {
			return &chats[i]
		}
	}
	return nil
}

// --------------------------------------------------------------------------
// CH-01: getChat / getChatMember / getChatAdministrators proxy passthrough
// --------------------------------------------------------------------------

func TestE2E_ChatAdmin(t *testing.T) {
	t.Run("CH-01_getChat_proxy", func(t *testing.T) {
		h := setupE2E(t, withStartedProxy(), withHTTPServer())
		token := "chatadmin:CH01"
		botID := setupChatAdminBot(h, token)
		_ = botID

		const chatID = int64(-1001234567890)

		// Override getChat to return a well-known response so we can verify passthrough.
		h.fake.SetHandler("getChat", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":-1001234567890,"type":"supergroup","title":"Test Group"}}`))
		})

		status, resp := h.CallTgapi("getChat", token, map[string]any{"chat_id": chatID})

		if status != 200 {
			t.Fatalf("CH-01: expected HTTP 200, got %d", status)
		}
		if ok, _ := resp["ok"].(bool); !ok {
			t.Fatalf("CH-01: response not ok: %v", resp)
		}
		result, _ := resp["result"].(map[string]any)
		if result == nil {
			t.Fatalf("CH-01: missing result in response: %v", resp)
		}
		if title, _ := result["title"].(string); title != "Test Group" {
			t.Fatalf("CH-01: expected title='Test Group', got %q", title)
		}
		if n := h.fake.RequestsCountFor("getChat"); n < 1 {
			t.Fatalf("CH-01: expected at least 1 getChat request forwarded to fake, got %d", n)
		}
	})

	// --------------------------------------------------------------------------
	// Admin operations A-01..A-07
	// --------------------------------------------------------------------------

	t.Run("A-01_banChatMember", func(t *testing.T) {
		h := setupE2E(t, withStartedProxy(), withHTTPServer())
		token := "chatadmin:A01"
		botID := setupChatAdminBot(h, token)
		_ = botID

		before := h.fake.RequestsCountFor("banChatMember")
		status, resp := h.CallTgapi("banChatMember", token, map[string]any{
			"chat_id": -1001234567890,
			"user_id": 555666777,
		})
		if status != 200 {
			t.Fatalf("A-01: expected HTTP 200, got %d", status)
		}
		if ok, _ := resp["ok"].(bool); !ok {
			t.Fatalf("A-01: response not ok: %v", resp)
		}
		if got := h.fake.RequestsCountFor("banChatMember") - before; got != 1 {
			t.Fatalf("A-01: expected 1 forwarded banChatMember request, got %d", got)
		}
	})

	t.Run("A-02_unbanChatMember", func(t *testing.T) {
		h := setupE2E(t, withStartedProxy(), withHTTPServer())
		token := "chatadmin:A02"
		botID := setupChatAdminBot(h, token)
		_ = botID

		before := h.fake.RequestsCountFor("unbanChatMember")
		status, resp := h.CallTgapi("unbanChatMember", token, map[string]any{
			"chat_id": -1001234567890,
			"user_id": 555666777,
		})
		if status != 200 {
			t.Fatalf("A-02: expected HTTP 200, got %d", status)
		}
		if ok, _ := resp["ok"].(bool); !ok {
			t.Fatalf("A-02: response not ok: %v", resp)
		}
		if got := h.fake.RequestsCountFor("unbanChatMember") - before; got != 1 {
			t.Fatalf("A-02: expected 1 forwarded unbanChatMember request, got %d", got)
		}
	})

	t.Run("A-03_restrictChatMember", func(t *testing.T) {
		h := setupE2E(t, withStartedProxy(), withHTTPServer())
		token := "chatadmin:A03"
		botID := setupChatAdminBot(h, token)
		_ = botID

		before := h.fake.RequestsCountFor("restrictChatMember")
		status, resp := h.CallTgapi("restrictChatMember", token, map[string]any{
			"chat_id": -1001234567890,
			"user_id": 555666777,
			"permissions": map[string]any{
				"can_send_messages": false,
			},
		})
		if status != 200 {
			t.Fatalf("A-03: expected HTTP 200, got %d", status)
		}
		if ok, _ := resp["ok"].(bool); !ok {
			t.Fatalf("A-03: response not ok: %v", resp)
		}
		if got := h.fake.RequestsCountFor("restrictChatMember") - before; got != 1 {
			t.Fatalf("A-03: expected 1 forwarded restrictChatMember request, got %d", got)
		}
	})

	t.Run("A-04_promoteChatMember", func(t *testing.T) {
		h := setupE2E(t, withStartedProxy(), withHTTPServer())
		token := "chatadmin:A04"
		botID := setupChatAdminBot(h, token)
		_ = botID

		before := h.fake.RequestsCountFor("promoteChatMember")
		status, resp := h.CallTgapi("promoteChatMember", token, map[string]any{
			"chat_id":             -1001234567890,
			"user_id":             555666777,
			"can_delete_messages": true,
			"can_invite_users":    true,
		})
		if status != 200 {
			t.Fatalf("A-04: expected HTTP 200, got %d", status)
		}
		if ok, _ := resp["ok"].(bool); !ok {
			t.Fatalf("A-04: response not ok: %v", resp)
		}
		if got := h.fake.RequestsCountFor("promoteChatMember") - before; got != 1 {
			t.Fatalf("A-04: expected 1 forwarded promoteChatMember request, got %d", got)
		}
	})

	t.Run("A-05_setChatAdministratorCustomTitle", func(t *testing.T) {
		h := setupE2E(t, withStartedProxy(), withHTTPServer())
		token := "chatadmin:A05"
		botID := setupChatAdminBot(h, token)
		_ = botID

		before := h.fake.RequestsCountFor("setChatAdministratorCustomTitle")
		status, resp := h.CallTgapi("setChatAdministratorCustomTitle", token, map[string]any{
			"chat_id":      -1001234567890,
			"user_id":      555666777,
			"custom_title": "VIP",
		})
		if status != 200 {
			t.Fatalf("A-05: expected HTTP 200, got %d", status)
		}
		if ok, _ := resp["ok"].(bool); !ok {
			t.Fatalf("A-05: response not ok: %v", resp)
		}
		if got := h.fake.RequestsCountFor("setChatAdministratorCustomTitle") - before; got != 1 {
			t.Fatalf("A-05: expected 1 forwarded setChatAdministratorCustomTitle request, got %d", got)
		}
	})

	t.Run("A-06_setChatPermissions", func(t *testing.T) {
		h := setupE2E(t, withStartedProxy(), withHTTPServer())
		token := "chatadmin:A06"
		botID := setupChatAdminBot(h, token)
		_ = botID

		before := h.fake.RequestsCountFor("setChatPermissions")
		status, resp := h.CallTgapi("setChatPermissions", token, map[string]any{
			"chat_id": -1001234567890,
			"permissions": map[string]any{
				"can_send_messages":       true,
				"can_send_media_messages": true,
			},
		})
		if status != 200 {
			t.Fatalf("A-06: expected HTTP 200, got %d", status)
		}
		if ok, _ := resp["ok"].(bool); !ok {
			t.Fatalf("A-06: response not ok: %v", resp)
		}
		if got := h.fake.RequestsCountFor("setChatPermissions") - before; got != 1 {
			t.Fatalf("A-06: expected 1 forwarded setChatPermissions request, got %d", got)
		}
	})

	t.Run("A-07_pin_unpin_messages", func(t *testing.T) {
		h := setupE2E(t, withStartedProxy(), withHTTPServer())
		token := "chatadmin:A07"
		botID := setupChatAdminBot(h, token)
		_ = botID

		pinBefore := h.fake.RequestsCountFor("pinChatMessage")
		unpinBefore := h.fake.RequestsCountFor("unpinChatMessage")
		unpinAllBefore := h.fake.RequestsCountFor("unpinAllChatMessages")

		// pinChatMessage
		status, resp := h.CallTgapi("pinChatMessage", token, map[string]any{
			"chat_id":    -1001234567890,
			"message_id": 100,
		})
		if status != 200 {
			t.Fatalf("A-07 pin: expected HTTP 200, got %d", status)
		}
		if ok, _ := resp["ok"].(bool); !ok {
			t.Fatalf("A-07 pin: response not ok: %v", resp)
		}

		// unpinChatMessage
		status, resp = h.CallTgapi("unpinChatMessage", token, map[string]any{
			"chat_id":    -1001234567890,
			"message_id": 100,
		})
		if status != 200 {
			t.Fatalf("A-07 unpin: expected HTTP 200, got %d", status)
		}
		if ok, _ := resp["ok"].(bool); !ok {
			t.Fatalf("A-07 unpin: response not ok: %v", resp)
		}

		// unpinAllChatMessages
		status, resp = h.CallTgapi("unpinAllChatMessages", token, map[string]any{
			"chat_id": -1001234567890,
		})
		if status != 200 {
			t.Fatalf("A-07 unpinAll: expected HTTP 200, got %d", status)
		}
		if ok, _ := resp["ok"].(bool); !ok {
			t.Fatalf("A-07 unpinAll: response not ok: %v", resp)
		}

		if got := h.fake.RequestsCountFor("pinChatMessage") - pinBefore; got != 1 {
			t.Fatalf("A-07: expected 1 pinChatMessage, got %d", got)
		}
		if got := h.fake.RequestsCountFor("unpinChatMessage") - unpinBefore; got != 1 {
			t.Fatalf("A-07: expected 1 unpinChatMessage, got %d", got)
		}
		if got := h.fake.RequestsCountFor("unpinAllChatMessages") - unpinAllBefore; got != 1 {
			t.Fatalf("A-07: expected 1 unpinAllChatMessages, got %d", got)
		}
	})

	// --------------------------------------------------------------------------
	// Chat member updates M-01..M-03
	// --------------------------------------------------------------------------

	// M-01: my_chat_member — bot status change (member → administrator).
	// After processUpdate, trackChat is called which upserts the chat into store.
	t.Run("M-01_my_chat_member", func(t *testing.T) {
		h := setupE2E(t, withStartedProxy())
		token := "chatadmin:M01"
		botID := setupChatAdminBot(h, token)

		// RestartBot creates and registers the managed Bot instance.
		if err := h.proxy.RestartBot(botID); err != nil {
			t.Fatalf("M-01: RestartBot: %v", err)
		}

		update := loadFixture(t, "update_my_chat_member.json")
		h.InjectUpdate(botID, update)

		const chatID = int64(-1001234567890)
		// trackChat is called synchronously inside processUpdate → should be immediate,
		// but use Eventually to handle any internal async.
		h.Eventually(func() bool {
			return findChatInStore(h, botID, chatID) != nil
		}, 1*time.Second, "M-01: chat should be upserted after my_chat_member update")
	})

	// M-02: chat_member — user status change (member → kicked).
	// handleChatMember calls store.TrackUser. Check known_users via GetChatUsers.
	t.Run("M-02_chat_member", func(t *testing.T) {
		h := setupE2E(t, withStartedProxy())
		token := "chatadmin:M02"
		botID := setupChatAdminBot(h, token)

		if err := h.proxy.RestartBot(botID); err != nil {
			t.Fatalf("M-02: RestartBot: %v", err)
		}

		update := loadFixture(t, "update_chat_member.json")
		// chat_member fixture: user Alice (555666777) status changes member→kicked
		h.InjectUpdate(botID, update)

		const chatID = int64(-1001234567890)
		const aliceID = int64(555666777)

		// handleChatMember → store.TrackUser; verify user appears in known_users
		h.Eventually(func() bool {
			users, err := h.store.GetChatUsers(chatID, "", 50, 0)
			if err != nil {
				return false
			}
			for _, u := range users {
				if u.UserID == aliceID {
					return true
				}
			}
			return false
		}, 1*time.Second, "M-02: Alice should be tracked after chat_member update")
	})

	// M-03: new_chat_members — message with new member list.
	// handleMessage → SaveMessage + TrackUser for each new member.
	t.Run("M-03_new_chat_members", func(t *testing.T) {
		h := setupE2E(t, withStartedProxy())
		token := "chatadmin:M03"
		botID := setupChatAdminBot(h, token)

		if err := h.proxy.RestartBot(botID); err != nil {
			t.Fatalf("M-03: RestartBot: %v", err)
		}

		update := loadFixture(t, "update_new_chat_members.json")
		// fixture: message_id=1240, chat_id=-1001234567890, new_chat_members=[Alice, Bob]
		h.InjectUpdate(botID, update)

		const chatID = int64(-1001234567890)
		const msgID = 1240

		// handleMessage → store.SaveMessage should record message 1240
		h.Eventually(func() bool {
			msgs, err := h.store.GetMessages(botID, chatID, 50, 0)
			if err != nil {
				return false
			}
			for _, m := range msgs {
				if m.ID == msgID {
					return true
				}
			}
			return false
		}, 1*time.Second, "M-03: message 1240 should be saved after new_chat_members update")
	})
}
