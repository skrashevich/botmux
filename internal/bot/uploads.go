package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	tgbotapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/skrashevich/botmux/internal/models"
)

type UploadedFile struct {
	Name, Type, Caption string
	Spoiler             bool
	Reader              io.Reader
}

func (b *Bot) SendUploadedFiles(ctx context.Context, chatID int64, threadID, replyID int, markup json.RawMessage, files []UploadedFile) ([]models.Message, error) {
	if err := ValidateUploadedFiles(markup, files); err != nil {
		return nil, err
	}
	params := tgbotapi.Params{"chat_id": strconv.FormatInt(chatID, 10)}
	if threadID > 0 {
		params["message_thread_id"] = strconv.Itoa(threadID)
	}
	if replyID > 0 {
		data, _ := json.Marshal(map[string]any{"message_id": replyID})
		params["reply_parameters"] = string(data)
	}
	method := ""
	var requestFiles []tgbotapi.RequestFile
	var media []map[string]any
	for i, f := range files {
		methods := map[string]string{"photo": "sendPhoto", "video": "sendVideo", "audio": "sendAudio", "document": "sendDocument", "voice": "sendVoice", "animation": "sendAnimation", "video_note": "sendVideoNote", "sticker": "sendSticker"}
		field := f.Type
		if len(files) > 1 {
			field = fmt.Sprintf("file%d", i)
		}
		requestFiles = append(requestFiles, tgbotapi.RequestFile{Name: field, Data: tgbotapi.FileReader{Name: f.Name, Reader: f.Reader}})
		item := map[string]any{"type": f.Type, "media": "attach://" + field, "caption": f.Caption}
		if f.Spoiler {
			item["has_spoiler"] = true
		}
		media = append(media, item)
		if len(files) == 1 {
			method = methods[f.Type]
			if f.Caption != "" {
				params["caption"] = f.Caption
			}
			if f.Spoiler {
				params["has_spoiler"] = "true"
			}
		}
	}
	if len(files) > 1 {
		method = "sendMediaGroup"
		data, _ := json.Marshal(media)
		params["media"] = string(data)
	} else if len(markup) > 0 && string(markup) != "null" {
		params["reply_markup"] = string(markup)
	}
	response, err := b.api.UploadFilesWithContext(ctx, method, params, requestFiles)
	if err != nil {
		return nil, err
	}
	var sent []tgbotapi.Message
	if len(files) > 1 {
		err = json.Unmarshal(response.Result, &sent)
	} else {
		var m tgbotapi.Message
		err = json.Unmarshal(response.Result, &m)
		sent = []tgbotapi.Message{m}
	}
	if err != nil {
		return nil, err
	}
	if len(sent) != len(files) {
		return nil, fmt.Errorf("Telegram returned an incomplete upload result; check history before resending")
	}
	for _, m := range sent {
		if m.MessageID <= 0 || m.Chat.ID != chatID {
			return nil, fmt.Errorf("Telegram returned an invalid upload result; check history before resending")
		}
	}
	result := make([]models.Message, 0, len(sent))
	var saveErr error
	for _, m := range sent {
		mediaType, fileID := ExtractMedia(&m)
		stored := models.Message{ID: m.MessageID, BotID: b.botID, ChatID: m.Chat.ID, FromUser: "@" + b.api.Self.UserName, FromID: b.api.Self.ID, FromIsBot: true, Date: int64(m.Date) * 1000, Text: m.Caption, MediaType: mediaType, FileID: fileID, ThreadID: m.MessageThreadID, MediaGroupID: m.MediaGroupID, ReplyMarkup: marshalOptional(m.ReplyMarkup), RichMessage: marshalOptional(m.RichMessage), RichText: ExtractRichText(m.RichMessage), HasMediaSpoiler: m.HasMediaSpoiler, Complete: true}
		if m.ReplyToMessage != nil {
			stored.ReplyToID = m.ReplyToMessage.MessageID
		}
		result = append(result, stored)
		if err := b.store.SaveMessage(stored); err != nil {
			saveErr = errors.Join(saveErr, err)
			continue
		}
		if b.onMessageSent != nil {
			b.onMessageSent(b.botID, chatID, stored.Text, stored.ID, stored.ReplyToID)
		}
	}
	if saveErr != nil {
		return result, fmt.Errorf("files sent but history save failed: %w", saveErr)
	}
	return result, nil
}
func (b *Bot) CustomEmojiFile(id string) (string, error) {
	resp, err := b.api.Request(tgbotapi.GetCustomEmojiStickersConfig{CustomEmojiIDs: []string{id}})
	if err != nil {
		return "", err
	}
	var stickers []struct {
		FileID    string `json:"file_id"`
		Thumbnail *struct {
			FileID string `json:"file_id"`
		} `json:"thumbnail"`
	}
	if err := json.Unmarshal(resp.Result, &stickers); err != nil {
		return "", err
	}
	if len(stickers) == 0 {
		return "", fmt.Errorf("emoji not found")
	}
	if stickers[0].Thumbnail != nil {
		return stickers[0].Thumbnail.FileID, nil
	}
	return stickers[0].FileID, nil
}

// ValidateUploadedFiles rejects incompatible albums before any network request.
func ValidateUploadedFiles(markup json.RawMessage, files []UploadedFile) error {
	if len(files) < 1 || len(files) > 10 {
		return fmt.Errorf("select 1–10 files")
	}
	if err := ValidateInlineMarkup(markup); err != nil {
		return err
	}
	if len(files) > 1 && len(markup) > 0 && string(markup) != "null" {
		return fmt.Errorf("album messages cannot have inline keyboards")
	}
	family := ""
	for _, f := range files {
		methods := map[string]string{"photo": "sendPhoto", "video": "sendVideo", "audio": "sendAudio", "document": "sendDocument", "voice": "sendVoice", "animation": "sendAnimation", "video_note": "sendVideoNote", "sticker": "sendSticker"}
		if methods[f.Type] == "" {
			return fmt.Errorf("unsupported upload type %q", f.Type)
		}
		if len(files) > 1 {
			kind := f.Type
			if kind == "photo" || kind == "video" {
				kind = "visual"
			}
			if kind != "visual" && kind != "audio" && kind != "document" {
				return fmt.Errorf("albums support photos/videos, audio only, or documents only")
			}
			if family != "" && family != kind {
				return fmt.Errorf("album media types are incompatible")
			}
			family = kind
		}
		if (f.Type == "video_note" || f.Type == "sticker") && f.Caption != "" {
			return fmt.Errorf("%s cannot have captions", f.Type)
		}
		if f.Spoiler && f.Type != "photo" && f.Type != "video" && f.Type != "animation" {
			return fmt.Errorf("spoilers require photo, video or animation")
		}
	}
	return nil
}
