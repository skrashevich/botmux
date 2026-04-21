package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/models"
)

// pollAPI sends a GET to /api/updates/poll with the given query params and returns the decoded body.
func pollAPI(t *testing.T, h *e2eHarness, botID int64, offset, limit, timeout int) map[string]any {
	t.Helper()
	if h.ts == nil {
		t.Fatal("pollAPI requires withHTTPServer()")
	}
	url := fmt.Sprintf("%s/api/updates/poll?bot_id=%d&offset=%d&limit=%d&timeout=%d",
		h.ts.URL, botID, offset, limit, timeout)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("pollAPI: NewRequest: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: h.session})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("pollAPI: Do: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("pollAPI: ReadAll: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("pollAPI: unmarshal (status=%d body=%s): %v", resp.StatusCode, body, err)
	}
	return out
}

// -------------------------------------------------------------------------
// G-01: getUpdates offset management
// pollLoop receives 3 updates (update_id=1,2,3) and must pass offset=N+1
// on the next request. Verify monotonic offsets in fake.RequestsFor("getUpdates").
// -------------------------------------------------------------------------

func TestE2E_Updates_G01_OffsetManagement(t *testing.T) {
	h := setupE2E(t, withFastBackoff())

	const token = "1001:g01token"
	h.fake.RegisterBot(token, "g01bot", 1001)

	botID := h.AddBot(models.BotConfig{
		Name:          "G01 Bot",
		Token:         token,
		BotUsername:   "g01bot",
		ManageEnabled: true,
		ProxyEnabled:  false,
	})

	// Enqueue 3 updates before starting the proxy so the first poll picks them all up.
	upd1 := loadFixture(t, "update_message_text.json")
	upd2 := loadFixture(t, "update_message_text.json")
	upd3 := loadFixture(t, "update_message_text.json")
	upd1["update_id"] = float64(1)
	upd2["update_id"] = float64(2)
	upd3["update_id"] = float64(3)
	// Give each update a distinct chat/message ID so they're not deduped.
	setMsg := func(u map[string]any, msgID float64) {
		if msg, ok := u["message"].(map[string]any); ok {
			msg["message_id"] = msgID
		}
	}
	setMsg(upd1, 101)
	setMsg(upd2, 102)
	setMsg(upd3, 103)

	h.fake.EnqueueUpdate(token, upd1)
	h.fake.EnqueueUpdate(token, upd2)
	h.fake.EnqueueUpdate(token, upd3)

	// Start polling after updates are enqueued.
	h.proxy.Start()

	// Wait until at least 2 getUpdates requests have been made so we can see the
	// second request carrying offset=4 (after consuming update_ids 1,2,3).
	h.Eventually(func() bool {
		return h.fake.RequestsCountFor("getUpdates") >= 2
	}, 3*time.Second, "at least 2 getUpdates requests")

	reqs := h.fake.RequestsFor("getUpdates")

	// Extract the "offset" value sent in each request body.
	offsets := make([]int, 0, len(reqs))
	for _, r := range reqs {
		var body map[string]any
		if err := json.Unmarshal(r.body, &body); err != nil {
			continue
		}
		if v, ok := body["offset"]; ok {
			switch val := v.(type) {
			case float64:
				offsets = append(offsets, int(val))
			}
		}
	}

	if len(offsets) < 2 {
		t.Fatalf("expected at least 2 offset values, got %d; reqs=%d", len(offsets), len(reqs))
	}

	// Offsets must be strictly monotonically non-decreasing.
	for i := 1; i < len(offsets); i++ {
		if offsets[i] < offsets[i-1] {
			t.Errorf("offset regression at index %d: %d < %d (offsets=%v)", i, offsets[i], offsets[i-1], offsets)
		}
	}

	// After consuming updates 1,2,3 the next offset must be 4.
	found4 := false
	for _, o := range offsets[1:] {
		if o == 4 {
			found4 = true
			break
		}
	}
	if !found4 {
		t.Errorf("expected offset=4 in a subsequent getUpdates call; offsets=%v", offsets)
	}

	// Store the bot ID in the test for traceability (no-op assertion).
	_ = botID
}

// -------------------------------------------------------------------------
// G-02: pollLoop processes update — message appears in store
// -------------------------------------------------------------------------

func TestE2E_Updates_G02_PollLoopProcessesUpdate(t *testing.T) {
	h := setupE2E(t, withFastBackoff())

	const token = "1002:g02token"
	h.fake.RegisterBot(token, "g02bot", 1002)

	botID := h.AddBot(models.BotConfig{
		Name:          "G02 Bot",
		Token:         token,
		BotUsername:   "g02bot",
		ManageEnabled: true,
		ProxyEnabled:  false,
	})

	upd := loadFixture(t, "update_message_text.json")
	// update_message_text.json has chat.id = 987654321 and text = "Hello, bot!"
	chatID := int64(987654321)
	wantText := "Hello, bot!"

	h.proxy.Start()

	// Enqueue AFTER proxy is running so pollLoop has to fetch it.
	h.fake.EnqueueUpdate(token, upd)

	msg := h.WaitForMessage(botID, chatID, func(m models.Message) bool {
		return m.Text == wantText
	})

	if msg.Text != wantText {
		t.Errorf("message text: got %q, want %q", msg.Text, wantText)
	}
	if msg.ChatID != chatID {
		t.Errorf("chat_id: got %d, want %d", msg.ChatID, chatID)
	}
}

// -------------------------------------------------------------------------
// G-03: Long-poll endpoint /api/updates/poll
// Bot with LongPollEnabled=true. Enqueue updates, fetch via HTTP endpoint,
// verify Telegram-compatible response, then verify offset cursor advances.
// -------------------------------------------------------------------------

func TestE2E_Updates_G03_LongPollEndpoint(t *testing.T) {
	h := setupE2E(t, withFastBackoff(), withHTTPServer())

	const token = "1003:g03token"
	h.fake.RegisterBot(token, "g03bot", 1003)

	botID := h.AddBot(models.BotConfig{
		Name:            "G03 Bot",
		Token:           token,
		BotUsername:     "g03bot",
		ManageEnabled:   false,
		ProxyEnabled:    false,
		LongPollEnabled: true,
	})

	// Build two distinct updates.
	upd1 := loadFixture(t, "update_message_text.json")
	upd2 := loadFixture(t, "update_message_text.json")
	upd1["update_id"] = float64(10)
	upd2["update_id"] = float64(11)
	if msg, ok := upd1["message"].(map[string]any); ok {
		msg["message_id"] = float64(201)
	}
	if msg, ok := upd2["message"].(map[string]any); ok {
		msg["message_id"] = float64(202)
	}

	// GetOrCreateUpdateQueue ensures the queue exists before enqueuing.
	// (EnqueueUpdate silently drops if no queue was created yet.)
	q := h.proxy.GetOrCreateUpdateQueue(botID)
	q.Enqueue(upd1)
	q.Enqueue(upd2)

	// First poll — should return both updates.
	resp1 := pollAPI(t, h, botID, 0, 10, 0)

	if ok, _ := resp1["ok"].(bool); !ok {
		t.Fatalf("first poll: ok=false, body=%v", resp1)
	}
	result1, _ := resp1["result"].([]any)
	if len(result1) < 2 {
		t.Errorf("first poll: expected >=2 updates, got %d", len(result1))
	}

	// Extract max update_id from first batch.
	var maxID int64
	for _, item := range result1 {
		if upd, ok := item.(map[string]any); ok {
			if v, ok := upd["update_id"].(float64); ok {
				if int64(v) > maxID {
					maxID = int64(v)
				}
			}
		}
	}

	// Second poll with offset = maxID + 1 — should return 0 updates (queue purged).
	resp2 := pollAPI(t, h, botID, int(maxID+1), 10, 0)

	if ok, _ := resp2["ok"].(bool); !ok {
		t.Fatalf("second poll: ok=false, body=%v", resp2)
	}
	result2, _ := resp2["result"].([]any)
	if len(result2) != 0 {
		t.Errorf("second poll (offset=%d): expected 0 updates, got %d", maxID+1, len(result2))
	}
}

// -------------------------------------------------------------------------
// G-04: Multi-client concurrent poll — at least 1 client receives the update.
// Race-detector clean (run with -race).
// -------------------------------------------------------------------------

func TestE2E_Updates_G04_MultiClientConcurrentPoll(t *testing.T) {
	h := setupE2E(t, withFastBackoff(), withHTTPServer())

	const token = "1004:g04token"
	h.fake.RegisterBot(token, "g04bot", 1004)

	botID := h.AddBot(models.BotConfig{
		Name:            "G04 Bot",
		Token:           token,
		BotUsername:     "g04bot",
		ManageEnabled:   false,
		ProxyEnabled:    false,
		LongPollEnabled: true,
	})

	const numClients = 3
	var received int64 // atomic counter of clients that got >=1 update

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each client polls with timeout=1 so it waits up to 1s for a notification.
			url := fmt.Sprintf("%s/api/updates/poll?bot_id=%d&offset=0&limit=10&timeout=1",
				h.ts.URL, botID)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return
			}
			req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: h.session})

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return
			}
			var out map[string]any
			if err := json.Unmarshal(body, &out); err != nil {
				return
			}
			result, _ := out["result"].([]any)
			if len(result) > 0 {
				atomic.AddInt64(&received, 1)
			}
		}()
	}

	// Give goroutines a moment to block on the long-poll wait.
	time.Sleep(50 * time.Millisecond)

	// Enqueue an update — should wake all blocked waiters.
	upd := loadFixture(t, "update_message_text.json")
	upd["update_id"] = float64(20)
	h.proxy.GetOrCreateUpdateQueue(botID).Enqueue(upd)

	wg.Wait()

	got := atomic.LoadInt64(&received)
	if got == 0 {
		t.Errorf("G-04: 0 out of %d clients received the update", numClients)
	}
}
