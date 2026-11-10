package service

import (
	"context"
	"time"
)

// NotificationStatus 只携带定位与去重所需的字段，系统通知无需读取私有正文。
type NotificationStatus struct {
	PendingCount      int            `json:"pending_count"`
	PendingApprovalID *string        `json:"pending_approval_id"`
	CompletedTurn     *CompletedTurn `json:"completed_turn"`
}

type CompletedTurn struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	FinishedAt     int64  `json:"finished_at"`
}

const notificationSnapshot = `WITH latest AS (
 SELECT id,conversation_id,finished_at FROM turns
 WHERE status='succeeded' AND source IN ('user','routine') AND finished_at IS NOT NULL
 ORDER BY finished_at DESC,id DESC LIMIT 1
) SELECT (SELECT id FROM approvals WHERE status='pending' AND expires_at>? ORDER BY id DESC LIMIT 1),
 (SELECT COUNT(*) FROM approvals WHERE status='pending' AND expires_at>?1),
 latest.id,latest.conversation_id,latest.finished_at FROM (SELECT 1) LEFT JOIN latest ON 1`

// NotificationStatus 在同一读快照内查找最新可处理审批与用户可见任务，返回查询错误供客户端重试。
func (s *Approvals) NotificationStatus(ctx context.Context) (NotificationStatus, error) {
	var status NotificationStatus
	var id, conversation *string
	var finished *int64
	err := s.db.QueryRowContext(ctx, notificationSnapshot, time.Now().UnixMilli()).Scan(&status.PendingApprovalID, &status.PendingCount, &id, &conversation, &finished)
	if err == nil && id != nil {
		status.CompletedTurn = &CompletedTurn{ID: *id, ConversationID: *conversation, FinishedAt: *finished}
	}
	return status, err
}
