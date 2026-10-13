package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	tgbotapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/skrashevich/botmux/internal/bot"
	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/server"
)

func TestBotAPI103PollingAndRawUpdates(t *testing.T) {
	h := setupE2E(t, withFastBackoff())
	token := "103:updates"
	id := h.AddBot(models.BotConfig{Token: token, ManageEnabled: true, ProxyEnabled: true, LongPollEnabled: true})
	var update map[string]any
	if err := json.Unmarshal([]byte(`{"update_id":103,"stopped_message_generation":{"chat":{"id":42,"type":"private"},"draft_id":7,"message_thread_id":9}}`), &update); err != nil {
		t.Fatal(err)
	}
	h.fake.EnqueueUpdate(token, update)
	h.proxy.Start()
	h.Eventually(func() bool { return len(h.proxy.GetOrCreateUpdateQueue(id).Get(0, 100)) > 0 }, 3*time.Second, "stopped generation queued")
	got := h.proxy.GetOrCreateUpdateQueue(id).Get(0, 100)
	if !reflect.DeepEqual(got[0].Data, update) {
		t.Fatalf("raw update changed: %#v", got)
	}
	reqs := h.fake.RequestsFor("getUpdates")
	var params struct {
		Allowed []string `json:"allowed_updates"`
	}
	if err := json.Unmarshal(reqs[0].body, &params); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"subscription", "stopped_message_generation"} {
		if !slices.Contains(params.Allowed, key) {
			t.Errorf("polling does not request %s", key)
		}
	}
}

func TestBotAPI103ProxyParameters(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	token := "103:params"
	h.AddBot(models.BotConfig{Token: token})
	for _, tc := range []struct{ method, body string }{
		{"sendRichMessage", `{"chat_id":42,"ephemeral_message_parameters":{"receiver_user_id":7,"callback_query_id":"q","replace_callback_query_message":true},"rich_message":{"blocks":[{"type":"buttons","buttons":[{"text":"Stop","callback_data":"stop","style":"danger"}]},{"type":"document","document":{"type":"document","media":"tg://document?id=123"}}]}}`},
		{"sendMessage", `{"chat_id":42,"text":"Hi","reply_markup":{"inline_keyboard":[[{"text":"Unavailable","disabled":{}}]],"force_reply":true}}`},
		{"sendMessageDraft", `{"chat_id":42,"draft_id":3,"text":"Thinking","can_stop":true,"keep_on_stop":true}`},
		{"sendRichMessageDraft", `{"chat_id":42,"draft_id":3,"rich_message":{"markdown":"Thinking"},"can_stop":true,"keep_on_stop":true}`},
		{"editEphemeralMessageText", `{"chat_id":42,"receiver_user_id":7,"ephemeral_message_id":8,"rich_message":{"markdown":"Updated"}}`},
		{"editEphemeralMessageCaption", `{"chat_id":42,"receiver_user_id":7,"ephemeral_message_id":8,"caption":"Updated","show_caption_above_media":true}`},
		{"promoteChatMember", `{"chat_id":42,"user_id":7,"can_send_welcome_messages":true}`},
	} {
		t.Run(tc.method, func(t *testing.T) {
			var want map[string]any
			if err := json.Unmarshal([]byte(tc.body), &want); err != nil {
				t.Fatal(err)
			}
			h.fake.SetHandler(tc.method, func(w http.ResponseWriter, r *http.Request) {
				var got map[string]any
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("payload changed: %#v", got)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"ok":true,"result":true}`))
			})
			status, resp := h.CallTgapi(tc.method, token, want)
			if status != 200 || resp["result"] != true {
				t.Fatalf("status=%d response=%v", status, resp)
			}
		})
	}
}

func TestBotAPI103TypedRichMessageAndEphemeralIsolation(t *testing.T) {
	st := newTestStore(t)
	id, err := st.AddBotConfig(models.BotConfig{Token: "103:rich"})
	if err != nil {
		t.Fatal(err)
	}
	h := setupE2E(t)
	h.fake.RegisterBot("103:rich", "richbot", 103)
	b, err := bot.NewBot("103:rich", st, id, h.fake.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	var u tgbotapi.Update
	if err := json.Unmarshal([]byte(`{"message":{"message_id":7,"date":1,"chat":{"id":42,"type":"private"},"rich_message":{"blocks":[{"type":"paragraph","text":["Hello",{"type":"button","button":{"text":"Go","callback_data":"secret"}}]}]}}}`), &u); err != nil {
		t.Fatal(err)
	}
	b.ProcessUpdate(u)
	msgs, err := st.GetMessages(id, 42, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].RichText != "Hello\nGo" || msgs[0].MediaType != "rich_message" {
		t.Fatalf("rich message not saved: %#v", msgs)
	}
	u.Message.EphemeralMessageID = 9
	u.Message.Text = "private overlay"
	b.ProcessUpdate(u)
	srv := server.NewServer(st, nil)
	srv.CaptureSentMessage("103:rich", "sendMessage", nil, "", []byte(`{"ok":true,"result":{"message_id":7,"ephemeral_message_id":9,"chat":{"id":42,"type":"private"},"text":"private overlay"}}`))
	msgs, err = st.GetMessages(id, 42, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].RichText != "Hello\nGo" || msgs[0].Text != "" {
		t.Fatalf("ephemeral message overwrote shared feed: %#v", msgs)
	}
}

func TestBotAPI103EphemeralMediaUpload(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	token := "103:upload"
	h.AddBot(models.BotConfig{Token: token})
	media := `{"type":"photo","media":"attach://photo"}`
	h.fake.SetHandler("editEphemeralMessageMedia", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		defer r.MultipartForm.RemoveAll()
		if r.FormValue("media") != media || r.FormValue("ephemeral_message_id") != "8" {
			t.Errorf("multipart fields changed: %v", r.Form)
		}
		file, _, err := r.FormFile("photo")
		if err != nil {
			t.Error(err)
			return
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil || string(data) != "test-image-bytes" {
			t.Errorf("upload changed: %q %v", data, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"result":true}`))
	})
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for key, value := range map[string]string{"chat_id": "42", "receiver_user_id": "7", "ephemeral_message_id": "8", "media": media} {
		if err := mw.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	file, err := mw.CreateFormFile("photo", "image.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("test-image-bytes")); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, h.ts.URL+"/tgapi/bot"+token+"/editEphemeralMessageMedia", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || result["result"] != true {
		t.Fatalf("upload response: %d %v", resp.StatusCode, result)
	}
}

func TestBotAPI103WebhookSubscription(t *testing.T) {
	h := setupE2E(t)
	token := "103:webhook"
	h.fake.RegisterBot(token, "testbot", 103)
	h.fake.SetHandler("setWebhook", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		var allowed []string
		if err := json.Unmarshal([]byte(r.FormValue("allowed_updates")), &allowed); err != nil {
			t.Error(err)
		}
		for _, key := range []string{"subscription", "stopped_message_generation"} {
			if !slices.Contains(allowed, key) {
				t.Errorf("webhook does not request %s", key)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"result":true}`))
	})
	b, err := bot.NewBot(token, h.store, 1, h.fake.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SetWebhook("https://example.com/webhook"); err != nil {
		t.Fatal(err)
	}
}
