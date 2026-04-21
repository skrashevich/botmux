package tests

import (
	"encoding/json"
	"testing"

	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/server"
)

// S-08: captureSentMessage with deleteMessage (not in sendMethods) must not write to store.
func TestUnit_Send_S08_DeleteMessageIsNoOp(t *testing.T) {
	store := newTestStore(t)
	server := server.NewServer(store, nil)

	botID, err := store.AddBotConfig(models.BotConfig{
		Name:        "Del Bot",
		Token:       "deltoken",
		BotUsername: "delbot",
	})
	if err != nil {
		t.Fatalf("AddBotConfig: %v", err)
	}

	// deleteMessage returns {"ok":true,"result":true} — result is bool, not a Message.
	server.CaptureSentMessage(
		"deltoken",
		"deleteMessage",
		[]byte(`{"chat_id":100,"message_id":55}`),
		"application/json",
		[]byte(`{"ok":true,"result":true}`),
	)

	msgs, err := store.GetMessages(botID, 100, 10, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages stored for deleteMessage, got %d: %#v", len(msgs), msgs)
	}
}

// S-09: inferTelegramMethod truth-table — body field → expected method name.
func TestUnit_Send_S09_InferTelegramMethodTruthTable(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		// Known send methods matched by body fields
		{"photo", `{"photo":"file_id","chat_id":1}`, "sendPhoto"},
		{"audio", `{"audio":"file_id","chat_id":1}`, "sendAudio"},
		{"document", `{"document":"file_id","chat_id":1}`, "sendDocument"},
		{"video", `{"video":"file_id","chat_id":1}`, "sendVideo"},
		{"animation", `{"animation":"file_id","chat_id":1}`, "sendAnimation"},
		{"voice", `{"voice":"file_id","chat_id":1}`, "sendVoice"},
		{"video_note", `{"video_note":"file_id","chat_id":1}`, "sendVideoNote"},
		{"sticker", `{"sticker":"file_id","chat_id":1}`, "sendSticker"},
		{"location", `{"latitude":1.0,"longitude":2.0,"chat_id":1}`, "sendLocation"},
		{"venue", `{"latitude":1.0,"longitude":2.0,"title":"Place","address":"Addr","chat_id":1}`, "sendVenue"},
		{"contact", `{"phone_number":"+1","first_name":"X","chat_id":1}`, "sendContact"},
		{"poll", `{"question":"?","options":[],"chat_id":1}`, "sendPoll"},
		{"dice", `{"emoji":"🎲","chat_id":1}`, "sendDice"},
		{"forwardMessage", `{"from_chat_id":5,"message_id":10,"chat_id":1}`, "forwardMessage"},
		{"editMessageText", `{"text":"new","message_id":10,"chat_id":1}`, "editMessageText"},
		{"sendMessage", `{"text":"hello","chat_id":1}`, "sendMessage"},

		// Edge cases — must return empty string
		{"empty_body", `{}`, ""},
		{"empty_string", ``, ""},
		{"malformed_json", `not json`, ""},
		{"getMe_no_relevant_fields", `{"user":{"id":1}}`, ""},
		{"null_body", `null`, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := server.InferTelegramMethod([]byte(tc.body))
			if got != tc.want {
				t.Errorf("server.InferTelegramMethod(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// S-10: editMessageText / editMessageCaption with result=true (inline message) must not panic;
// result=models.Message object must save to store.
func TestUnit_Send_S10_EditMessageBothResultShapes(t *testing.T) {
	t.Run("result_is_true_inline_message", func(t *testing.T) {
		store := newTestStore(t)
		server := server.NewServer(store, nil)

		// When editing an inline message Telegram returns result:true — must not panic, must not store.
		server.CaptureSentMessage(
			"token",
			"editMessageText",
			[]byte(`{"inline_message_id":"abc","text":"updated"}`),
			"application/json",
			[]byte(`{"ok":true,"result":true}`),
		)
		// No bot registered for this token, nothing to verify in store — just confirm no panic.
	})

	t.Run("result_is_message_object", func(t *testing.T) {
		store := newTestStore(t)
		server := server.NewServer(store, nil)

		botID, err := store.AddBotConfig(models.BotConfig{
			Name:        "Edit Bot",
			Token:       "edittoken",
			BotUsername: "editbot",
		})
		if err != nil {
			t.Fatalf("AddBotConfig: %v", err)
		}

		resp := map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": 77,
				"chat":       map[string]any{"id": 200, "type": "private"},
				"from":       map[string]any{"id": 99, "first_name": "Bot", "is_bot": true},
				"text":       "updated text",
				"date":       1700000000,
			},
		}
		respBody, _ := json.Marshal(resp)

		server.CaptureSentMessage(
			"edittoken",
			"editMessageText",
			[]byte(`{"chat_id":200,"message_id":77,"text":"updated text"}`),
			"application/json",
			respBody,
		)

		msgs, err := store.GetMessages(botID, 200, 10, 0)
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("expected 1 captured edited message, got %d", len(msgs))
		}
		if msgs[0].ID != 77 || msgs[0].Text != "updated text" {
			t.Fatalf("unexpected message: %#v", msgs[0])
		}
	})
}

// S-11: captureSentMessage error resilience — malformed JSON, empty body, ok:false.
// Must not panic; must not write to store.
func TestUnit_Send_S11_CaptureErrorResilience(t *testing.T) {
	cases := []struct {
		name     string
		method   string
		reqBody  []byte
		respBody []byte
	}{
		{
			name:     "malformed_response_json",
			method:   "sendMessage",
			reqBody:  []byte(`{"chat_id":1,"text":"hi"}`),
			respBody: []byte(`{not valid json`),
		},
		{
			name:     "empty_response_body",
			method:   "sendMessage",
			reqBody:  []byte(`{"chat_id":1,"text":"hi"}`),
			respBody: []byte(``),
		},
		{
			name:     "ok_false",
			method:   "sendMessage",
			reqBody:  []byte(`{"chat_id":1,"text":"hi"}`),
			respBody: []byte(`{"ok":false,"error_code":400,"description":"Bad Request"}`),
		},
		{
			name:     "result_missing_message_id",
			method:   "sendMessage",
			reqBody:  []byte(`{"chat_id":1,"text":"hi"}`),
			respBody: []byte(`{"ok":true,"result":{"chat":{"id":1},"text":"hi"}}`),
		},
		{
			// Empty request body with an ok:false response — store must stay empty.
			name:     "empty_request_body_ok_false",
			method:   "sendPhoto",
			reqBody:  []byte(``),
			respBody: []byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`),
		},
		{
			name:     "null_bodies",
			method:   "sendMessage",
			reqBody:  nil,
			respBody: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			server := server.NewServer(store, nil)

			// Register a bot so token lookup succeeds; this makes the "save" path reachable
			// but responses above are all invalid so nothing should be stored.
			botID, err := store.AddBotConfig(models.BotConfig{
				Name:        "Resilience Bot",
				Token:       "restoken",
				BotUsername: "resbot",
			})
			if err != nil {
				t.Fatalf("AddBotConfig: %v", err)
			}

			// Must not panic.
			server.CaptureSentMessage("restoken", tc.method, tc.reqBody, "application/json", tc.respBody)

			msgs, err := store.GetMessages(botID, 1, 10, 0)
			if err != nil {
				t.Fatalf("GetMessages: %v", err)
			}
			if len(msgs) != 0 {
				t.Fatalf("case %q: expected 0 messages, got %d: %#v", tc.name, len(msgs), msgs)
			}
		})
	}
}
