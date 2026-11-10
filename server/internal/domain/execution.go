package domain

import (
	"encoding/json"
	"time"
)

type Turn struct {
	ID             string          `json:"id"`
	ConversationID string          `json:"conversation_id"`
	Seq            int64           `json:"seq"`
	Source         string          `json:"source"`
	Status         string          `json:"status"`
	Plan           json.RawMessage `json:"plan,omitempty"`
	ErrorCode      *string         `json:"error_code"`
	CostMicro      int64           `json:"cost_micro"`
	CreatedAt      time.Time       `json:"created_at"`
	FinishedAt     *time.Time      `json:"finished_at"`
}

type Run struct {
	ID             string     `json:"id"`
	TurnID         string     `json:"turn_id"`
	ConversationID string     `json:"conversation_id"`
	BotID          string     `json:"bot_id"`
	ModelID        string     `json:"model_id"`
	Source         string     `json:"source"`
	PlanRole       string     `json:"plan_role"`
	ParentRunID    *string    `json:"parent_run_id"`
	HopDepth       int        `json:"hop_depth"`
	Status         string     `json:"status"`
	ErrorCode      *string    `json:"error_code"`
	CostMicro      int64      `json:"cost_micro"`
	BudgetMicro    *int64     `json:"budget_micro"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at"`
}
