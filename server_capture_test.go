package main

import (
	"path/filepath"
	"testing"
	"time"
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

// TestSaveMessageUpsertUpdatesTextOnEdit verifies that re-saving a message
// with the same (bot_id, chat_id, id) PK updates the text — this is the
// streaming / editMessageText path for bots like "openclaw" that edit the
// same message repeatedly with growing content.
func TestSaveMessageUpsertUpdatesTextOnEdit(t *testing.T) {
	store := newTestStore(t)

	base := Message{
		ID:       100,
		BotID:    1,
		ChatID:   42,
		FromUser: "@streambot",
		FromID:   500,
		Text:     "Да. Вот где именно кривая ножка",
		Date:     time.Now().UnixMilli(),
	}
	if err := store.SaveMessage(base); err != nil {
		t.Fatalf("SaveMessage (initial): %v", err)
	}

	edited := base
	edited.Text = "Да. Вот где именно кривая ножка — и весь остальной длинный стриминговый ответ."
	if err := store.SaveMessage(edited); err != nil {
		t.Fatalf("SaveMessage (edit): %v", err)
	}

	got, err := store.GetMessage(base.BotID, base.ChatID, base.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.Text != edited.Text {
		t.Fatalf("text not updated on re-save: got %q want %q", got.Text, edited.Text)
	}
	if got.FromID != base.FromID || got.FromUser != base.FromUser {
		t.Fatalf("identity fields mutated: %#v", got)
	}
}

// TestSaveMessageUpsertPreservesDeletedTombstone verifies that a message
// marked as deleted keeps its tombstone — re-saving the same id must not
// resurrect content or flip the deleted flag.
func TestSaveMessageUpsertPreservesDeletedTombstone(t *testing.T) {
	store := newTestStore(t)

	m := Message{
		ID:       200,
		BotID:    1,
		ChatID:   99,
		FromUser: "@bot",
		FromID:   1,
		Text:     "original",
		Date:     time.Now().UnixMilli(),
	}
	if err := store.SaveMessage(m); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	if err := store.MarkMessageDeleted(m.BotID, m.ChatID, m.ID); err != nil {
		t.Fatalf("MarkMessageDeleted: %v", err)
	}

	edited := m
	edited.Text = "resurrection attempt"
	if err := store.SaveMessage(edited); err != nil {
		t.Fatalf("SaveMessage (edit-after-delete): %v", err)
	}

	got, err := store.GetMessage(m.BotID, m.ChatID, m.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if !got.Deleted {
		t.Fatalf("deleted flag was cleared by re-save: %#v", got)
	}
	if got.Text != "original" {
		t.Fatalf("deleted message content was overwritten: got %q", got.Text)
	}
}

// TestSaveMessageUpsertNotifiesSubscribersOnEdit verifies subscribers receive
// updates for both the initial insert and subsequent edits (so the SSE
// stream to the web UI can refresh the rendered message in place).
func TestSaveMessageUpsertNotifiesSubscribersOnEdit(t *testing.T) {
	store := newTestStore(t)
	ch := store.Subscribe()
	defer store.Unsubscribe(ch)

	m := Message{
		ID:       300,
		BotID:    1,
		ChatID:   5,
		FromUser: "@bot",
		FromID:   1,
		Text:     "first chunk",
		Date:     time.Now().UnixMilli(),
	}
	if err := store.SaveMessage(m); err != nil {
		t.Fatalf("SaveMessage (initial): %v", err)
	}
	select {
	case got := <-ch:
		if got.Text != "first chunk" {
			t.Fatalf("subscriber got wrong initial text: %q", got.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive initial insert notification")
	}

	m.Text = "first chunk + streamed continuation"
	if err := store.SaveMessage(m); err != nil {
		t.Fatalf("SaveMessage (edit): %v", err)
	}
	select {
	case got := <-ch:
		if got.Text != "first chunk + streamed continuation" {
			t.Fatalf("subscriber got stale text on edit: %q", got.Text)
		}
		if got.ID != m.ID || got.ChatID != m.ChatID {
			t.Fatalf("subscriber got wrong message on edit: %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive edit notification")
	}
}

// TestCaptureSentMessageEditMessageTextUpdatesDB exercises the full
// editMessageText proxy path: initial sendMessage stored, then
// editMessageText with new text must overwrite the stored text.
func TestCaptureSentMessageEditMessageTextUpdatesDB(t *testing.T) {
	store := newTestStore(t)
	server := NewServer(store, nil)

	botID, err := store.AddBotConfig(BotConfig{
		Name:        "Stream Bot",
		Token:       "streamtoken",
		BotUsername: "streambot",
	})
	if err != nil {
		t.Fatalf("AddBotConfig: %v", err)
	}
	_ = botID

	server.captureSentMessage("streamtoken", "sendMessage", nil, "", []byte(`{
		"ok": true,
		"result": {
			"message_id": 1001,
			"chat": {"id": 77, "type": "private"},
			"from": {"id": 42, "first_name": "Bot", "is_bot": true},
			"text": "Да. Вот где именно кривая ножка",
			"date": 1700000000
		}
	}`))

	server.captureSentMessage("streamtoken", "editMessageText", nil, "", []byte(`{
		"ok": true,
		"result": {
			"message_id": 1001,
			"chat": {"id": 77, "type": "private"},
			"from": {"id": 42, "first_name": "Bot", "is_bot": true},
			"text": "Да. Вот где именно кривая ножка. А вот и продолжение длинного стриминга, которое раньше не показывалось в UI.",
			"date": 1700000000
		}
	}`))

	got, err := store.GetMessage(botID, 77, 1001)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	wantSuffix := "продолжение длинного стриминга, которое раньше не показывалось в UI."
	if len(got.Text) < len(wantSuffix) || got.Text[len(got.Text)-len(wantSuffix):] != wantSuffix {
		t.Fatalf("editMessageText did not overwrite text in DB: got %q", got.Text)
	}
}

// TestCaptureSentMessageEditMessageCaptionCaptured verifies that caption
// edits are routed through the send-methods map and update the stored text.
func TestCaptureSentMessageEditMessageCaptionCaptured(t *testing.T) {
	store := newTestStore(t)
	server := NewServer(store, nil)

	botID, err := store.AddBotConfig(BotConfig{
		Name:        "Caption Bot",
		Token:       "captiontoken",
		BotUsername: "captionbot",
	})
	if err != nil {
		t.Fatalf("AddBotConfig: %v", err)
	}
	_ = botID

	server.captureSentMessage("captiontoken", "sendPhoto", nil, "", []byte(`{
		"ok": true,
		"result": {
			"message_id": 2002,
			"chat": {"id": 88, "type": "private"},
			"from": {"id": 43, "first_name": "Bot", "is_bot": true},
			"caption": "original caption",
			"photo": [{"file_id": "photo-1"}],
			"date": 1700000000
		}
	}`))

	server.captureSentMessage("captiontoken", "editMessageCaption", nil, "", []byte(`{
		"ok": true,
		"result": {
			"message_id": 2002,
			"chat": {"id": 88, "type": "private"},
			"from": {"id": 43, "first_name": "Bot", "is_bot": true},
			"caption": "edited caption with much more content",
			"photo": [{"file_id": "photo-1"}],
			"date": 1700000000
		}
	}`))

	got, err := store.GetMessage(botID, 88, 2002)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.Text != "edited caption with much more content" {
		t.Fatalf("editMessageCaption did not overwrite caption text: got %q", got.Text)
	}
	if got.MediaType != "photo" || got.FileID != "photo-1" {
		t.Fatalf("media fields lost on caption edit: %#v", got)
	}
}
