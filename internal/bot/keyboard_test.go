package bot

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInlineKeyboardActionValidation(t *testing.T) {
	actions := []string{`"callback_data":"yes"`, `"url":"https://example.com"`, `"web_app":{"url":"https://example.com"}`, `"login_url":{"url":"https://example.com"}`, `"switch_inline_query":""`, `"switch_inline_query_current_chat":""`, `"switch_inline_query_chosen_chat":{}`, `"copy_text":{"text":"copy"}`, `"callback_game":{}`, `"pay":true`, `"disabled":{}`}
	for _, action := range actions {
		t.Run(action, func(t *testing.T) {
			if err := ValidateInlineMarkup(json.RawMessage(`{"inline_keyboard":[[{"text":"Action",` + action + `}]]}`)); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, markup := range []string{`{"inline_keyboard":[[{"text":"Bad"}]]}`, `{"inline_keyboard":[[{"text":"Bad","callback_data":"a","url":"https://example.com"}]]}`, `{"inline_keyboard":[[{"text":"Bad","callback_data":""}]]}`, `{"inline_keyboard":[[{"text":"Bad","callback_data":"` + strings.Repeat("я", 33) + `"}]]}`, `{"inline_keyboard":[[{"text":"Bad","callback_data":"a","style":"link"}]]}`} {
		if ValidateInlineMarkup(json.RawMessage(markup)) == nil {
			t.Fatalf("accepted invalid markup: %s", markup)
		}
	}
	if err := ValidateInlineMarkup(json.RawMessage(`{"inline_keyboard":[[{"text":"UTF8","callback_data":"` + strings.Repeat("я", 32) + `"}]]}`)); err != nil {
		t.Fatalf("64 bytes rejected: %v", err)
	}
}
