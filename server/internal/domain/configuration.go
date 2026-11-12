package domain

import "time"

type ReviewerSettings struct {
	Enabled bool    `json:"enabled"`
	ModelID *string `json:"model_id"`
}

type Settings struct {
	Reviewer      ReviewerSettings `json:"reviewer"`
	MonthlyBudget MonthlyBudget    `json:"monthly_budget"`
}

type Provider struct {
	ID                 string    `json:"id"`
	Preset             string    `json:"preset,omitempty"`
	ModelLimitDefaults []string  `json:"model_limit_defaults,omitempty"`
	Kind               string    `json:"kind"`
	DisplayName        string    `json:"display_name"`
	BaseURL            string    `json:"base_url"`
	SecretID           *string   `json:"secret_id"`
	IsEnabled          bool      `json:"is_enabled"`
	CreatedAt          time.Time `json:"created_at"`
}

type Model struct {
	ID                  string    `json:"id"`
	ProviderID          string    `json:"provider_id"`
	ModelKey            string    `json:"model_key"`
	DisplayName         string    `json:"display_name"`
	ContextWindow       int       `json:"context_window"`
	MaxOutput           int       `json:"max_output"`
	InputPriceMicro     int64     `json:"input_price_micro"`
	OutputPriceMicro    int64     `json:"output_price_micro"`
	CacheReadPriceMicro *int64    `json:"cache_read_price_micro"`
	SupportsTools       bool      `json:"supports_tools"`
	SupportsVision      bool      `json:"supports_vision"`
	IsDefault           bool      `json:"is_default"`
	IsPriceEstimated    bool      `json:"is_price_estimated"`
	CreatedAt           time.Time `json:"created_at"`
}

type ToolPolicy struct {
	AllowedTools []string `json:"allowed_tools"`
	Approval     string   `json:"approval"`
}
type MessagingPolicy struct {
	AcceptDM          bool `json:"accept_dm"`
	CanInitiate       bool `json:"can_initiate"`
	AcceptGroupInvite bool `json:"accept_group_invite"`
}
type Bot struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	AvatarPath      *string         `json:"avatar_path"`
	AvatarPreset    string          `json:"-"`
	RoleTitle       string          `json:"role_title"`
	Soul            string          `json:"soul"`
	ModelID         *string         `json:"model_id"`
	ToolPolicy      ToolPolicy      `json:"tool_policy"`
	MessagingPolicy MessagingPolicy `json:"messaging_policy"`
	WorkspaceDir    string          `json:"workspace_dir"`
	AccentHue       int             `json:"accent_hue"`
	IsArchived      bool            `json:"is_archived"`
	ConversationID  string          `json:"conversation_id"`
	Status          string          `json:"status"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type Conversation struct {
	ID                 string        `json:"id"`
	Kind               string        `json:"kind"`
	Source             string        `json:"source"`
	BotID              *string       `json:"bot_id"`
	GroupID            *string       `json:"group_id"`
	InitiatorBotID     *string       `json:"initiator_bot_id"`
	Title              string        `json:"title"`
	Summary            string        `json:"summary"`
	LastReadSeq        int64         `json:"last_read_seq"`
	IsArchived         bool          `json:"is_archived"`
	LastMessagePreview string        `json:"last_message_preview"`
	UnreadCount        int           `json:"unread_count"`
	UnreadArtifacts    int           `json:"unread_artifacts"`
	HasActiveTurn      bool          `json:"has_active_turn"`
	BotStatus          string        `json:"bot_status"`
	AccentHue          int           `json:"accent_hue"`
	Members            []GroupMember `json:"members"`
	CreatedAt          time.Time     `json:"created_at"`
	UpdatedAt          time.Time     `json:"updated_at"`
}

type MessageBlock struct {
	Kind       string           `json:"kind"`
	Text       string           `json:"text,omitempty"`
	ToolUse    *ToolUseBlock    `json:"tool_use,omitempty"`
	ToolResult *ToolResultBlock `json:"tool_result,omitempty"`
	Artifact   *ArtifactRef     `json:"artifact,omitempty"`
	Plan       *GroupPlan       `json:"plan,omitempty"`
	Handoff    *HandoffRef      `json:"handoff,omitempty"`
}
type ToolUseBlock struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input any    `json:"input"`
}
type ToolResultBlock struct {
	ToolCallID string       `json:"tool_call_id"`
	Content    string       `json:"content"`
	IsError    bool         `json:"is_error"`
	Artifact   *ArtifactRef `json:"artifact,omitempty"`
}
type Message struct {
	Audience       []string       `json:"audience,omitempty"`
	Mentions       []string       `json:"mentions,omitempty"`
	ToolCalls      []ToolActivity `json:"tool_calls,omitempty"`
	ID             string         `json:"id"`
	ConversationID string         `json:"conversation_id"`
	TurnID         *string        `json:"turn_id"`
	RunID          *string        `json:"run_id"`
	Seq            int64          `json:"seq"`
	SenderKind     string         `json:"sender_kind"`
	SenderBotID    *string        `json:"sender_bot_id"`
	Blocks         []MessageBlock `json:"blocks"`
	Status         string         `json:"status"`
	TokenIn        int            `json:"token_in"`
	TokenOut       int            `json:"token_out"`
	CreatedAt      time.Time      `json:"created_at"`
}
