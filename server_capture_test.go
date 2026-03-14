package main

import (
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()

	store, err := NewStore(filepath.Join(t.TempDir(), "botmux-test.db"))
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	return store
}

func TestCaptureSentMessageStoresCopiedMessageUsingRequestAndSourceMessage(t *testing.T) {
	store := newTestStore(t)
	server := NewServer(store, nil)

	botID, err := store.AddBotConfig(BotConfig{
		Name:        "Copy Bot",
		Token:       "token",
		BotUsername: "copybot",
	})
	if err != nil {
		t.Fatalf("AddBotConfig() error = %v", err)
	}

	if err := store.SaveMessage(Message{
		ID:        50,
		BotID:     botID,
		ChatID:    555,
		FromUser:  "@alice",
		FromID:    7,
		Text:      "original text",
		Date:      1700000000,
		MediaType: "photo",
		FileID:    "file-1",
	}); err != nil {
		t.Fatalf("SaveMessage() error = %v", err)
	}

	server.captureSentMessage(
		"token",
		"copyMessage",
		[]byte(`{"chat_id":777,"from_chat_id":555,"message_id":50}`),
		"application/json",
		[]byte(`{"ok":true,"result":{"message_id":123}}`),
	)

	msgs, err := store.GetMessages(botID, 777, 10, 0)
	if err != nil {
		t.Fatalf("GetMessages() error = %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 copied message, got %#v", msgs)
	}
	if msgs[0].ID != 123 || msgs[0].ChatID != 777 || msgs[0].Text != "original text" {
		t.Fatalf("unexpected copied message: %#v", msgs[0])
	}
	if msgs[0].MediaType != "photo" || msgs[0].FileID != "file-1" {
		t.Fatalf("copied media was not preserved: %#v", msgs[0])
	}
	if msgs[0].FromUser != "@copybot" {
		t.Fatalf("expected copied message sender to be bot, got %#v", msgs[0])
	}
}

func TestCaptureSentMessageStoresFullSendMessageResults(t *testing.T) {
	store := newTestStore(t)
	server := NewServer(store, nil)

	botID, err := store.AddBotConfig(BotConfig{
		Name:        "Send Bot",
		Token:       "token",
		BotUsername: "sendbot",
	})
	if err != nil {
		t.Fatalf("AddBotConfig() error = %v", err)
	}
	_ = botID

	server.captureSentMessage("token", "sendMessage", nil, "", []byte(`{
		"ok": true,
		"result": {
			"message_id": 321,
			"chat": {"id": 777, "type": "private"},
			"from": {"id": 88, "first_name": "Bot"},
			"text": "hello",
			"date": 1700000000
		}
	}`))

	msgs, err := store.GetMessages(botID, 777, 10, 0)
	if err != nil {
		t.Fatalf("GetMessages() error = %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 captured message, got %d", len(msgs))
	}
	if msgs[0].ID != 321 || msgs[0].Text != "hello" || msgs[0].FromID != 88 {
		t.Fatalf("unexpected captured message: %#v", msgs[0])
	}
}
