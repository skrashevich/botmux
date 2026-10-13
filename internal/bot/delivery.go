package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	tgbotapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/skrashevich/botmux/internal/models"
)

// SendRoute sends only the remote request. The durable dispatcher commits its
// response before writing history, so local failures do not repeat the send.
func (b *Bot) SendRoute(ctx context.Context, chatID, sourceBotID, sourceChatID int64, messageID, replyID int, text, action string) (models.Message, error) {
	var m models.Message
	if action == "copy" {
		resp, err := b.routeRequest(ctx, "copyMessage", map[string]any{"chat_id": chatID, "from_chat_id": sourceChatID, "message_id": messageID})
		if err != nil {
			return m, err
		}
		var result struct {
			ID int `json:"message_id"`
		}
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			return m, err
		}
		if source, err := b.store.GetMessage(sourceBotID, sourceChatID, messageID); err == nil {
			m = *source
		}
		m.ID = result.ID
		m.ReplyMarkup = nil
		m.ReplyToID = 0
		m.ThreadID = 0
		m.MediaGroupID = ""
		m.CallbackID = ""
		m.Deleted = false
		m.Date = time.Now().UnixMilli()
	} else {
		params := map[string]any{"chat_id": chatID, "text": text}
		if replyID > 0 {
			params["reply_parameters"] = map[string]any{"message_id": replyID, "allow_sending_without_reply": true}
		}
		resp, err := b.routeRequest(ctx, "sendMessage", params)
		if err != nil {
			return m, err
		}
		var sent tgbotapi.Message
		if err := json.Unmarshal(resp.Result, &sent); err != nil {
			return m, err
		}
		media, file := ExtractMedia(&sent)
		m = models.Message{ID: sent.MessageID, Text: text, Date: int64(sent.Date) * 1000, MediaType: media, FileID: file, RichMessage: marshalOptional(sent.RichMessage), RichText: ExtractRichText(sent.RichMessage), ReplyMarkup: marshalOptional(sent.ReplyMarkup), ThreadID: sent.MessageThreadID}
		if sent.ReplyToMessage != nil {
			m.ReplyToID = sent.ReplyToMessage.MessageID
		}
	}
	if m.ID <= 0 {
		return models.Message{}, fmt.Errorf("Telegram did not return a message ID")
	}
	m.BotID = b.botID
	m.ChatID = chatID
	m.FromID = b.api.Self.ID
	m.FromUser = "@" + b.api.Self.UserName
	m.FromIsBot = true
	m.Complete = true
	return m, nil
}

// NotifyRouteSent runs only after the durable delivery result has been committed.
func (b *Bot) NotifyRouteSent(m models.Message) {
	if b.onMessageSent != nil {
		b.onMessageSent(m.BotID, m.ChatID, m.Text, m.ID, m.ReplyToID)
	}
}

func (b *Bot) routeRequest(ctx context.Context, method string, params map[string]any) (*tgbotapi.APIResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	base := b.baseURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/bot"+b.api.Token+"/"+method, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.api.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, err
	}
	var result tgbotapi.APIResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	if !result.Ok {
		var retry struct {
			Parameters tgbotapi.ResponseParameters `json:"parameters"`
		}
		json.Unmarshal(body, &retry)
		return nil, tgbotapi.Error{Code: result.ErrorCode, Message: result.Description, ResponseParameters: retry.Parameters}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Telegram returned status %d", resp.StatusCode)
	}
	return &result, nil
}
