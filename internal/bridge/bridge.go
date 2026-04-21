package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skrashevich/botmux/internal/bot"
	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/proxy"
	"github.com/skrashevich/botmux/internal/store"
)

// Manager manages all active protocol bridges
type Manager struct {
	store        *store.Store
	proxy        *proxy.Manager
	mu           sync.RWMutex
	bridges      map[int64]*models.BridgeConfig
	client       *http.Client
	updateIDSeq  atomic.Int64 // synthetic update_id generator
	tgAPIBaseURL string
}

func NewManager(store *store.Store, proxy *proxy.Manager, tgAPIBaseURL string) *Manager {
	bm := &Manager{
		store:        store,
		proxy:        proxy,
		bridges:      make(map[int64]*models.BridgeConfig),
		client:       &http.Client{Timeout: 15 * time.Second},
		tgAPIBaseURL: tgAPIBaseURL,
	}
	bm.updateIDSeq.Store(time.Now().Unix())
	return bm
}

// Start loads all enabled bridges
func (bm *Manager) Start() {
	configs, err := bm.store.GetBridges()
	if err != nil {
		log.Printf("[bridge] Start: failed to load bridges: %v", err)
		return
	}
	for _, cfg := range configs {
		if cfg.Enabled {
			bm.mu.Lock()
			bm.bridges[cfg.ID] = &cfg
			bm.mu.Unlock()
			log.Printf("[bridge] Started bridge id=%d name=%q protocol=%s linked_bot=%d",
				cfg.ID, cfg.Name, cfg.Protocol, cfg.LinkedBotID)
		}
	}
	log.Printf("[bridge] Start: loaded %d enabled bridges", len(bm.bridges))
}

// Reload reloads a single bridge config (after update)
func (bm *Manager) Reload(bridgeID int64) {
	cfg, err := bm.store.GetBridge(bridgeID)
	if err != nil {
		log.Printf("[bridge] Reload: bridge %d not found: %v", bridgeID, err)
		bm.mu.Lock()
		delete(bm.bridges, bridgeID)
		bm.mu.Unlock()
		return
	}
	bm.mu.Lock()
	if cfg.Enabled {
		bm.bridges[cfg.ID] = cfg
	} else {
		delete(bm.bridges, cfg.ID)
	}
	bm.mu.Unlock()
}

// Remove stops and removes a bridge
func (bm *Manager) Remove(bridgeID int64) {
	bm.mu.Lock()
	delete(bm.bridges, bridgeID)
	bm.mu.Unlock()
}

// HandleIncoming processes an incoming message from a bridge and injects it as a Telegram update
func (bm *Manager) HandleIncoming(bridgeID int64, msg models.BridgeIncomingMessage) error {
	bm.mu.RLock()
	cfg, ok := bm.bridges[bridgeID]
	bm.mu.RUnlock()
	if !ok {
		return fmt.Errorf("bridge %d not active", bridgeID)
	}

	if msg.Text == "" {
		return fmt.Errorf("empty message text")
	}

	// Resolve or create Telegram chat mapping
	tgChatID, err := bm.store.GetBridgeChatMapping(bridgeID, msg.ExternalChatID)
	if err != nil || tgChatID == 0 {
		// Use a synthetic negative chat ID derived from bridge+external chat
		// Format: -bridge_id * 1000000 - hash of external_chat_id
		tgChatID = bm.syntheticChatID(bridgeID, msg.ExternalChatID)
		bm.store.SaveBridgeChatMapping(bridgeID, msg.ExternalChatID, tgChatID)
	}

	// Build a Telegram-format update
	updateID := bm.updateIDSeq.Add(1)
	syntheticMsgID := int(updateID & 0x7FFFFFFF)

	update := map[string]any{
		"update_id": float64(updateID),
		"message": map[string]any{
			"message_id": float64(syntheticMsgID),
			"text":       msg.Text,
			"date":       float64(time.Now().Unix()),
			"chat": map[string]any{
				"id":    float64(tgChatID),
				"type":  "private",
				"title": fmt.Sprintf("[%s] %s", cfg.Protocol, msg.ExternalChatID),
			},
			"from": map[string]any{
				"id":         float64(bm.syntheticUserID(msg.ExternalUserID)),
				"is_bot":     false,
				"first_name": msg.Username,
				"username":   fmt.Sprintf("%s_%s", cfg.Protocol, msg.ExternalUserID),
			},
		},
	}

	// Add reply context if available
	if msg.ReplyToMsgID != "" {
		if mapping, err := bm.store.GetBridgeMsgMapping(bridgeID, msg.ReplyToMsgID); err == nil && mapping != nil {
			msgMap := update["message"].(map[string]any)
			msgMap["reply_to_message"] = map[string]any{
				"message_id": float64(mapping.TelegramMsgID),
				"chat": map[string]any{
					"id": float64(tgChatID),
				},
			}
		}
	}

	// Save message mapping
	bm.store.SaveBridgeMsgMapping(bridgeID, msg.ExternalMsgID, syntheticMsgID, tgChatID)

	// Ensure the linked bot has a managed instance
	bm.ensureManagedBot(cfg.LinkedBotID)

	// Inject into the processing pipeline
	log.Printf("[bridge] id=%d injecting update for bot %d: chat=%d from=%q text=%q",
		bridgeID, cfg.LinkedBotID, tgChatID, msg.Username, truncate(msg.Text, 80))

	bm.proxy.ProcessUpdate(cfg.LinkedBotID, update)

	// Update activity
	bm.store.UpdateBridgeActivity(bridgeID, "")
	return nil
}

// NotifyOutgoing sends an outgoing bot message to the appropriate bridge callback
func (bm *Manager) NotifyOutgoing(botID int64, chatID int64, text string, telegramMsgID int, replyToMsgID int) {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	for _, cfg := range bm.bridges {
		if cfg.LinkedBotID != botID {
			continue
		}

		// Check if this chat belongs to this bridge
		extChatID, err := bm.store.GetBridgeChatMappingReverse(cfg.ID, chatID)
		if err != nil || extChatID == "" {
			continue
		}

		// Slack bridges: send directly via Slack API
		if IsSlackBridge(cfg) {
			go bm.notifySlackOutgoing(cfg, extChatID, text, replyToMsgID)
			continue
		}

		// Generic webhook bridges: POST to callback URL
		if cfg.CallbackURL == "" {
			continue
		}

		outMsg := models.BridgeOutgoingMessage{
			BridgeID:       cfg.ID,
			ExternalChatID: extChatID,
			Text:           text,
			TelegramMsgID:  telegramMsgID,
		}

		// Map reply-to back to external message ID
		if replyToMsgID != 0 {
			if extMsgID, err := bm.store.GetBridgeMsgMappingReverse(cfg.ID, replyToMsgID); err == nil && extMsgID != "" {
				outMsg.ReplyToExtID = extMsgID
			}
		}

		go bm.postCallback(cfg, outMsg)
	}
}

func (bm *Manager) postCallback(cfg *models.BridgeConfig, msg models.BridgeOutgoingMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("[bridge] id=%d callback marshal error: %v", cfg.ID, err)
		return
	}

	resp, err := bm.client.Post(cfg.CallbackURL, "application/json", bytes.NewReader(data))
	if err != nil {
		log.Printf("[bridge] id=%d callback POST to %s failed: %v", cfg.ID, cfg.CallbackURL, err)
		bm.store.UpdateBridgeActivity(cfg.ID, fmt.Sprintf("callback error: %v", err))
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.Printf("[bridge] id=%d callback POST success (%d)", cfg.ID, resp.StatusCode)
	} else {
		log.Printf("[bridge] id=%d callback POST returned %d", cfg.ID, resp.StatusCode)
		bm.store.UpdateBridgeActivity(cfg.ID, fmt.Sprintf("callback returned %d", resp.StatusCode))
	}
}

func (bm *Manager) ensureManagedBot(botID int64) {
	if bm.proxy.GetManagedBot(botID) != nil {
		return
	}
	cfg, err := bm.store.GetBotConfig(botID)
	if err != nil {
		return
	}
	managedBot, err := bot.NewBot(cfg.Token, bm.store, botID, bm.tgAPIBaseURL)
	if err != nil {
		log.Printf("[bridge] ensureManagedBot: failed for bot %d: %v", botID, err)
		return
	}
	bm.proxy.RegisterManagedBot(botID, managedBot)
}

// syntheticChatID generates a deterministic negative chat ID for a bridge+external chat
func (bm *Manager) syntheticChatID(bridgeID int64, externalChatID string) int64 {
	// Use negative IDs to avoid collision with real Telegram chat IDs
	// Telegram uses negative IDs for groups/channels, but our synthetic range is distinct
	h := int64(0)
	for _, c := range externalChatID {
		h = h*31 + int64(c)
	}
	if h < 0 {
		h = -h
	}
	return -(bridgeID*10000000 + (h % 10000000))
}

// syntheticUserID generates a deterministic user ID from external user ID
func (bm *Manager) syntheticUserID(externalUserID string) int64 {
	h := int64(9000000000) // base to avoid collision with real Telegram user IDs
	for _, c := range externalUserID {
		h = h*31 + int64(c)
	}
	if h < 0 {
		h = -h
	}
	return h
}

// InstallHooks sets the onMessageSent callback on all currently managed bots
func (bm *Manager) InstallHooks() {
	bm.proxy.ForEachManagedBot(func(botID int64, b *bot.Bot) {
		b.SetOnMessageSent(bm.NotifyOutgoing)
		log.Printf("[bridge] installed outgoing hook on bot %d", botID)
	})
}

// InstallHookOnBot sets the onMessageSent callback on a specific bot
func (bm *Manager) InstallHookOnBot(b *bot.Bot) {
	b.SetOnMessageSent(bm.NotifyOutgoing)
}

// GetBridge returns a bridge config by ID (from in-memory cache)
func (bm *Manager) GetBridge(bridgeID int64) *models.BridgeConfig {
	bm.mu.RLock()
	defer bm.mu.RUnlock()
	return bm.bridges[bridgeID]
}

// GetBridgesForBot returns all bridges linked to a specific bot
func (bm *Manager) GetBridgesForBot(botID int64) []*models.BridgeConfig {
	bm.mu.RLock()
	defer bm.mu.RUnlock()
	var result []*models.BridgeConfig
	for _, cfg := range bm.bridges {
		if cfg.LinkedBotID == botID {
			result = append(result, cfg)
		}
	}
	return result
}

// truncate shortens s to at most maxLen runes, appending "..." if truncated.
func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}
