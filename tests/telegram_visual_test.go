package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/bot"
	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/store"
)

func visualUpload(t *testing.T, h *e2eHarness, id int64, types []string, fields map[string]string, authenticated bool) (int, []byte) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for i := range types {
		f, err := writer.CreateFormFile("files", fmt.Sprintf("file%d.png", i))
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte("test-file-bytes"))
	}
	encoded, _ := json.Marshal(types)
	writer.WriteField("types", string(encoded))
	for key, value := range fields {
		writer.WriteField(key, value)
	}
	writer.Close()
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/messages/upload?bot_id=%d&chat_id=42", id), &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if authenticated {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: h.session})
	}
	w := httptest.NewRecorder()
	h.server.BuildMux().ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}
func visualBot(t *testing.T) (*e2eHarness, int64) {
	t.Helper()
	h := setupE2E(t)
	id := registerAndManage(h, "visual:files", "visual_bot")
	if err := h.store.UpsertChat(id, models.Chat{ID: 42, Title: "Visual QA", Type: "supergroup"}); err != nil {
		t.Fatal(err)
	}
	return h, id
}
func TestVisualUploadSingleNativeContract(t *testing.T) {
	h, id := visualBot(t)
	markup := `{"inline_keyboard":[[{"text":"Copy","copy_text":{"text":"value"},"style":"primary"}]]}`
	h.fake.SetHandler("sendPhoto", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1024); err != nil {
			t.Error(err)
			return
		}
		defer r.MultipartForm.RemoveAll()
		if r.FormValue("message_thread_id") != "9" || r.FormValue("caption") != "Caption" || r.FormValue("has_spoiler") != "true" || r.FormValue("reply_markup") != markup || r.FormValue("reply_parameters") != `{"message_id":7}` {
			t.Errorf("lost native parameters: %v", r.Form)
		}
		f, _, err := r.FormFile("photo")
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		data, _ := io.ReadAll(f)
		if string(data) != "test-file-bytes" {
			t.Error("wrong bytes")
		}
		io.WriteString(w, `{"ok":true,"result":{"message_id":81,"date":1,"chat":{"id":42,"type":"supergroup"},"message_thread_id":9,"caption":"Caption","photo":[{"file_id":"native-photo"}],"has_media_spoiler":true,"reply_to_message":{"message_id":7},"reply_markup":`+markup+`}}`)
	})
	status, data := visualUpload(t, h, id, []string{"photo"}, map[string]string{"captions": `["Caption"]`, "spoilers": `[true]`, "message_thread_id": "9", "reply_to_message_id": "7", "reply_markup": markup}, true)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	saved, err := h.store.GetMessage(id, 42, 81)
	if err != nil {
		t.Fatal(err)
	}
	if saved.FileID != "native-photo" || saved.ThreadID != 9 || saved.ReplyToID != 7 || !saved.FromIsBot || !saved.HasMediaSpoiler || !visualJSONEqual(saved.ReplyMarkup, json.RawMessage(markup)) {
		t.Fatalf("lost metadata: %+v", saved)
	}
	// Reopen the database to verify persistence instead of just an in-memory response.
	reopened, err := store.NewStore(h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	saved, err = reopened.GetMessage(id, 42, 81)
	if err != nil || !saved.HasMediaSpoiler || !visualJSONEqual(saved.ReplyMarkup, json.RawMessage(markup)) {
		t.Fatalf("restart lost message: %+v, %v", saved, err)
	}
}
func TestVisualUploadAlbumNativeContract(t *testing.T) {
	h, id := visualBot(t)
	h.fake.SetHandler("sendMediaGroup", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1024); err != nil {
			t.Error(err)
			return
		}
		defer r.MultipartForm.RemoveAll()
		var media []map[string]any
		if err := json.Unmarshal([]byte(r.FormValue("media")), &media); err != nil {
			t.Error(err)
		}
		if len(media) != 2 || media[0]["media"] != "attach://file0" || media[1]["media"] != "attach://file1" || media[0]["caption"] != "First" || media[1]["type"] != "video" {
			t.Errorf("invalid media: %v", media)
		}
		for i := range 2 {
			f, _, err := r.FormFile(fmt.Sprintf("file%d", i))
			if err != nil {
				t.Error(err)
			} else {
				f.Close()
			}
		}
		io.WriteString(w, `{"ok":true,"result":[{"message_id":91,"date":1,"chat":{"id":42},"media_group_id":"album1","photo":[{"file_id":"photo1"}]},{"message_id":92,"date":1,"chat":{"id":42},"media_group_id":"album1","video":{"file_id":"video1"}}]}`)
	})
	status, data := visualUpload(t, h, id, []string{"photo", "video"}, map[string]string{"captions": `["First","Second"]`}, true)
	if status != 200 {
		t.Fatalf("%d %s", status, data)
	}
	saved, err := h.store.GetMessages(id, 42, 10, 0)
	if err != nil || len(saved) != 2 || saved[0].MediaGroupID != "album1" || saved[1].FileID != "photo1" {
		t.Fatalf("album not saved: %v %v", saved, err)
	}
}
func TestVisualUploadValidationAndPrivacy(t *testing.T) {
	cases := []struct {
		name   string
		types  []string
		fields map[string]string
		auth   bool
		status int
	}{
		{"authentication", []string{"photo"}, nil, false, 401},
		{"empty", nil, nil, true, 400},
		{"unknown_type", []string{"nope"}, nil, true, 400},
		{"mixed_album", []string{"photo", "audio"}, nil, true, 400},
		{"invalid_album_kind", []string{"voice", "voice"}, nil, true, 400},
		{"malformed_captions", []string{"photo"}, map[string]string{"captions": "invalid"}, true, 400},
		{"count_mismatch", []string{"photo"}, map[string]string{"captions": `["a","b"]`}, true, 400},
		{"caption_limit", []string{"photo"}, map[string]string{"captions": `["` + strings.Repeat("я", 1025) + `"]`}, true, 400},
		{"sticker_caption", []string{"sticker"}, map[string]string{"captions": `["a"]`}, true, 400},
		{"invalid_spoiler", []string{"document"}, map[string]string{"spoilers": `[true]`}, true, 400},
		{"topic", []string{"photo"}, map[string]string{"message_thread_id": "-1"}, true, 400},
		{"album_keyboard", []string{"photo", "photo"}, map[string]string{"reply_markup": `{"inline_keyboard":[[{"text":"Go","callback_data":"go"}]]}`}, true, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, id := visualBot(t)
			before := len(h.fake.Requests())
			code, data := visualUpload(t, h, id, tc.types, tc.fields, tc.auth)
			if code != tc.status {
				t.Fatalf("%d want %d: %s", code, tc.status, data)
			}
			if len(h.fake.Requests()) != before {
				t.Fatal("invalid upload contacted Telegram")
			}
		})
	}
	h, id := visualBot(t)
	code, _ := visualUpload(t, h, id+999, []string{"photo"}, nil, true)
	if code == 200 {
		t.Fatal("foreign bot upload accepted")
	}
	if h.fake.RequestsCountFor("sendPhoto") != 0 {
		t.Fatal("foreign bot upload sent")
	}
}
func TestVisualUploadFailureAndHistoryFailure(t *testing.T) {
	for _, history := range []bool{false, true} {
		t.Run(fmt.Sprint(history), func(t *testing.T) {
			h, id := visualBot(t)
			if history {
				execFault(t, faultDB(t, h), `CREATE TRIGGER deny_upload_history BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'disk unavailable'); END`)
			}
			h.fake.SetHandler("sendDocument", func(w http.ResponseWriter, r *http.Request) {
				if history {
					io.WriteString(w, `{"ok":true,"result":{"message_id":99,"date":1,"chat":{"id":42},"document":{"file_id":"doc1"}}}`)
				} else {
					w.WriteHeader(500)
					io.WriteString(w, `{"ok":false,"description":"offline","error_code":500}`)
				}
			})
			code, data := visualUpload(t, h, id, []string{"document"}, nil, true)
			if code != 502 {
				t.Fatalf("%d %s", code, data)
			}
			var result struct {
				SentCount int `json:"sent_count"`
			}
			json.Unmarshal(data, &result)
			if history && result.SentCount != 1 || !history && result.SentCount != 0 {
				t.Fatalf("wrong delivery report: %s", data)
			}
			if h.fake.RequestsCountFor("sendDocument") != 1 {
				t.Fatal("ambiguous failure retried")
			}
		})
	}
}
func TestVisualAlbumsPageBoundaryAndIsolation(t *testing.T) {
	h, id := visualBot(t)
	for _, m := range []models.Message{{ID: 1, BotID: id, ChatID: 42, Date: 1, MediaGroupID: "same", ThreadID: 9}, {ID: 2, BotID: id, ChatID: 42, Date: 2, MediaGroupID: "same", ThreadID: 9, Text: "match"}, {ID: 3, BotID: id, ChatID: 43, Date: 3, MediaGroupID: "same", ThreadID: 9}, {ID: 4, BotID: id, ChatID: 42, Date: 4, MediaGroupID: "same", ThreadID: 10}, {ID: 5, BotID: id + 1, ChatID: 42, Date: 5, MediaGroupID: "same", ThreadID: 9}} {
		if err := h.store.SaveMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	page, err := h.store.GetMessagesByThread(id, 42, 9, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	expanded, err := h.store.ExpandMessageAlbums(page)
	if err != nil || len(expanded) != 2 || expanded[0].ID != 2 || expanded[1].ID != 1 {
		t.Fatalf("wrong album expansion: %v %v", expanded, err)
	}
	search, err := h.store.SearchMessages(id, 42, "match", 50)
	if err != nil {
		t.Fatal(err)
	}
	expanded, err = h.store.ExpandMessageAlbums(search)
	if err != nil || len(expanded) != 2 {
		t.Fatalf("search truncated album: %v %v", expanded, err)
	}
	dedup, err := h.store.ExpandMessageAlbums(append(page, page...))
	if err != nil || len(dedup) != 2 {
		t.Fatalf("duplicate members: %v %v", dedup, err)
	}
}
func TestVisualEmojiAndAssets(t *testing.T) {
	h, id := visualBot(t)
	h.fake.PutFile("emoji-thumb.png", []byte("PNG-FIXTURE"), "image/png")
	h.fake.SetHandler("getCustomEmojiStickers", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true,"result":[{"file_id":"animated-emoji","thumbnail":{"file_id":"emoji-thumb"}}]}`)
	})
	h.fake.SetHandler("getFile", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true,"result":{"file_path":"emoji-thumb.png","file_size":11}}`)
	})
	for _, logged := range []bool{false, true} {
		r := httptest.NewRequest("GET", fmt.Sprintf("/api/emoji?bot_id=%d&id=123", id), nil)
		if logged {
			r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: h.session})
		}
		w := httptest.NewRecorder()
		h.server.BuildMux().ServeHTTP(w, r)
		if logged {
			if w.Code != 200 || w.Body.String() != "PNG-FIXTURE" || !strings.HasPrefix(w.Header().Get("Cache-Control"), "private") {
				t.Fatalf("emoji: %d %s %v", w.Code, w.Body.String(), w.Header())
			}
		} else if w.Code != 401 {
			t.Fatalf("private emoji publicly accessible: %d", w.Code)
		}
	}
	for _, path := range []string{"telegram-ui.js"} {
		w := httptest.NewRecorder()
		h.server.BuildMux().ServeHTTP(w, httptest.NewRequest("GET", "/assets/"+path, nil))
		if w.Code != 200 || w.Body.Len() == 0 || strings.Contains(w.Body.String(), "<!DOCTYPE html>") {
			t.Fatalf("missing embedded asset %s: %d", path, w.Code)
		}
	}
	for _, path := range []string{"vendor/katex/katex.min.js", "vendor/katex/katex.min.css", "vendor/katex/fonts/KaTeX_Main-Regular.woff2"} {
		w := httptest.NewRecorder()
		h.server.BuildMux().ServeHTTP(w, httptest.NewRequest("GET", "/assets/"+path, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("removed vendor asset still served: %s, status %d", path, w.Code)
		}
	}
}
func TestVisualKeyboardNullAndImmutableReply(t *testing.T) {
	if err := bot.ValidateInlineMarkup(json.RawMessage(`{"inline_keyboard":[[{"text":"Link","url":"https://example.com","callback_data":null,"pay":null,"callback_game":null,"style":null}]]}`)); err != nil {
		t.Fatal(err)
	}
	original := json.RawMessage(`{"inline_keyboard":[],"force_reply":true}`)
	normalized, err := bot.PreserveForceReply(original, json.RawMessage(`{"inline_keyboard":[]}`))
	if err != nil || !bytes.Contains(normalized, []byte(`"force_reply":true`)) {
		t.Fatalf("lost reply setting: %s %v", normalized, err)
	}
	if _, err := bot.PreserveForceReply(original, json.RawMessage(`{"inline_keyboard":[],"force_reply":false}`)); err == nil {
		t.Fatal("immutable setting changed")
	}
}

func visualJSONEqual(a, b json.RawMessage) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

func TestVisualUploadKinds(t *testing.T) {
	for _, kind := range []string{"video", "audio", "document", "voice", "animation", "video_note", "sticker"} {
		t.Run(kind, func(t *testing.T) {
			h, id := visualBot(t)
			method := map[string]string{"video": "sendVideo", "audio": "sendAudio", "document": "sendDocument", "voice": "sendVoice", "animation": "sendAnimation", "video_note": "sendVideoNote", "sticker": "sendSticker"}[kind]
			h.fake.SetHandler(method, func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseMultipartForm(1024); err != nil {
					t.Error(err)
					return
				}
				defer r.MultipartForm.RemoveAll()
				f, _, err := r.FormFile(kind)
				if err != nil {
					t.Error(err)
				} else {
					f.Close()
				}
				json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 77, "chat": map[string]any{"id": 42}, kind: map[string]any{"file_id": "native-" + kind}}})
			})
			status, data := visualUpload(t, h, id, []string{kind}, nil, true)
			if status != 200 {
				t.Fatalf("%d %s", status, data)
			}
			m, err := h.store.GetMessage(id, 42, 77)
			if err != nil || m.FileID != "native-"+kind || m.MediaType != kind {
				t.Fatalf("lost file kind: %+v %v", m, err)
			}
		})
	}
}
func TestVisualUploadAlbumPartialHistoryFailure(t *testing.T) {
	h, id := visualBot(t)
	execFault(t, faultDB(t, h), `CREATE TRIGGER deny_first_album BEFORE INSERT ON messages WHEN NEW.id=91 BEGIN SELECT RAISE(ABORT,'disk failure'); END`)
	h.fake.SetHandler("sendMediaGroup", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true,"result":[{"message_id":91,"chat":{"id":42},"media_group_id":"group","photo":[{"file_id":"p1"}]},{"message_id":92,"chat":{"id":42},"media_group_id":"group","photo":[{"file_id":"p2"}]}]}`)
	})
	status, data := visualUpload(t, h, id, []string{"photo", "photo"}, nil, true)
	var response struct {
		SentCount int `json:"sent_count"`
	}
	json.Unmarshal(data, &response)
	if status != 502 || response.SentCount != 2 {
		t.Fatalf("incorrect delivery count: %d %s", status, data)
	}
	if m, err := h.store.GetMessage(id, 42, 92); err != nil || m.FileID != "p2" {
		t.Fatalf("did not attempt remaining history saves: %+v %v", m, err)
	}
	if h.fake.RequestsCountFor("sendMediaGroup") != 1 {
		t.Fatal("album retried after successful Telegram send")
	}
}
func TestVisualHistoryAPIExpandsAlbum(t *testing.T) {
	h, id := visualBot(t)
	for _, m := range []models.Message{{ID: 1, BotID: id, ChatID: 42, Date: 1, MediaGroupID: "group", Text: "first"}, {ID: 2, BotID: id, ChatID: 42, Date: 2, MediaGroupID: "group", Text: "needle"}} {
		if err := h.store.SaveMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{fmt.Sprintf("/api/messages?bot_id=%d&chat_id=42&limit=1", id), fmt.Sprintf("/api/messages/search?bot_id=%d&chat_id=42&q=needle", id)} {
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: h.session})
		w := httptest.NewRecorder()
		h.server.BuildMux().ServeHTTP(w, r)
		var rows []models.Message
		err := json.Unmarshal(w.Body.Bytes(), &rows)
		if w.Code != 200 || err != nil || len(rows) != 2 {
			t.Fatalf("API truncated album: %s %d %s %v", path, w.Code, w.Body.String(), err)
		}
	}
}
