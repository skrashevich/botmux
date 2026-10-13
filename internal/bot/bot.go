package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	tgbotapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/store"
)

// AllowedUpdateTypes is the shared list of update types requested from the Telegram API.
// Used by both StartPolling (library path) and ProxyManager.getUpdates (raw HTTP path).
// Kept in sync with github.com/OvyFlash/telegram-bot-api UpdateType* constants (Bot API 10.3).
var AllowedUpdateTypes = []string{
	tgbotapi.UpdateTypeMessage,
	tgbotapi.UpdateTypeEditedMessage,
	tgbotapi.UpdateTypeChannelPost,
	tgbotapi.UpdateTypeEditedChannelPost,
	tgbotapi.UpdateTypeBusinessConnection,
	tgbotapi.UpdateTypeBusinessMessage,
	tgbotapi.UpdateTypeEditedBusinessMessage,
	tgbotapi.UpdateTypeDeletedBusinessMessages,
	tgbotapi.UpdateTypeGuestMessage,
	tgbotapi.UpdateTypeMessageReaction,
	tgbotapi.UpdateTypeMessageReactionCount,
	tgbotapi.UpdateTypeInlineQuery,
	tgbotapi.UpdateTypeChosenInlineResult,
	tgbotapi.UpdateTypeCallbackQuery,
	tgbotapi.UpdateTypeShippingQuery,
	tgbotapi.UpdateTypePreCheckoutQuery,
	tgbotapi.UpdateTypePurchasedPaidMedia,
	tgbotapi.UpdateTypePoll,
	tgbotapi.UpdateTypePollAnswer,
	tgbotapi.UpdateTypeMyChatMember,
	tgbotapi.UpdateTypeChatMember,
	tgbotapi.UpdateTypeChatJoinRequest,
	tgbotapi.UpdateTypeChatBoost,
	tgbotapi.UpdateTypeRemovedChatBoost,
	tgbotapi.UpdateTypeManagedBot,
	tgbotapi.UpdateTypeSubscription,
	tgbotapi.UpdateTypeStoppedMessageGeneration,
}

type Bot struct {
	api           *tgbotapi.BotAPI
	store         *store.Store
	botID         int64 // ID in bots table
	baseURL       string
	onMessageSent models.OnMessageSentFunc
}

type initializationClient struct {
	ctx    context.Context
	client *http.Client
}

func (c initializationClient) Do(r *http.Request) (*http.Response, error) {
	return c.client.Do(r.WithContext(c.ctx))
}
func NewBot(token string, s *store.Store, botID int64, baseURL string) (*Bot, error) {
	return NewBotWithContext(context.Background(), token, s, botID, baseURL)
}
func NewBotWithContext(ctx context.Context, token string, s *store.Store, botID int64, baseURL string) (*Bot, error) {
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	endpoint := baseURL
	if endpoint == "" {
		endpoint = "https://api.telegram.org"
	}
	client := &http.Client{Timeout: 75 * time.Second}
	api, err := tgbotapi.NewBotAPIWithOptions(token, tgbotapi.WithAPIEndpoint(endpoint+"/bot%s/%s"), tgbotapi.WithHTTPClient(initializationClient{initCtx, client}))
	if err == nil {
		api.Client = client
		api.SetFileEndpoint(endpoint + "/file/bot%s/%s")
	}

	if err != nil {
		return nil, err
	}
	log.Printf("Bot [%d] authorized as @%s", botID, api.Self.UserName)
	return &Bot{api: api, store: s, botID: botID, baseURL: baseURL}, nil
}

// SetOnMessageSent sets the callback invoked when the bot sends a message.
func (b *Bot) SetOnMessageSent(fn models.OnMessageSentFunc) {
	b.onMessageSent = fn
}

// BotID returns the bot's database ID.
func (b *Bot) BotID() int64 {
	return b.botID
}

// SetBotID sets the bot's database ID.
func (b *Bot) SetBotID(id int64) {
	b.botID = id
}

// WebhookStatus represents the current webhook state of the bot
type WebhookStatus struct {
	URL            string
	HasWebhook     bool
	PendingUpdates int
}

func (b *Bot) CheckWebhook() (*WebhookStatus, error) {
	info, err := b.api.GetWebhookInfo()
	if err != nil {
		return nil, fmt.Errorf("getWebhookInfo failed: %w", err)
	}
	return &WebhookStatus{
		URL:            info.URL,
		HasWebhook:     info.URL != "",
		PendingUpdates: info.PendingUpdateCount,
	}, nil
}

func (b *Bot) SetWebhook(url string) error {
	wh, err := tgbotapi.NewWebhook(url)
	if err != nil {
		return fmt.Errorf("invalid webhook URL: %w", err)
	}
	wh.AllowedUpdates = AllowedUpdateTypes
	_, err = b.api.Request(wh)
	if err != nil {
		return fmt.Errorf("setWebhook failed: %w", err)
	}
	log.Printf("Webhook set to %s", url)
	return nil
}

func (b *Bot) RemoveWebhook() error {
	_, err := b.api.Request(tgbotapi.DeleteWebhookConfig{})
	return err
}

// ForwardMessage forwards a message from one chat to another via Telegram API
func (b *Bot) ForwardMessage(toChatID, fromChatID int64, messageID int) error {
	fwd := tgbotapi.NewForward(toChatID, fromChatID, messageID)
	_, err := b.api.Send(fwd)
	return err
}

// StartPolling for CLI bot only (UI bots use ProxyManager)
func (b *Bot) StartPolling() {
	if err := b.RemoveWebhook(); err != nil {
		log.Printf("Warning: could not remove webhook: %v", err)
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	u.AllowedUpdates = AllowedUpdateTypes

	updates := b.api.GetUpdatesChan(u)

	for update := range updates {
		b.processUpdate(update)
	}
}

func (b *Bot) WebhookHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", 405)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 10<<20))
		if err != nil {
			http.Error(w, "Bad request", 400)
			return
		}
		var update tgbotapi.Update
		if err := json.Unmarshal(body, &update); err != nil {
			http.Error(w, "Bad request", 400)
			return
		}
		if err := b.processUpdate(update); err != nil {
			http.Error(w, "history save failed", 503)
			return
		}
		var rawUpdate map[string]any
		if err := json.Unmarshal(body, &rawUpdate); err == nil {
			if err := b.ProcessRawUpdate(rawUpdate); err != nil {
				http.Error(w, "history save failed", 503)
				return
			}
		}
		w.WriteHeader(200)
	}
}

// ProcessUpdate dispatches an incoming Telegram update (exported for proxy package use).
func (b *Bot) ProcessUpdate(update tgbotapi.Update) error {
	return b.processUpdate(update)
}

func (b *Bot) processUpdate(update tgbotapi.Update) error {
	if update.Message != nil {
		if err := b.handleMessage(update.Message); err != nil {
			return err
		}
	}
	if update.EditedMessage != nil {
		// Treat edited messages as updates to an existing message (save latest state)
		if err := b.handleMessage(update.EditedMessage); err != nil {
			return err
		}
	}
	if update.ChannelPost != nil {
		if err := b.handleChannelPost(update.ChannelPost); err != nil {
			return err
		}
	}
	if update.EditedChannelPost != nil {
		if err := b.handleChannelPost(update.EditedChannelPost); err != nil {
			return err
		}
	}
	if update.BusinessMessage != nil {
		if err := b.handleMessage(update.BusinessMessage); err != nil {
			return err
		}
	}
	if update.EditedBusinessMessage != nil {
		if err := b.handleMessage(update.EditedBusinessMessage); err != nil {
			return err
		}
	}
	if update.GuestMessage != nil {
		if err := b.handleMessage(update.GuestMessage); err != nil {
			return err
		}
	}
	if update.ManagedBot != nil {
		log.Printf("Bot [%d] managed_bot update: bot_id=%d user_id=%d",
			b.botID, update.ManagedBot.Bot.ID, update.ManagedBot.User.ID)
	}
	if update.MyChatMember != nil {
		if err := b.handleMyChatMember(update.MyChatMember); err != nil {
			return err
		}
	}
	if update.ChatMember != nil {
		if err := b.handleChatMember(update.ChatMember); err != nil {
			return err
		}
	}
	if update.ChatJoinRequest != nil {
		if err := b.trackChat(&update.ChatJoinRequest.Chat); err != nil {
			return err
		}
	}
	if update.CallbackQuery != nil {
		if err := b.handleCallbackQuery(update.CallbackQuery); err != nil {
			return err
		}
	}
	return nil
}

// handleCallbackQuery tracks inline-button interactions so they appear in message
// history / user profile and can participate in routing. Stored as a synthetic
// message with MediaType="callback" and a negative local ID. Telegram callback
// IDs and inline callbacks are retained in callback_events.
func (b *Bot) handleCallbackQuery(cb *tgbotapi.CallbackQuery) error {
	if cb == nil || cb.From == nil {
		return nil
	}
	e := models.CallbackEvent{BotID: b.botID, CallbackID: cb.ID, InlineMessageID: cb.InlineMessageID, FromID: cb.From.ID, FromUser: FormatUsername(cb.From), Data: cb.Data, GameShortName: cb.GameShortName, Date: time.Now().UnixMilli()}
	if cb.Message != nil {
		if cb.Message.ReceiverUser != nil || cb.Message.EphemeralMessageID != 0 {
			return nil
		}
		e.ChatID = cb.Message.Chat.ID
		e.MessageID = cb.Message.MessageID
		if err := b.trackChat(&cb.Message.Chat); err != nil {
			return err
		}
	}
	if e.CallbackID == "" {
		return nil
	}
	id, _, err := b.store.SaveCallbackEvent(e)
	if err != nil {
		return fmt.Errorf("callback save: %w", err)
	}
	if e.ChatID == 0 {
		return nil
	} // Inline callbacks are available via the event API.
	if err := b.store.TrackUser(e.ChatID, e.FromID, e.FromUser); err != nil {
		return err
	}
	// Negative IDs are local events, never actionable Telegram message IDs.
	if err := b.store.SaveMessage(models.Message{ID: -int(id), BotID: b.botID, ChatID: e.ChatID, FromUser: e.FromUser, FromID: e.FromID, Text: e.Data, Date: e.Date, ReplyToID: e.MessageID, MediaType: "callback", CallbackID: e.CallbackID}); err != nil {
		return fmt.Errorf("callback history save: %w", err)
	}
	return nil
}

// FormatUsername formats a Telegram user's display name.
func FormatUsername(user *tgbotapi.User) string {
	name := user.FirstName
	if user.LastName != "" {
		name += " " + user.LastName
	}
	if user.UserName != "" {
		name = "@" + user.UserName
	}
	return name
}

func (b *Bot) handleMessage(msg *tgbotapi.Message) error {
	if msg.EphemeralMessageID != 0 || msg.ReceiverUser != nil {
		return nil // Ephemeral messages do not belong in the shared message feed.
	}
	if err := b.trackChat(&msg.Chat); err != nil {
		return err
	}
	if err := b.trackTopic(msg); err != nil {
		return err
	}

	fromUser := ""
	var fromID int64
	var fromIsBot bool
	if msg.From != nil {
		fromUser = FormatUsername(msg.From)
		fromID = msg.From.ID
		fromIsBot = msg.From.IsBot
		if err := b.store.TrackUser(msg.Chat.ID, fromID, fromUser); err != nil {
			return err
		}
	}

	if msg.NewChatMembers != nil {
		for i := range msg.NewChatMembers {
			u := &msg.NewChatMembers[i]
			if err := b.store.TrackUser(msg.Chat.ID, u.ID, FormatUsername(u)); err != nil {
				return err
			}
		}
	}

	replyToID := 0
	if msg.ReplyToMessage != nil {
		replyToID = msg.ReplyToMessage.MessageID
	}

	text := msg.Text
	if text == "" {
		text = msg.Caption
	}
	if text == "" && msg.Invoice != nil {
		text = msg.Invoice.Title
	}
	if text == "" && msg.Game != nil {
		text = msg.Game.Text
		if text == "" {
			text = msg.Game.Title
		}
	}

	mediaType, fileID := ExtractMedia(msg)

	m := models.Message{
		ID:           msg.MessageID,
		BotID:        b.botID,
		ChatID:       msg.Chat.ID,
		FromUser:     fromUser,
		FromID:       fromID,
		Text:         text,
		RichText:     ExtractRichText(msg.RichMessage),
		ReplyMarkup:  marshalOptional(msg.ReplyMarkup),
		RichMessage:  marshalOptional(msg.RichMessage),
		ThreadID:     msg.MessageThreadID,
		MediaGroupID: msg.MediaGroupID,
		Complete:     true,
		Date:         int64(msg.Date) * 1000,
		ReplyToID:    replyToID,
		MediaType:    mediaType,
		FileID:       fileID,
		FromIsBot:    fromIsBot,
		SenderTag:    msg.SenderTag,
	}
	if err := b.store.SaveMessage(m); err != nil {
		return fmt.Errorf("save message: %w", err)
	}
	return nil
}

func (b *Bot) handleChannelPost(msg *tgbotapi.Message) error {
	if msg.EphemeralMessageID != 0 || msg.ReceiverUser != nil {
		return nil
	}
	if err := b.trackChat(&msg.Chat); err != nil {
		return err
	}
	if err := b.trackTopic(msg); err != nil {
		return err
	}

	fromUser := "Channel"
	if msg.AuthorSignature != "" {
		fromUser = msg.AuthorSignature
	}

	text := msg.Text
	if text == "" {
		text = msg.Caption
	}
	if text == "" && msg.Invoice != nil {
		text = msg.Invoice.Title
	}
	if text == "" && msg.Game != nil {
		text = msg.Game.Text
		if text == "" {
			text = msg.Game.Title
		}
	}

	mediaType, fileID := ExtractMedia(msg)

	m := models.Message{
		ID:           msg.MessageID,
		BotID:        b.botID,
		ChatID:       msg.Chat.ID,
		FromUser:     fromUser,
		Text:         text,
		RichText:     ExtractRichText(msg.RichMessage),
		ReplyMarkup:  marshalOptional(msg.ReplyMarkup),
		RichMessage:  marshalOptional(msg.RichMessage),
		ThreadID:     msg.MessageThreadID,
		MediaGroupID: msg.MediaGroupID,
		Complete:     true,
		Date:         int64(msg.Date) * 1000,
		MediaType:    mediaType,
		FileID:       fileID,
		SenderTag:    msg.SenderTag,
	}
	if err := b.store.SaveMessage(m); err != nil {
		return fmt.Errorf("save channel post: %w", err)
	}
	return nil
}

func (b *Bot) handleMyChatMember(update *tgbotapi.ChatMemberUpdated) error {
	if err := b.trackChat(&update.Chat); err != nil {
		return err
	}
	return nil
}

func (b *Bot) handleChatMember(update *tgbotapi.ChatMemberUpdated) error {
	user := update.NewChatMember.User
	if user != nil {
		name := FormatUsername(user)
		if err := b.store.TrackUser(update.Chat.ID, user.ID, name); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bot) trackChat(chat *tgbotapi.Chat) error {
	isAdmin := false
	memberCount := 0

	me, err := b.api.GetChatMember(tgbotapi.GetChatMemberConfig{
		ChatConfigWithUser: tgbotapi.ChatConfigWithUser{
			ChatConfig: tgbotapi.ChatConfig{ChatID: chat.ID},
			UserID:     b.api.Self.ID,
		},
	})
	if err == nil {
		isAdmin = me.IsAdministrator() || me.IsCreator()
	}

	count, err := b.api.GetChatMembersCount(tgbotapi.ChatMemberCountConfig{
		ChatConfig: tgbotapi.ChatConfig{ChatID: chat.ID},
	})
	if err == nil {
		memberCount = count
	}

	desc := ""
	fullChat, err := b.api.GetChat(tgbotapi.ChatInfoConfig{
		ChatConfig: tgbotapi.ChatConfig{ChatID: chat.ID},
	})
	if err == nil {
		desc = fullChat.Description
	}

	title := chat.Title
	if title == "" && chat.Type == "private" {
		title = chat.FirstName
		if chat.LastName != "" {
			title += " " + chat.LastName
		}
	}

	c := models.Chat{
		ID:          chat.ID,
		Type:        chat.Type,
		Title:       title,
		Username:    chat.UserName,
		MemberCount: memberCount,
		Description: desc,
		IsAdmin:     isAdmin,
		UpdatedAt:   time.Now().Format(time.RFC3339),
	}

	if err := b.store.UpsertChat(b.botID, c); err != nil {
		return fmt.Errorf("save chat: %w", err)
	}
	return nil
}

func (b *Bot) RefreshChat(chatID int64) (*models.Chat, error) {
	fullChat, err := b.api.GetChat(tgbotapi.ChatInfoConfig{
		ChatConfig: tgbotapi.ChatConfig{ChatID: chatID},
	})
	if err != nil {
		return nil, err
	}

	isAdmin := false
	me, err := b.api.GetChatMember(tgbotapi.GetChatMemberConfig{
		ChatConfigWithUser: tgbotapi.ChatConfigWithUser{
			ChatConfig: tgbotapi.ChatConfig{ChatID: chatID},
			UserID:     b.api.Self.ID,
		},
	})
	if err == nil {
		isAdmin = me.IsAdministrator() || me.IsCreator()
	}

	memberCount := 0
	count, err := b.api.GetChatMembersCount(tgbotapi.ChatMemberCountConfig{
		ChatConfig: tgbotapi.ChatConfig{ChatID: chatID},
	})
	if err == nil {
		memberCount = count
	}

	c := models.Chat{
		ID:          fullChat.ID,
		Type:        fullChat.Type,
		Title:       fullChat.Title,
		Username:    fullChat.UserName,
		MemberCount: memberCount,
		Description: fullChat.Description,
		IsAdmin:     isAdmin,
		UpdatedAt:   time.Now().Format(time.RFC3339),
	}

	if err := b.store.UpsertChat(b.botID, c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (b *Bot) SendMessage(chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	sent, err := b.api.Send(msg)
	if err != nil {
		return err
	}
	// Save the sent message to DB
	fromUser := "@" + b.api.Self.UserName
	if err := b.store.SaveMessage(models.Message{
		ID:        sent.MessageID,
		BotID:     b.botID,
		ChatID:    sent.Chat.ID,
		FromUser:  fromUser,
		FromID:    b.api.Self.ID,
		FromIsBot: true,
		Text:      text,
		Date:      int64(sent.Date) * 1000,
	}); err != nil {
		return fmt.Errorf("message sent but history save failed: %w", err)
	}
	if b.onMessageSent != nil {
		b.onMessageSent(b.botID, chatID, text, sent.MessageID, 0)
	}
	return nil
}

// SendMessageGetID sends a message and returns the sent message ID
func (b *Bot) SendMessageGetID(chatID int64, text string) (int, error) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	sent, err := b.api.Send(msg)
	if err != nil {
		return 0, err
	}
	fromUser := "@" + b.api.Self.UserName
	if err := b.store.SaveMessage(models.Message{
		ID: sent.MessageID, BotID: b.botID, ChatID: sent.Chat.ID,
		FromUser: fromUser, FromID: b.api.Self.ID, FromIsBot: true,
		Text: text, Date: int64(sent.Date) * 1000,
	}); err != nil {
		return sent.MessageID, fmt.Errorf("message sent but history save failed: %w", err)
	}
	if b.onMessageSent != nil {
		b.onMessageSent(b.botID, chatID, text, sent.MessageID, 0)
	}
	return sent.MessageID, nil
}

// SendMessageReply sends a message as a reply to a specific message
func (b *Bot) SendMessageReply(chatID int64, text string, replyToMsgID int) (int, error) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	msg.ReplyParameters.MessageID = replyToMsgID
	sent, err := b.api.Send(msg)
	if err != nil {
		return 0, err
	}
	fromUser := "@" + b.api.Self.UserName
	if err := b.store.SaveMessage(models.Message{
		ID: sent.MessageID, BotID: b.botID, ChatID: sent.Chat.ID,
		FromUser: fromUser, FromID: b.api.Self.ID, FromIsBot: true,
		Text: text, Date: int64(sent.Date) * 1000,
		ReplyToID: replyToMsgID,
	}); err != nil {
		return sent.MessageID, fmt.Errorf("message sent but history save failed: %w", err)
	}
	if b.onMessageSent != nil {
		b.onMessageSent(b.botID, chatID, text, sent.MessageID, replyToMsgID)
	}
	return sent.MessageID, nil
}

// ForwardMessageGetID forwards a message and returns the new message ID
func (b *Bot) ForwardMessageGetID(toChatID, fromChatID int64, messageID int) (int, error) {
	fwd := tgbotapi.NewForward(toChatID, fromChatID, messageID)
	sent, err := b.api.Send(fwd)
	if err != nil {
		return 0, err
	}
	return sent.MessageID, nil
}

func (b *Bot) PinMessage(chatID int64, messageID int) error {
	pin := tgbotapi.PinChatMessageConfig{BaseChatMessage: tgbotapi.BaseChatMessage{ChatConfig: tgbotapi.ChatConfig{ChatID: chatID}, MessageID: messageID}}
	_, err := b.api.Request(pin)
	return err
}

func (b *Bot) UnpinMessage(chatID int64, messageID int) error {
	unpin := tgbotapi.UnpinChatMessageConfig{BaseChatMessage: tgbotapi.BaseChatMessage{ChatConfig: tgbotapi.ChatConfig{ChatID: chatID}, MessageID: messageID}}
	_, err := b.api.Request(unpin)
	return err
}

func (b *Bot) DeleteMessage(chatID int64, messageID int) error {
	del := tgbotapi.NewDeleteMessage(chatID, messageID)
	_, err := b.api.Request(del)
	return err
}

func (b *Bot) BanUser(chatID int64, userID int64) error {
	ban := tgbotapi.BanChatMemberConfig{
		ChatMemberConfig: tgbotapi.ChatMemberConfig{ChatConfig: tgbotapi.ChatConfig{ChatID: chatID}, UserID: userID},
	}
	_, err := b.api.Request(ban)
	return err
}

func (b *Bot) UnbanUser(chatID int64, userID int64) error {
	unban := tgbotapi.UnbanChatMemberConfig{
		ChatMemberConfig: tgbotapi.ChatMemberConfig{ChatConfig: tgbotapi.ChatConfig{ChatID: chatID}, UserID: userID},
		OnlyIfBanned:     true,
	}
	_, err := b.api.Request(unban)
	return err
}

func (b *Bot) GetAdmins(chatID int64) ([]models.AdminInfo, error) {
	admins, err := b.api.GetChatAdministrators(tgbotapi.ChatAdministratorsConfig{
		ChatConfig: tgbotapi.ChatConfig{ChatID: chatID},
	})
	if err != nil {
		return nil, err
	}

	var result []models.AdminInfo
	for _, a := range admins {
		username := ""
		if a.User != nil {
			username = FormatUsername(a.User)
		}
		info := models.AdminInfo{
			UserID:      a.User.ID,
			Username:    username,
			Status:      a.Status,
			CustomTitle: a.CustomTitle,
		}
		if a.CanDeleteMessages {
			info.CanDeleteMessages = true
		}
		if a.CanRestrictMembers {
			info.CanRestrictMembers = true
		}
		if a.CanPromoteMembers {
			info.CanPromoteMembers = true
		}
		if a.CanChangeInfo {
			info.CanChangeInfo = true
		}
		if a.CanInviteUsers {
			info.CanInviteUsers = true
		}
		if a.CanPinMessages {
			info.CanPinMessages = true
		}
		if a.CanManageChat {
			info.CanManageChat = true
		}
		if a.Status == "creator" {
			info.CanDeleteMessages = true
			info.CanRestrictMembers = true
			info.CanPromoteMembers = true
			info.CanChangeInfo = true
			info.CanInviteUsers = true
			info.CanPinMessages = true
			info.CanManageChat = true
		}
		result = append(result, info)
	}
	return result, nil
}

func (b *Bot) PromoteAdmin(chatID int64, userID int64, perms models.AdminInfo) error {
	promo := tgbotapi.PromoteChatMemberConfig{
		ChatMemberConfig:   tgbotapi.ChatMemberConfig{ChatConfig: tgbotapi.ChatConfig{ChatID: chatID}, UserID: userID},
		CanDeleteMessages:  perms.CanDeleteMessages,
		CanRestrictMembers: perms.CanRestrictMembers,
		CanPromoteMembers:  perms.CanPromoteMembers,
		CanChangeInfo:      perms.CanChangeInfo,
		CanInviteUsers:     perms.CanInviteUsers,
		CanPinMessages:     perms.CanPinMessages,
		CanManageChat:      perms.CanManageChat,
	}
	_, err := b.api.Request(promo)
	return err
}

func (b *Bot) DemoteAdmin(chatID int64, userID int64) error {
	promo := tgbotapi.PromoteChatMemberConfig{
		ChatMemberConfig: tgbotapi.ChatMemberConfig{ChatConfig: tgbotapi.ChatConfig{ChatID: chatID}, UserID: userID},
	}
	_, err := b.api.Request(promo)
	return err
}

func (b *Bot) SetAdminTitle(chatID int64, userID int64, title string) error {
	cfg := tgbotapi.SetChatAdministratorCustomTitle{
		ChatMemberConfig: tgbotapi.ChatMemberConfig{ChatConfig: tgbotapi.ChatConfig{ChatID: chatID}, UserID: userID},
		CustomTitle:      title,
	}
	_, err := b.api.Request(cfg)
	return err
}

func (b *Bot) GetBotInfo() string {
	return b.api.Self.UserName
}

func (b *Bot) GetBotName() string {
	return "Bot (@" + b.api.Self.UserName + ")"
}

func (b *Bot) GetSelfID() int64 {
	return b.api.Self.ID
}

// GetToken returns the bot's API token.
func (b *Bot) GetToken() string {
	return b.api.Token
}

// GetSelfUserName returns the bot's username.
func (b *Bot) GetSelfUserName() string {
	return b.api.Self.UserName
}
