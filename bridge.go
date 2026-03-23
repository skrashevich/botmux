package main

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
)

// BridgeConfig represents a protocol bridge in the database
type BridgeConfig struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Protocol     string `json:"protocol"`      // "webhook", "slack", "discord", "meshtastic"
	LinkedBotID  int64  `json:"linked_bot_id"` // Telegram bot that this bridge is linked to
	Config       string `json:"config"`        // JSON protocol-specific configuration
	CallbackURL  string `json:"callback_url"`  // URL to POST outgoing messages to
	Enabled      bool   `json:"enabled"`
	CreatedAt    string `json:"created_at"`
	LastActivity string `json:"last_activity,omitempty"`
	LastError    string `json:"last_error,omitempty"`
}

// BridgeIncomingMessage is the simple format external sources POST to us
type BridgeIncomingMessage struct {
	ExternalChatID string `json:"chat_id"`    // external chat/channel ID (string for flexibility)
	ExternalUserID string `json:"user_id"`    // external user ID
	Username       string `json:"username"`   // display name
	Text           string `json:"text"`       // message text
	ExternalMsgID  string `json:"message_id"` // external message ID
	ReplyToMsgID   string `json:"reply_to"`   // external reply-to message ID
}

// BridgeOutgoingMessage is what we POST back to the bridge callback
type BridgeOutgoingMessage struct {
	BridgeID       int64  `json:"bridge_id"`
	ExternalChatID string `json:"chat_id"` // mapped back to external chat ID
	Text           string `json:"text"`
	TelegramMsgID  int    `json:"telegram_msg_id"`
	ReplyToExtID   string `json:"reply_to,omitempty"` // external msg ID being replied to
}

// BridgeChatMapping tracks external_chat_id <-> telegram_chat_id
type BridgeChatMapping struct {
	BridgeID       int64  `json:"bridge_id"`
	ExternalChatID string `json:"external_chat_id"`
	TelegramChatID int64  `json:"telegram_chat_id"`
}

// BridgeMsgMapping tracks external_msg_id <-> telegram_msg_id for reply threading
type BridgeMsgMapping struct {
	BridgeID       int64  `json:"bridge_id"`
	ExternalMsgID  string `json:"external_msg_id"`
	TelegramMsgID  int    `json:"telegram_msg_id"`
	TelegramChatID int64  `json:"telegram_chat_id"`
}

// BridgeManager manages all active protocol bridges
type BridgeManager struct {
	store       *Store
	proxy       *ProxyManager
	mu          sync.RWMutex
	bridges     map[int64]*BridgeConfig
	client      *http.Client
	updateIDSeq atomic.Int64 // synthetic update_id generator
}

func NewBridgeManager(store *Store, proxy *ProxyManager) *BridgeManager {
	bm := &BridgeManager{
		store:   store,
		proxy:   proxy,
		bridges: make(map[int64]*BridgeConfig),
		client:  &http.Client{Timeout: 15 * time.Second},
	}
	bm.updateIDSeq.Store(time.Now().Unix())
	return bm
}

// Start loads all enabled bridges
func (bm *BridgeManager) Start() {
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
func (bm *BridgeManager) Reload(bridgeID int64) {
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
func (bm *BridgeManager) Remove(bridgeID int64) {
	bm.mu.Lock()
	delete(bm.bridges, bridgeID)
	bm.mu.Unlock()
}

// HandleIncoming processes an incoming message from a bridge and injects it as a Telegram update
func (bm *BridgeManager) HandleIncoming(bridgeID int64, msg BridgeIncomingMessage) error {
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

	bm.proxy.processUpdate(cfg.LinkedBotID, update)

	// Update activity
	bm.store.UpdateBridgeActivity(bridgeID, "")
	return nil
}

// NotifyOutgoing sends an outgoing bot message to the appropriate bridge callback
func (bm *BridgeManager) NotifyOutgoing(botID int64, chatID int64, text string, telegramMsgID int, replyToMsgID int) {
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
		if isSlackBridge(cfg) {
			go bm.notifySlackOutgoing(cfg, extChatID, text, replyToMsgID)
			continue
		}

		// Generic webhook bridges: POST to callback URL
		if cfg.CallbackURL == "" {
			continue
		}

		outMsg := BridgeOutgoingMessage{
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

func (bm *BridgeManager) postCallback(cfg *BridgeConfig, msg BridgeOutgoingMessage) {
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

func (bm *BridgeManager) ensureManagedBot(botID int64) {
	if bm.proxy.GetManagedBot(botID) != nil {
		return
	}
	bot, err := bm.store.GetBotConfig(botID)
	if err != nil {
		return
	}
	managedBot, err := NewBot(bot.Token, bm.store, botID)
	if err != nil {
		log.Printf("[bridge] ensureManagedBot: failed for bot %d: %v", botID, err)
		return
	}
	bm.proxy.RegisterManagedBot(botID, managedBot)
}

// syntheticChatID generates a deterministic negative chat ID for a bridge+external chat
func (bm *BridgeManager) syntheticChatID(bridgeID int64, externalChatID string) int64 {
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
func (bm *BridgeManager) syntheticUserID(externalUserID string) int64 {
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
func (bm *BridgeManager) InstallHooks() {
	bm.proxy.mu.Lock()
	defer bm.proxy.mu.Unlock()
	for botID, bot := range bm.proxy.managedBots {
		bot.onMessageSent = bm.NotifyOutgoing
		log.Printf("[bridge] installed outgoing hook on bot %d", botID)
	}
}

// InstallHookOnBot sets the onMessageSent callback on a specific bot
func (bm *BridgeManager) InstallHookOnBot(bot *Bot) {
	bot.onMessageSent = bm.NotifyOutgoing
}

// GetBridge returns a bridge config by ID (from in-memory cache)
func (bm *BridgeManager) GetBridge(bridgeID int64) *BridgeConfig {
	bm.mu.RLock()
	defer bm.mu.RUnlock()
	return bm.bridges[bridgeID]
}

// GetBridgesForBot returns all bridges linked to a specific bot
func (bm *BridgeManager) GetBridgesForBot(botID int64) []*BridgeConfig {
	bm.mu.RLock()
	defer bm.mu.RUnlock()
	var result []*BridgeConfig
	for _, cfg := range bm.bridges {
		if cfg.LinkedBotID == botID {
			result = append(result, cfg)
		}
	}
	return result
}
