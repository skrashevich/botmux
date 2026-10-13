package bot

import (
	"log"
	"time"

	"github.com/skrashevich/botmux/internal/models"
)

// ProcessRawUpdate preserves Bot API fields not yet modeled by the Telegram SDK.
func (b *Bot) ProcessRawUpdate(rawUpdate map[string]any) {
	for _, key := range []string{"message", "edited_message", "channel_post", "edited_channel_post", "business_message", "edited_business_message", "guest_message"} {
		msg, ok := rawUpdate[key].(map[string]any)
		if !ok {
			continue
		}
		b.saveRawRichMessage(msg, key == "channel_post" || key == "edited_channel_post")
	}
}

func (b *Bot) saveRawRichMessage(msg map[string]any, channelPost bool) {
	if msg["ephemeral_message_id"] != nil || msg["receiver_user"] != nil {
		return
	}
	rich, ok := msg["rich_message"]
	if !ok {
		return
	}
	richText := ExtractRichText(rich)

	messageID := rawMapInt(msg["message_id"])
	chat := rawMap(msg["chat"])
	chatID := rawMapInt64(chat["id"])
	if messageID == 0 || chatID == 0 {
		return
	}

	fromUser := ""
	var fromID int64
	var fromIsBot bool
	if from := rawMap(msg["from"]); from != nil {
		fromID = rawMapInt64(from["id"])
		fromIsBot, _ = from["is_bot"].(bool)
		fromUser = rawUserName(from)
	}
	if channelPost {
		fromUser = "Channel"
		if sig, ok := msg["author_signature"].(string); ok && sig != "" {
			fromUser = sig
		}
	}

	replyToID := 0
	if reply := rawMap(msg["reply_to_message"]); reply != nil {
		replyToID = rawMapInt(reply["message_id"])
	}

	text := rawString(msg["text"])
	if text == "" {
		text = rawString(msg["caption"])
	}

	date := rawMapInt64(msg["date"]) * 1000
	if date == 0 {
		date = time.Now().UnixMilli()
	}

	m := models.Message{
		ID:           messageID,
		BotID:        b.botID,
		ChatID:       chatID,
		FromUser:     fromUser,
		FromID:       fromID,
		Text:         text,
		RichText:     richText,
		Date:         date,
		ReplyToID:    replyToID,
		MediaType:    "rich_message",
		FromIsBot:    fromIsBot,
		SenderTag:    rawString(msg["sender_tag"]),
		ReplyMarkup:  marshalOptional(msg["reply_markup"]),
		RichMessage:  marshalOptional(rich),
		ThreadID:     rawMapInt(msg["message_thread_id"]),
		MediaGroupID: rawString(msg["media_group_id"]),
		Complete:     true,
	}
	if err := b.store.SaveMessage(m); err != nil {
		log.Printf("Error saving raw rich message: %v", err)
	}
}

func rawMap(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return nil
}

func rawString(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func rawMapInt(value any) int {
	return int(rawMapInt64(value))
}

func rawMapInt64(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

func rawUserName(user map[string]any) string {
	if username := rawString(user["username"]); username != "" {
		return "@" + username
	}
	name := rawString(user["first_name"])
	if lastName := rawString(user["last_name"]); lastName != "" {
		if name != "" {
			name += " "
		}
		name += lastName
	}
	return name
}
