package tests

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/skrashevich/botmux/internal/models"
)

// TestE2E_Send covers S-01..S-07: send-method calls through the /tgapi/ proxy
// captured by captureSentMessage and persisted in the store.
//
// Each sub-test:
//  1. Registers a bot in store + fake TG.
//  2. POSTs to /tgapi/bot{TOKEN}/{method} via h.CallTgapi.
//  3. Asserts HTTP 200 + ok:true + non-zero result.message_id.
//  4. Polls the store and verifies media_type / file_id / text as expected.

// S-01 -----------------------------------------------------------------------

func TestE2E_Send_S01_SendMessageText(t *testing.T) {
	h := setupE2E(t, withHTTPServer())

	token := "s01:sendmsg"
	botID := h.AddBot(models.BotConfig{
		Token:       token,
		Name:        "s01bot",
		BotUsername: "s01bot",
	})

	const chatID int64 = 100001
	const text = "hello"

	status, resp := h.CallTgapi("sendMessage", token, map[string]any{
		"chat_id": chatID,
		"text":    text,
	})

	if status != 200 {
		t.Fatalf("expected HTTP 200, got %d; resp=%v", status, resp)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("expected ok:true, got %v", resp)
	}
	result, _ := resp["result"].(map[string]any)
	if result == nil {
		t.Fatalf("result is nil in response: %v", resp)
	}
	msgID, _ := result["message_id"].(float64)
	if msgID == 0 {
		t.Fatalf("expected non-zero message_id, got %v", result)
	}

	msg := h.WaitForMessage(botID, chatID, func(m models.Message) bool {
		return m.Text == text
	})

	if msg.Text != text {
		t.Errorf("store: expected Text=%q, got %q", text, msg.Text)
	}
	if msg.ChatID != chatID {
		t.Errorf("store: expected ChatID=%d, got %d", chatID, msg.ChatID)
	}
	if msg.BotID != botID {
		t.Errorf("store: expected BotID=%d, got %d", botID, msg.BotID)
	}
}

// S-02 -----------------------------------------------------------------------

func TestE2E_Send_S02_SendMessageWithParseMode(t *testing.T) {
	h := setupE2E(t, withHTTPServer())

	token := "s02:parsemodebot"
	botID := h.AddBot(models.BotConfig{
		Token:       token,
		Name:        "s02bot",
		BotUsername: "s02bot",
	})

	const chatID int64 = 100002
	const text = "<b>bold</b>"

	status, resp := h.CallTgapi("sendMessage", token, map[string]any{
		"chat_id":             chatID,
		"text":                text,
		"parse_mode":          "HTML",
		"reply_to_message_id": 42,
	})

	if status != 200 {
		t.Fatalf("expected HTTP 200, got %d", status)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("expected ok:true, got %v", resp)
	}

	// captureSentMessage parses the TG *response* (which doesn't echo reply_to),
	// so ReplyToID will be 0 in the store. We verify the message is stored at all
	// and the text round-trips correctly.
	msg := h.WaitForMessage(botID, chatID, func(m models.Message) bool {
		return m.Text == text
	})

	if msg.Text != text {
		t.Errorf("store: expected Text=%q, got %q", text, msg.Text)
	}
	// The fake TG does not echo reply_to back in the response body,
	// so ReplyToID is expected to be 0.
	if msg.ReplyToID != 0 {
		t.Errorf("store: expected ReplyToID=0 (not echoed by TG), got %d", msg.ReplyToID)
	}
}

// S-03 -----------------------------------------------------------------------

func TestE2E_Send_S03_SendPhoto(t *testing.T) {
	h := setupE2E(t, withHTTPServer())

	token := "s03:photobot"
	botID := h.AddBot(models.BotConfig{
		Token:       token,
		Name:        "s03bot",
		BotUsername: "s03bot",
	})

	const chatID int64 = 100003
	const fileID = "test_photo_file_id"

	status, resp := h.CallTgapi("sendPhoto", token, map[string]any{
		"chat_id": chatID,
		"photo":   fileID,
	})

	if status != 200 {
		t.Fatalf("expected HTTP 200, got %d", status)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("expected ok:true, got %v", resp)
	}

	msg := h.WaitForMessage(botID, chatID, func(m models.Message) bool {
		return m.MediaType == "photo"
	})

	if msg.MediaType != "photo" {
		t.Errorf("store: expected MediaType=photo, got %q", msg.MediaType)
	}
	if msg.FileID == "" {
		t.Errorf("store: expected non-empty FileID")
	}
}

// S-04 -----------------------------------------------------------------------

func TestE2E_Send_S04_SendVideoAndAnimation(t *testing.T) {
	h := setupE2E(t, withHTTPServer())

	token := "s04:videobot"
	botID := h.AddBot(models.BotConfig{
		Token:       token,
		Name:        "s04bot",
		BotUsername: "s04bot",
	})

	t.Run("sendVideo", func(t *testing.T) {
		const chatID int64 = 100041
		status, resp := h.CallTgapi("sendVideo", token, map[string]any{
			"chat_id": chatID,
			"video":   "video_file_id",
		})
		if status != 200 {
			t.Fatalf("expected HTTP 200, got %d", status)
		}
		if ok, _ := resp["ok"].(bool); !ok {
			t.Fatalf("expected ok:true, got %v", resp)
		}
		msg := h.WaitForMessage(botID, chatID, func(m models.Message) bool {
			return m.MediaType == "video"
		})
		if msg.MediaType != "video" {
			t.Errorf("store: expected MediaType=video, got %q", msg.MediaType)
		}
		if msg.FileID == "" {
			t.Errorf("store: expected non-empty FileID for video")
		}
	})

	t.Run("sendAnimation", func(t *testing.T) {
		const chatID int64 = 100042
		status, resp := h.CallTgapi("sendAnimation", token, map[string]any{
			"chat_id":   chatID,
			"animation": "anim_file_id",
		})
		if status != 200 {
			t.Fatalf("expected HTTP 200, got %d", status)
		}
		if ok, _ := resp["ok"].(bool); !ok {
			t.Fatalf("expected ok:true, got %v", resp)
		}
		msg := h.WaitForMessage(botID, chatID, func(m models.Message) bool {
			return m.MediaType == "animation"
		})
		if msg.MediaType != "animation" {
			t.Errorf("store: expected MediaType=animation, got %q", msg.MediaType)
		}
		if msg.FileID == "" {
			t.Errorf("store: expected non-empty FileID for animation")
		}
	})
}

// S-05 -----------------------------------------------------------------------

func TestE2E_Send_S05_SendSticker(t *testing.T) {
	h := setupE2E(t, withHTTPServer())

	token := "s05:stickerbot"
	botID := h.AddBot(models.BotConfig{
		Token:       token,
		Name:        "s05bot",
		BotUsername: "s05bot",
	})

	// Override fakeTG sendSticker handler to return a sticker with a thumbnail,
	// matching the real Telegram API response format.
	// captureSentMessage in server.go uses msg.Sticker.FileID (the main sticker
	// file_id), NOT the thumbnail — so FileID in store will be the sticker's
	// own file_id, not the thumbnail's.
	h.fake.SetHandler("sendSticker", func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		chatID := parseChatID(r, bodyBytes)
		stickerFileID := parseStringParam(r, bodyBytes, "sticker")
		if stickerFileID == "" {
			stickerFileID = "sticker_main_file_id"
		}

		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": int64(9001),
				"date":       int64(1745064480),
				"chat":       map[string]any{"id": chatID, "type": "private"},
				"from":       map[string]any{"id": int64(101), "is_bot": true, "username": "s05bot"},
				"sticker": map[string]any{
					"file_id":        stickerFileID,
					"file_unique_id": "unique_sticker",
					"width":          512,
					"height":         512,
					"thumbnail": map[string]any{
						"file_id":        "sticker_thumb_file_id",
						"file_unique_id": "unique_thumb",
						"width":          128,
						"height":         128,
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	const chatID int64 = 100005
	const stickerFileID = "sticker_main_file_id"

	status, resp := h.CallTgapi("sendSticker", token, map[string]any{
		"chat_id": chatID,
		"sticker": stickerFileID,
	})

	if status != 200 {
		t.Fatalf("expected HTTP 200, got %d", status)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("expected ok:true, got %v", resp)
	}

	msg := h.WaitForMessage(botID, chatID, func(m models.Message) bool {
		return m.MediaType == "sticker"
	})

	if msg.MediaType != "sticker" {
		t.Errorf("store: expected MediaType=sticker, got %q", msg.MediaType)
	}
	// captureSentMessage in server.go uses msg.Sticker.FileID (the main sticker
	// file_id). bot.go:extractMedia uses Thumbnail.FileID — that path is for
	// incoming updates (processUpdate), not for outgoing messages captured here.
	if msg.FileID != stickerFileID {
		t.Errorf("store: expected FileID=%q (main sticker), got %q", stickerFileID, msg.FileID)
	}
}

// S-06 -----------------------------------------------------------------------

func TestE2E_Send_S06_TableDrivenMediaTypes(t *testing.T) {
	h := setupE2E(t, withHTTPServer())

	token := "s06:mediabot"
	botID := h.AddBot(models.BotConfig{
		Token:       token,
		Name:        "s06bot",
		BotUsername: "s06bot",
	})

	cases := []struct {
		method    string
		paramKey  string
		fileID    string
		mediaType string
		chatID    int64
	}{
		{"sendVoice", "voice", "voice_fid", "voice", 100061},
		{"sendAudio", "audio", "audio_fid", "audio", 100062},
		{"sendDocument", "document", "doc_fid", "document", 100063},
		{"sendVideoNote", "video_note", "vnote_fid", "video_note", 100064},
	}

	for _, tc := range cases {
		tc := tc // capture range var
		t.Run(tc.method, func(t *testing.T) {
			status, resp := h.CallTgapi(tc.method, token, map[string]any{
				"chat_id":   tc.chatID,
				tc.paramKey: tc.fileID,
			})
			if status != 200 {
				t.Fatalf("expected HTTP 200, got %d", status)
			}
			if ok, _ := resp["ok"].(bool); !ok {
				t.Fatalf("expected ok:true, got %v", resp)
			}

			msg := h.WaitForMessage(botID, tc.chatID, func(m models.Message) bool {
				return m.MediaType == tc.mediaType
			})

			if msg.MediaType != tc.mediaType {
				t.Errorf("store: expected MediaType=%q, got %q", tc.mediaType, msg.MediaType)
			}
			if msg.FileID != tc.fileID {
				t.Errorf("store: expected FileID=%q, got %q", tc.fileID, msg.FileID)
			}
		})
	}
}

// S-07 -----------------------------------------------------------------------

func TestE2E_Send_S07_CopyMessage(t *testing.T) {
	h := setupE2E(t, withHTTPServer())

	token := "s07:copybot"
	botID := h.AddBot(models.BotConfig{
		Token:       token,
		Name:        "s07bot",
		BotUsername: "s07bot",
	})

	// captureCopiedMessage looks up the source message in store by (botID, fromChatID, messageID).
	// We must pre-insert the source message so the lookup succeeds.
	const fromChatID int64 = 100070
	const srcMsgID = 7001
	const destChatID int64 = 100071
	const srcText = "original text"

	sourceMsg := models.Message{
		ID:       srcMsgID,
		BotID:    botID,
		ChatID:   fromChatID,
		FromUser: "@sourceuser",
		FromID:   9999,
		Text:     srcText,
		Date:     1745064480000,
	}
	if err := h.store.SaveMessage(sourceMsg); err != nil {
		t.Fatalf("SaveMessage (source): %v", err)
	}

	status, resp := h.CallTgapi("copyMessage", token, map[string]any{
		"chat_id":      destChatID,
		"from_chat_id": fromChatID,
		"message_id":   srcMsgID,
	})

	if status != 200 {
		t.Fatalf("expected HTTP 200, got %d", status)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("expected ok:true, got %v", resp)
	}
	result, _ := resp["result"].(map[string]any)
	if result == nil {
		t.Fatalf("result is nil: %v", resp)
	}
	newMsgID, _ := result["message_id"].(float64)
	if newMsgID == 0 {
		t.Fatalf("expected non-zero message_id in copyMessage result, got %v", result)
	}

	// The copied message must appear in store at destChatID.
	msg := h.WaitForMessage(botID, destChatID, func(m models.Message) bool {
		return m.ID == int(newMsgID)
	})

	if msg.ChatID != destChatID {
		t.Errorf("store: expected ChatID=%d, got %d", destChatID, msg.ChatID)
	}
	if msg.Text != srcText {
		t.Errorf("store: expected Text=%q (copied from source), got %q", srcText, msg.Text)
	}
}
