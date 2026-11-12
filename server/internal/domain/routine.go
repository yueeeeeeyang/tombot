package domain

import "time"

type RoutineTarget struct {
	BotID   string `json:"bot_id,omitempty"`
	GroupID string `json:"group_id,omitempty"`
}

type RoutineDefinition struct {
	Name                 string        `json:"name"`
	Cron                 string        `json:"cron"`
	Timezone             string        `json:"timezone"`
	Prompt               string        `json:"prompt"`
	Target               RoutineTarget `json:"target"`
	OverlapPolicy        string        `json:"overlap_policy"`
	CatchUp              string        `json:"catch_up"`
	TargetMode           string        `json:"target_mode"`
	PinnedConversationID string        `json:"pinned_conversation_id,omitempty"`
	IsEnabled            bool          `json:"is_enabled"`
}

type Routine struct {
	RoutineDefinition
	ID        string     `json:"id"`
	Revision  int64      `json:"revision"`
	NextRunAt *time.Time `json:"next_run_at"`
	LastRunAt *time.Time `json:"last_run_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type RoutineRun struct {
	ID             string            `json:"id"`
	RoutineID      string            `json:"routine_id"`
	TurnID         string            `json:"turn_id,omitempty"`
	ConversationID string            `json:"conversation_id,omitempty"`
	TriggerKind    string            `json:"trigger_kind"`
	Status         string            `json:"status"`
	TurnStatus     string            `json:"turn_status,omitempty"`
	ErrorCode      string            `json:"error_code,omitempty"`
	ScheduledAt    time.Time         `json:"scheduled_at"`
	CreatedAt      time.Time         `json:"created_at"`
	Definition     RoutineDefinition `json:"-"`
	Revision       int64             `json:"-"`
}
