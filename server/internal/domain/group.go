package domain

import "time"

const MaxGroupMembers = 32
const MaxGroupMentionRuns = 64
const MaxGroupMentionDepth = 4
const DefaultGroupBudgetMicro int64 = 6_000_000
const MaxGroupPostBytes = 16 << 10
const MaxGroupRunMessages = 64

type GroupMember struct {
	BotID      string `json:"bot_id"`
	Name       string `json:"name"`
	RoleTitle  string `json:"role_title"`
	AccentHue  int    `json:"accent_hue"`
	IsArchived bool   `json:"is_archived"`
}

type Group struct {
	AvatarPresets  []string      `json:"avatar_presets"`
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	Purpose        string        `json:"purpose"`
	MaxCostMicro   int64         `json:"max_cost_micro"`
	Revision       int64         `json:"revision"`
	ConversationID string        `json:"conversation_id"`
	Members        []GroupMember `json:"members"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

type GroupStep struct {
	BotID string `json:"bot_id"`
	Brief string `json:"brief"`
}

type GroupPlan struct {
	Steps    []GroupStep `json:"steps"`
	StopCode string      `json:"stop_code,omitempty"`
}
