package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	tgbotapi "github.com/OvyFlash/telegram-bot-api"
)

// OnMessageSentFunc is called when a bot sends a message (for bridge outgoing notifications)
type OnMessageSentFunc func(botID int64, chatID int64, text string, msgID int, replyToMsgID int)

type Bot struct {
	api           *tgbotapi.BotAPI
	store         *Store
	botID         int64 // ID in bots table
	baseURL       string
	onMessageSent OnMessageSentFunc
}

func NewBot(token string, store *Store, botID int64, baseURL string) (*Bot, error) {
	var api *tgbotapi.BotAPI
	var err error
	if baseURL != "" && baseURL != "https://api.telegram.org" {
		api, err = tgbotapi.NewBotAPIWithAPIEndpoint(token, baseURL+"/bot%s/%s")
	} else {
		api, err = tgbotapi.NewBotAPI(token)
	}
	if err != nil {
		return nil, err
	}
	log.Printf("Bot [%d] authorized as @%s", botID, api.Self.UserName)
	return &Bot{api: api, store: store, botID: botID, baseURL: baseURL}, nil
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
	u.AllowedUpdates = []string{"message", "channel_post", "my_chat_member", "chat_member"}

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
		var update tgbotapi.Update
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			http.Error(w, "Bad request", 400)
			return
		}
		b.processUpdate(update)
		w.WriteHeader(200)
	}
}

func (b *Bot) processUpdate(update tgbotapi.Update) {
	if update.Message != nil {
		b.handleMessage(update.Message)
	}
	if update.EditedMessage != nil {
		// Treat edited messages as updates to an existing message (save latest state)
		b.handleMessage(update.EditedMessage)
	}
	if update.ChannelPost != nil {
		b.handleChannelPost(update.ChannelPost)
	}
	if update.EditedChannelPost != nil {
		b.handleChannelPost(update.EditedChannelPost)
	}
	if update.MyChatMember != nil {
		b.handleMyChatMember(update.MyChatMember)
	}
	if update.ChatMember != nil {
		b.handleChatMember(update.ChatMember)
	}
	if update.CallbackQuery != nil {
		b.handleCallbackQuery(update.CallbackQuery)
	}
}

// handleCallbackQuery tracks inline-button interactions so they appear in message
// history / user profile and can participate in routing. Stored as a synthetic
// message with MediaType="callback" and Text set to the callback data.
func (b *Bot) handleCallbackQuery(cb *tgbotapi.CallbackQuery) {
	if cb == nil || cb.From == nil {
		return
	}
	var chatID int64
	var replyToID int
	if cb.Message != nil {
		chatID = cb.Message.Chat.ID
		b.trackChat(&cb.Message.Chat)
		replyToID = cb.Message.MessageID
	}
	if chatID == 0 {
		return
	}
	fromUser := formatUsername(cb.From)
	b.store.TrackUser(chatID, cb.From.ID, fromUser)

	// Synthetic message id for callback tracking. Use high positive range (>= 2e9)
	// so it never collides with real Telegram msg ids (~1e9 max in practice).
	// Stable across runs for same (from, timestamp) so routing mappings are idempotent.
	syntheticID := 2_000_000_000 + int(time.Now().UnixNano()%1_000_000_000)
	m := Message{
		ID:        syntheticID,
		BotID:     b.botID,
		ChatID:    chatID,
		FromUser:  fromUser,
		FromID:    cb.From.ID,
		Text:      cb.Data,
		Date:      time.Now().UnixMilli(),
		ReplyToID: replyToID,
		MediaType: "callback",
	}
	if err := b.store.SaveMessage(m); err != nil {
		log.Printf("[bot %d] handleCallbackQuery: SaveMessage failed: %v", b.botID, err)
	}
}

func formatUsername(user *tgbotapi.User) string {
	name := user.FirstName
	if user.LastName != "" {
		name += " " + user.LastName
	}
	if user.UserName != "" {
		name = "@" + user.UserName
	}
	return name
}

// extractMedia returns media type and file_id from a Telegram message
func extractMedia(msg *tgbotapi.Message) (mediaType, fileID string) {
	switch {
	case msg.Photo != nil && len(msg.Photo) > 0:
		// Pick the largest photo (last in array)
		mediaType = "photo"
		fileID = msg.Photo[len(msg.Photo)-1].FileID
	case msg.Video != nil:
		mediaType = "video"
		fileID = msg.Video.FileID
	case msg.Animation != nil:
		mediaType = "animation"
		fileID = msg.Animation.FileID
	case msg.Sticker != nil:
		mediaType = "sticker"
		if msg.Sticker.Thumbnail != nil {
			fileID = msg.Sticker.Thumbnail.FileID
		} else {
			fileID = msg.Sticker.FileID
		}
	case msg.Voice != nil:
		mediaType = "voice"
		fileID = msg.Voice.FileID
	case msg.Audio != nil:
		mediaType = "audio"
		fileID = msg.Audio.FileID
	case msg.Document != nil:
		mediaType = "document"
		fileID = msg.Document.FileID
	case msg.VideoNote != nil:
		mediaType = "video_note"
		fileID = msg.VideoNote.FileID
	}
	return
}

func (b *Bot) handleMessage(msg *tgbotapi.Message) {
	b.trackChat(&msg.Chat)

	fromUser := ""
	var fromID int64
	if msg.From != nil {
		fromUser = formatUsername(msg.From)
		fromID = msg.From.ID
		b.store.TrackUser(msg.Chat.ID, fromID, fromUser)
	}

	if msg.NewChatMembers != nil {
		for i := range msg.NewChatMembers {
			u := &msg.NewChatMembers[i]
			b.store.TrackUser(msg.Chat.ID, u.ID, formatUsername(u))
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

	mediaType, fileID := extractMedia(msg)

	m := Message{
		ID:        msg.MessageID,
		BotID:     b.botID,
		ChatID:    msg.Chat.ID,
		FromUser:  fromUser,
		FromID:    fromID,
		Text:      text,
		Date:      int64(msg.Date) * 1000,
		ReplyToID: replyToID,
		MediaType: mediaType,
		FileID:    fileID,
	}
	if err := b.store.SaveMessage(m); err != nil {
		log.Printf("Error saving message: %v", err)
	}
}

func (b *Bot) handleChannelPost(msg *tgbotapi.Message) {
	b.trackChat(&msg.Chat)

	fromUser := "Channel"
	if msg.AuthorSignature != "" {
		fromUser = msg.AuthorSignature
	}

	text := msg.Text
	if text == "" {
		text = msg.Caption
	}

	mediaType, fileID := extractMedia(msg)

	m := Message{
		ID:        msg.MessageID,
		BotID:     b.botID,
		ChatID:    msg.Chat.ID,
		FromUser:  fromUser,
		Text:      text,
		Date:      int64(msg.Date) * 1000,
		MediaType: mediaType,
		FileID:    fileID,
	}
	if err := b.store.SaveMessage(m); err != nil {
		log.Printf("Error saving channel post: %v", err)
	}
}

func (b *Bot) handleMyChatMember(update *tgbotapi.ChatMemberUpdated) {
	b.trackChat(&update.Chat)
}

func (b *Bot) handleChatMember(update *tgbotapi.ChatMemberUpdated) {
	user := update.NewChatMember.User
	if user != nil {
		name := formatUsername(user)
		b.store.TrackUser(update.Chat.ID, user.ID, name)
	}
}

func (b *Bot) trackChat(chat *tgbotapi.Chat) {
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

	c := Chat{
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
		log.Printf("Error upserting chat: %v", err)
	}
}

func (b *Bot) RefreshChat(chatID int64) (*Chat, error) {
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

	c := Chat{
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
	b.store.SaveMessage(Message{
		ID:       sent.MessageID,
		BotID:    b.botID,
		ChatID:   sent.Chat.ID,
		FromUser: fromUser,
		FromID:   b.api.Self.ID,
		Text:     text,
		Date:     int64(sent.Date) * 1000,
	})
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
	b.store.SaveMessage(Message{
		ID: sent.MessageID, BotID: b.botID, ChatID: sent.Chat.ID,
		FromUser: fromUser, FromID: b.api.Self.ID,
		Text: text, Date: int64(sent.Date) * 1000,
	})
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
	b.store.SaveMessage(Message{
		ID: sent.MessageID, BotID: b.botID, ChatID: sent.Chat.ID,
		FromUser: fromUser, FromID: b.api.Self.ID,
		Text: text, Date: int64(sent.Date) * 1000,
		ReplyToID: replyToMsgID,
	})
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

func (b *Bot) GetAdmins(chatID int64) ([]AdminInfo, error) {
	admins, err := b.api.GetChatAdministrators(tgbotapi.ChatAdministratorsConfig{
		ChatConfig: tgbotapi.ChatConfig{ChatID: chatID},
	})
	if err != nil {
		return nil, err
	}

	var result []AdminInfo
	for _, a := range admins {
		username := ""
		if a.User != nil {
			username = formatUsername(a.User)
		}
		info := AdminInfo{
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

func (b *Bot) PromoteAdmin(chatID int64, userID int64, perms AdminInfo) error {
	promo := tgbotapi.PromoteChatMemberConfig{
		ChatMemberConfig: tgbotapi.ChatMemberConfig{ChatConfig: tgbotapi.ChatConfig{ChatID: chatID}, UserID: userID},
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
