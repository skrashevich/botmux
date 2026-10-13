package bot

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	tgbotapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/skrashevich/botmux/internal/models"
)

func marshalOptional(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil || string(data) == "null" {
		return nil
	}
	return data
}

func (b *Bot) trackTopic(msg *tgbotapi.Message) error {
	if msg.MessageThreadID == 0 {
		return nil
	}
	name := ""
	if msg.ForumTopicCreated != nil {
		name = msg.ForumTopicCreated.Name
	}
	if msg.ForumTopicEdited != nil {
		name = msg.ForumTopicEdited.Name
	}
	if err := b.store.SaveTopic(b.botID, msg.Chat.ID, msg.MessageThreadID, name); err != nil {
		return fmt.Errorf("topic save: %w", err)
	}
	if msg.ForumTopicClosed != nil || msg.ForumTopicReopened != nil {
		if err := b.store.SetTopicClosed(b.botID, msg.Chat.ID, msg.MessageThreadID, msg.ForumTopicClosed != nil); err != nil {
			return fmt.Errorf("topic state save: %w", err)
		}
	}
	return nil
}

// ValidateInlineMarkup preserves every Telegram button action while validating
// exactly-one-action and the byte limit for callback data.
func ValidateInlineMarkup(markup json.RawMessage) error {
	if len(markup) == 0 || string(markup) == "null" {
		return nil
	}
	var value struct {
		Rows [][]map[string]json.RawMessage `json:"inline_keyboard"`
	}
	if err := json.Unmarshal(markup, &value); err != nil {
		return fmt.Errorf("invalid keyboard: %w", err)
	}
	if value.Rows == nil {
		return fmt.Errorf("inline_keyboard is required")
	}
	actions := []string{"url", "callback_data", "web_app", "login_url", "switch_inline_query", "switch_inline_query_current_chat", "switch_inline_query_chosen_chat", "copy_text", "callback_game", "pay", "disabled"}
	for row, buttons := range value.Rows {
		for col, button := range buttons {
			var text string
			if json.Unmarshal(button["text"], &text) != nil || strings.TrimSpace(text) == "" {
				return fmt.Errorf("button %d:%d requires text", row+1, col+1)
			}
			n := 0
			for _, key := range actions {
				if raw, ok := button[key]; ok && string(raw) != "null" {
					n++
				}
			}
			if n != 1 {
				return fmt.Errorf("button %d:%d must have exactly one action", row+1, col+1)
			}
			if raw, ok := button["callback_data"]; ok {
				var data string
				if json.Unmarshal(raw, &data) != nil || len(data) < 1 || len(data) > 64 {
					return fmt.Errorf("callback_data must contain 1–64 bytes")
				}
			}
			if raw, ok := button["style"]; ok {
				var style string
				if json.Unmarshal(raw, &style) != nil || (style != "" && style != "primary" && style != "success" && style != "danger") {
					return fmt.Errorf("invalid inline button style")
				}
			}
			if raw, ok := button["pay"]; ok {
				var pay bool
				if json.Unmarshal(raw, &pay) != nil || !pay || row != 0 || col != 0 {
					return fmt.Errorf("pay must be true on the first button")
				}
			}
			if _, ok := button["callback_game"]; ok && (row != 0 || col != 0) {
				return fmt.Errorf("game button must be first")
			}
		}
	}
	return nil
}

func (b *Bot) SendConfiguredMessage(chatID int64, text string, replyID, threadID int, markup json.RawMessage) (int, error) {
	if err := ValidateInlineMarkup(markup); err != nil {
		return 0, err
	}
	config := tgbotapi.NewMessage(chatID, text)
	config.ParseMode = "HTML"
	config.ReplyParameters.MessageID = replyID
	config.MessageThreadID = threadID
	if len(markup) > 0 && string(markup) != "null" {
		var kb tgbotapi.InlineKeyboardMarkup
		if err := json.Unmarshal(markup, &kb); err != nil {
			return 0, err
		}
		config.ReplyMarkup = kb
	}
	sent, err := b.api.Send(config)
	if err != nil {
		return 0, err
	}
	media, file := ExtractMedia(&sent)
	m := models.Message{ID: sent.MessageID, BotID: b.botID, ChatID: chatID, FromUser: "@" + b.api.Self.UserName, FromID: b.api.Self.ID, FromIsBot: true, Text: text, Date: int64(sent.Date) * 1000, ReplyToID: replyID, ThreadID: threadID, ReplyMarkup: markup, RichMessage: marshalOptional(sent.RichMessage), RichText: ExtractRichText(sent.RichMessage), MediaType: media, FileID: file, Complete: true}
	if err := b.store.SaveMessage(m); err != nil {
		return 0, err
	}
	if b.onMessageSent != nil {
		b.onMessageSent(b.botID, chatID, text, sent.MessageID, replyID)
	}
	return sent.MessageID, nil
}

func (b *Bot) EditKeyboard(chatID int64, messageID int, markup json.RawMessage) error {
	if messageID <= 0 {
		return fmt.Errorf("a real Telegram message is required")
	}
	if err := ValidateInlineMarkup(markup); err != nil {
		return err
	}
	var kb tgbotapi.InlineKeyboardMarkup
	if len(markup) > 0 {
		if err := json.Unmarshal(markup, &kb); err != nil {
			return err
		}
	}
	config := tgbotapi.NewEditMessageReplyMarkup(chatID, messageID, kb)
	if _, err := b.api.Request(config); err != nil {
		return err
	}
	return b.store.UpdateReplyMarkup(b.botID, chatID, messageID, markup)
}

func (b *Bot) CopyMessageGetID(toChatID, fromChatID int64, messageID int) (int, error) {
	config := tgbotapi.NewCopyMessage(toChatID, fromChatID, messageID)
	resp, err := b.api.Request(config)
	if err != nil {
		return 0, err
	}
	var result struct {
		MessageID int `json:"message_id"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return 0, err
	}
	return result.MessageID, nil
}

// AnswerCallback is explicit: it does not race a backend with automatic answers.
func (b *Bot) AnswerCallback(id, text string, alert bool, url string) error {
	if id == "" || utf8.RuneCountInString(text) > 200 {
		return fmt.Errorf("callback id is required and answer must not exceed 200 characters")
	}
	_, err := b.api.Request(tgbotapi.CallbackConfig{CallbackQueryID: id, Text: text, ShowAlert: alert, URL: url})
	return err
}
func (b *Bot) EditInlineKeyboard(id string, markup json.RawMessage) error {
	if err := ValidateInlineMarkup(markup); err != nil {
		return err
	}
	var kb tgbotapi.InlineKeyboardMarkup
	if len(markup) > 0 {
		if err := json.Unmarshal(markup, &kb); err != nil {
			return err
		}
	}
	config := tgbotapi.EditMessageReplyMarkupConfig{BaseEdit: tgbotapi.BaseEdit{InlineMessageID: id, ReplyMarkup: &kb}}
	_, err := b.api.Request(config)
	if err != nil {
		return err
	}
	return b.store.SaveInlineKeyboard(b.botID, id, markup)
}

func (b *Bot) SendAlbum(chatID int64, threadID int, media json.RawMessage) ([]models.Message, error) {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(media, &items); err != nil || len(items) < 2 || len(items) > 10 {
		return nil, fmt.Errorf("album must contain 2–10 media items")
	}
	params := tgbotapi.Params{"chat_id": fmt.Sprint(chatID), "media": string(media)}
	if threadID > 0 {
		params["message_thread_id"] = fmt.Sprint(threadID)
	}
	response, err := b.api.MakeRequest("sendMediaGroup", params)
	if err != nil {
		return nil, err
	}
	var messages []tgbotapi.Message
	if err := json.Unmarshal(response.Result, &messages); err != nil {
		return nil, err
	}
	result := make([]models.Message, 0, len(messages))
	for _, sent := range messages {
		mediaType, file := ExtractMedia(&sent)
		m := models.Message{ID: sent.MessageID, BotID: b.botID, ChatID: sent.Chat.ID, FromUser: "@" + b.api.Self.UserName, FromID: b.api.Self.ID, FromIsBot: true, Text: sent.Caption, Date: int64(sent.Date) * 1000, MediaType: mediaType, FileID: file, MediaGroupID: sent.MediaGroupID, ThreadID: sent.MessageThreadID, Complete: true}
		if err := b.store.SaveMessage(m); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, nil
}
func (b *Bot) ManageTopic(chatID int64, threadID int, action, name string) (int, error) {
	method := ""
	switch action {
	case "create":
		method = "createForumTopic"
	case "edit":
		method = "editForumTopic"
	case "close":
		method = "closeForumTopic"
	case "reopen":
		method = "reopenForumTopic"
	default:
		return 0, fmt.Errorf("unknown topic action")
	}
	if action != "create" && threadID <= 0 {
		return 0, fmt.Errorf("topic id required")
	}
	if (action == "create" || action == "edit") && strings.TrimSpace(name) == "" {
		return 0, fmt.Errorf("topic name required")
	}
	general := false
	if threadID == 1 && action != "create" {
		chats, err := b.store.GetChats(b.botID)
		if err != nil {
			return 0, err
		}
		for _, chat := range chats {
			if chat.ID == chatID && chat.Type == "supergroup" {
				general = true
				break
			}
		}
	}
	if general {
		switch action {
		case "edit":
			method = "editGeneralForumTopic"
		case "close":
			method = "closeGeneralForumTopic"
		case "reopen":
			method = "reopenGeneralForumTopic"
		}
	}
	params := tgbotapi.Params{"chat_id": fmt.Sprint(chatID)}
	if threadID > 0 && !general {
		params["message_thread_id"] = fmt.Sprint(threadID)
	}
	if name != "" {
		params["name"] = name
	}
	resp, err := b.api.MakeRequest(method, params)
	if err != nil {
		return 0, err
	}
	if action == "create" {
		var topic tgbotapi.ForumTopic
		if err := json.Unmarshal(resp.Result, &topic); err != nil {
			return 0, err
		}
		threadID = topic.MessageThreadID
	}
	if err := b.store.SaveTopic(b.botID, chatID, threadID, name); err != nil {
		return 0, err
	}
	if action == "close" || action == "reopen" {
		if err := b.store.SetTopicClosed(b.botID, chatID, threadID, action == "close"); err != nil {
			return 0, err
		}
	}
	return threadID, nil
}
