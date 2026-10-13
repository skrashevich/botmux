package bot

import tgbotapi "github.com/OvyFlash/telegram-bot-api"

// ExtractMedia returns media type and file_id from a Telegram message.
func ExtractMedia(msg *tgbotapi.Message) (mediaType, fileID string) {
	if msg == nil {
		return "", ""
	}
	switch {
	case msg.Invoice != nil:
		mediaType = "invoice"
	case msg.Game != nil:
		mediaType = "game"
	case msg.RichMessage != nil:
		mediaType = "rich_message"
	case msg.LivePhoto != nil:
		mediaType = "live_photo"
		if len(msg.LivePhoto.Photo) > 0 {
			fileID = msg.LivePhoto.Photo[len(msg.LivePhoto.Photo)-1].FileID
		} else {
			fileID = msg.LivePhoto.FileID
		}
	case len(msg.Photo) > 0:
		mediaType = "photo"
		fileID = msg.Photo[len(msg.Photo)-1].FileID
	case msg.Video != nil:
		mediaType = "video"
		fileID = msg.Video.FileID
	case msg.Animation != nil:
		mediaType = "animation"
		fileID = msg.Animation.FileID
	case msg.Sticker != nil:
		mediaType = "sticker"
		if msg.Sticker.Thumbnail != nil {
			fileID = msg.Sticker.Thumbnail.FileID
		} else {
			fileID = msg.Sticker.FileID
		}
	case msg.Voice != nil:
		mediaType = "voice"
		fileID = msg.Voice.FileID
	case msg.Audio != nil:
		mediaType = "audio"
		fileID = msg.Audio.FileID
	case msg.Document != nil:
		mediaType = "document"
		fileID = msg.Document.FileID
	case msg.VideoNote != nil:
		mediaType = "video_note"
		fileID = msg.VideoNote.FileID
	}
	return mediaType, fileID
}

// LivePhotoFileID picks a preview file_id from a live_photo JSON object (API responses).
func LivePhotoFileID(live map[string]any) string {
	if live == nil {
		return ""
	}
	if photos, ok := live["photo"].([]any); ok && len(photos) > 0 {
		if last, ok := photos[len(photos)-1].(map[string]any); ok {
			if id, ok := last["file_id"].(string); ok {
				return id
			}
		}
	}
	if id, ok := live["file_id"].(string); ok {
		return id
	}
	return ""
}
