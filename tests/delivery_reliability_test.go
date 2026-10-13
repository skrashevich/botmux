package tests

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/bot"
	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/proxy"
	"github.com/skrashevich/botmux/internal/store"
)

func faultDB(t *testing.T, h *e2eHarness) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
func execFault(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
}
func addDeliveryRoute(t *testing.T, h *e2eHarness, src, dest, chat int64, action string) {
	t.Helper()
	if _, err := h.store.AddRoute(models.Route{SourceBotID: src, TargetBotID: dest, TargetChatID: chat, ConditionType: "text", ConditionValue: "hello", Action: action, Enabled: true}); err != nil {
		t.Fatal(err)
	}
}
func retryDeliveries(t *testing.T, pm *proxy.Manager) {
	t.Helper()
	time.Sleep(5 * time.Millisecond)
	if err := pm.RetryRouteDeliveries(t.Context()); err != nil {
		t.Fatal(err)
	}
}
func reopenDelivery(t *testing.T, h *e2eHarness) (*store.Store, *proxy.Manager) {
	t.Helper()
	h.proxy.StopAll()
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := store.NewStore(h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	pm := proxy.NewManager(st, h.fake.URL())
	pm.SetRetryDelays(time.Millisecond, time.Millisecond)
	t.Cleanup(func() { pm.StopAll(); st.Close() })
	return st, pm
}

func TestRouteDeliveriesRetryOnlyFailedSiblingAfterRestart(t *testing.T) {
	h := setupE2E(t, withFastBackoff())
	src := registerAndManage(h, "delivery:source", "src")
	dest := registerAndManage(h, "delivery:dest", "dest")
	addDeliveryRoute(t, h, src, dest, 200, "forward")
	addDeliveryRoute(t, h, src, dest, 300, "forward")
	var fail atomic.Bool
	fail.Store(true)
	h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		chat := strconv.FormatInt(parseChatID(nil, data), 10)
		if chat == "200" && fail.Load() {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"ok":false,"description":"temporary failure"}`)
			return
		}
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":77,"date":1700000000,"chat":{"id":%s},"text":"hello"}}`, chat)
	})
	update := makeTextUpdate(42, 100, 7, "alice", "hello")
	if !h.proxy.ProcessUpdate(src, update) {
		t.Fatal("durably scheduled event must be acknowledged")
	}
	health, err := h.store.QueueHealth(src, time.Now())
	if err != nil || health.RoutePending != 1 || health.RouteFailed != 1 {
		t.Fatalf("health=%+v error=%v", health, err)
	}
	if h.fake.RequestsCountFor("sendMessage") != 2 {
		t.Fatal("sibling was skipped")
	}
	st, pm := reopenDelivery(t, h)
	fail.Store(false)
	retryDeliveries(t, pm)
	if h.fake.RequestsCountFor("sendMessage") != 3 {
		t.Fatal("successful sibling resent or failed sibling lost")
	}
	if !pm.ProcessUpdate(src, update) || h.fake.RequestsCountFor("sendMessage") != 3 {
		t.Fatal("redelivery reran completed jobs")
	}
	for _, chat := range []int64{200, 300} {
		if _, err := st.GetMessage(dest, chat, 77); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHistoryFailurePreventsAcknowledgementAndQueueExposure(t *testing.T) {
	for _, table := range []string{"messages", "chats", "known_users", "callback_events", "topics"} {
		t.Run(table, func(t *testing.T) {
			h := setupE2E(t)
			id := registerAndManage(h, "history:"+table, "history")
			cfg, _ := h.store.GetBotConfig(id)
			cfg.LongPollEnabled = true
			if err := h.store.UpdateBotConfig(*cfg); err != nil {
				t.Fatal(err)
			}
			db := faultDB(t, h)
			execFault(t, db, "CREATE TRIGGER deny_history BEFORE INSERT ON "+table+" BEGIN SELECT RAISE(ABORT,'simulated disk failure'); END")
			update := makeTextUpdate(80, 100, 7, "alice", "hello")
			if table == "callback_events" {
				update = map[string]any{"update_id": float64(80), "callback_query": map[string]any{"id": "history-callback", "from": map[string]any{"id": float64(7)}, "message": map[string]any{"message_id": float64(1), "chat": map[string]any{"id": float64(100)}}}}
			}
			if table == "topics" {
				update["message"].(map[string]any)["message_thread_id"] = float64(2)
			}
			if h.proxy.ProcessUpdate(id, update) {
				t.Fatal("failed history acknowledged")
			}
			cfg, _ = h.store.GetBotConfig(id)
			if cfg.Offset != 0 {
				t.Fatal("offset advanced")
			}
			if n, err := h.store.PendingUpdateCount(id); err != nil || n != 0 {
				t.Fatalf("incomplete history exposed to consumer: %d %v", n, err)
			}
			handled, err := h.store.HandledTelegramUpdate(id, 80)
			if err != nil || handled {
				t.Fatal("failed history marked handled")
			}
			execFault(t, db, "DROP TRIGGER deny_history")
			if !h.proxy.ProcessUpdate(id, update) {
				t.Fatal("recovery did not succeed")
			}
			if n, _ := h.store.PendingUpdateCount(id); n != 1 {
				t.Fatal("recovered event lost")
			}
		})
	}
}

func TestRouteHistoryAndMappingFailuresDoNotResendAfterRestart(t *testing.T) {
	for _, table := range []string{"messages", "route_mappings"} {
		t.Run(table, func(t *testing.T) {
			h := setupE2E(t, withFastBackoff())
			src := registerAndManage(h, "commit:src", "src")
			dest := registerAndManage(h, "commit:dest", "dest")
			addDeliveryRoute(t, h, src, dest, 200, "forward")
			db := faultDB(t, h)
			condition := ""
			if table == "messages" {
				condition = fmt.Sprintf(" WHEN NEW.bot_id=%d", dest)
			}
			execFault(t, db, "CREATE TRIGGER deny_commit BEFORE INSERT ON "+table+condition+" BEGIN SELECT RAISE(ABORT,'local commit unavailable'); END")
			if !h.proxy.ProcessUpdate(src, makeTextUpdate(50, 100, 7, "alice", "hello")) {
				t.Fatal("scheduling failed")
			}
			if h.fake.RequestsCountFor("sendMessage") != 1 {
				t.Fatal("remote send missing")
			}
			st, pm := reopenDelivery(t, h)
			execFault(t, db, "DROP TRIGGER deny_commit")
			retryDeliveries(t, pm)
			if h.fake.RequestsCountFor("sendMessage") != 1 {
				t.Fatal("local commit retry resent message")
			}
			health, err := st.QueueHealth(src, time.Now())
			if err != nil || health.RoutePending != 0 {
				t.Fatalf("health=%+v err=%v", health, err)
			}
			messages, err := st.GetMessages(dest, 200, 10, 0)
			if err != nil || len(messages) != 1 {
				t.Fatalf("history=%v err=%v", messages, err)
			}
			if _, err := st.FindReverseMappingByReply(dest, 200, messages[0].ID); err != nil {
				t.Fatal("reverse mapping lost", err)
			}
		})
	}
}

func TestRouteResultWriteFailureRetriesCheckpointBeforeResending(t *testing.T) {
	h := setupE2E(t, withFastBackoff())
	src := registerAndManage(h, "checkpoint:src", "src")
	dest := registerAndManage(h, "checkpoint:dest", "dest")
	addDeliveryRoute(t, h, src, dest, 200, "forward")
	db := faultDB(t, h)
	execFault(t, db, `CREATE TRIGGER deny_result BEFORE UPDATE OF result ON route_deliveries BEGIN SELECT RAISE(ABORT,'checkpoint unavailable'); END`)
	if !h.proxy.ProcessUpdate(src, makeTextUpdate(51, 100, 7, "alice", "hello")) {
		t.Fatal("schedule failed")
	}
	execFault(t, db, "DROP TRIGGER deny_result")
	retryDeliveries(t, h.proxy)
	if h.fake.RequestsCountFor("sendMessage") != 1 {
		t.Fatal("checkpoint retry repeated remote send")
	}
}

func TestRoutingScheduleAndReceiptFailuresDoNotAcknowledge(t *testing.T) {
	for _, table := range []string{"route_deliveries", "handled_updates"} {
		t.Run(table, func(t *testing.T) {
			h := setupE2E(t)
			src := registerAndManage(h, "schedule:"+table, "src")
			dest := registerAndManage(h, "schedule:dest", "dest")
			addDeliveryRoute(t, h, src, dest, 200, "forward")
			db := faultDB(t, h)
			execFault(t, db, "CREATE TRIGGER deny_schedule BEFORE INSERT ON "+table+" BEGIN SELECT RAISE(ABORT,'schedule unavailable'); END")
			update := makeTextUpdate(52, 100, 7, "alice", "hello")
			if h.proxy.ProcessUpdate(src, update) {
				t.Fatal("uncommitted scheduling acknowledged")
			}
			if h.fake.RequestsCountFor("sendMessage") != 0 {
				t.Fatal("sent before committing schedule")
			}
			execFault(t, db, "DROP TRIGGER deny_schedule")
			if !h.proxy.ProcessUpdate(src, update) || h.fake.RequestsCountFor("sendMessage") != 1 {
				t.Fatal("schedule recovery failed")
			}
		})
	}
}

func TestRouteCopyAndReverseDeliveryRetry(t *testing.T) {
	h := setupE2E(t, withFastBackoff())
	src := registerAndManage(h, "copy:src", "src")
	dest := registerAndManage(h, "copy:dest", "dest")
	addDeliveryRoute(t, h, src, dest, 200, "copy")
	var fail atomic.Bool
	fail.Store(true)
	h.fake.SetHandler("copyMessage", func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"ok":false,"description":"copy failure"}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":99}}`)
	})
	h.proxy.ProcessUpdate(src, makeTextUpdate(60, 100, 7, "alice", "hello"))
	fail.Store(false)
	retryDeliveries(t, h.proxy)
	if _, err := h.store.FindReverseMappingByReply(dest, 200, 99); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"ok":false,"description":"reverse failure"}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":100,"date":1700000000,"chat":{"id":100},"text":"reply"}}`)
	})
	reply := makeTextUpdate(61, 200, 8, "bob", "reply")
	reply["message"].(map[string]any)["reply_to_message"] = map[string]any{"message_id": float64(99)}
	if !h.proxy.ProcessUpdate(dest, reply) {
		t.Fatal("reverse schedule failed")
	}
	fail.Store(false)
	retryDeliveries(t, h.proxy)
	if h.fake.RequestsCountFor("sendMessage") != 2 {
		t.Fatal("reverse failure caused immediate fallback duplicate or retry lost")
	}
}

func TestQueueLimitsBackpressureAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bounded.db")
	st, err := store.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.AddBotConfig(models.BotConfig{Token: "limit:bot", LongPollEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	p := store.DefaultQueuePolicy()
	p.MaxUpdates = 2
	p.WarnUpdates = 1
	p.WarnAge = time.Millisecond
	if err := st.SetQueuePolicy(p); err != nil {
		t.Fatal(err)
	}
	pm := proxy.NewManager(st, "http://127.0.0.1:1")
	for _, i := range []int{1, 2} {
		if !pm.ProcessUpdate(id, map[string]any{"update_id": float64(i)}) {
			t.Fatal("initial intake failed")
		}
	}
	if pm.ProcessUpdate(id, map[string]any{"update_id": float64(3)}) {
		t.Fatal("full queue acknowledged event")
	}
	h, err := st.QueueHealth(id, time.Now())
	if err != nil || h.Pending != 2 || h.Warning == "" {
		t.Fatalf("health=%+v %v", h, err)
	}
	pm.StopAll()
	st.Close()
	st, err = store.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetQueuePolicy(p)
	pm = proxy.NewManager(st, "http://127.0.0.1:1")
	defer pm.StopAll()
	if _, err := pm.GetOrCreateUpdateQueue(id).GetChecked(3, 100); err != nil {
		t.Fatal(err)
	}
	if !pm.ProcessUpdate(id, map[string]any{"update_id": float64(3)}) {
		t.Fatal("intake did not resume after confirmation")
	}
	updates, err := pm.GetOrCreateUpdateQueue(id).GetChecked(0, 100)
	if err != nil || len(updates) != 1 || updates[0].UpdateID != 3 {
		t.Fatalf("backpressure lost update: %v %v", updates, err)
	}
}

func TestQueueByteLimitAndStalledConsumerHealth(t *testing.T) {
	h := setupE2E(t)
	id := h.AddBot(models.BotConfig{Token: "bytes:bot", LongPollEnabled: true})
	p := store.DefaultQueuePolicy()
	p.MaxBytes = 128
	p.WarnAge = time.Millisecond
	if err := h.store.SetQueuePolicy(p); err != nil {
		t.Fatal(err)
	}
	q := h.proxy.GetOrCreateUpdateQueue(id)
	if err := q.EnqueueChecked(map[string]any{"update_id": float64(1), "message": map[string]any{"text": strings.Repeat("x", 200)}}); !errors.Is(err, store.ErrQueueFull) {
		t.Fatalf("byte limit=%v", err)
	}
	if err := q.EnqueueChecked(map[string]any{"update_id": float64(1)}); err != nil {
		t.Fatal(err)
	}
	health, err := h.store.QueueHealth(id, time.Now().Add(time.Hour))
	if err != nil || !strings.Contains(health.Warning, "stalled") {
		t.Fatalf("stalled health=%+v err=%v", health, err)
	}
	if _, err := q.GetChecked(2, 100); err != nil {
		t.Fatal(err)
	}
	health, _ = h.store.QueueHealth(id, time.Now())
	if health.Pending != 0 || health.Bytes != 0 || health.Warning != "" || health.LastPollAt == 0 {
		t.Fatalf("recovered health=%+v", health)
	}
}

func TestQueueMaintenanceCompactsReceiptsWithoutRedelivery(t *testing.T) {
	h := setupE2E(t)
	id := h.AddBot(models.BotConfig{Token: "maintenance:bot", LongPollEnabled: true})
	db := faultDB(t, h)
	for _, i := range []int{1, 2, 3, 5} {
		if !h.proxy.ProcessUpdate(id, map[string]any{"update_id": float64(i)}) {
			t.Fatal("process failed")
		}
	}
	h.proxy.GetOrCreateUpdateQueue(id).GetChecked(4, 100) // update 5 stays unread
	future := time.Now().Add(31 * 24 * time.Hour)
	if err := h.store.MaintainQueues(future); err != nil {
		t.Fatal(err)
	}
	var receipts, ranges int
	db.QueryRow("SELECT COUNT(*) FROM handled_updates WHERE bot_id=?", id).Scan(&receipts)
	db.QueryRow("SELECT COUNT(*) FROM handled_update_ranges WHERE bot_id=?", id).Scan(&ranges)
	if receipts != 1 || ranges != 1 {
		t.Fatalf("compaction receipts=%d ranges=%d", receipts, ranges)
	}
	st, pm := reopenDelivery(t, h)
	for _, i := range []int64{1, 2, 3, 5} {
		handled, err := st.HandledTelegramUpdate(id, i)
		if err != nil || !handled {
			t.Fatalf("receipt %d lost: %v", i, err)
		}
	}
	handled, _ := st.HandledTelegramUpdate(id, 4)
	if handled {
		t.Fatal("compaction filled a missing ID")
	}
	if !pm.ProcessUpdate(id, map[string]any{"update_id": float64(1)}) {
		t.Fatal("duplicate recovery failed")
	}
	if count, _ := st.PendingUpdateCount(id); count != 1 {
		t.Fatal("compacted duplicate re-enqueued or unread event removed")
	}
}

func TestQueueMaintenanceKeepsFailedRoutesAndRollsBackOnError(t *testing.T) {
	h := setupE2E(t)
	src := registerAndManage(h, "prune:src", "src")
	dest := registerAndManage(h, "prune:dest", "dest")
	addDeliveryRoute(t, h, src, dest, 200, "forward")
	h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"ok":false,"description":"down"}`)
	})
	h.proxy.ProcessUpdate(src, makeTextUpdate(90, 100, 7, "alice", "hello"))
	db := faultDB(t, h)
	if err := h.store.MaintainQueues(time.Now().Add(31 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	handled, err := h.store.HandledTelegramUpdate(src, 90)
	if err != nil || !handled {
		t.Fatal("failed job receipt removed")
	}
	health, _ := h.store.QueueHealth(src, time.Now())
	if health.RoutePending != 1 {
		t.Fatal("pending route pruned")
	}
	h.store.MarkTelegramUpdateHandled(src, 91)
	execFault(t, db, `CREATE TRIGGER deny_prune BEFORE DELETE ON handled_updates BEGIN SELECT RAISE(ABORT,'cleanup failure'); END`)
	if err := h.store.MaintainQueues(time.Now().Add(31 * 24 * time.Hour)); err == nil {
		t.Fatal("cleanup error ignored")
	}
	var count int
	db.QueryRow("SELECT COUNT(*) FROM handled_update_ranges WHERE bot_id=?", src).Scan(&count)
	if count != 0 {
		t.Fatal("partial compaction committed")
	}
}

func TestRouteLeaseExclusionAndCrashRecovery(t *testing.T) {
	h := setupE2E(t)
	id := h.AddBot(models.BotConfig{Token: "lease:bot"})
	if err := h.store.ScheduleRouting(store.DeliveryJob{BotID: id, EventKey: "tg:1", Operation: "plan", Payload: json.RawMessage(`{}`)}, 1); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	j, err := h.store.ClaimDelivery("crashed", now, time.Second)
	if err != nil || j == nil {
		t.Fatal("claim failed", err)
	}
	if other, err := h.store.ClaimDelivery("other", now, time.Second); err != nil || other != nil {
		t.Fatal("active lease stolen", err)
	}
	// Reopen the DB without releasing the crashed owner's lease.
	st, pm := reopenDelivery(t, h)
	_ = pm
	if recovered, err := st.ClaimDelivery("restarted", now.Add(2*time.Second), time.Second); err != nil || recovered == nil || recovered.ID != j.ID {
		t.Fatal("expired lease was not recovered", err)
	}
}

func TestRouteConcurrentWorkersSendOnce(t *testing.T) {
	h := setupE2E(t)
	src := registerAndManage(h, "workers:src", "src")
	dest := registerAndManage(h, "workers:dest", "dest")
	addDeliveryRoute(t, h, src, dest, 200, "forward")
	db := faultDB(t, h)
	execFault(t, db, `CREATE TRIGGER deny_claim BEFORE UPDATE OF lease_owner ON route_deliveries BEGIN SELECT RAISE(ABORT,'pause workers'); END`)
	h.proxy.ProcessUpdate(src, makeTextUpdate(92, 100, 7, "alice", "hello"))
	execFault(t, db, "DROP TRIGGER deny_claim")
	other := proxy.NewManager(h.store, h.fake.URL())
	defer other.StopAll()
	var wg sync.WaitGroup
	for _, pm := range []*proxy.Manager{h.proxy, other} {
		wg.Go(func() {
			for range 4 {
				pm.RetryRouteDeliveries(t.Context())
			}
		})
	}
	wg.Wait()
	retryDeliveries(t, h.proxy)
	if h.fake.RequestsCountFor("sendMessage") != 1 {
		t.Fatal("concurrent workers duplicated send")
	}
}

func TestQueueStatusAuthorization(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	id := h.AddBot(models.BotConfig{Token: "status:bot"})
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/queues/status?bot_id=%d", h.ts.URL, id), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "botmux_session", Value: h.session})
	authorized, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	authorized.Body.Close()
	if authorized.StatusCode != 200 {
		t.Fatal("authorized status", authorized.StatusCode)
	}
	resp, err := http.Get(fmt.Sprintf("%s/api/queues/status?bot_id=%d", h.ts.URL, id))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("anonymous status", resp.StatusCode)
	}
}

func TestBotWebhookHistoryFailureReturnsRetryStatus(t *testing.T) {
	h := setupE2E(t)
	id := registerAndManage(h, "webhook:history", "history")
	b, err := bot.NewBot("webhook:history", h.store, id, h.fake.URL())
	if err != nil {
		t.Fatal(err)
	}
	db := faultDB(t, h)
	execFault(t, db, `CREATE TRIGGER deny_webhook_history BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'disk unavailable'); END`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hook", strings.NewReader(`{"update_id":1,"message":{"message_id":1,"date":1,"chat":{"id":100},"text":"hello"}}`))
	b.WebhookHandler()(rec, req)
	if rec.Code != 503 {
		t.Fatal("webhook swallowed history failure", rec.Code)
	}
}

// Set BOTMUX_REMOTE_MOCK to opt in to the user's deployed server. It uses an
// isolated synthetic bot token and does not consume another bot's updates.
func TestRemoteMockDurableRouteRestart(t *testing.T) {
	base := os.Getenv("BOTMUX_REMOTE_MOCK")
	if base == "" {
		t.Skip("set BOTMUX_REMOTE_MOCK for deployed mock integration")
	}
	path := filepath.Join(t.TempDir(), "remote.db")
	st, err := store.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	token := fmt.Sprintf("botmuxqa%d:reliability", time.Now().UnixNano())
	id, err := st.AddBotConfig(models.BotConfig{Token: token})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	forward := httputil.NewSingleHostReverseProxy(upstream)
	director := forward.Director
	forward.Director = func(r *http.Request) { director(r); r.Host = upstream.Host }
	upstreamTransport := http.DefaultTransport.(*http.Transport).Clone()
	upstreamTransport.ForceAttemptHTTP2 = false
	upstreamTransport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	upstreamTransport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
	forward.Transport = upstreamTransport
	defer upstreamTransport.CloseIdleConnections()
	var failOnce atomic.Bool
	failOnce.Store(true)
	transport := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") && failOnce.Swap(false) {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"ok":false,"description":"injected transport outage"}`)
			return
		}
		forward.ServeHTTP(w, r)
	}))
	defer transport.Close()
	pm := proxy.NewManager(st, transport.URL)
	pm.SetRetryDelays(time.Millisecond, time.Millisecond)
	remoteChat := time.Now().UnixMilli() + 8000000000000
	data, _ := json.Marshal(routeActionForRemote(id, remoteChat))
	if err := st.ScheduleRouting(store.DeliveryJob{BotID: id, EventKey: "remote:restart", Operation: "route:remote", Payload: data}, 0); err != nil {
		t.Fatal(err)
	}
	if err := pm.RetryRouteDeliveries(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, err := st.QueueHealth(id, time.Now())
	if err != nil || before.RoutePending != 1 || before.RouteFailed != 1 {
		t.Fatalf("injected outage was not retained: %+v %v", before, err)
	}
	pm.StopAll()
	st.Close()
	st, err = store.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pm = proxy.NewManager(st, transport.URL)
	pm.SetRetryDelays(time.Millisecond, time.Millisecond)
	defer pm.StopAll()
	if err := pm.RetryRouteDeliveries(t.Context()); err != nil {
		t.Fatal(err)
	}
	health, err := st.QueueHealth(id, time.Now())
	if err != nil || health.RoutePending != 0 {
		t.Fatalf("remote delivery=%+v err=%v", health, err)
	}
	messages, err := st.GetMessages(id, remoteChat, 10, 0)
	if err != nil || len(messages) != 1 {
		t.Fatalf("remote captured history=%v err=%v", messages, err)
	}
	// Read the actual Telegram result back; this checks more than an HTTP 200.
	params := strings.NewReader(fmt.Sprintf("chat_id=%d&message_id=%d&text=botmux-reliability-verified", remoteChat, messages[0].ID))
	resp, err := http.Post(base+"/bot"+token+"/editMessageText", "application/x-www-form-urlencoded", params)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct {
		OK      bool `json:"ok"`
		Message struct {
			ID   int    `json:"message_id"`
			Text string `json:"text"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || !result.OK || result.Message.ID != messages[0].ID || result.Message.Text != "botmux-reliability-verified" {
		t.Fatalf("remote read-back=%+v err=%v", result, err)
	}
	// Clean up only the message created by this test.
	resp2, err := http.Post(base+"/bot"+token+"/deleteMessage", "application/x-www-form-urlencoded", strings.NewReader(fmt.Sprintf("chat_id=%d&message_id=%d", remoteChat, messages[0].ID)))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
}
func routeActionForRemote(id, chat int64) map[string]any {
	return map[string]any{"target_bot_id": id, "target_chat_id": chat, "action": "forward", "text": "botmux isolated reliability QA"}
}

func TestHistoryRetryAfterDatabaseRestart(t *testing.T) {
	h := setupE2E(t)
	id := registerAndManage(h, "history:restart", "history")
	db := faultDB(t, h)
	execFault(t, db, `CREATE TRIGGER deny_restart_history BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'disk unavailable'); END`)
	update := makeTextUpdate(95, 100, 7, "alice", "hello")
	if h.proxy.ProcessUpdate(id, update) {
		t.Fatal("history error acknowledged")
	}
	st, pm := reopenDelivery(t, h)
	execFault(t, db, "DROP TRIGGER deny_restart_history")
	b, err := bot.NewBot("history:restart", st, id, h.fake.URL())
	if err != nil {
		t.Fatal(err)
	}
	pm.RegisterManagedBot(id, b)
	if !pm.ProcessUpdate(id, update) {
		t.Fatal("history did not recover after restart")
	}
	messages, err := st.GetMessages(id, 100, 10, 0)
	if err != nil || len(messages) != 1 {
		t.Fatalf("history=%v err=%v", messages, err)
	}
}

func TestLLMDeliveryRetriesDecisionAndPersistsItBeforeSend(t *testing.T) {
	h := setupE2E(t, withFastBackoff())
	src := registerAndManage(h, "llmretry:src", "src")
	dest := registerAndManage(h, "llmretry:dest", "dest")
	var decisions atomic.Int64
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if decisions.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		answer := fmt.Sprintf(`{"target_bot_id":%d,"target_chat_id":200,"action":"forward"}`, dest)
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": answer}}}})
	}))
	defer llm.Close()
	if err := h.store.SaveLLMConfig(models.LLMConfig{APIURL: llm.URL, APIKey: "test", Model: "test", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	var fail atomic.Bool
	fail.Store(true)
	h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"ok":false,"description":"send unavailable"}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":123,"date":1,"chat":{"id":200},"text":"hello"}}`)
	})
	if !h.proxy.ProcessUpdate(src, makeTextUpdate(96, 100, 7, "alice", "hello")) {
		t.Fatal("LLM scheduling failed")
	}
	st, pm := reopenDelivery(t, h)
	retryDeliveries(t, pm)
	if decisions.Load() != 2 {
		t.Fatal("LLM decision retry missing")
	}
	fail.Store(false)
	retryDeliveries(t, pm)
	if decisions.Load() != 2 {
		t.Fatal("LLM decision repeated after a send failure")
	}
	if _, err := st.GetMessage(dest, 200, 123); err != nil {
		t.Fatal(err)
	}
}

func TestRouteRateLimitPersistsRetryAfter(t *testing.T) {
	h := setupE2E(t, withFastBackoff())
	src := registerAndManage(h, "flood:src", "src")
	dest := registerAndManage(h, "flood:dest", "dest")
	addDeliveryRoute(t, h, src, dest, 200, "forward")
	h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"ok":false,"error_code":429,"description":"flood wait","parameters":{"retry_after":3600}}`)
	})
	before := time.Now()
	h.proxy.ProcessUpdate(src, makeTextUpdate(97, 100, 7, "alice", "hello"))
	db := faultDB(t, h)
	var due int64
	if err := db.QueryRow("SELECT next_attempt FROM route_deliveries WHERE bot_id=? AND operation LIKE 'route:%'", src).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if time.UnixMilli(due).Sub(before) < time.Hour {
		t.Fatal("retry_after was capped or ignored")
	}
	retryDeliveries(t, h.proxy)
	if h.fake.RequestsCountFor("sendMessage") != 1 {
		t.Fatal("flood wait retried early")
	}
}

func TestBackgroundWorkerResumesAfterRestartWithoutRedelivery(t *testing.T) {
	h := setupE2E(t, withFastBackoff())
	src := registerAndManage(h, "background:src", "src")
	dest := registerAndManage(h, "background:dest", "dest")
	addDeliveryRoute(t, h, src, dest, 200, "forward")
	var fail atomic.Bool
	fail.Store(true)
	h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"ok":false,"description":"offline"}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":124,"date":1,"chat":{"id":200},"text":"hello"}}`)
	})
	if !h.proxy.ProcessUpdate(src, makeTextUpdate(98, 100, 7, "alice", "hello")) {
		t.Fatal("schedule failed")
	}
	st, pm := reopenDelivery(t, h)
	fail.Store(false)
	pm.Start()
	h.Eventually(func() bool {
		health, err := st.QueueHealth(src, time.Now())
		return err == nil && health.RoutePending == 0
	}, 3*time.Second, "background worker did not resume delivery")
	if h.fake.RequestsCountFor("sendMessage") != 2 {
		t.Fatal("background worker resent successful work")
	}
}

func TestRouteDispatcherCancellationRetainsPendingJob(t *testing.T) {
	h := setupE2E(t, withFastBackoff())
	src := registerAndManage(h, "cancel:src", "src")
	dest := registerAndManage(h, "cancel:dest", "dest")
	addDeliveryRoute(t, h, src, dest, 200, "forward")
	db := faultDB(t, h)
	execFault(t, db, `CREATE TRIGGER pause_cancel_claim BEFORE UPDATE OF lease_owner ON route_deliveries BEGIN SELECT RAISE(ABORT,'paused'); END`)
	h.proxy.ProcessUpdate(src, makeTextUpdate(99, 100, 7, "alice", "hello"))
	execFault(t, db, "DROP TRIGGER pause_cancel_claim")
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.proxy.RetryRouteDeliveries(ctx) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("send not entered")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation ignored", err)
		}
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not cancel HTTP send")
	}
	health, err := h.store.QueueHealth(src, time.Now())
	if err != nil || health.RoutePending != 1 {
		t.Fatal("cancellation discarded job", err)
	}
}

func TestDeliveryMigrationPreservesLegacyQueuesAndReceipts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-queues.db")
	st, err := store.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.AddBotConfig(models.BotConfig{Token: "legacy:queues", LongPollEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueTelegramUpdate(id, map[string]any{"update_id": float64(120), "message": map[string]any{"text": "legacy unread"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTelegramUpdateHandled(id, 120); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"DROP TABLE route_deliveries", "DROP TABLE handled_update_ranges", "ALTER TABLE pending_updates DROP COLUMN created_at", "ALTER TABLE pending_updates DROP COLUMN size_bytes", "ALTER TABLE update_consumers DROP COLUMN last_poll_at", "ALTER TABLE handled_updates DROP COLUMN handled_at"} {
		execFault(t, db, query)
	}
	db.Close()
	st, err = store.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	health, err := st.QueueHealth(id, time.Now())
	if err != nil || health.Pending != 1 || health.Bytes == 0 || health.OldestAt == 0 {
		t.Fatalf("migrated health=%+v err=%v", health, err)
	}
	handled, err := st.HandledTelegramUpdate(id, 120)
	if err != nil || !handled {
		t.Fatal("legacy receipt lost", err)
	}
	updates, err := st.PendingTelegramUpdates(id, 0, 100)
	if err != nil || len(updates) != 1 || updates[0].UpdateID != 120 {
		t.Fatalf("legacy queue=%+v err=%v", updates, err)
	}
}
