package domain

import "time"

const MaxGroupMembers = 32
const MaxGroupSpeakers = 8
const DefaultGroupBudgetMicro int64 = 6_000_000
const MaxGroupPostBytes = 16 << 10
const MaxGroupRunMessages = 64

type GroupMember struct {
	BotID      string `json:"bot_id"`
	Name       string `json:"name"`
	RoleTitle  string `json:"role_title"`
	RoleNote   string `json:"role_note"`
	AccentHue  int    `json:"accent_hue"`
	IsArchived bool   `json:"is_archived"`
}

type Group struct {
	ID               string        `json:"id"`
	Name             string        `json:"name"`
	Purpose          string        `json:"purpose"`
	CoordinatorBotID *string       `json:"coordinator_bot_id"`
	Routing          string        `json:"routing"`
	MaxSpeakers      int           `json:"max_speakers"`
	MaxCostMicro     int64         `json:"max_cost_micro"`
	AutoSummary      bool          `json:"auto_summary"`
	Revision         int64         `json:"revision"`
	ConversationID   string        `json:"conversation_id"`
	Members          []GroupMember `json:"members"`
	CreatedAt        time.Time     `json:"created_at"`
	UpdatedAt        time.Time     `json:"updated_at"`
}

type GroupStep struct {
	BotID string `json:"bot_id"`
	Brief string `json:"brief"`
}

type GroupPlan struct {
	Steps    []GroupStep `json:"steps"`
	Reason   string      `json:"reason"`
	Fallback bool        `json:"fallback"`
	Clamped  bool        `json:"clamped"`
	StopCode string      `json:"stop_code,omitempty"`
}
