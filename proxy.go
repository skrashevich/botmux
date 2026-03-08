package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// ProxyManager manages polling and forwarding for all non-CLI bots
type ProxyManager struct {
	store       *Store
	mu          sync.Mutex
	runners     map[int64]*proxyRunner
	managedBots map[int64]*Bot // botID -> Bot instance for management processing
	client      *http.Client
}

type proxyRunner struct {
	cancel context.CancelFunc
	botID  int64
}

func NewProxyManager(store *Store) *ProxyManager {
	return &ProxyManager{
		store:       store,
		runners:     make(map[int64]*proxyRunner),
		managedBots: make(map[int64]*Bot),
		client:      &http.Client{Timeout: 120 * time.Second},
	}
}

// RegisterManagedBot registers a Bot instance for management processing
func (pm *ProxyManager) RegisterManagedBot(botID int64, bot *Bot) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.managedBots[botID] = bot
}

// UnregisterManagedBot removes a Bot instance
func (pm *ProxyManager) UnregisterManagedBot(botID int64) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	delete(pm.managedBots, botID)
}

// Start launches goroutines for all active non-CLI bots
func (pm *ProxyManager) Start() {
	bots, err := pm.store.GetBotConfigs()
	if err != nil {
		log.Printf("Proxy: failed to load bots: %v", err)
		return
	}
	for _, bot := range bots {
		if bot.Source == "cli" {
			continue // CLI bot uses its own polling
		}
		if bot.ManageEnabled || bot.ProxyEnabled {
			pm.startBot(bot.ID)
		}
	}
}

func (pm *ProxyManager) startBot(botID int64) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if r, ok := pm.runners[botID]; ok {
		r.cancel()
		delete(pm.runners, botID)
	}

	ctx, cancel := context.WithCancel(context.Background())
	pm.runners[botID] = &proxyRunner{cancel: cancel, botID: botID}

	go pm.pollLoop(ctx, botID)
}

func (pm *ProxyManager) stopBot(botID int64) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if r, ok := pm.runners[botID]; ok {
		r.cancel()
		delete(pm.runners, botID)
	}
}

func (pm *ProxyManager) StopAll() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for id, r := range pm.runners {
		r.cancel()
		delete(pm.runners, id)
	}
}

func (pm *ProxyManager) IsRunning(botID int64) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	_, ok := pm.runners[botID]
	return ok
}

// RestartBot restarts a bot after config changes. Creates/removes managed Bot instance as needed.
func (pm *ProxyManager) RestartBot(botID int64) error {
	bot, err := pm.store.GetBotConfig(botID)
	if err != nil {
		return err
	}

	pm.stopBot(botID)

	if bot.Source == "cli" {
		return nil // CLI bot manages its own polling
	}

	active := bot.ManageEnabled || bot.ProxyEnabled

	// Create or remove managed Bot instance
	if bot.ManageEnabled {
		pm.mu.Lock()
		_, hasManagedBot := pm.managedBots[botID]
		pm.mu.Unlock()
		if !hasManagedBot {
			managedBot, err := NewBot(bot.Token, pm.store, botID)
			if err != nil {
				return fmt.Errorf("failed to create bot instance: %w", err)
			}
			pm.RegisterManagedBot(botID, managedBot)
		}
	} else {
		pm.UnregisterManagedBot(botID)
	}

	if active {
		pm.startBot(botID)
	}
	return nil
}

func (pm *ProxyManager) pollLoop(ctx context.Context, botID int64) {
	retryDelay := time.Second
	maxRetryDelay := 30 * time.Second
	lastHealthCheck := time.Time{}
	healthCheckInterval := 60 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		bot, err := pm.store.GetBotConfig(botID)
		if err != nil {
			return
		}
		if !bot.ManageEnabled && !bot.ProxyEnabled {
			return
		}

		timeout := bot.PollingTimeout
		if timeout <= 0 {
			timeout = 30
		}

		updates, err := pm.getUpdates(ctx, bot.Token, bot.Offset, timeout)
		if err != nil {
			pm.store.UpdateBotStatus(botID, fmt.Sprintf("getUpdates error: %v", err), "")
			log.Printf("Proxy [%s]: getUpdates error: %v", bot.Name, err)

			select {
			case <-ctx.Done():
				return
			case <-time.After(retryDelay):
			}
			retryDelay = min(retryDelay*2, maxRetryDelay)
			continue
		}

		retryDelay = time.Second

		// Periodic backend health check for proxy bots
		if bot.ProxyEnabled && bot.BackendURL != "" && time.Since(lastHealthCheck) >= healthCheckInterval {
			lastHealthCheck = time.Now()
			pm.CheckAndStoreHealth(botID)
		}

		for _, update := range updates {
			select {
			case <-ctx.Done():
				return
			default:
			}

			updateID, ok := update["update_id"].(float64)
			if !ok {
				continue
			}

			// Proxy: forward to backend
			if bot.ProxyEnabled && bot.BackendURL != "" {
				err := pm.forwardUpdate(ctx, bot, update)
				if err != nil {
					pm.store.UpdateBotStatus(botID, fmt.Sprintf("forward error: %v", err), "")
					log.Printf("Proxy [%s]: forward error for update %d: %v", bot.Name, int64(updateID), err)

					select {
					case <-ctx.Done():
						return
					case <-time.After(retryDelay):
					}
					retryDelay = min(retryDelay*2, maxRetryDelay)
					continue
				}
				pm.store.IncrementBotForwarded(botID)
			}

			// Management: process update for chat/message tracking
			if bot.ManageEnabled {
				pm.processForManagement(botID, update)
			}

			newOffset := int64(updateID) + 1
			pm.store.UpdateBotOffset(botID, newOffset)
			pm.store.UpdateBotStatus(botID, "", time.Now().Format(time.RFC3339))
		}
	}
}

func (pm *ProxyManager) processForManagement(botID int64, rawUpdate map[string]interface{}) {
	pm.mu.Lock()
	bot := pm.managedBots[botID]
	pm.mu.Unlock()
	if bot == nil {
		return
	}

	data, err := json.Marshal(rawUpdate)
	if err != nil {
		return
	}
	var update tgbotapi.Update
	if err := json.Unmarshal(data, &update); err != nil {
		return
	}
	bot.processUpdate(update)
}

func (pm *ProxyManager) getUpdates(ctx context.Context, token string, offset int64, timeout int) ([]map[string]interface{}, error) {
	reqBody, _ := json.Marshal(map[string]interface{}{
		"offset":  offset,
		"timeout": timeout,
		"limit":   100,
	})

	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates", token)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: time.Duration(timeout+10) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result struct {
		OK          bool                     `json:"ok"`
		Result      []map[string]interface{} `json:"result"`
		Description string                   `json:"description"`
		ErrorCode   int                      `json:"error_code"`
		RetryAfter  int                      `json:"retry_after"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("invalid response: %s", string(body[:min(200, len(body))]))
	}

	if !result.OK {
		if result.RetryAfter > 0 {
			return nil, fmt.Errorf("rate limited, retry after %ds: %s", result.RetryAfter, result.Description)
		}
		return nil, fmt.Errorf("API error %d: %s", result.ErrorCode, result.Description)
	}

	return result.Result, nil
}

func (pm *ProxyManager) forwardUpdate(ctx context.Context, bot *BotConfig, update map[string]interface{}) error {
	data, err := json.Marshal(update)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", bot.BackendURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bot.SecretToken != "" {
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", bot.SecretToken)
	}

	backendClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := backendClient.Do(req)
	if err != nil {
		return fmt.Errorf("backend request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("backend returned %d: %s", resp.StatusCode, string(respBody[:min(200, len(respBody))]))
	}

	pm.handleWebhookReply(bot.Token, respBody)
	return nil
}

func (pm *ProxyManager) handleWebhookReply(token string, body []byte) {
	if len(body) == 0 {
		return
	}

	var reply map[string]interface{}
	if err := json.Unmarshal(body, &reply); err != nil {
		return
	}

	methodRaw, ok := reply["method"]
	if !ok {
		return
	}
	method, ok := methodRaw.(string)
	if !ok || method == "" {
		return
	}

	delete(reply, "method")
	data, err := json.Marshal(reply)
	if err != nil {
		return
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/%s", token, method)
	resp, err := pm.client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		log.Printf("Proxy: webhook reply error: %v", err)
		return
	}
	resp.Body.Close()
}

func (pm *ProxyManager) ValidateToken(token string) (string, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getMe", token)
	resp, err := pm.client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			Username string `json:"username"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if !result.OK {
		return "", fmt.Errorf("invalid token: %s", result.Description)
	}
	return result.Result.Username, nil
}

func (pm *ProxyManager) DeleteWebhook(token string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/deleteWebhook", token)
	resp, err := pm.client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("deleteWebhook failed: %s", result.Description)
	}
	return nil
}

// CheckBackendHealth sends a test POST to the backend URL and returns status
func (pm *ProxyManager) CheckBackendHealth(backendURL, secretToken string) (string, error) {
	if backendURL == "" {
		return "no_url", fmt.Errorf("no backend URL configured")
	}

	testPayload := []byte(`{"health_check":true}`)
	req, err := http.NewRequest("POST", backendURL, bytes.NewReader(testPayload))
	if err != nil {
		return "error", fmt.Errorf("invalid URL: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if secretToken != "" {
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secretToken)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "unreachable", fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 500 {
		return fmt.Sprintf("ok:%d", resp.StatusCode), nil
	}
	return fmt.Sprintf("error:%d", resp.StatusCode), fmt.Errorf("backend returned %d", resp.StatusCode)
}

// CheckAndStoreHealth runs a health check and stores the result
func (pm *ProxyManager) CheckAndStoreHealth(botID int64) (string, error) {
	bot, err := pm.store.GetBotConfig(botID)
	if err != nil {
		return "", err
	}
	status, checkErr := pm.CheckBackendHealth(bot.BackendURL, bot.SecretToken)
	now := time.Now().Format(time.RFC3339)
	if checkErr != nil {
		pm.store.UpdateBackendHealth(botID, status+": "+checkErr.Error(), now)
		return status, checkErr
	}
	pm.store.UpdateBackendHealth(botID, status, now)
	return status, nil
}

// GetManagedBot returns a managed Bot instance by botID
func (pm *ProxyManager) GetManagedBot(botID int64) *Bot {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.managedBots[botID]
}
