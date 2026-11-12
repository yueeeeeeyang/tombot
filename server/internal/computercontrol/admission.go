package computercontrol

import (
	"context"
	"database/sql"
	"errors"
)

const (
	WaitDesktopBusy     = "desktop_busy"
	WaitUserCapacity    = "user_capacity"
	WaitStartingDesktop = "starting_desktop"
)

// WaitError 保留可展示的等待原因，同时允许 Worker 用 errors.Is 统一处理等待。
type WaitError struct{ Reason string }

func (wait WaitError) Error() string {
	switch wait.Reason {
	case WaitDesktopBusy:
		return "正在等待当前机器人的桌面控制权"
	case WaitUserCapacity:
		return "正在等待用户桌面并发额度"
	default:
		return "正在等待桌面启动确认"
	}
}
func (wait WaitError) Unwrap() error { return ErrWaiting }

// admission 在用户准入锁内读取额度快照；旧任务、旧实例和被占用桌面的队列不预占其他空闲桌面的名额。
// 已启动但尚未确认清理的会话始终占用额度，不能仅凭墙上时间消失。
func admission(ctx context.Context, tx *sql.Tx, value record) (string, error) {
	var maximum, active, ahead int
	var occupied bool
	err := tx.QueryRowContext(ctx, `WITH held AS (
		SELECT desktop_id FROM tombot.computer_sessions WHERE user_id=$1 AND status IN ('starting','active','draining','quarantined')
	), eligible AS (
		SELECT s.desktop_id,s.ticket FROM tombot.computer_sessions s
		JOIN tombot.jobs j ON j.user_id=s.user_id AND j.id=s.job_id AND j.run_id=s.run_id AND j.bot_id=s.bot_id
		JOIN tombot.bots b ON b.user_id=s.user_id AND b.id=s.bot_id
		JOIN tombot.conversations v ON v.user_id=j.user_id AND v.id=j.conversation_id
		JOIN tombot.computers c ON c.user_id=s.user_id AND c.id=s.computer_id
		WHERE s.user_id=$1 AND s.status='waiting' AND s.wait_deadline_at>clock_timestamp()
		AND j.status='leased' AND j.lease_owner=s.job_owner AND j.fencing_token=s.job_fencing_token AND j.attempt=s.job_attempt
		AND j.lease_expires_at>clock_timestamp() AND j.cancel_requested_at IS NULL AND NOT b.is_archived AND NOT v.is_archived
		AND c.status='ready' AND c.node_id=s.node_id AND c.container_id=s.container_id AND c.generation=s.generation
	)
	SELECT concurrent_desktops,(SELECT count(*) FROM held),
		EXISTS(SELECT 1 FROM held WHERE desktop_id=$2) OR EXISTS(SELECT 1 FROM eligible WHERE desktop_id=$2 AND ticket<$3),
		(SELECT count(DISTINCT e.desktop_id) FROM eligible e WHERE ticket<$3 AND NOT EXISTS(SELECT 1 FROM held h WHERE h.desktop_id=e.desktop_id))
	FROM tombot.user_limits WHERE user_id=$1`, value.UserID, value.DesktopID, value.Ticket).Scan(&maximum, &active, &occupied, &ahead)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUnavailable
	}
	if err != nil {
		return "", err
	}
	if occupied {
		return WaitDesktopBusy, nil
	}
	if active+ahead >= maximum {
		return WaitUserCapacity, nil
	}
	return "", nil
}

func setWaiting(ctx context.Context, tx *sql.Tx, value record, reason string) error {
	if value.WaitReason == reason {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tombot.computer_sessions SET wait_reason=$3,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, value.UserID, value.SessionID, reason); err != nil {
		return err
	}
	value.WaitReason = reason
	return sessionEvent(ctx, tx, value, "waiting", "")
}
