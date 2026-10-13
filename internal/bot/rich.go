package bot

import (
	"encoding/json"
	"strings"

	tgbotapi "github.com/OvyFlash/telegram-bot-api"
)

// ExtractRichText returns a readable plain-text fallback for Bot API 10.3
// rich_message payloads. The Telegram schema is intentionally represented as
// generic JSON here so botmux can preserve new rich-message updates even when
// the typed Telegram SDK is behind the upstream Bot API.
func ExtractRichText(value any) string {
	if rich, ok := value.(*tgbotapi.RichMessage); ok {
		if rich == nil {
			return ""
		}
		data, err := json.Marshal(rich)
		value = nil
		if err != nil || json.Unmarshal(data, &value) != nil {
			return ""
		}
	}
	var parts []string
	collectRichText(value, &parts)
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func collectRichText(value any, parts *[]string) {
	switch v := value.(type) {
	case map[string]any:
		// Traverse only visible content; never include callback data, file IDs,
		// login URLs or other button actions in the message preview.
		for _, key := range []string{"text", "texts", "title", "subtitle", "url", "blocks", "items", "rows", "cells", "caption", "header", "footer", "quote", "author", "credit", "button", "buttons", "document", "file_name"} {
			if key == "url" && v["type"] == nil {
				continue
			}
			if child, ok := v[key]; ok {
				collectRichText(child, parts)
			}
		}
	case string:
		if strings.TrimSpace(v) != "" {
			*parts = append(*parts, v)
		}
	case []any:
		for _, item := range v {
			collectRichText(item, parts)
		}
	}
}
