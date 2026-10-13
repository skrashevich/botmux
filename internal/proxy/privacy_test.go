package proxy

import (
	"strings"
	"testing"
)

func TestEphemeralSummaryDoesNotExposeContent(t *testing.T) {
	update := map[string]any{"update_id": float64(1), "message": map[string]any{"text": "private-secret", "ephemeral_message_id": float64(9), "receiver_user": map[string]any{"id": float64(7)}}}
	if strings.Contains(summarizeUpdate(update), "private-secret") {
		t.Fatal("ephemeral text leaked into logs")
	}
	if updateMessagePayload(update) != nil {
		t.Fatal("ephemeral message eligible for routing")
	}
}
