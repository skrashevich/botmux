package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/version"
)

// Opt-in fixture for real browser QA. All Telegram requests stay in the local mock.
// BOTMUX_VISUAL_QA_DIR=/tmp/botmux-visual-qa go test ./tests -run TestVisualBrowserHarness -count=1 -timeout=30m
// Read URL from DIR/url, log in as admin / visual-qa-password, create DIR/stop to finish.
func TestVisualBrowserHarness(t *testing.T) {
	dir := os.Getenv("BOTMUX_VISUAL_QA_DIR")
	if dir == "" {
		t.Skip("opt-in browser fixture")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	h, id := visualBot(t)
	hash, err := auth.HashPassword("visual-qa-password")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.store.UpdateUserPassword(1, hash); err != nil {
		t.Fatal(err)
	}
	h.server.VersionChecker = version.NewChecker("qa", "qa", "")
	img := image.NewRGBA(image.Rect(0, 0, 240, 160))
	for y := range 160 {
		for x := range 240 {
			img.Set(x, y, color.RGBA{uint8(20 + x/3), uint8(50 + y), 180, 255})
		}
	}
	var buffer bytes.Buffer
	png.Encode(&buffer, img)
	for _, name := range []string{"qa1.png", "qa2.png"} {
		if err := os.WriteFile(filepath.Join(dir, name), buffer.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	h.fake.PutFile("qa.png", buffer.Bytes(), "image/png")
	h.fake.SetHandler("getFile", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_path": "qa.png", "file_size": buffer.Len()}})
	})
	h.fake.SetHandler("getCustomEmojiStickers", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true,"result":[{"file_id":"qa-emoji","thumbnail":{"file_id":"qa-thumb"}}]}`)
	})
	kb := json.RawMessage(`{"inline_keyboard":[[{"text":"Открыть сайт","url":"https://example.com","style":"primary"},{"text":"Копировать","copy_text":{"text":"QA-value"},"style":"success"}],[{"text":"Ответить","callback_data":"qa:answer","icon_custom_emoji_id":"123"}]]}`)
	rich := json.RawMessage(`{"blocks":[{"type":"section_heading","size":2,"text":"Rich messages — визуальная проверка"},{"type":"paragraph","text":["Обычный текст, ",{"type":"bold","text":"жирный"},", ",{"type":"italic","text":"курсив"},", ",{"type":"marked","text":"маркер"}," и ",{"type":"spoiler","text":"секрет"},"."]},{"type":"table","is_bordered":true,"is_striped":true,"cells":[[{"text":"Возможность","is_header":true},{"text":"Статус","is_header":true}],[{"text":"Таблица"},{"text":"Готово"}],[{"text":"Формула"},{"text":"Ниже"}]]},{"type":"mathematical_expression","expression":"\\frac{a^2+b^2}{c}"},{"type":"mathematical_expression","expression":"\\int_0^\\infty e^{-x^2}\\,dx=\\frac{\\sqrt{\\pi}}{2}"},{"type":"mathematical_expression","expression":"\\begin{pmatrix}1 & 2 \\\\ 3 & 4\\end{pmatrix}"},{"type":"mathematical_expression","expression":"\\sum_{n=1}^{\\infty}\\frac1{n^2}=\\frac{\\pi^2}{6}"},{"type":"details","summary":"Подробнее","blocks":[{"type":"paragraph","text":"Раскрываемый блок работает"}]},{"type":"list","items":[{"has_checkbox":true,"is_checked":true,"blocks":[{"type":"paragraph","text":"Проверить форматирование"}]},{"label":"2.","blocks":[{"type":"paragraph","text":"Проверить загрузку"}]}]},{"type":"block_quotation","blocks":[{"type":"paragraph","text":"Цитата с автором"}],"credit":"QA"},{"type":"collage","blocks":[{"type":"photo","photo":[{"file_id":"qa-photo"}]},{"type":"paragraph","text":"Текст в коллаже"}],"caption":{"text":"Коллаж"}},{"type":"buttons","buttons":[{"text":{"type":"bold","text":"Rich кнопка"},"copy_text":{"text":"Rich QA"}}]},{"type":"preformatted","language":"go","text":"fmt.Println(\"safe HTML: <tag>\")"}]}`)
	for _, m := range []models.Message{{ID: 200, BotID: id, ChatID: 42, Date: time.Now().UnixMilli() - 5000, FromUser: "@visual_bot", FromID: h.proxy.GetManagedBot(id).GetSelfID(), FromIsBot: true, Text: "Сообщение бота с кнопками", ReplyMarkup: kb, Complete: true}, {ID: 201, BotID: id, ChatID: 42, Date: time.Now().UnixMilli() - 4000, FromUser: "@qa", Text: "Первое фото", MediaType: "photo", FileID: "qa-photo1", MediaGroupID: "qa-album", Complete: true}, {ID: 202, BotID: id, ChatID: 42, Date: time.Now().UnixMilli() - 4000, FromUser: "@qa", Text: "Второе фото", MediaType: "photo", FileID: "qa-photo2", MediaGroupID: "qa-album", HasMediaSpoiler: true, Complete: true}, {ID: 203, BotID: id, ChatID: 42, Date: time.Now().UnixMilli() - 3000, FromUser: "@visual_bot", FromID: h.proxy.GetManagedBot(id).GetSelfID(), FromIsBot: true, RichMessage: rich, MediaType: "rich_message", Complete: true}} {
		if err := h.store.SaveMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	var next atomic.Int64
	next.Store(300)
	upload := func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		defer r.MultipartForm.RemoveAll()
		time.Sleep(150 * time.Millisecond)
		base := func(caption string) map[string]any {
			return map[string]any{"message_id": next.Add(1), "date": time.Now().Unix(), "chat": map[string]any{"id": 42, "type": "supergroup"}, "caption": caption, "message_thread_id": 0}
		}
		if r.FormValue("media") != "" {
			var media []map[string]any
			json.Unmarshal([]byte(r.FormValue("media")), &media)
			group := fmt.Sprint(next.Load())
			sent := []map[string]any{}
			for _, item := range media {
				caption, _ := item["caption"].(string)
				m := base(caption)
				m["media_group_id"] = group
				if item["type"] == "photo" {
					m["photo"] = []any{map[string]any{"file_id": "qa-upload", "width": 240, "height": 160}}
				} else {
					m["document"] = map[string]any{"file_id": "qa-upload", "file_name": "QA file"}
				}
				sent = append(sent, m)
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": sent})
		} else {
			m := base(r.FormValue("caption"))
			m["photo"] = []any{map[string]any{"file_id": "qa-upload", "width": 240, "height": 160}}
			if markup := r.FormValue("reply_markup"); markup != "" {
				m["reply_markup"] = json.RawMessage(markup)
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": m})
		}
	}
	for _, method := range []string{"sendPhoto", "sendDocument", "sendMediaGroup"} {
		h.fake.SetHandler(method, upload)
	}
	h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		m := map[string]any{"message_id": next.Add(1), "date": time.Now().Unix(), "chat": map[string]any{"id": 42}, "text": r.FormValue("text")}
		if raw := r.FormValue("reply_markup"); raw != "" {
			m["reply_markup"] = json.RawMessage(raw)
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": m})
	})
	h.fake.SetHandler("editMessageReplyMarkup", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"ok":true,"result":true}`) })
	h.ts = httptest.NewServer(h.server.BuildMux())
	t.Cleanup(h.ts.Close)
	if err := os.WriteFile(filepath.Join(dir, "url"), []byte(h.ts.URL), 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("Browser fixture:", h.ts.URL)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-t.Context().Done():
			return
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(dir, "stop")); err == nil {
				return
			}
		}
	}
}
