package tests

import (
	"bytes"
	"encoding/json"
	tgbotapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/bot"
	"github.com/skrashevich/botmux/internal/models"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestTelegramRegressionQueryParameters(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	token := "audit:query"
	h.AddBot(models.BotConfig{Token: token})
	h.fake.SetHandler("answerCallbackQuery", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("callback_query_id") != "callback-1" {
			t.Error("callback_query_id lost in upstream GET")
		}
		w.Write([]byte(`{"ok":true,"result":true}`))
	})
	resp, err := http.Get(h.ts.URL + "/tgapi/bot" + token + "/answerCallbackQuery?callback_query_id=callback-1&text=Done")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}
func TestTelegramRegressionCallbackRedelivery(t *testing.T) {
	h := setupE2E(t)
	token := "audit:callback"
	id := h.AddBot(models.BotConfig{Token: token})
	b, err := bot.NewBot(token, h.store, id, h.fake.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	fixture := loadFixture(t, "update_callback_query.json")
	data, _ := json.Marshal(fixture)
	var u tgbotapi.Update
	json.Unmarshal(data, &u)
	b.ProcessUpdate(u)
	time.Sleep(time.Millisecond)
	b.ProcessUpdate(u)
	msgs, err := h.store.GetMessages(id, 987654321, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Errorf("same callback stored %d times", len(msgs))
	}
}
func TestTelegramRegressionWebhookFailureAck(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "offline", 500) }))
	defer backend.Close()
	h := setupE2E(t)
	id := h.AddBot(models.BotConfig{Token: "audit:hook", ProxyEnabled: true, BackendURL: backend.URL})
	body, _ := json.Marshal(loadFixture(t, "update_callback_query.json"))
	r := httptest.NewRequest("POST", "/hook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.proxy.WebhookHandler(id)(w, r)
	if w.Code == 200 {
		t.Error("webhook acknowledged despite backend HTTP 500")
	}
}
func TestTelegramRegressionMediaGroupCapture(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	token := "audit:album"
	id := h.AddBot(models.BotConfig{Token: token})
	h.fake.SetHandler("sendMediaGroup", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true,"result":[{"message_id":1,"date":1,"chat":{"id":42,"type":"private"},"photo":[{"file_id":"p1"}]},{"message_id":2,"date":1,"chat":{"id":42,"type":"private"},"photo":[{"file_id":"p2"}]}]}`)
	})
	h.CallTgapi("sendMediaGroup", token, map[string]any{"chat_id": 42, "media": []any{map[string]any{"type": "photo", "media": "p1"}, map[string]any{"type": "photo", "media": "p2"}}})
	msgs, err := h.store.GetMessages(id, 42, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Errorf("album messages captured: %d, want 2", len(msgs))
	}
}
func TestTelegramRegressionEphemeralRouting(t *testing.T) {
	h := setupE2E(t)
	src := registerAndManage(h, "audit:src", "source")
	dst := registerAndManage(h, "audit:dst", "target")
	_, err := h.store.AddRoute(models.Route{SourceBotID: src, TargetBotID: dst, SourceChatID: 100, TargetChatID: 200, ConditionType: "text", ConditionValue: "private", Action: "forward", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	u := makeTextUpdate(123, 100, 7, "user", "private reply")
	m := u["message"].(map[string]any)
	m["ephemeral_message_id"] = float64(9)
	m["receiver_user"] = map[string]any{"id": float64(7), "first_name": "User", "is_bot": false}
	h.proxy.ProcessUpdate(src, u)
	if h.fake.RequestsCountFor("sendMessage") != 0 {
		t.Error("ephemeral text forwarded as ordinary message to target chat")
	}
}

func TestTelegramKeyboardLifecycleAndTopics(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	token := "keyboard:lifecycle"
	id := registerAndManage(h, token, "keyboard")
	cfg := h.proxy.GetManagedBot(id)
	botUser := cfg.GetSelfID()
	markup := map[string]any{"inline_keyboard": []any{[]any{map[string]any{"text": "Choose", "callback_data": "yes", "style": "success"}, map[string]any{"text": "Copy", "copy_text": map[string]any{"text": "value"}}}}}
	response := map[string]any{"ok": true, "result": map[string]any{"message_id": 77, "date": 1, "chat": map[string]any{"id": 42, "type": "supergroup"}, "from": map[string]any{"id": botUser, "is_bot": true}, "text": "Choose", "reply_markup": markup, "message_thread_id": 9}}
	h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(response) })
	h.CallTgapi("sendMessage", token, map[string]any{"chat_id": 42, "text": "Choose", "reply_markup": markup, "message_thread_id": 9})
	saved, err := h.store.GetMessage(id, 42, 77)
	if err != nil {
		t.Fatal(err)
	}
	var actual any
	json.Unmarshal(saved.ReplyMarkup, &actual)
	if !reflect.DeepEqual(actual, markup) || saved.ThreadID != 9 || !saved.FromIsBot {
		t.Fatalf("metadata not preserved: %#v", saved)
	}
	h.fake.SetHandler("editMessageReplyMarkup", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"ok":true,"result":true}`) })
	h.CallTgapi("editMessageReplyMarkup", token, map[string]any{"chat_id": 42, "message_id": 77, "reply_markup": map[string]any{"inline_keyboard": []any{}}})
	saved, err = h.store.GetMessage(id, 42, 77)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved.ReplyMarkup) != `{"inline_keyboard":[]}` {
		t.Fatalf("keyboard removal not stored: %s", saved.ReplyMarkup)
	}
	var u tgbotapi.Update
	data, _ := json.Marshal(map[string]any{"message": map[string]any{"message_id": 88, "date": 1, "chat": map[string]any{"id": 42, "type": "supergroup"}, "message_thread_id": 9, "forum_topic_created": map[string]any{"name": "Support", "icon_color": 1}, "text": "Topic message", "reply_markup": markup}})
	if err := json.Unmarshal(data, &u); err != nil {
		t.Fatal(err)
	}
	cfg.ProcessUpdate(u)
	topics, err := h.store.GetTopics(id, 42)
	if err != nil || len(topics) != 1 || topics[0]["name"] != "Support" {
		t.Fatalf("topics: %v %v", topics, err)
	}
	msgs, err := h.store.GetMessagesByThread(id, 42, 9, 100, 0)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("topic filter: %v %v", msgs, err)
	}
	// Changing from rich to plain content must clear stale rich text and keyboard.
	h.server.CaptureSentMessage(token, "editMessageText", nil, "", []byte(`{"ok":true,"result":{"message_id":88,"date":1,"chat":{"id":42},"text":"Plain"}}`))
	saved, err = h.store.GetMessage(id, 42, 88)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved.ReplyMarkup) != "null" || saved.RichText != "" {
		t.Fatalf("stale metadata: %#v", saved)
	}
}

func TestTelegramInlineCallbacksAndAnswer(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	id := registerAndManage(h, "callback:inline", "inline")
	b := h.proxy.GetManagedBot(id)
	u := tgbotapi.Update{CallbackQuery: &tgbotapi.CallbackQuery{ID: "unique-callback", InlineMessageID: "inline-message", From: &tgbotapi.User{ID: 7, FirstName: "User"}, Data: "yes"}}
	b.ProcessUpdate(u)
	b.ProcessUpdate(u)
	events, err := h.store.GetCallbackEvents(id, 0, 100)
	if err != nil || len(events) != 1 || events[0].InlineMessageID != "inline-message" {
		t.Fatalf("inline callback not tracked: %v %v", events, err)
	}
	h.fake.SetHandler("answerCallbackQuery", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"ok":true,"result":true}`) })
	status, resp := h.CallTgapi("answerCallbackQuery", "callback:inline", map[string]any{"callback_query_id": "unique-callback", "text": "Done"})
	if status != 200 || resp["ok"] != true {
		t.Fatal(resp)
	}
	events, err = h.store.GetCallbackEvents(id, 0, 100)
	if err != nil || !events[0].Answered {
		t.Fatalf("answer not tracked: %v %v", events, err)
	}
}

func TestTelegramCallbackRouting(t *testing.T) {
	h := setupE2E(t)
	src := registerAndManage(h, "callback:source", "source")
	dst := registerAndManage(h, "callback:target", "target")
	_, err := h.store.AddRoute(models.Route{SourceBotID: src, TargetBotID: dst, SourceChatID: 987654321, TargetChatID: 200, ConditionType: "text", ConditionValue: "option_a", Action: "forward", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	h.proxy.ProcessUpdate(src, loadFixture(t, "update_callback_query.json"))
	if h.fake.RequestsCountFor("sendMessage") != 1 {
		t.Fatal("callback route not executed")
	}
	// Callback events must never be copied/forwarded using synthetic IDs.

}

func TestTelegramKeyboardAPIAndTopicComposer(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	id := registerAndManage(h, "keyboard:api", "api")
	h.store.UpsertChat(id, models.Chat{ID: 42, Type: "supergroup"})
	h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.FormValue("message_thread_id") != "9" {
			t.Errorf("topic not sent: %v", r.Form)
		}
		var markup map[string]any
		if json.Unmarshal([]byte(r.FormValue("reply_markup")), &markup) != nil {
			t.Error("keyboard not sent")
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 7, "date": 1, "chat": map[string]any{"id": 42}, "text": "Topic"}})
	})
	call := func(path string, body any) (int, map[string]any) {
		t.Helper()
		data, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", h.ts.URL+path, bytes.NewReader(data))
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: h.session})
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var result map[string]any
		json.NewDecoder(resp.Body).Decode(&result)
		return resp.StatusCode, result
	}
	status, result := call("/api/messages/send", map[string]any{"bot_id": id, "chat_id": 42, "text": "Topic", "message_thread_id": 9, "reply_markup": map[string]any{"inline_keyboard": [][]map[string]any{{{"text": "Yes", "callback_data": "yes"}}}}})
	if status != 200 {
		t.Fatalf("send failed: %d %v", status, result)
	}
	m, err := h.store.GetMessage(id, 42, 7)
	if err != nil || m.ThreadID != 9 || string(m.ReplyMarkup) == "null" {
		t.Fatalf("composer metadata: %v %v", m, err)
	}
	status, _ = call("/api/messages/keyboard", map[string]any{"bot_id": id, "chat_id": 42, "message_id": 7, "reply_markup": map[string]any{"inline_keyboard": []any{}}})
	if status != 200 {
		t.Fatalf("own keyboard edit rejected: %d", status)
	}
	m, _ = h.store.GetMessage(id, 42, 7)
	if string(m.ReplyMarkup) != `{"inline_keyboard":[]}` {
		t.Fatalf("removal failed: %s", m.ReplyMarkup)
	}
	h.store.SaveMessage(models.Message{ID: 8, BotID: id, ChatID: 42, FromID: 999, FromIsBot: false, Text: "Other"})
	status, _ = call("/api/messages/keyboard", map[string]any{"bot_id": id, "chat_id": 42, "message_id": 8, "reply_markup": map[string]any{"inline_keyboard": []any{}}})
	if status != 403 {
		t.Fatalf("editing other sender allowed: %d", status)
	}
	// No cookie: the new message mutation endpoint must remain protected.
	req, _ := http.NewRequest("POST", h.ts.URL+"/api/messages/keyboard", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("unauthorized mutation: %d", resp.StatusCode)
	}
}

func TestTelegramTopicManagementAndAlbumMetadata(t *testing.T) {
	h := setupE2E(t)
	id := registerAndManage(h, "topic:api", "api")
	b := h.proxy.GetManagedBot(id)
	h.fake.SetHandler("createForumTopic", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true,"result":{"message_thread_id":9,"name":"Support","icon_color":1}}`)
	})
	thread, err := b.ManageTopic(42, 0, "create", "Support")
	if err != nil || thread != 9 {
		t.Fatalf("create: %d %v", thread, err)
	}
	if _, err := b.ManageTopic(42, 9, "close", ""); err != nil {
		t.Fatal(err)
	}
	topics, err := h.store.GetTopics(id, 42)
	if err != nil || !topics[0]["closed"].(bool) {
		t.Fatalf("topic close not saved: %v %v", topics, err)
	}
	if _, err := b.ManageTopic(42, 9, "reopen", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ManageTopic(42, 9, "edit", "Renamed"); err != nil {
		t.Fatal(err)
	}
	topics, _ = h.store.GetTopics(id, 42)
	if topics[0]["closed"].(bool) || topics[0]["name"] != "Renamed" {
		t.Fatalf("topic edits: %v", topics)
	}
	h.fake.SetHandler("sendMediaGroup", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.FormValue("message_thread_id") != "9" {
			t.Error("album thread lost")
		}
		io.WriteString(w, `{"ok":true,"result":[{"message_id":1,"date":1,"chat":{"id":42},"message_thread_id":9,"media_group_id":"album","photo":[{"file_id":"p1"}]},{"message_id":2,"date":1,"chat":{"id":42},"message_thread_id":9,"media_group_id":"album","photo":[{"file_id":"p2"}]}]}`)
	})
	messages, err := b.SendAlbum(42, 9, json.RawMessage(`[{"type":"photo","media":"p1"},{"type":"photo","media":"p2"}]`))
	if err != nil || len(messages) != 2 {
		t.Fatalf("album: %v %v", messages, err)
	}
	for _, m := range messages {
		saved, err := h.store.GetMessage(id, 42, m.ID)
		if err != nil || saved.MediaGroupID != "album" || saved.ThreadID != 9 {
			t.Fatalf("album metadata: %v %v", saved, err)
		}
	}
}

func TestTelegramInlineKeyboardPersistsAcrossEdits(t *testing.T) {
	h := setupE2E(t)
	id := registerAndManage(h, "inline:edit", "edit")
	b := h.proxy.GetManagedBot(id)
	b.ProcessUpdate(tgbotapi.Update{CallbackQuery: &tgbotapi.CallbackQuery{ID: "cb", InlineMessageID: "inline-id", From: &tgbotapi.User{ID: 7, FirstName: "User"}, Data: "action"}})
	markup := json.RawMessage(`{"inline_keyboard":[[{"text":"Updated","callback_data":"next"}]]}`)
	if err := b.EditInlineKeyboard("inline-id", markup); err != nil {
		t.Fatal(err)
	}
	events, err := h.store.GetCallbackEvents(id, 0, 100)
	if err != nil || string(events[0].ReplyMarkup) != string(markup) {
		t.Fatalf("inline keyboard state not retained: %v %v", events, err)
	}
	if err := b.EditInlineKeyboard("inline-id", json.RawMessage(`{"inline_keyboard":[]}`)); err != nil {
		t.Fatal(err)
	}
	events, err = h.store.GetCallbackEvents(id, 0, 100)
	if err != nil || string(events[0].ReplyMarkup) != `{"inline_keyboard":[]}` {
		t.Fatalf("inline removal: %v %v", events, err)
	}
}

func TestTelegramSubscriptionIncludesInlineAndPayments(t *testing.T) {
	for _, key := range []string{"inline_query", "chosen_inline_result", "shipping_query", "pre_checkout_query", "business_connection", "deleted_business_messages", "purchased_paid_media", "message_reaction_count", "chat_boost", "removed_chat_boost"} {
		if !slices.Contains(bot.AllowedUpdateTypes, key) {
			t.Fatalf("missing subscription: %s", key)
		}
	}
	if len(bot.AllowedUpdateTypes) != 27 {
		t.Fatalf("Bot API 10.3 expects 27 types: %d", len(bot.AllowedUpdateTypes))
	}
}

type repeatedByteReader struct{}

func (repeatedByteReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
func TestTelegramChunkedUploadPreservesTailAndCapture(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	token := "chunked:media"
	id := h.AddBot(models.BotConfig{Token: token})
	const size = int64(51 * 1024 * 1024)
	h.fake.SetHandler("sendDocument", func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil || n != size {
			t.Errorf("chunked upload truncated: %d/%d %v", n, size, err)
		}
		io.WriteString(w, `{"ok":true,"result":{"message_id":7,"date":1,"chat":{"id":42},"document":{"file_id":"large-document"}}}`)
	})
	req, err := http.NewRequest("POST", h.ts.URL+"/tgapi/bot"+token+"/sendDocument", io.LimitReader(repeatedByteReader{}, size))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("upload status %d", resp.StatusCode)
	}
	saved, err := h.store.GetMessage(id, 42, 7)
	if err != nil || saved.FileID != "large-document" {
		t.Fatalf("large send not captured: %v %v", saved, err)
	}
}

func TestTelegramInlineCallbackRoutingExplicitDestination(t *testing.T) {
	h := setupE2E(t)
	src := registerAndManage(h, "inline:src", "src")
	dst := registerAndManage(h, "inline:dst", "dst")
	_, err := h.store.AddRoute(models.Route{SourceBotID: src, TargetBotID: dst, TargetChatID: 200, ConditionType: "text", ConditionValue: "inline-action", Action: "forward", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	update := map[string]any{"update_id": float64(42), "callback_query": map[string]any{"id": "inline-cb", "inline_message_id": "inline-msg", "from": map[string]any{"id": float64(7), "first_name": "User"}, "data": "inline-action"}}
	if !h.proxy.ProcessUpdate(src, update) {
		t.Fatal("inline callback delivery failed")
	}
	if n := h.fake.RequestsCountFor("sendMessage"); n != 1 {
		t.Fatalf("inline callback route not executed: %d", n)
	}
}

func TestTelegramTopicSearchIsIsolated(t *testing.T) {
	st := newTestStore(t)
	for i, thread := range []int{9, 10} {
		if err := st.SaveMessage(models.Message{ID: i + 1, BotID: 1, ChatID: 42, Text: "Matching text", ThreadID: thread, Complete: true}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := st.SearchMessages(1, 42, "Matching", 50, 9)
	if err != nil || len(result) != 1 || result[0].ThreadID != 9 {
		t.Fatalf("topic search included another thread: %v %v", result, err)
	}
}

func TestTelegramPayAndGameKeyboardsCapture(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	token := "special:buttons"
	id := h.AddBot(models.BotConfig{Token: token})
	for _, tc := range []struct {
		method, kind string
		message      int
	}{{"sendInvoice", "invoice", 1}, {"sendGame", "game", 2}} {
		button := map[string]any{"text": "Action"}
		content := map[string]any{"title": "Title", "description": "Description"}
		payload := map[string]any{"chat_id": 42}
		if tc.kind == "invoice" {
			button["pay"] = true
			content["currency"] = "RUB"
			content["total_amount"] = 100
			content["start_parameter"] = "invoice"
			payload["title"] = "Title"
			payload["description"] = "Description"
			payload["payload"] = "invoice"
			payload["currency"] = "RUB"
			payload["prices"] = []any{map[string]any{"label": "Item", "amount": 100}}
		} else {
			button["callback_game"] = map[string]any{}
			payload["game_short_name"] = "example"
		}
		markup := map[string]any{"inline_keyboard": []any{[]any{button}}}
		payload["reply_markup"] = markup
		h.fake.SetHandler(tc.method, func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": tc.message, "date": 1, "chat": map[string]any{"id": 42, "type": "private"}, "from": map[string]any{"id": 100, "is_bot": true, "first_name": "Bot"}, tc.kind: content, "reply_markup": markup}})
		})
		h.CallTgapi(tc.method, token, payload)
		saved, err := h.store.GetMessage(id, 42, tc.message)
		if err != nil || saved.MediaType != tc.kind || len(saved.ReplyMarkup) == 0 || saved.Text == "" {
			t.Fatalf("special keyboard lost: %v %v", saved, err)
		}
		var got map[string]any
		if err := json.Unmarshal(saved.ReplyMarkup, &got); err != nil || !reflect.DeepEqual(got, markup) {
			t.Fatalf("special button action changed: %s", saved.ReplyMarkup)
		}
	}
}

func TestTelegramGeneralTopicUsesDedicatedMethods(t *testing.T) {
	h := setupE2E(t)
	id := registerAndManage(h, "general:topic", "topic")
	b := h.proxy.GetManagedBot(id)
	h.store.UpsertChat(id, models.Chat{ID: 42, Type: "supergroup"})
	for _, tc := range []struct{ action, method string }{{"edit", "editGeneralForumTopic"}, {"close", "closeGeneralForumTopic"}, {"reopen", "reopenGeneralForumTopic"}} {
		if _, err := b.ManageTopic(42, 1, tc.action, "General"); err != nil {
			t.Fatal(err)
		}
		if h.fake.RequestsCountFor(tc.method) != 1 {
			t.Fatalf("general topic did not use %s", tc.method)
		}
	}
}

func TestTelegramProxyLogRedactsTokenAndQueryEphemeralPayload(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	token := "privacy:token"
	botID := h.AddBot(models.BotConfig{Token: token})
	output, err := os.CreateTemp(t.TempDir(), "proxy-log")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	previous := log.Writer()
	log.SetOutput(output)
	defer log.SetOutput(previous)
	req, err := http.NewRequest(http.MethodPost, h.ts.URL+"/tgapi/bot"+token+"/sendMessage?ephemeral_message_parameters=%7B%7D", strings.NewReader(`{"chat_id":100,"text":"private-payload-sentinel"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	data, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) || strings.Contains(string(data), "private-payload-sentinel") {
		t.Fatalf("private data leaked: %s", data)
	}
	messages, err := h.store.GetMessages(botID, 100, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) > 0 {
		t.Fatal("ephemeral request saved to shared history")
	}
	if !strings.Contains(string(data), "[ephemeral payload]") {
		t.Fatalf("request was not logged with redaction: %s", data)
	}
}

func TestTelegramProxyNetworkErrorsDoNotExposeCredentials(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	token := "privacy:network"
	h.AddBot(models.BotConfig{Token: token})
	h.fake.server.Close()
	output, err := os.CreateTemp(t.TempDir(), "proxy-errors")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	previous := log.Writer()
	log.SetOutput(output)
	defer log.SetOutput(previous)
	resp, err := http.Get(h.ts.URL + "/tgapi/bot" + token + "/sendMessage?text=private-query-sentinel&ephemeral_message_parameters=%7B%7D")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	data, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, sensitive := range []string{token, "private-query-sentinel"} {
		if strings.Contains(string(data), sensitive) || strings.Contains(string(body), sensitive) {
			t.Fatalf("network error exposes %q", sensitive)
		}
	}
}
