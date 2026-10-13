package proxy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/skrashevich/botmux/internal/bot"
	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/store"
)

type routeAction struct {
	TargetBotID     int64  `json:"target_bot_id"`
	TargetChatID    int64  `json:"target_chat_id"`
	SourceBotID     int64  `json:"source_bot_id"`
	SourceChatID    int64  `json:"source_chat_id"`
	SourceMessageID int    `json:"source_message_id"`
	ReplyID         int    `json:"reply_id"`
	RouteID         int64  `json:"route_id"`
	Text            string `json:"text"`
	Action          string `json:"action"`
	Reverse         bool   `json:"reverse"`
}

func routePayload(raw map[string]any) (map[string]any, string, int64, int64, int) {
	msg := updateMessagePayload(raw)
	if msg == nil {
		return nil, "", 0, 0, 0
	}
	text, _ := msg["text"].(string)
	if text == "" {
		text, _ = msg["caption"].(string)
	}
	if text == "" {
		text = bot.ExtractRichText(msg["rich_message"])
	}
	var chatID, fromID int64
	var messageID int
	if chat, ok := msg["chat"].(map[string]any); ok {
		if id, ok := chat["id"].(float64); ok {
			chatID = int64(id)
		}
	}
	if from, ok := msg["from"].(map[string]any); ok {
		if id, ok := from["id"].(float64); ok {
			fromID = int64(id)
		}
	}
	if id, ok := msg["message_id"].(float64); ok {
		messageID = int(id)
	}
	return msg, text, chatID, fromID, messageID
}
func deliveryEvent(raw map[string]any) (string, int64, error) {
	data, err := json.Marshal(raw)
	if err != nil {
		return "", 0, err
	}
	if id, _ := raw["update_id"].(float64); id > 0 && raw["_botmux_bridge"] != true {
		return fmt.Sprintf("tg:%d", int64(id)), int64(id), nil
	}
	return fmt.Sprintf("bridge:%x", sha256.Sum256(data)), 0, nil
}
func childJob(parent store.DeliveryJob, operation string, value any) (store.DeliveryJob, error) {
	data, err := json.Marshal(value)
	return store.DeliveryJob{BotID: parent.BotID, EventKey: parent.EventKey, Operation: operation, Payload: data}, err
}
func (pm *Manager) planRoutes(parent store.DeliveryJob, raw map[string]any) ([]store.DeliveryJob, error) {
	msg, text, chatID, fromID, messageID := routePayload(raw)
	if msg == nil {
		return nil, nil
	}
	jobs := make([]store.DeliveryJob, 0)
	add := func(operation string, a routeAction) error {
		j, err := childJob(parent, operation, a)
		if err == nil {
			jobs = append(jobs, j)
		}
		return err
	}
	if reply, ok := msg["reply_to_message"].(map[string]any); ok && chatID != 0 {
		id, _ := reply["message_id"].(float64)
		mapping, err := pm.store.FindReverseMappingByReply(parent.BotID, chatID, int(id))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		isBot := false
		if from, ok := msg["from"].(map[string]any); ok {
			isBot, _ = from["is_bot"].(bool)
		}
		if mapping != nil && !isBot && text != "" {
			if err := add("reverse", routeAction{TargetBotID: mapping.SourceBotID, TargetChatID: mapping.SourceChatID, SourceBotID: parent.BotID, SourceChatID: chatID, SourceMessageID: messageID, ReplyID: mapping.SourceMsgID, RouteID: mapping.RouteID, Text: text, Action: "forward", Reverse: true}); err != nil {
				return nil, err
			}
		}
	}
	routes, err := pm.store.GetRoutes(parent.BotID)
	if err != nil {
		return nil, err
	}
	for _, route := range routes {
		if !route.Enabled || (route.SourceChatID != 0 && route.SourceChatID != chatID) {
			continue
		}
		matched := false
		switch route.ConditionType {
		case "text":
			if text != "" && route.ConditionValue != "" {
				re, err := regexp.Compile("(?i)" + route.ConditionValue)
				if err != nil {
					return nil, fmt.Errorf("route %d invalid pattern: %w", route.ID, err)
				}
				matched = re.MatchString(text)
			}
		case "user_id":
			id, _ := strconv.ParseInt(route.ConditionValue, 10, 64)
			matched = fromID != 0 && id == fromID
		case "chat_id":
			id, _ := strconv.ParseInt(route.ConditionValue, 10, 64)
			matched = chatID != 0 && id == chatID
		}
		if !matched {
			continue
		}
		if route.Action == "drop" {
			break
		}
		dest := route.TargetChatID
		if dest == 0 {
			dest = chatID
		}
		if dest == 0 {
			continue
		}
		if (route.Action == "copy" && messageID == 0) || (route.Action == "forward" && text == "") {
			continue
		}
		if route.Action != "copy" && route.Action != "forward" {
			return nil, fmt.Errorf("unknown route action %q", route.Action)
		}
		if err := add(fmt.Sprintf("route:%d", route.ID), routeAction{TargetBotID: route.TargetBotID, TargetChatID: dest, SourceBotID: parent.BotID, SourceChatID: chatID, SourceMessageID: messageID, RouteID: route.ID, Text: text, Action: route.Action}); err != nil {
			return nil, err
		}
	}
	if text != "" {
		cfg, err := pm.store.GetLLMConfig()
		if err != nil {
			return nil, err
		}
		if cfg != nil && cfg.Enabled {
			j, err := childJob(parent, "llm", raw)
			if err != nil {
				return nil, err
			}
			jobs = append(jobs, j)
		}
	}
	return jobs, nil
}
func (pm *Manager) deliveryBot(ctx context.Context, id int64) (*bot.Bot, error) {
	cfg, err := pm.store.GetBotConfig(id)
	if err != nil {
		return nil, err
	}
	if cfg.Disabled {
		return nil, fmt.Errorf("target bot %d disabled", id)
	}
	pm.mu.Lock()
	b := pm.managedBots[id]
	pm.mu.Unlock()
	if b != nil {
		return b, nil
	}
	b, err = bot.NewBotWithContext(ctx, cfg.Token, pm.store, id, pm.tgAPIBaseURL)
	if err != nil {
		return nil, err
	}
	pm.RegisterManagedBot(id, b)
	return b, nil
}
func (pm *Manager) executeDelivery(ctx context.Context, j store.DeliveryJob) error {
	switch j.Operation {
	case "plan":
		var raw map[string]any
		if err := json.Unmarshal(j.Payload, &raw); err != nil {
			return err
		}
		jobs, err := pm.planRoutes(j, raw)
		if err != nil {
			return err
		}
		return pm.store.ExpandDelivery(j.ID, pm.deliveryOwner, jobs)
	case "llm":
		var raw map[string]any
		if err := json.Unmarshal(j.Payload, &raw); err != nil {
			return err
		}
		msg, text, chatID, fromID, messageID := routePayload(raw)
		var fromUser string
		if from, ok := msg["from"].(map[string]any); ok {
			fromUser, _ = from["username"].(string)
		}
		result, err := pm.llmRouter.RouteMessage(ctx, j.BotID, text, chatID, fromID, fromUser)
		if err != nil {
			return err
		}
		var jobs []store.DeliveryJob
		if result != nil && result.TargetBotID != 0 && result.Action != "drop" && result.Action != "ignore" {
			if result.Action != "forward" && result.Action != "copy" {
				return fmt.Errorf("unknown LLM action %q", result.Action)
			}
			dest := result.TargetChatID
			if dest == 0 {
				dest = chatID
			}
			if dest != 0 && (result.Action != "copy" || messageID != 0) {
				child, err := childJob(j, "llm:send", routeAction{TargetBotID: result.TargetBotID, TargetChatID: dest, SourceBotID: j.BotID, SourceChatID: chatID, SourceMessageID: messageID, Text: text, Action: result.Action})
				if err != nil {
					return err
				}
				jobs = append(jobs, child)
			}
		}
		return pm.store.ExpandDelivery(j.ID, pm.deliveryOwner, jobs)
	}
	var action routeAction
	if err := json.Unmarshal(j.Payload, &action); err != nil {
		return err
	}
	b, err := pm.deliveryBot(ctx, action.TargetBotID)
	if err != nil {
		return err
	}
	result := j.Result
	if cached := pm.deliveryResults[j.ID]; len(cached) > 0 {
		result = cached
	}
	var sent models.Message
	if len(result) == 0 || string(result) == "null" {
		sent, err = b.SendRoute(ctx, action.TargetChatID, action.SourceBotID, action.SourceChatID, action.SourceMessageID, action.ReplyID, action.Text, action.Action)
		if err != nil {
			return err
		}
		result, err = json.Marshal(sent)
		if err != nil {
			return err
		}
		pm.deliveryResults[j.ID] = result
	}
	if err := pm.store.SaveDeliveryResult(j.ID, pm.deliveryOwner, result); err != nil {
		return err
	}
	delete(pm.deliveryResults, j.ID)
	if err := json.Unmarshal(result, &sent); err != nil {
		return err
	}
	sent.Complete = true
	if err := pm.store.SaveMessage(sent); err != nil {
		return err
	}
	var mapping *models.RouteMapping
	if action.SourceMessageID > 0 {
		m := models.RouteMapping{RouteID: action.RouteID, SourceBotID: action.SourceBotID, SourceChatID: action.SourceChatID, SourceMsgID: action.SourceMessageID, TargetBotID: action.TargetBotID, TargetChatID: action.TargetChatID, TargetMsgID: sent.ID, CreatedAt: time.Now().Format(time.RFC3339)}
		if action.Reverse {
			m.SourceBotID = action.TargetBotID
			m.SourceChatID = action.TargetChatID
			m.SourceMsgID = sent.ID
			m.TargetBotID = action.SourceBotID
			m.TargetChatID = action.SourceChatID
			m.TargetMsgID = action.SourceMessageID
		}
		mapping = &m
	}
	if err := pm.store.CompleteRouteDelivery(j.ID, pm.deliveryOwner, mapping); err != nil {
		return err
	}
	b.NotifyRouteSent(sent)
	return nil
}

// RetryRouteDeliveries processes a bounded batch. Failed jobs remain durable and
// keep independent backoff; successful sibling actions are not retried.
func (pm *Manager) RetryRouteDeliveries(ctx context.Context) error {
	pm.deliveryMu.Lock()
	defer pm.deliveryMu.Unlock()
	dueBefore := time.Now()
	for range 100 {
		if err := ctx.Err(); err != nil {
			return err
		}
		j, err := pm.store.ClaimDeliveryBefore(pm.deliveryOwner, time.Now(), dueBefore, 5*time.Minute)
		if err != nil {
			return err
		}
		if j == nil {
			return nil
		}
		err = pm.executeDelivery(ctx, *j)
		if err != nil {
			delay := min(pm.retryDelayMax, pm.retryDelayInitial*time.Duration(1<<min(j.Attempts, 10)))
			if apiErr, ok := errors.AsType[tgbotapi.Error](err); ok && apiErr.RetryAfter > 0 {
				delay = max(delay, time.Duration(apiErr.RetryAfter)*time.Second)
			}
			reason := err.Error()
			// URL errors from Telegram SDK can contain credentials.
			configs, _ := pm.store.GetBotConfigs()
			for _, cfg := range configs {
				if cfg.Token != "" {
					reason = strings.ReplaceAll(reason, cfg.Token, "[bot token]")
				}
			}
			if retryErr := pm.store.RetryDelivery(j.ID, pm.deliveryOwner, time.Now().Add(delay), reason); retryErr != nil {
				return errors.Join(err, retryErr)
			}
			log.Printf("[route-delivery] job=%d attempt=%d retry=%s error=%s", j.ID, j.Attempts+1, delay, reason)
		}
	}
	return nil
}
func (pm *Manager) startMaintenance() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.stopped = false
	if pm.maintenanceCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	pm.maintenanceCancel = cancel
	pm.maintenanceDone = make(chan struct{})
	done := pm.maintenanceDone
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		lastSweep := time.Time{}
		warnings := map[int64]string{}
		for {
			if err := pm.RetryRouteDeliveries(ctx); err != nil && ctx.Err() == nil {
				log.Printf("[route-delivery] worker: %v", err)
			}
			if time.Since(lastSweep) >= time.Minute {
				now := time.Now()
				if err := pm.store.MaintainQueues(now); err != nil {
					log.Printf("[queue] maintenance failed: %v", err)
				}
				bots, err := pm.store.GetBotConfigs()
				if err != nil {
					log.Printf("[queue] health check failed: %v", err)
				}
				for _, b := range bots {
					h, err := pm.store.QueueHealth(b.ID, now)
					if err != nil {
						continue
					}
					if warnings[b.ID] != h.Warning {
						warnings[b.ID] = h.Warning
						if h.Warning != "" {
							log.Printf("[queue] bot=%d warning=%s pending=%d bytes=%d routes=%d", b.ID, h.Warning, h.Pending, h.Bytes, h.RoutePending)
						}
					}
				}
				lastSweep = now
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
