package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SlackConfig holds Slack-specific bridge configuration (stored as JSON in BridgeConfig.Config)
type SlackConfig struct {
	BotToken      string `json:"bot_token"`      // xoxb-... Slack bot token
	SigningSecret string `json:"signing_secret"` // Slack app signing secret for request verification
}

// SlackEventPayload is the outer envelope Slack sends for all Events API requests
type SlackEventPayload struct {
	Type      string          `json:"type"`      // "url_verification" or "event_callback"
	Token     string          `json:"token"`
	Challenge string          `json:"challenge"` // only for url_verification
	TeamID    string          `json:"team_id"`
	Event     json.RawMessage `json:"event"`
}

// SlackMessageEvent represents a Slack message event
type SlackMessageEvent struct {
	Type     string `json:"type"`      // "message"
	SubType  string `json:"subtype"`   // "" for normal messages, "bot_message", etc.
	Channel  string `json:"channel"`   // channel ID
	User     string `json:"user"`      // user ID
	Text     string `json:"text"`
	TS       string `json:"ts"`        // message timestamp (serves as message ID)
	ThreadTS string `json:"thread_ts"` // thread parent timestamp (for replies)
	BotID    string `json:"bot_id"`    // present if sent by a bot
}

// parseSlackConfig extracts SlackConfig from the bridge's Config JSON field
func parseSlackConfig(configJSON string) (*SlackConfig, error) {
	if configJSON == "" {
		return nil, fmt.Errorf("empty slack config")
	}
	var sc SlackConfig
	if err := json.Unmarshal([]byte(configJSON), &sc); err != nil {
		return nil, fmt.Errorf("invalid slack config JSON: %w", err)
	}
	if sc.BotToken == "" {
		return nil, fmt.Errorf("slack config missing bot_token")
	}
	return &sc, nil
}

// VerifySlackSignature validates the X-Slack-Signature header using HMAC-SHA256
func VerifySlackSignature(signingSecret string, header http.Header, body []byte) bool {
	signature := header.Get("X-Slack-Signature")
	timestamp := header.Get("X-Slack-Request-Timestamp")
	if signature == "" || timestamp == "" {
		return false
	}

	// Reject requests older than 5 minutes to prevent replay attacks
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	if abs64(time.Now().Unix()-ts) > 300 {
		return false
	}

	// Compute expected signature: v0=HMAC-SHA256(signing_secret, "v0:{timestamp}:{body}")
	baseString := fmt.Sprintf("v0:%s:%s", timestamp, string(body))
	mac := hmac.New(sha256.New, []byte(signingSecret))
	mac.Write([]byte(baseString))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(signature))
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// HandleSlackEvent processes an incoming Slack Events API request.
// Returns (response body, content-type, http status, error).
func (bm *BridgeManager) HandleSlackEvent(bridgeID int64, header http.Header, body []byte) ([]byte, string, int, error) {
	bm.mu.RLock()
	cfg, ok := bm.bridges[bridgeID]
	bm.mu.RUnlock()
	if !ok {
		return nil, "", 404, fmt.Errorf("bridge %d not active", bridgeID)
	}

	slackCfg, err := parseSlackConfig(cfg.Config)
	if err != nil {
		return nil, "", 500, fmt.Errorf("bridge %d slack config error: %w", bridgeID, err)
	}

	// Verify signature using signing_secret
	if slackCfg.SigningSecret == "" {
		log.Printf("[bridge] WARNING: Slack bridge %d has no signing_secret - requests are NOT verified", bridgeID)
	} else if !VerifySlackSignature(slackCfg.SigningSecret, header, body) {
		return nil, "", 401, fmt.Errorf("invalid slack signature")
	}

	var payload SlackEventPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", 400, fmt.Errorf("invalid slack event JSON: %w", err)
	}

	// Handle URL verification challenge (Slack sends this when you first configure the Events API URL)
	if payload.Type == "url_verification" {
		log.Printf("[bridge] id=%d slack URL verification challenge received", bridgeID)
		resp, _ := json.Marshal(map[string]string{"challenge": payload.Challenge})
		return resp, "application/json", 200, nil
	}

	if payload.Type != "event_callback" {
		log.Printf("[bridge] id=%d ignoring slack event type=%q", bridgeID, payload.Type)
		return []byte(`{"ok":true}`), "application/json", 200, nil
	}

	// Parse the inner event
	var msgEvent SlackMessageEvent
	if err := json.Unmarshal(payload.Event, &msgEvent); err != nil {
		log.Printf("[bridge] id=%d failed to parse slack event: %v", bridgeID, err)
		return []byte(`{"ok":true}`), "application/json", 200, nil
	}

	// Only process normal user messages (ignore bot messages, subtypes like channel_join, etc.)
	if msgEvent.Type != "message" || msgEvent.SubType != "" || msgEvent.BotID != "" {
		return []byte(`{"ok":true}`), "application/json", 200, nil
	}

	if msgEvent.Text == "" {
		return []byte(`{"ok":true}`), "application/json", 200, nil
	}

	// Resolve Slack user display name (use user ID as fallback)
	username := bm.resolveSlackUsername(slackCfg, msgEvent.User)

	// Build bridge incoming message
	incoming := BridgeIncomingMessage{
		ExternalChatID: msgEvent.Channel,
		ExternalUserID: msgEvent.User,
		Username:       username,
		Text:           msgEvent.Text,
		ExternalMsgID:  msgEvent.TS,
	}
	// Map thread replies
	if msgEvent.ThreadTS != "" && msgEvent.ThreadTS != msgEvent.TS {
		incoming.ReplyToMsgID = msgEvent.ThreadTS
	}

	if err := bm.HandleIncoming(bridgeID, incoming); err != nil {
		log.Printf("[bridge] id=%d slack HandleIncoming error: %v", bridgeID, err)
		return nil, "", 400, err
	}

	return []byte(`{"ok":true}`), "application/json", 200, nil
}

// resolveSlackUsername fetches user info from Slack API. Falls back to user ID.
func (bm *BridgeManager) resolveSlackUsername(cfg *SlackConfig, userID string) string {
	if cfg.BotToken == "" || userID == "" {
		return userID
	}

	req, err := http.NewRequest("GET", "https://slack.com/api/users.info?"+url.Values{"user": {userID}}.Encode(), nil)
	if err != nil {
		return userID
	}
	req.Header.Set("Authorization", "Bearer "+cfg.BotToken)

	resp, err := bm.client.Do(req)
	if err != nil {
		return userID
	}
	defer resp.Body.Close()

	var result struct {
		OK   bool `json:"ok"`
		User struct {
			Name    string `json:"name"`
			Profile struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || !result.OK {
		return userID
	}

	// Prefer display_name > real_name > name
	if result.User.Profile.DisplayName != "" {
		return result.User.Profile.DisplayName
	}
	if result.User.Profile.RealName != "" {
		return result.User.Profile.RealName
	}
	if result.User.Name != "" {
		return result.User.Name
	}
	return userID
}

// postSlackMessage sends a message to Slack using chat.postMessage API
func (bm *BridgeManager) postSlackMessage(slackCfg *SlackConfig, channel, text, threadTS string) error {
	payload := map[string]string{
		"channel": channel,
		"text":    text,
	}
	if threadTS != "" {
		payload["thread_ts"] = threadTS
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal error: %w", err)
	}

	req, err := http.NewRequest("POST", "https://slack.com/api/chat.postMessage", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("request error: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+slackCfg.BotToken)

	resp, err := bm.client.Do(req)
	if err != nil {
		return fmt.Errorf("POST error: %w", err)
	}
	defer resp.Body.Close()

	var slackResp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&slackResp); err != nil {
		return fmt.Errorf("decode response error: %w", err)
	}
	if !slackResp.OK {
		return fmt.Errorf("slack API error: %s", slackResp.Error)
	}

	return nil
}

// notifySlackOutgoing sends an outgoing bot message directly to Slack via API
func (bm *BridgeManager) notifySlackOutgoing(cfg *BridgeConfig, extChatID string, text string, replyToMsgID int) {
	slackCfg, err := parseSlackConfig(cfg.Config)
	if err != nil {
		log.Printf("[bridge] id=%d slack config error for outgoing: %v", cfg.ID, err)
		bm.store.UpdateBridgeActivity(cfg.ID, fmt.Sprintf("slack config error: %v", err))
		return
	}

	// Resolve thread_ts for reply threading
	var threadTS string
	if replyToMsgID != 0 {
		if extMsgID, err := bm.store.GetBridgeMsgMappingReverse(cfg.ID, replyToMsgID); err == nil && extMsgID != "" {
			threadTS = extMsgID // Slack message TS is the external message ID
		}
	}

	if err := bm.postSlackMessage(slackCfg, extChatID, text, threadTS); err != nil {
		log.Printf("[bridge] id=%d slack postMessage failed: %v", cfg.ID, err)
		bm.store.UpdateBridgeActivity(cfg.ID, fmt.Sprintf("slack error: %v", err))
		return
	}

	log.Printf("[bridge] id=%d slack message sent to channel=%s", cfg.ID, extChatID)
	bm.store.UpdateBridgeActivity(cfg.ID, "")
}

// isSlackBridge checks if a bridge uses the Slack protocol
func isSlackBridge(cfg *BridgeConfig) bool {
	return strings.EqualFold(cfg.Protocol, "slack")
}
