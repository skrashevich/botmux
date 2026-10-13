package tests

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/proxy"
	"github.com/skrashevich/botmux/internal/store"
)

func TestTelegramQueueRestartAndNoEviction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "durable.db")
	st, err := store.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.AddBotConfig(models.BotConfig{Token: "queue:restart", LongPollEnabled: true, ProxyEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	pm := proxy.NewManager(st, "http://127.0.0.1:1")
	q := pm.GetOrCreateUpdateQueue(id)
	for i := range 1205 {
		if err := q.EnqueueChecked(map[string]any{"update_id": float64(i + 1), "message": map[string]any{"text": "queued"}}); err != nil {
			t.Fatal(err)
		}
	}
	if q.QueueDepth() != 1205 {
		t.Fatalf("queue evicted unread updates: %d", q.QueueDepth())
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	q = proxy.NewManager(st, "http://127.0.0.1:1").GetOrCreateUpdateQueue(id)
	updates, err := q.GetChecked(0, 100)
	if err != nil || len(updates) != 100 || updates[0].UpdateID != 1 {
		t.Fatalf("restart lost updates: %v %v", updates, err)
	}
	updates, err = q.GetChecked(1001, 1000)
	if err != nil || len(updates) != 205 {
		t.Fatalf("durable confirm: %d %v", len(updates), err)
	}
	if err := q.EnqueueChecked(map[string]any{"update_id": float64(1)}); err != nil {
		t.Fatal(err)
	}
	if q.QueueDepth() != 205 {
		t.Fatal("confirmed update redelivered")
	}
	updates, err = q.GetChecked(-2, 100)
	if err != nil || len(updates) != 2 || updates[0].UpdateID != 1204 {
		t.Fatalf("negative offset: %v %v", updates, err)
	}
	if q.QueueDepth() != 2 {
		t.Fatal("negative offset did not discard older pending updates")
	}
}

func TestTelegramQueueFailureDoesNotAcknowledge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "failure.db")
	st, err := store.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.AddBotConfig(models.BotConfig{Token: "queue:failure", ProxyEnabled: true, LongPollEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER deny_queue BEFORE INSERT ON pending_updates BEGIN SELECT RAISE(ABORT,'simulated disk failure'); END`); err != nil {
		t.Fatal(err)
	}
	pm := proxy.NewManager(st, "http://127.0.0.1:1")
	if pm.ProcessUpdate(id, map[string]any{"update_id": float64(42), "message": map[string]any{"text": "lost?"}}) {
		t.Fatal("failed persistence acknowledged")
	}
	cfg, err := st.GetBotConfig(id)
	if err != nil || cfg.Offset != 0 {
		t.Fatalf("offset advanced after failure: %v %v", cfg, err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hook", strings.NewReader(`{"update_id":43}`))
	pm.WebhookHandler(id)(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed persistence webhook status: %d", rec.Code)
	}
}

func TestTelegramSubscriptionsStickyAndIsolated(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	token := "updates:filter"
	id := h.AddBot(models.BotConfig{Token: token, ProxyEnabled: true, LongPollEnabled: true})
	h.proxy.ProcessUpdate(id, map[string]any{"update_id": float64(1), "message": map[string]any{"text": "normal"}})
	h.proxy.ProcessUpdate(id, map[string]any{"update_id": float64(2), "callback_query": map[string]any{"id": "cb"}})
	status, resp := h.CallTgapi("getUpdates", token, map[string]any{"allowed_updates": []string{"callback_query"}, "timeout": 0})
	if status != 200 || len(resp["result"].([]any)) != 1 {
		t.Fatalf("filter ignored: %v", resp)
	}
	_, resp = h.CallTgapi("getUpdates", token, map[string]any{"timeout": 0})
	if len(resp["result"].([]any)) != 1 {
		t.Fatalf("filter not sticky: %v", resp)
	}
	// A second manager reading the same DB must retain subscription state.
	q := proxy.NewManager(h.store, h.fake.URL()).GetOrCreateUpdateQueue(id)
	updates, err := q.GetChecked(0, 100)
	if err != nil || len(updates) != 1 || updates[0].UpdateID != 2 {
		t.Fatalf("persistent subscription: %v %v", updates, err)
	}
	other := h.AddBot(models.BotConfig{Token: "updates:other", ProxyEnabled: true, LongPollEnabled: true})
	h.proxy.ProcessUpdate(other, map[string]any{"update_id": float64(1), "inline_query": map[string]any{"id": "inline"}})
	if len(h.proxy.GetOrCreateUpdateQueue(other).Get(0, 100)) != 1 {
		t.Fatal("filter leaked across bots")
	}
	_, resp = h.CallTgapi("getUpdates", token, map[string]any{"allowed_updates": []string{}, "timeout": 0})
	if len(resp["result"].([]any)) != 2 {
		t.Fatalf("empty array did not restore default subscriptions: %v", resp)
	}
	// Invalid shape must not silently destroy a previous filter.
	status, _ = h.CallTgapi("getUpdates", token, map[string]any{"allowed_updates": "not-json"})
	if status != 400 {
		t.Fatalf("invalid filter accepted: %d", status)
	}
}

func TestTelegramKeyboardWebhookReplyCapture(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	token := "keyboard:webhook"
	id := h.AddBot(models.BotConfig{Token: token, ProxyEnabled: true})
	h.store.SaveMessage(models.Message{ID: 7, BotID: id, ChatID: 42, Text: "Message", ReplyMarkup: json.RawMessage(`{"inline_keyboard":[[{"text":"Old","callback_data":"old"}]]}`), Complete: true})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"method": "editMessageReplyMarkup", "chat_id": 42, "message_id": 7, "reply_markup": map[string]any{"inline_keyboard": []any{}}})
	}))
	defer backend.Close()
	cfg, err := h.store.GetBotConfig(id)
	if err != nil {
		t.Fatal(err)
	}
	cfg.BackendURL = backend.URL
	h.store.UpdateBotConfig(*cfg)
	h.fake.SetHandler("editMessageReplyMarkup", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ok":true,"result":true}`)) })
	if !h.proxy.ProcessUpdate(id, map[string]any{"update_id": float64(1), "callback_query": map[string]any{"id": "cb"}}) {
		t.Fatal("webhook reply failed")
	}
	saved, err := h.store.GetMessage(id, 42, 7)
	if err != nil || string(saved.ReplyMarkup) != `{"inline_keyboard":[]}` {
		t.Fatalf("webhook edit not stored: %v %v", saved, err)
	}
	h.fake.SetHandler("editMessageReplyMarkup", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ok":false,"description":"expired"}`)) })
	if h.proxy.ProcessUpdate(id, map[string]any{"update_id": float64(2)}) {
		t.Fatal("failed Telegram reply acknowledged")
	}
}

func TestTelegramRepeatedDeliveryDoesNotReroute(t *testing.T) {
	h := setupE2E(t)
	src := registerAndManage(h, "receipt:src", "src")
	dst := registerAndManage(h, "receipt:dst", "dst")
	_, err := h.store.AddRoute(models.Route{SourceBotID: src, TargetBotID: dst, SourceChatID: 987654321, TargetChatID: 200, ConditionType: "text", ConditionValue: "option_a", Action: "forward", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	update := loadFixture(t, "update_callback_query.json")
	if !h.proxy.ProcessUpdate(src, update) || !h.proxy.ProcessUpdate(src, update) {
		t.Fatal("delivery failed")
	}
	if h.fake.RequestsCountFor("sendMessage") != 1 {
		t.Fatal("redelivery rerouted callback")
	}
	receipts, err := h.store.GetCallbackEvents(src, 0, 100)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("duplicate callback events: %v %v", receipts, err)
	}
}

func TestTelegramHighUpdateIDAndBotRemoval(t *testing.T) {
	h := setupE2E(t)
	id := h.AddBot(models.BotConfig{Token: "update:high", ProxyEnabled: true, LongPollEnabled: true})
	update := map[string]any{"update_id": float64(1_500_000_000), "inline_query": map[string]any{"id": "query"}}
	if !h.proxy.ProcessUpdate(id, update) {
		t.Fatal("high Telegram update rejected")
	}
	cfg, err := h.store.GetBotConfig(id)
	if err != nil || cfg.Offset != 1_500_000_001 {
		t.Fatalf("artificial update-id ceiling: %v %v", cfg, err)
	}
	if err := h.store.DeleteBotConfig(id); err != nil {
		t.Fatal(err)
	}
	if n, _ := h.store.PendingUpdateCount(id); n != 0 {
		t.Fatal("deleted bot retained pending data")
	}
}

func TestTelegramMigrationPreservesExistingMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	st, err := store.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveMessage(models.Message{ID: 7, BotID: 1, ChatID: 42, Text: "Existing history", Date: 1}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	// Simulate a database from before the new Telegram metadata migration.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"reply_markup", "rich_message", "thread_id", "media_group_id", "callback_id"} {
		if _, err := db.Exec("ALTER TABLE messages DROP COLUMN " + col); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	st, err = store.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m, err := st.GetMessage(1, 42, 7)
	if err != nil || m.Text != "Existing history" {
		t.Fatalf("migration damaged history: %v %v", m, err)
	}
}

func TestTelegramConcurrentCallbackRedelivery(t *testing.T) {
	h := setupE2E(t)
	src := registerAndManage(h, "serial:src", "src")
	dst := registerAndManage(h, "serial:dst", "dst")
	_, err := h.store.AddRoute(models.Route{SourceBotID: src, TargetBotID: dst, SourceChatID: 987654321, TargetChatID: 200, ConditionType: "text", ConditionValue: "option_a", Action: "forward", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	update := loadFixture(t, "update_callback_query.json")
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if !h.proxy.ProcessUpdate(src, update) {
				t.Error("delivery failed")
			}
		})
	}
	wg.Wait()
	if n := h.fake.RequestsCountFor("sendMessage"); n != 1 {
		t.Fatalf("concurrent callback rerouted %d times", n)
	}
}

func TestTelegramReceiptRecoversOffsetAfterRestart(t *testing.T) {
	h := setupE2E(t)
	id := h.AddBot(models.BotConfig{Token: "receipt:recovery", ProxyEnabled: true, LongPollEnabled: true})
	update := map[string]any{"update_id": float64(42), "message": map[string]any{"text": "pending"}}
	if !h.proxy.ProcessUpdate(id, update) {
		t.Fatal("initial delivery failed")
	}
	h.store.UpdateBotOffset(id, 0) // A crash between committing receipt and offset.
	pm := proxy.NewManager(h.store, h.fake.URL())
	if !pm.ProcessUpdate(id, update) {
		t.Fatal("receipt recovery failed")
	}
	cfg, err := h.store.GetBotConfig(id)
	if err != nil || cfg.Offset != 43 {
		t.Fatalf("receipt trapped polling on old offset: %v %v", cfg, err)
	}
	h.store.UpdateBotOffset(id, 100)
	if !pm.ProcessUpdate(id, update) {
		t.Fatal("repeated receipt failed")
	}
	cfg, _ = h.store.GetBotConfig(id)
	if cfg.Offset != 100 {
		t.Fatal("duplicate receipt regressed offset")
	}
}

func TestTelegramPollingHonorsNestedRetryAfter(t *testing.T) {
	h := setupE2E(t, withFastBackoff())
	h.AddBot(models.BotConfig{Token: "retry:params", ProxyEnabled: true, LongPollEnabled: true})
	h.fake.SetHandler("getUpdates", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too many requests","parameters":{"retry_after":1}}`))
	})
	h.proxy.Start()
	h.Eventually(func() bool { return h.fake.RequestsCountFor("getUpdates") > 0 }, time.Second, "first rate-limited request")
	select {
	case <-time.After(200 * time.Millisecond):
	case <-t.Context().Done():
		t.Fatal("test cancelled")
	}
	if n := h.fake.RequestsCountFor("getUpdates"); n != 1 {
		t.Fatalf("retry_after ignored: %d attempts in 200 ms", n)
	}
}
