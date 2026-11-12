package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"io.yueyang/tom/internal/infra/bus"
)

type ApprovalSummary struct {
	ID               string          `json:"id"`
	RunID            string          `json:"run_id"`
	BotID            string          `json:"bot_id"`
	ActionClass      string          `json:"action_class"`
	Title            string          `json:"title"`
	RiskLevel        string          `json:"risk_level"`
	Status           string          `json:"status"`
	Detail           json.RawMessage `json:"detail"`
	ReviewerNote     *string         `json:"reviewer_note"`
	ExpiresAt        time.Time       `json:"expires_at"`
	CreatedAt        time.Time       `json:"created_at"`
	TurnID           string          `json:"turn_id"`
	WaitingQuestions int             `json:"waiting_questions"`
}

// Approvals 以数据库状态为权威，事件仅负责唤醒审批等待者。
type Approvals struct {
	db              *sql.DB
	events          *bus.Bus
	timeout         time.Duration
	questionTimeout time.Duration
}

func NewApprovals(db *sql.DB, events *bus.Bus) *Approvals {
	return &Approvals{db: db, events: events, timeout: 30 * time.Minute, questionTimeout: 24 * time.Hour}
}
func (s *Approvals) List(ctx context.Context, status, cursor string, limit int) ([]ApprovalSummary, error) {
	if limit < 1 || limit > 200 {
		return nil, badInput("Limit must be between 1 and 200.")
	}
	if status == "" {
		status = "pending"
	}
	switch status {
	case "pending", "approved", "denied", "expired":
	default:
		return nil, badInput("Unknown approval status.")
	}
	comparison, order, deadline := "<", "DESC", ""
	now := time.Now().UnixMilli()
	args := []any{now, status}
	if status == "pending" {
		comparison, order, deadline = ">", "ASC", " AND a.expires_at>?"
		args = append(args, now)
	}
	rows, err := s.db.QueryContext(ctx, `WITH question_counts AS (SELECT r.turn_id,COUNT(*) AS waiting FROM approvals a JOIN runs r ON r.id=a.run_id WHERE a.status='pending' AND a.action_class='user.question' AND a.expires_at>? GROUP BY r.turn_id)
SELECT a.id,COALESCE(a.run_id,''),COALESCE(a.bot_id,''),a.action_class,a.title,a.risk_level,a.status,a.expires_at,a.detail_json,a.reviewer_note,a.created_at,COALESCE(r.turn_id,''),COALESCE(q.waiting,0)
FROM approvals a LEFT JOIN runs r ON r.id=a.run_id LEFT JOIN question_counts q ON q.turn_id=r.turn_id WHERE a.status=?`+deadline+` AND (?='' OR a.id`+comparison+`?) ORDER BY a.id `+order+` LIMIT ?`, append(args, cursor, cursor, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	approvals := []ApprovalSummary{}
	for rows.Next() {
		var a ApprovalSummary
		var expires, created int64
		var detail string
		if err = rows.Scan(&a.ID, &a.RunID, &a.BotID, &a.ActionClass, &a.Title, &a.RiskLevel, &a.Status, &expires, &detail, &a.ReviewerNote, &created, &a.TurnID, &a.WaitingQuestions); err != nil {
			return nil, err
		}
		a.Detail = json.RawMessage(detail)
		a.ExpiresAt = time.UnixMilli(expires).UTC()
		a.CreatedAt = time.UnixMilli(created).UTC()
		approvals = append(approvals, a)
	}
	return approvals, rows.Err()
}
