package computerupgrade

import (
	"context"
	"database/sql"
	"errors"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computercontrol"
	"io.yueyang/tom/internal/infra/ids"
)

func (service *Service) drain(ctx context.Context, plan migration) error {
	current := plan.upgrade.Current
	read, cancel := context.WithTimeout(ctx, service.timeout)
	var references []computercontrol.RunReference
	err := service.database.Platform(read, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(read, `SELECT user_id,job_id,run_id,bot_id,job_owner,job_fencing_token,job_attempt
 FROM tombot.computer_sessions WHERE user_id=$1 AND computer_id=$2 AND node_id=$3 AND generation=$4
 AND container_id=$5 AND status<>'released' AND next_check_at<=clock_timestamp()
 ORDER BY last_checked_at NULLS FIRST,id LIMIT 4`, current.UserID, current.ComputerID, current.NodeID, current.Generation, plan.upgrade.CurrentContainerID)
		if err != nil {
			return err
		}
		return readReferences(rows, &references)
	})
	cancel()
	if err != nil {
		return err
	}
	var blocked bool
	for _, reference := range references {
		err := service.sessions.RecoverUpgradeSession(ctx, reference)
		if errors.Is(err, computercontrol.ErrCleanupBlocked) {
			blocked = true
			break
		}
		if err != nil && !errors.Is(err, computercontrol.ErrUnavailable) && !errors.Is(err, computercontrol.ErrWaiting) && !errors.Is(err, computercontrol.ErrLeaseLost) {
			service.logger.WarnContext(ctx, "已有电脑升级等待旧会话清理回执", "node_id", current.NodeID, "computer_id", current.ComputerID, "run_id", reference.RunID)
		}
	}
	return service.advanceDrain(ctx, plan, blocked)
}

func readReferences(rows *sql.Rows, references *[]computercontrol.RunReference) (result error) {
	defer func() { result = errors.Join(result, rows.Close()) }()
	for rows.Next() {
		var reference computercontrol.RunReference
		if err := rows.Scan(&reference.UserID, &reference.JobID, &reference.RunID, &reference.BotID, &reference.OwnerID, &reference.FencingToken, &reference.Attempt); err != nil {
			return err
		}
		*references = append(*references, reference)
	}
	return rows.Err()
}

func (service *Service) advanceDrain(ctx context.Context, plan migration, blocked bool) error {
	ctx, cancel := context.WithTimeout(ctx, service.timeout)
	defer cancel()
	current := plan.upgrade.Current
	var outcome string
	err := service.database.Platform(ctx, func(tx *sql.Tx) error {
		if err := lockComputer(ctx, tx, current.UserID, current.ComputerID); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `UPDATE tombot.computer_image_upgrades p SET state=CASE
 WHEN NOT EXISTS(SELECT 1 FROM tombot.computer_sessions s WHERE s.user_id=p.user_id AND s.status<>'released') THEN 'switching'
 WHEN $3 OR p.created_at+$4::bigint*interval '1 millisecond'<=clock_timestamp() THEN 'quarantined' ELSE 'draining' END,
 next_attempt_at=clock_timestamp()+$5::bigint*interval '1 millisecond',lease_expires_at=NULL,updated_at=clock_timestamp()
 WHERE p.user_id=$1 AND p.state='draining' AND p.lease_expires_at=$2 AND p.lease_expires_at>clock_timestamp()
 RETURNING state`, current.UserID, plan.lease, blocked, drainTimeout.Milliseconds(), scanInterval.Milliseconds()).Scan(&outcome)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return errLeaseLost
	}
	if err == nil && outcome == "quarantined" {
		service.logger.ErrorContext(ctx, "已有电脑升级因旧会话清理未确认而隔离", "node_id", current.NodeID, "computer_id", current.ComputerID)
	}
	return err
}

func (service *Service) retry(ctx context.Context, plan migration) error {
	state := "switching"
	if plan.attempt >= maxAttempts {
		state = "quarantined"
	}
	delay := scanInterval * (1 << min(plan.attempt-1, maxAttempts-1))
	return service.database.Platform(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE tombot.computer_image_upgrades SET state=$3,lease_expires_at=NULL,
 next_attempt_at=clock_timestamp()+$4::bigint*interval '1 millisecond',updated_at=clock_timestamp()
 WHERE user_id=$1 AND state='switching' AND lease_expires_at=$2 AND lease_expires_at>clock_timestamp()`,
			plan.upgrade.Current.UserID, plan.lease, state, delay.Milliseconds())
		return checkUpdated(result, err)
	})
}

func (service *Service) finish(ctx context.Context, plan migration, observation computer.Observation) error {
	return service.database.Platform(ctx, func(tx *sql.Tx) error {
		current, target := plan.upgrade.Current, plan.upgrade.Target
		if err := lockComputer(ctx, tx, current.UserID, current.ComputerID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE tombot.computer_image_upgrades SET state='succeeded',lease_expires_at=NULL,updated_at=clock_timestamp()
 WHERE user_id=$1 AND state='switching' AND lease_expires_at=$2 AND lease_expires_at>clock_timestamp()`, current.UserID, plan.lease)
		if err = checkUpdated(result, err); err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx, `UPDATE tombot.computers c SET image_id=$7,generation=$8,container_id=$9,status='ready',error_code=NULL,updated_at=clock_timestamp()
 WHERE c.user_id=$1 AND c.id=$2 AND c.node_id=$3 AND c.image_id=$4 AND c.generation=$5 AND c.container_id=$6 AND c.status='stopping'
 AND NOT EXISTS (SELECT 1 FROM tombot.computer_sessions s WHERE s.user_id=c.user_id AND s.status<>'released')`,
			current.UserID, current.ComputerID, current.NodeID, plan.sourceImage, current.Generation, plan.upgrade.CurrentContainerID,
			plan.targetImage, target.Generation, observation.ContainerID)
		if err = checkUpdated(result, err); err != nil {
			return err
		}
		auditID := ids.New()
		_, err = tx.ExecContext(ctx, `INSERT INTO tombot.admin_audit_events(id,action,target_kind,target_id,request_id,result,changes)
 VALUES($1,'已有电脑切换镜像','computer',$2,$1,'succeeded',jsonb_build_object('image_id',$3::text,'generation',$4::bigint))`, auditID, current.ComputerID, plan.targetImage, target.Generation)
		return err
	})
}

func checkUpdated(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errLeaseLost
	}
	return nil
}
