package models

// OnMessageSentFunc is called when a bot sends a message (for bridge outgoing notifications)
type OnMessageSentFunc func(botID int64, chatID int64, text string, msgID int, replyToMsgID int)

// BotConfig represents a bot in the unified bots table
type BotConfig struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	Token            string `json:"token"`
	BotUsername      string `json:"bot_username"`
	ManageEnabled    bool   `json:"manage_enabled"`
	ProxyEnabled     bool   `json:"proxy_enabled"`
	BackendURL       string `json:"backend_url"`
	SecretToken      string `json:"secret_token"`
	PollingTimeout   int    `json:"polling_timeout"`
	Offset           int64  `json:"offset"`
	LastError        string `json:"last_error,omitempty"`
	LastActivity     string `json:"last_activity,omitempty"`
	UpdatesForwarded int64  `json:"updates_forwarded"`
	Source           string `json:"source"` // "cli" or "web"
	BackendStatus    string `json:"backend_status"`
	BackendCheckedAt string `json:"backend_checked_at"`
	LongPollEnabled  bool   `json:"long_poll_enabled"`
	Disabled         bool   `json:"disabled"`
}

type Chat struct {
	ID          int64  `json:"id"`
	Type        string `json:"type"`
	Title       string `json:"title"`
	Username    string `json:"username"`
	MemberCount int    `json:"member_count"`
	Description string `json:"description"`
	IsAdmin     bool   `json:"is_admin"`
	UpdatedAt   string `json:"updated_at"`
	LastMsgText string `json:"last_msg_text,omitempty"`
	LastMsgFrom string `json:"last_msg_from,omitempty"`
	LastMsgDate int64  `json:"last_msg_date,omitempty"`
}

type Message struct {
	ID        int    `json:"id"`
	BotID     int64  `json:"bot_id"`
	ChatID    int64  `json:"chat_id"`
	FromUser  string `json:"from_user"`
	FromID    int64  `json:"from_id"`
	Text      string `json:"text"`
	Date      int64  `json:"date"`
	DateStr   string `json:"date_str"`
	ReplyToID int    `json:"reply_to_id,omitempty"`
	Deleted   bool   `json:"deleted"`
	MediaType string `json:"media_type,omitempty"`
	FileID    string `json:"file_id,omitempty"`
	FromIsBot bool   `json:"from_is_bot,omitempty"`
	SenderTag string `json:"sender_tag,omitempty"`
}

type ChatStats struct {
	ChatID        int64          `json:"chat_id"`
	Title         string         `json:"title"`
	TotalMessages int            `json:"total_messages"`
	TodayMessages int            `json:"today_messages"`
	ActiveUsers   int            `json:"active_users"`
	TopUsers      []UserActivity `json:"top_users"`
	HourlyStats   []HourlyStat   `json:"hourly_stats"`
}

type UserActivity struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	Count    int    `json:"count"`
}

type HourlyStat struct {
	Hour  int `json:"hour"`
	Count int `json:"count"`
}

type AdminLog struct {
	ID         int64  `json:"id"`
	ChatID     int64  `json:"chat_id"`
	Action     string `json:"action"`
	ActorName  string `json:"actor_name"`
	TargetID   int64  `json:"target_id,omitempty"`
	TargetName string `json:"target_name,omitempty"`
	Details    string `json:"details,omitempty"`
	CreatedAt  string `json:"created_at"`
}

type UserTag struct {
	ID       int64  `json:"id"`
	ChatID   int64  `json:"chat_id"`
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	Tag      string `json:"tag"`
	Color    string `json:"color"`
}

type ChatUser struct {
	UserID       int64     `json:"user_id"`
	Username     string    `json:"username"`
	MessageCount int       `json:"message_count"`
	LastSeen     string    `json:"last_seen"`
	Tags         []UserTag `json:"tags"`
}

// RouteMapping tracks source↔target message pairs for reverse routing (Source-NAT)
type RouteMapping struct {
	ID           int64  `json:"id"`
	RouteID      int64  `json:"route_id"`
	SourceBotID  int64  `json:"source_bot_id"`
	SourceChatID int64  `json:"source_chat_id"`
	SourceMsgID  int    `json:"source_msg_id"`
	TargetBotID  int64  `json:"target_bot_id"`
	TargetChatID int64  `json:"target_chat_id"`
	TargetMsgID  int    `json:"target_msg_id"`
	CreatedAt    string `json:"created_at"`
}

// Route defines a routing rule: updates matching conditions on source bot get forwarded to target bot
type Route struct {
	ID             int64  `json:"id"`
	SourceBotID    int64  `json:"source_bot_id"`
	TargetBotID    int64  `json:"target_bot_id"`
	SourceChatID   int64  `json:"source_chat_id"`
	ConditionType  string `json:"condition_type"`
	ConditionValue string `json:"condition_value"`
	Action         string `json:"action"`
	TargetChatID   int64  `json:"target_chat_id"`
	Enabled        bool   `json:"enabled"`
	Description    string `json:"description"`
	CreatedAt      string `json:"created_at"`
}

type AdminInfo struct {
	UserID             int64  `json:"user_id"`
	Username           string `json:"username"`
	Status             string `json:"status"`
	CustomTitle        string `json:"custom_title"`
	CanDeleteMessages  bool   `json:"can_delete_messages"`
	CanRestrictMembers bool   `json:"can_restrict_members"`
	CanPromoteMembers  bool   `json:"can_promote_members"`
	CanChangeInfo      bool   `json:"can_change_info"`
	CanInviteUsers     bool   `json:"can_invite_users"`
	CanPinMessages     bool   `json:"can_pin_messages"`
	CanManageChat      bool   `json:"can_manage_chat"`
}

// AuthUser represents an authenticated user
type AuthUser struct {
	ID                 int64  `json:"id"`
	Username           string `json:"username"`
	PasswordHash       string `json:"-"`
	DisplayName        string `json:"display_name"`
	Role               string `json:"role"` // "admin" or "user"
	MustChangePassword bool   `json:"must_change_password"`
	CreatedAt          string `json:"created_at"`
	LastLogin          string `json:"last_login"`
}

// LLMConfig holds configuration for the LLM routing service
type LLMConfig struct {
	ID           int64  `json:"id"`
	APIURL       string `json:"api_url"`
	APIKey       string `json:"api_key"`
	Model        string `json:"model"`
	SystemPrompt string `json:"system_prompt"`
	Enabled      bool   `json:"enabled"`
}

// LLMRouteResult holds the routing decision from the LLM
type LLMRouteResult struct {
	TargetBotID  int64  `json:"target_bot_id"`
	TargetChatID int64  `json:"target_chat_id"`
	Action       string `json:"action"`
	Reason       string `json:"reason"`
}

// BridgeConfig represents a protocol bridge in the database
type BridgeConfig struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Protocol     string `json:"protocol"`
	LinkedBotID  int64  `json:"linked_bot_id"`
	Config       string `json:"config"`
	CallbackURL  string `json:"callback_url"`
	Enabled      bool   `json:"enabled"`
	CreatedAt    string `json:"created_at"`
	LastActivity string `json:"last_activity,omitempty"`
	LastError    string `json:"last_error,omitempty"`
}

// BridgeIncomingMessage is the simple format external sources POST to us
type BridgeIncomingMessage struct {
	ExternalChatID string `json:"chat_id"`
	ExternalUserID string `json:"user_id"`
	Username       string `json:"username"`
	Text           string `json:"text"`
	ExternalMsgID  string `json:"message_id"`
	ReplyToMsgID   string `json:"reply_to"`
}

// BridgeOutgoingMessage is what we POST back to the bridge callback
type BridgeOutgoingMessage struct {
	BridgeID       int64  `json:"bridge_id"`
	ExternalChatID string `json:"chat_id"`
	Text           string `json:"text"`
	TelegramMsgID  int    `json:"telegram_msg_id"`
	ReplyToExtID   string `json:"reply_to,omitempty"`
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

// VersionInfo holds build-time version information
type VersionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
}

// UpdateCheck holds the result of a version update check
type UpdateCheck struct {
	Current         string `json:"current_version"`
	Latest          string `json:"latest_version,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	ReleaseURL      string `json:"release_url,omitempty"`
	CheckedAt       string `json:"checked_at,omitempty"`
	Error           string `json:"error,omitempty"`
}

// QueuedUpdate holds a single raw Telegram update with its update_id.
type QueuedUpdate struct {
	UpdateID int64
	Data     map[string]any
}
