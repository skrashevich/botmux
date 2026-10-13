package server

import (
	"encoding/json"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/skrashevich/botmux/internal/bot"
)

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	botID, err := strconv.ParseInt(r.URL.Query().Get("bot_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid bot_id", 400)
		return
	}
	chatID, err := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid chat_id", 400)
		return
	}
	if !s.requireBotChatAccess(w, r, botID, chatID) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 510<<20)
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		http.Error(w, "invalid multipart upload or upload too large", 400)
		return
	}
	defer r.MultipartForm.RemoveAll()
	threadID, err := strconv.Atoi(r.FormValue("message_thread_id"))
	if err != nil && r.FormValue("message_thread_id") != "" || threadID < 0 {
		http.Error(w, "invalid topic", 400)
		return
	}
	replyID, err := strconv.Atoi(r.FormValue("reply_to_message_id"))
	if err != nil && r.FormValue("reply_to_message_id") != "" || replyID < 0 {
		http.Error(w, "invalid reply", 400)
		return
	}
	headers := r.MultipartForm.File["files"]
	if len(headers) < 1 || len(headers) > 10 {
		http.Error(w, "select 1–10 files", 400)
		return
	}
	var types, captions []string
	var spoilers []bool
	for _, field := range []struct {
		name   string
		target any
	}{{"types", &types}, {"captions", &captions}, {"spoilers", &spoilers}} {
		if value := r.FormValue(field.name); value != "" {
			if err := json.Unmarshal([]byte(value), field.target); err != nil {
				http.Error(w, "invalid "+field.name, 400)
				return
			}
		}
	}
	if len(types) != 0 && len(types) != len(headers) || len(captions) != 0 && len(captions) != len(headers) || len(spoilers) != 0 && len(spoilers) != len(headers) {
		http.Error(w, "file metadata count mismatch", 400)
		return
	}
	b := s.resolveBot(botID)
	if b == nil {
		http.Error(w, "bot not running", 400)
		return
	}
	files := make([]bot.UploadedFile, 0, len(headers))
	for i, h := range headers {
		if h.Size <= 0 || h.Size > 50<<20 {
			http.Error(w, "files must contain 1 byte to 50 MiB", 400)
			return
		}
		kind := "document"
		ct, _, _ := mime.ParseMediaType(h.Header.Get("Content-Type"))
		if ct == "image/gif" {
			kind = "animation"
		} else if ct == "image/jpeg" || ct == "image/png" || ct == "image/webp" {
			kind = "photo"
		} else if strings.HasPrefix(ct, "video/") {
			kind = "video"
		} else if strings.HasPrefix(ct, "audio/") {
			kind = "audio"
		}
		if len(types) > 0 {
			kind = types[i]
		}
		if kind == "photo" && h.Size > 10<<20 {
			http.Error(w, "photos must not exceed 10 MiB; send larger files as documents", 400)
			return
		}
		caption := ""
		if len(captions) > 0 {
			caption = captions[i]
		}
		if utf8.RuneCountInString(caption) > 1024 {
			http.Error(w, "caption must not exceed 1024 characters", 400)
			return
		}
		f, err := h.Open()
		if err != nil {
			writeError(w, err)
			return
		}
		defer f.Close()
		spoiler := len(spoilers) > 0 && spoilers[i]
		files = append(files, bot.UploadedFile{Name: path.Base(strings.ReplaceAll(h.Filename, "\\", "/")), Reader: f, Type: kind, Caption: caption, Spoiler: spoiler})
	}
	markup := json.RawMessage(r.FormValue("reply_markup"))
	if err := bot.ValidateUploadedFiles(markup, files); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	messages, err := b.SendUploadedFiles(r.Context(), chatID, threadID, replyID, markup, files)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(502)
		writeJSON(w, map[string]any{"error": err.Error(), "sent_count": len(messages)})
		return
	}
	writeJSON(w, messages)
}
func (s *Server) handleCustomEmoji(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" || len(id) > 30 {
		http.Error(w, "emoji id required", 400)
		return
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			http.Error(w, "invalid emoji id", 400)
			return
		}
	}
	botID, err := strconv.ParseInt(r.URL.Query().Get("bot_id"), 10, 64)
	if err != nil || botID <= 0 {
		http.Error(w, "bot_id required", 400)
		return
	}
	if !s.requireBotAccess(w, r, botID) {
		return
	}
	b := s.resolveBot(botID)
	if b == nil {
		http.Error(w, "bot not running", 400)
		return
	}
	fileID, err := b.CustomEmojiFile(id)
	if err != nil {
		http.Error(w, "emoji unavailable", 404)
		return
	}
	query := r.URL.Query()
	query.Set("file_id", fileID)
	r.URL.RawQuery = query.Encode()
	w.Header().Set("Cache-Control", "private, max-age=3600")
	s.handleMediaProxy(w, r)
}
