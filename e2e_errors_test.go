package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/models"
)

// TestE2E_Errors covers error paths in pollLoop and the /tgapi/ proxy handler.
// All subtests use withFastBackoff() so retries complete in <10ms instead of seconds.
func TestE2E_Errors(t *testing.T) {

	// -----------------------------------------------------------------
	// E-01: getUpdates 400 Bad Request
	// pollLoop must retry on 400 and not stop permanently.
	// -----------------------------------------------------------------
	t.Run("E-01_getUpdates_400", func(t *testing.T) {
		h := setupE2E(t, withFastBackoff())

		h.fake.SetHandler("getUpdates", func(w http.ResponseWriter, r *http.Request) {
			h.fake.writeJSON(w, 400, map[string]any{
				"ok":          false,
				"error_code":  400,
				"description": "Bad Request: message text is empty",
			})
		})

		token := "err01:T"
		h.fake.RegisterBot(token, "errbot01", 10001)
		botID := h.AddBot(models.BotConfig{
			Token:         token,
			Name:          "errbot01",
			BotUsername:   "errbot01",
			ManageEnabled: true,
		})

		if err := h.proxy.RestartBot(botID); err != nil {
			t.Fatalf("RestartBot: %v", err)
		}

		// With retryDelayInitial=1ms, retryDelayMax=10ms we should see >=2 requests
		// within 100ms even with exponential backoff.
		h.Eventually(func() bool {
			return h.fake.RequestsCountFor("getUpdates") >= 2
		}, 100*time.Millisecond, "expected >=2 getUpdates retries after 400")
	})

	// -----------------------------------------------------------------
	// E-02: getUpdates 401 Unauthorized
	// Observed behaviour: pollLoop retries indefinitely (no stop-on-401 logic).
	// This test documents the actual behaviour. If the project later adds
	// "stop on 401", this test must be updated.
	// -----------------------------------------------------------------
	t.Run("E-02_getUpdates_401", func(t *testing.T) {
		h := setupE2E(t, withFastBackoff())

		h.fake.SetHandler("getUpdates", func(w http.ResponseWriter, r *http.Request) {
			h.fake.writeJSON(w, 401, map[string]any{
				"ok":          false,
				"error_code":  401,
				"description": "Unauthorized",
			})
		})

		token := "err02:T"
		h.fake.RegisterBot(token, "errbot02", 10002)
		botID := h.AddBot(models.BotConfig{
			Token:         token,
			Name:          "errbot02",
			BotUsername:   "errbot02",
			ManageEnabled: true,
		})

		if err := h.proxy.RestartBot(botID); err != nil {
			t.Fatalf("RestartBot: %v", err)
		}

		// Observed behaviour: pollLoop does NOT stop on 401 — it retries with backoff.
		// Verify >=2 retries, meaning the loop keeps running despite the 401.
		h.Eventually(func() bool {
			return h.fake.RequestsCountFor("getUpdates") >= 2
		}, 100*time.Millisecond, "expected pollLoop to retry on 401 (does not stop)")
	})

	// -----------------------------------------------------------------
	// E-03: /tgapi/sendMessage 400 passthrough
	// Server must pass the 400 response straight back to the caller.
	// captureSentMessage must NOT store anything (capture only on 200).
	// -----------------------------------------------------------------
	t.Run("E-03_tgapi_sendMessage_400_passthrough", func(t *testing.T) {
		h := setupE2E(t, withHTTPServer())

		h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) {
			h.fake.writeJSON(w, 400, map[string]any{
				"ok":          false,
				"error_code":  400,
				"description": "Bad Request: message text is empty",
			})
		})

		token := "err03:T"
		h.fake.RegisterBot(token, "errbot03", 10003)
		h.AddBot(models.BotConfig{
			Token:       token,
			Name:        "errbot03",
			BotUsername: "errbot03",
		})

		status, body := h.CallTgapi("sendMessage", token, map[string]any{
			"chat_id": 999,
			"text":    "",
		})

		if status != 400 {
			t.Errorf("expected status 400, got %d", status)
		}
		if ok, _ := body["ok"].(bool); ok {
			t.Errorf("expected ok=false in body, got %v", body)
		}

		// Verify nothing was captured in store for chat 999
		msgs, err := h.store.GetMessages(1, 999, 50, 0)
		if err == nil && len(msgs) != 0 {
			t.Errorf("expected 0 messages in store after 400, got %d", len(msgs))
		}
	})

	// -----------------------------------------------------------------
	// E-04: /tgapi/sendMessage 429 rate limit passthrough
	// Server must pass 429 + body (including retry_after) to caller.
	// -----------------------------------------------------------------
	t.Run("E-04_tgapi_sendMessage_429_passthrough", func(t *testing.T) {
		h := setupE2E(t, withHTTPServer())

		h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) {
			h.fake.writeJSON(w, 429, map[string]any{
				"ok":          false,
				"error_code":  429,
				"description": "Too Many Requests: retry after 1",
				"parameters": map[string]any{
					"retry_after": 1,
				},
			})
		})

		token := "err04:T"
		h.fake.RegisterBot(token, "errbot04", 10004)
		h.AddBot(models.BotConfig{
			Token:       token,
			Name:        "errbot04",
			BotUsername: "errbot04",
		})

		status, body := h.CallTgapi("sendMessage", token, map[string]any{
			"chat_id": 999,
			"text":    "hello",
		})

		if status != 429 {
			t.Errorf("expected status 429, got %d", status)
		}
		if ok, _ := body["ok"].(bool); ok {
			t.Errorf("expected ok=false in body, got %v", body)
		}
		// Verify retry_after is present in the body
		params, _ := body["parameters"].(map[string]any)
		if params == nil {
			t.Errorf("expected parameters in 429 body, got %v", body)
		} else if params["retry_after"] == nil {
			t.Errorf("expected retry_after in parameters, got %v", params)
		}
	})

	// -----------------------------------------------------------------
	// E-05: /tgapi/sendMessage 403 Forbidden passthrough
	// Server must pass 403 straight back. No special store state change
	// expected (no "mark chat blocked" logic exists in this codebase).
	// -----------------------------------------------------------------
	t.Run("E-05_tgapi_sendMessage_403_passthrough", func(t *testing.T) {
		h := setupE2E(t, withHTTPServer())

		h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) {
			h.fake.writeJSON(w, 403, map[string]any{
				"ok":          false,
				"error_code":  403,
				"description": "Forbidden: bot was blocked by the user",
			})
		})

		token := "err05:T"
		h.fake.RegisterBot(token, "errbot05", 10005)
		h.AddBot(models.BotConfig{
			Token:       token,
			Name:        "errbot05",
			BotUsername: "errbot05",
		})

		status, body := h.CallTgapi("sendMessage", token, map[string]any{
			"chat_id": 999,
			"text":    "hello",
		})

		if status != 403 {
			t.Errorf("expected status 403, got %d", status)
		}
		if ok, _ := body["ok"].(bool); ok {
			t.Errorf("expected ok=false in body, got %v", body)
		}
		desc, _ := body["description"].(string)
		if !strings.Contains(desc, "blocked") {
			t.Errorf("expected 'blocked' in description, got %q", desc)
		}
	})

	// -----------------------------------------------------------------
	// E-06: Network failure — fake server closed mid-session
	// pollLoop must handle connection errors without panic or goroutine leak.
	// -----------------------------------------------------------------
	t.Run("E-06_network_failure", func(t *testing.T) {
		h := setupE2E(t, withFastBackoff())

		token := "err06:T"
		h.fake.RegisterBot(token, "errbot06", 10006)
		botID := h.AddBot(models.BotConfig{
			Token:         token,
			Name:          "errbot06",
			BotUsername:   "errbot06",
			ManageEnabled: true,
		})

		if err := h.proxy.RestartBot(botID); err != nil {
			t.Fatalf("RestartBot: %v", err)
		}

		// Wait for first successful poll before closing the server.
		h.Eventually(func() bool {
			return h.fake.RequestsCountFor("getUpdates") >= 1
		}, 100*time.Millisecond, "first getUpdates poll")

		// Close the fake server — simulates network reset / server crash.
		h.fake.Close()

		// Give the pollLoop time to encounter the error and enter backoff.
		// 50ms is well under the 100ms sleep limit and << retryDelayMax(10ms).
		time.Sleep(50 * time.Millisecond)

		// No assertion beyond: no panic, and goleak (via TestMain) finds no leaked goroutines.
		// StopAll in cleanup stops the runner goroutine cleanly.
	})

	// -----------------------------------------------------------------
	// E-07: /tgapi/ 500 server error passthrough
	// Server must forward the 500 body to the caller unchanged.
	// -----------------------------------------------------------------
	t.Run("E-07_tgapi_5xx_passthrough", func(t *testing.T) {
		h := setupE2E(t, withHTTPServer())

		h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":500,"description":"Internal Server Error"}`))
		})

		token := "err07:T"
		h.fake.RegisterBot(token, "errbot07", 10007)
		h.AddBot(models.BotConfig{
			Token:       token,
			Name:        "errbot07",
			BotUsername: "errbot07",
		})

		status, body := h.CallTgapi("sendMessage", token, map[string]any{
			"chat_id": 999,
			"text":    "hello",
		})

		if status != 500 {
			t.Errorf("expected status 500, got %d", status)
		}
		if ok, _ := body["ok"].(bool); ok {
			t.Errorf("expected ok=false in body, got %v", body)
		}
	})

	// -----------------------------------------------------------------
	// E-08: getUpdates 409 Conflict
	// Observed behaviour: pollLoop treats 409 the same as any other API
	// error — logs it, retries with backoff. There is no special "stop on
	// 409" logic. This test documents that behaviour.
	// -----------------------------------------------------------------
	t.Run("E-08_getUpdates_409_conflict", func(t *testing.T) {
		h := setupE2E(t, withFastBackoff())

		h.fake.SetHandler("getUpdates", func(w http.ResponseWriter, r *http.Request) {
			h.fake.writeJSON(w, 409, map[string]any{
				"ok":          false,
				"error_code":  409,
				"description": "Conflict: terminated by other getUpdates request; make sure that only one bot instance is running",
			})
		})

		token := "err08:T"
		h.fake.RegisterBot(token, "errbot08", 10008)
		botID := h.AddBot(models.BotConfig{
			Token:         token,
			Name:          "errbot08",
			BotUsername:   "errbot08",
			ManageEnabled: true,
		})

		if err := h.proxy.RestartBot(botID); err != nil {
			t.Fatalf("RestartBot: %v", err)
		}

		// Observed behaviour: pollLoop does NOT stop on 409 — retries with backoff.
		h.Eventually(func() bool {
			return h.fake.RequestsCountFor("getUpdates") >= 2
		}, 100*time.Millisecond, "expected pollLoop to retry on 409 (does not stop)")
	})
}
