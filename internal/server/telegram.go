package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"

	"github.com/skrashevich/botmux/internal/bot"
)

func telegramParams(r *http.Request) (url.Values, error) {
	data, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return telegramParamsFromBody(data, r.URL.Query(), r.Header.Get("Content-Type"))
}
func telegramParamsFromBody(data []byte, query url.Values, ct string) (url.Values, error) {
	params := url.Values{}
	if len(bytes.TrimSpace(data)) > 0 {
		mediaType, options, _ := mime.ParseMediaType(ct)
		switch {
		case mediaType == "application/json" || bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")):
			var object map[string]json.RawMessage
			if err := json.Unmarshal(data, &object); err != nil {
				return nil, err
			}
			for key, value := range object {
				var str string
				if json.Unmarshal(value, &str) == nil {
					params.Set(key, str)
				} else {
					params.Set(key, string(value))
				}
			}
		case mediaType == "multipart/form-data":
			reader := multipart.NewReader(bytes.NewReader(data), options["boundary"])
			form, err := reader.ReadForm(1 << 20)
			if err != nil {
				return nil, err
			}
			defer form.RemoveAll()
			params = form.Value
		default:
			parsed, err := url.ParseQuery(string(data))
			if err != nil {
				return nil, err
			}
			params = parsed
		}
	}
	for key, values := range query {
		params[key] = values
	}
	return params, nil
}
func parseAllowedUpdates(raw string) ([]string, error) {
	var keys []string
	if err := json.Unmarshal([]byte(raw), &keys); err != nil || keys == nil {
		return nil, fmt.Errorf("allowed_updates must be an array of strings")
	}
	if len(keys) == 0 {
		for _, key := range bot.AllowedUpdateTypes {
			if key != "chat_member" && key != "message_reaction" && key != "message_reaction_count" {
				keys = append(keys, key)
			}
		}
	}
	return keys, nil
}
func respRichMessage(result json.RawMessage) json.RawMessage {
	var msg struct {
		Rich json.RawMessage `json:"rich_message"`
	}
	json.Unmarshal(result, &msg)
	return msg.Rich
}
func (s *Server) captureKeyboardEdit(token string, body []byte, ct string, result []byte) {
	var resp struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal(result, &resp) != nil || !resp.OK {
		return
	}
	params, err := telegramParamsFromBody(body, nil, ct)
	if err != nil {
		return
	}
	chatID, _ := strconv.ParseInt(params.Get("chat_id"), 10, 64)
	id, _ := strconv.Atoi(params.Get("message_id"))
	cfg := s.findBotByToken(token)
	if cfg == nil {
		return
	}
	markup := json.RawMessage(params.Get("reply_markup"))
	if chatID != 0 && id > 0 {
		s.store.UpdateReplyMarkup(cfg.ID, chatID, id, markup)
	}
	if inline := params.Get("inline_message_id"); inline != "" {
		s.store.SaveInlineKeyboard(cfg.ID, inline, markup)
	}
}
func (s *Server) handleEditKeyboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	var req struct {
		BotID           int64           `json:"bot_id"`
		ChatID          int64           `json:"chat_id"`
		MessageID       int             `json:"message_id"`
		InlineMessageID string          `json:"inline_message_id"`
		Markup          json.RawMessage `json:"reply_markup"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	if req.InlineMessageID != "" {
		if !s.requireBotAccess(w, r, req.BotID) {
			return
		}
		if !s.store.HasInlineCallback(req.BotID, req.InlineMessageID) {
			http.Error(w, "unknown inline message", 404)
			return
		}
		b := s.resolveBot(req.BotID)
		if b == nil {
			http.Error(w, "bot not running", 400)
			return
		}
		if err := b.EditInlineKeyboard(req.InlineMessageID, req.Markup); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	if !s.requireBotChatAccess(w, r, req.BotID, req.ChatID) {
		return
	}
	if req.MessageID <= 0 {
		http.Error(w, "real Telegram message required", 400)
		return
	}
	m, err := s.store.GetMessage(req.BotID, req.ChatID, req.MessageID)
	if err != nil || m.Deleted {
		http.Error(w, "message not found", 404)
		return
	}
	b := s.resolveBot(req.BotID)
	if b == nil {
		http.Error(w, "bot not running", 400)
		return
	}
	if m.FromID != b.GetSelfID() || !m.FromIsBot {
		http.Error(w, "only this bot's messages can be edited", 403)
		return
	}
	if err := b.EditKeyboard(req.ChatID, req.MessageID, req.Markup); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
func (s *Server) handleCallbacks(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("bot_id"), 10, 64)
	if id == 0 {
		telegramAPIError(w, "bot_id required", 400)
		return
	}
	if !s.requireBotAccess(w, r, id) {
		return
	}
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	events, err := s.store.GetCallbackEvents(id, chatID, 100)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, events)
}
func (s *Server) handleTopics(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("bot_id"), 10, 64)
	chat, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	if id == 0 || chat == 0 {
		telegramAPIError(w, "bot_id and chat_id required", 400)
		return
	}
	if !s.requireBotChatAccess(w, r, id, chat) {
		return
	}
	topics, err := s.store.GetTopics(id, chat)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, topics)
}

func (s *Server) handleAnswerCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	var req struct {
		BotID int64  `json:"bot_id"`
		ID    string `json:"callback_id"`
		Text  string `json:"text"`
		Alert bool   `json:"show_alert"`
		URL   string `json:"url"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.ID == "" {
		http.Error(w, "invalid callback", 400)
		return
	}
	if !s.requireBotAccess(w, r, req.BotID) {
		return
	}
	found, err := s.store.HasCallback(req.BotID, req.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	if !found {
		http.Error(w, "callback not found", 404)
		return
	}

	b := s.resolveBot(req.BotID)
	if b == nil {
		http.Error(w, "bot not running", 400)
		return
	}
	if err := b.AnswerCallback(req.ID, req.Text, req.Alert, req.URL); err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.MarkCallbackAnswered(req.BotID, req.ID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func telegramAPIError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": status, "description": message})
}

func (s *Server) handleSendAlbum(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	var req struct {
		BotID    int64           `json:"bot_id"`
		ChatID   int64           `json:"chat_id"`
		ThreadID int             `json:"message_thread_id"`
		Media    json.RawMessage `json:"media"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.ThreadID < 0 {
		http.Error(w, "invalid album", 400)
		return
	}
	if !s.requireBotChatAccess(w, r, req.BotID, req.ChatID) {
		return
	}
	b := s.resolveBot(req.BotID)
	if b == nil {
		http.Error(w, "bot not running", 400)
		return
	}
	messages, err := b.SendAlbum(req.ChatID, req.ThreadID, req.Media)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, messages)
}
func (s *Server) handleManageTopic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	var req struct {
		BotID    int64  `json:"bot_id"`
		ChatID   int64  `json:"chat_id"`
		ThreadID int    `json:"message_thread_id"`
		Action   string `json:"action"`
		Name     string `json:"name"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "invalid topic request", 400)
		return
	}
	if !s.requireBotChatAccess(w, r, req.BotID, req.ChatID) {
		return
	}
	b := s.resolveBot(req.BotID)
	if b == nil {
		http.Error(w, "bot not running", 400)
		return
	}
	id, err := b.ManageTopic(req.ChatID, req.ThreadID, req.Action, req.Name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "message_thread_id": id})
}
