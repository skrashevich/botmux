package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/store"
)

// DefaultSystemPrompt is the default system prompt for the LLM router.
const DefaultSystemPrompt = `You are a message router for Telegram bots. Given a message and a list of available bots with their descriptions and chats, decide which bot should handle the message.
Respond with JSON: {"target_bot_id": <id>, "target_chat_id": <chat_id or 0>, "action": "<forward|copy|drop>", "reason": "<brief reason>"}
If no bot should handle it, respond with: {"target_bot_id": 0, "action": "drop", "reason": "no matching bot"}`

// Router routes messages to the appropriate bot using an LLM.
type Router struct {
	store  *store.Store
	client *http.Client
}

// NewRouter creates a new Router.
func NewRouter(s *store.Store) *Router {
	return &Router{
		store: s,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// RouteMessage decides which bot/chat should handle the given message.
// Returns *models.LLMRouteResult or nil if routing is disabled, and any error.
func (r *Router) RouteMessage(ctx context.Context, sourceBotID int64, messageText string, chatID int64, fromID int64, fromUser string) (*models.LLMRouteResult, error) {
	cfg, err := r.store.GetLLMConfig()
	if err != nil {
		return nil, fmt.Errorf("llm-router: failed to load config: %w", err)
	}
	if cfg == nil || !cfg.Enabled {
		return nil, nil
	}

	bots, err := r.store.GetBotConfigs()
	if err != nil {
		return nil, fmt.Errorf("llm-router: failed to load bots: %w", err)
	}

	// Build context: collect bot info with descriptions and chats
	type chatInfo struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
		Type  string `json:"type"`
	}
	type botInfo struct {
		ID          int64      `json:"id"`
		Username    string     `json:"username"`
		Name        string     `json:"name"`
		Description string     `json:"description"`
		Chats       []chatInfo `json:"chats"`
	}

	var botInfos []botInfo
	for _, b := range bots {
		if b.ID == sourceBotID {
			continue // skip the source bot
		}
		desc, _ := r.store.GetBotDescription(b.ID)
		info := botInfo{
			ID:          b.ID,
			Username:    b.BotUsername,
			Name:        b.Name,
			Description: desc,
		}
		chats, err := r.store.GetChats(b.ID)
		if err == nil {
			for _, c := range chats {
				info.Chats = append(info.Chats, chatInfo{ID: c.ID, Title: c.Title, Type: c.Type})
			}
		}
		botInfos = append(botInfos, info)
	}

	botsJSON, err := json.Marshal(botInfos)
	if err != nil {
		return nil, fmt.Errorf("llm-router: failed to marshal bots: %w", err)
	}

	userContent := fmt.Sprintf(
		"Message: %q\nFrom user: %s (id: %d)\nSource chat ID: %d\n\nAvailable bots:\n%s",
		messageText, fromUser, fromID, chatID, string(botsJSON),
	)

	systemPrompt := cfg.SystemPrompt
	if strings.TrimSpace(systemPrompt) == "" {
		systemPrompt = DefaultSystemPrompt
	}

	result, err := r.callLLM(ctx, cfg, systemPrompt, userContent)
	if err != nil {
		return nil, err
	}

	log.Printf("[llm-router] routed message from bot %d chat %d: target_bot=%d target_chat=%d action=%s reason=%s",
		sourceBotID, chatID, result.TargetBotID, result.TargetChatID, result.Action, result.Reason)

	return result, nil
}

// callLLM calls the OpenAI-compatible Chat Completions API.
func (r *Router) callLLM(ctx context.Context, cfg *models.LLMConfig, systemPrompt, userContent string) (*models.LLMRouteResult, error) {
	reqBody, err := json.Marshal(map[string]any{
		"model": cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userContent},
		},
		"response_format": map[string]string{"type": "json_object"},
	})
	if err != nil {
		return nil, fmt.Errorf("llm-router: failed to marshal request: %w", err)
	}

	apiURL := strings.TrimRight(cfg.APIURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("llm-router: failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm-router: API call failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("llm-router: failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("llm-router: API returned status %d: %s", resp.StatusCode, string(body))
	}

	// Parse OpenAI response envelope
	var apiResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("llm-router: failed to parse API response: %w", err)
	}
	if len(apiResp.Choices) == 0 {
		return nil, fmt.Errorf("llm-router: API returned no choices")
	}

	content := apiResp.Choices[0].Message.Content
	var result models.LLMRouteResult
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return nil, fmt.Errorf("llm-router: failed to parse routing JSON %q: %w", content, err)
	}

	return &result, nil
}
