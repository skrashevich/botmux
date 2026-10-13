package bot

import "testing"

func TestExtractRichText(t *testing.T) {
	got := ExtractRichText(map[string]any{
		"blocks": []any{
			map[string]any{
				"type": "paragraph",
				"text": map[string]any{
					"texts": []any{
						map[string]any{"type": "bold", "text": "Hello"},
						map[string]any{"type": "plain", "text": "world"},
					},
				},
			},
			map[string]any{
				"type":    "footer",
				"caption": "Generated",
			},
		},
	})
	want := "Hello\nworld\nGenerated"
	if got != want {
		t.Fatalf("ExtractRichText() = %q, want %q", got, want)
	}
}

func TestExtractRichText103(t *testing.T) {
	got := ExtractRichText(map[string]any{"blocks": []any{
		map[string]any{"type": "paragraph", "text": []any{"Plain", map[string]any{"type": "button", "button": map[string]any{"text": "Inline", "callback_data": "secret"}}}},
		map[string]any{"type": "buttons", "buttons": []any{map[string]any{"text": "Open", "url": "https://example.com/private", "style": "primary"}}},
		map[string]any{"type": "expandable_blockquote", "text": "Quote", "credit": "Author"},
		map[string]any{"type": "table", "is_compact": true, "rows": []any{map[string]any{"cells": []any{map[string]any{"text": "Cell"}}}}},
		map[string]any{"type": "document", "document": map[string]any{"file_id": "secret", "file_name": "report.pdf"}, "caption": map[string]any{"text": "Report"}},
	}})
	if want := "Plain\nInline\nOpen\nQuote\nAuthor\nCell\nReport\nreport.pdf"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
