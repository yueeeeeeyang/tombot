package nodecatalog

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"io.yueyang/tom/internal/computer"
)

var ErrStaleHeartbeat = errors.New("节点心跳已过期或被更新结果替代")

// observationStart 在调用节点之前取得数据库时钟；数据库事务不会跨越外部调用。
func (service *Service) observationStart(ctx context.Context) (time.Time, error) {
	var started time.Time
	err := service.transaction(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT clock_timestamp() FROM tombot.computer_nodes WHERE id=$1`, service.nodeID).Scan(&started)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return started, err
}

// recordHeartbeat 不授予 ready；首次绑定、进程重启或租约中断均要求重新通过能力检查。
// 期限锚定调用前的数据库时刻，网络延迟、等待锁及提交延迟不会刷新旧观察的有效期。
func (service *Service) recordHeartbeat(ctx context.Context, started time.Time, heartbeat computer.NodeHeartbeat, lifetime time.Duration) error {
	if heartbeat.Validate(computer.NodeQuery{NodeID: service.nodeID}) != nil || started.IsZero() || lifetime < time.Second || lifetime > 5*time.Minute {
		return ErrInvalidInput
	}
	expires := started.Add(lifetime)
	return service.transaction(ctx, func(tx *sql.Tx) error {
		var instance sql.NullString
		var generation, sequence int64
		err := tx.QueryRowContext(ctx, `SELECT runtime_id,generation,control_sequence FROM tombot.computer_nodes WHERE id=$1 FOR UPDATE`, service.nodeID).Scan(&instance, &generation, &sequence)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var now time.Time
		if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		if started.After(now) || !expires.After(now) || heartbeat.Generation < generation {
			return ErrStaleHeartbeat
		}
		if instance.Valid && heartbeat.Generation == generation {
			if instance.String != heartbeat.InstanceID {
				return ErrConflict
			}
			if heartbeat.Sequence <= sequence {
				return ErrStaleHeartbeat
			}
		} else if instance.Valid && instance.String == heartbeat.InstanceID {
			return ErrConflict
		}
		result, err := tx.ExecContext(ctx, `UPDATE tombot.computer_nodes SET
			status=CASE WHEN status='draining' THEN status WHEN runtime_id IS NULL OR generation<$3 OR control_expires_at<=clock_timestamp() THEN 'bootstrapping' ELSE status END,
			runtime_id=$2,generation=$3,control_sequence=$4,last_seen_at=$5,control_expires_at=$6
			WHERE id=$1 AND $6::timestamptz>clock_timestamp()`, service.nodeID, heartbeat.InstanceID, heartbeat.Generation, heartbeat.Sequence, started, expires)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrStaleHeartbeat
		}
		return nil
	})
}
