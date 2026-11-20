package accounts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"io.yueyang/tom/internal/computer"
)

const maxEditAttempts = 5

type diskEdit struct {
	computer.DiskCopy
	Stage    string
	Copied   int64
	Attempts int
	Lease    time.Time
}

// RunEdits 逐段搬迁离线盘。仅在目标回执持久化后推进游标，进程退出后可从同一位置继续。
func (s *Service) RunEdits(ctx context.Context, logger *slog.Logger) error {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		work, err := s.processEdit(ctx, logger)
		if err != nil && ctx.Err() == nil {
			logger.ErrorContext(ctx, "电脑配置迁移对账失败", "reason", err)
		}
		if work && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
	return ctx.Err()
}
func (s *Service) claimEdit(ctx context.Context) (plan *diskEdit, err error) {
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		var p diskEdit
		var from, to []byte
		err := tx.QueryRowContext(ctx, `WITH next AS (SELECT e.user_id FROM tombot.computer_edits e JOIN tombot.users u ON u.id=e.user_id AND u.status='disabled'
 WHERE e.stage<>'succeeded' AND e.attempts<$1 AND e.next_attempt_at<=clock_timestamp() AND (e.lease_until IS NULL OR e.lease_until<=clock_timestamp())
 ORDER BY e.next_attempt_at,e.user_id LIMIT 1 FOR UPDATE OF e SKIP LOCKED)
 UPDATE tombot.computer_edits e SET lease_until=clock_timestamp()+$2::bigint*interval '1 millisecond',
 target_spec=jsonb_set(target_spec,'{fencing_token}',to_jsonb((target_spec->>'fencing_token')::bigint+1))
 FROM next WHERE next.user_id=e.user_id RETURNING e.edit_id,e.source_spec,e.target_spec,e.source_container,e.stage,e.copied,e.attempts,e.lease_until`, maxEditAttempts, s.options.LeaseDuration.Milliseconds()).Scan(&p.EditID, &from, &to, &p.Container, &p.Stage, &p.Copied, &p.Attempts, &p.Lease)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = json.Unmarshal(from, &p.Source); err != nil {
			return err
		}
		if err = json.Unmarshal(to, &p.Target); err != nil {
			return err
		}
		p.Target.LeaseExpiresAt = p.Lease
		plan = &p
		return nil
	})
	return
}

func (s *Service) processEdit(ctx context.Context, logger *slog.Logger) (bool, error) {
	p, err := s.claimEdit(ctx)
	if err != nil || p == nil {
		return false, err
	}
	stage, offset := p.Stage, p.Copied
	call := func(action string, chunk *computer.DiskChunk) (computer.DiskReceipt, error) {
		q := p.DiskCopy
		q.Action, q.Chunk = action, chunk
		if action == "read" || action == "write" {
			q.Offset = offset
		}
		if err := q.Validate(); err != nil {
			return computer.DiskReceipt{}, err
		}
		copier, ok := s.runtime.(computer.DiskCopier)
		if !ok {
			return computer.DiskReceipt{}, &computer.Failure{Code: "disk_copy_unavailable"}
		}
		bounded, cancel := context.WithTimeout(ctx, s.options.CallTimeout)
		defer cancel()
		r, err := copier.CopyDisk(bounded, q)
		if err == nil && !r.Matches(q) {
			return r, &computer.Failure{Code: "disk_receipt_invalid"}
		}
		return r, err
	}
	var result *computer.Observation
	switch stage {
	case "checking":
		if p.Source.NodeID == p.Target.NodeID && p.Source.Resources.DiskBytes == p.Target.Resources.DiskBytes {
			upgrader, ok := s.runtime.(computer.ImageUpgrader)
			if !ok {
				err = &computer.Failure{Code: "computer_upgrade_unavailable"}
				break
			}
			bounded, cancel := context.WithTimeout(ctx, s.options.CallTimeout)
			observed, e := upgrader.UpgradeComputer(bounded, computer.ImageUpgrade{Current: p.Source, Target: p.Target, CurrentContainerID: p.Container})
			cancel()
			err = e
			result = &observed
		} else {
			if _, err = call("preflight", nil); err == nil {
				_, err = call("freeze", nil)
			}
			if err == nil {
				stage = "copying"
			}
		}
	case "copying":
		// 每次领取只使用有限租约。已确认块在两端均可重放，回包丢失时不会重复追加。
		var read, write computer.DiskReceipt
		if read, err = call("read", nil); err == nil {
			write, err = call("write", read.Chunk)
		}
		if err == nil {
			offset = write.Offset
			if offset == p.Source.Resources.DiskBytes {
				stage = "installing"
			}
		}
	case "installing":
		var installed computer.DiskReceipt
		installed, err = call("install", nil)
		result = installed.Observation
	}
	if err == nil && result != nil {
		if !result.Matches(p.Target) || result.State != "ready" || !result.Capabilities.Ready() || result.ContainerID == p.Container {
			err = &computer.Failure{Code: "target_probe_failed"}
		} else {
			stage = "succeeded"
		}
	}
	code := ""
	if err != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		code = "node_call_unconfirmed"
		var classified *computer.Failure
		if errors.As(err, &classified) {
			code = classified.Code
		}
		logger.WarnContext(ctx, "电脑配置迁移等待重试", "user_id", p.Source.UserID, "edit_id", p.EditID, "stage", stage, "attempt", p.Attempts+1, "error_code", code)
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if code != "" {
			_, err := tx.ExecContext(ctx, `UPDATE tombot.computer_edits SET attempts=attempts+1,failure=$3,lease_until=NULL,next_attempt_at=CASE WHEN attempts+1>=$5 THEN 'infinity'::timestamptz ELSE clock_timestamp()+$4::bigint*interval '1 second' END WHERE edit_id=$1 AND lease_until=$2`, p.EditID, p.Lease, code, 1<<p.Attempts, maxEditAttempts)
			return err
		}
		to, err := json.Marshal(p.Target)
		if err != nil {
			return err
		}
		updated, err := tx.ExecContext(ctx, `UPDATE tombot.computer_edits SET stage=$3,copied=$4,attempts=0,lease_until=NULL,failure=NULL,target_spec=$5,next_attempt_at=clock_timestamp() WHERE edit_id=$1 AND lease_until=$2 AND lease_until>clock_timestamp()`, p.EditID, p.Lease, stage, offset, to)
		if err != nil {
			return err
		}
		n, err := updated.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrLeaseLost
		}
		if stage != "succeeded" {
			return nil
		}
		t := p.Target
		r := t.Resources
		if err = requireUpdated(tx.QueryRowContext(ctx, `UPDATE tombot.computers c SET node_id=$4,image_id=e.target_image_id,generation=$5,container_id=$6,cpu_millis=$7,memory_bytes=$8,disk_bytes=$9,process_limit=$10,status='ready',error_code=NULL,updated_at=clock_timestamp() FROM tombot.computer_edits e WHERE e.edit_id=$1 AND c.user_id=e.user_id AND c.id=e.computer_id AND c.generation=$2 AND c.node_id=$3 AND c.status='stopping' AND c.container_id=e.source_container RETURNING c.id`, p.EditID, p.Source.Generation, p.Source.NodeID, t.NodeID, t.Generation, result.ContainerID, r.CPUMillis, r.MemoryBytes, r.DiskBytes, r.ProcessLimit)); err != nil {
			return err
		}
		if t.NodeID != p.Source.NodeID {
			if err = requireUpdated(tx.QueryRowContext(ctx, `UPDATE tombot.computer_nodes SET reserved=reserved-1 WHERE id=$1 AND reserved>0 RETURNING id`, p.Source.NodeID)); err != nil {
				return err
			}
		}
		return audit(ctx, tx, "", t.UserID, "完成用户电脑配置迁移", p.EditID, "succeeded", map[string]any{"node_id": t.NodeID, "generation": t.Generation})
	})
	return true, err
}
