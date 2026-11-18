package computerupgrade

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"io.yueyang/tom/internal/computer"
)

// eligibleNode 与新电脑准入使用同一固定镜像和有效期；未发布支持声明的节点不迁移已有电脑。
const eligibleNode = `nc.supports_computer_upgrades AND n.status='ready' AND n.verified_image_id=nc.image_id
 AND n.control_expires_at>clock_timestamp() AND n.capability_expires_at>clock_timestamp()
 AND NOT EXISTS (SELECT 1 FROM tombot.node_upgrades m WHERE m.node_id=n.id AND NOT m.is_finished)`

func (service *Service) claim(ctx context.Context) (*migration, error) {
	ctx, cancel := context.WithTimeout(ctx, service.timeout)
	defer cancel()
	var plan *migration
	err := service.database.Platform(ctx, func(tx *sql.Tx) error {
		var userID, computerID string
		err := tx.QueryRowContext(ctx, `SELECT c.user_id,c.id FROM tombot.computers c
 JOIN tombot.computer_nodes n ON n.id=c.node_id JOIN tombot.node_connections nc ON nc.node_id=n.id
 LEFT JOIN tombot.computer_image_upgrades p ON p.user_id=c.user_id
 WHERE `+eligibleNode+` AND ((p.state IN ('draining','switching') AND p.target_image_id=nc.image_id
 AND p.next_attempt_at<=clock_timestamp() AND (p.lease_expires_at IS NULL OR p.lease_expires_at<=clock_timestamp()))
 OR (c.status='ready' AND c.image_id<>nc.image_id AND (p.user_id IS NULL OR p.state='succeeded')))
 ORDER BY p.next_attempt_at NULLS FIRST,c.user_id LIMIT 1`).Scan(&userID, &computerID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = lockComputer(ctx, tx, userID, computerID); err != nil {
			return err
		}
		plan, err = service.claimComputer(ctx, tx, userID, computerID)
		return err
	})
	return plan, err
}

func (service *Service) claimComputer(ctx context.Context, tx *sql.Tx, userID, computerID string) (*migration, error) {
	var current computer.Specification
	var sourceImage, targetImage, targetReference, containerID, status string
	err := tx.QueryRowContext(ctx, `SELECT c.node_id,c.image_id,i.reference,c.generation,c.container_id,c.status,c.volume_ref,
 c.cpu_millis,c.memory_bytes,c.disk_bytes,c.process_limit,nc.image_id,target.reference
 FROM tombot.computers c JOIN tombot.computer_images i ON i.id=c.image_id
 JOIN tombot.computer_nodes n ON n.id=c.node_id JOIN tombot.node_connections nc ON nc.node_id=n.id
 JOIN tombot.computer_images target ON target.id=nc.image_id AND target.is_enabled
 WHERE c.user_id=$1 AND c.id=$2 AND c.status IN ('ready','stopping') AND `+eligibleNode+`
 FOR SHARE OF n`, userID, computerID).Scan(&current.NodeID, &sourceImage, &current.ImageReference, &current.Generation,
		&containerID, &status, &current.VolumeReference, &current.Resources.CPUMillis, &current.Resources.MemoryBytes,
		&current.Resources.DiskBytes, &current.Resources.ProcessLimit, &targetImage, &targetReference)
	if errors.Is(err, sql.ErrNoRows) || err == nil && sourceImage == targetImage {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	current.UserID, current.ComputerID = userID, computerID
	var state, plannedImage string
	var target []byte
	var claimable bool
	var attempts int
	err = tx.QueryRowContext(ctx, `SELECT state,target_image_id,target_specification,attempt_count,
 next_attempt_at<=clock_timestamp() AND (lease_expires_at IS NULL OR lease_expires_at<=clock_timestamp())
 FROM tombot.computer_image_upgrades WHERE user_id=$1`, userID).Scan(&state, &plannedImage, &target, &attempts, &claimable)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if state == "quarantined" || status == "stopping" && (!claimable || plannedImage != targetImage) {
		return nil, nil
	}
	if state == "switching" && attempts >= maxAttempts {
		_, err = tx.ExecContext(ctx, `UPDATE tombot.computer_image_upgrades SET state='quarantined',lease_expires_at=NULL,updated_at=clock_timestamp() WHERE user_id=$1`, userID)
		return nil, err
	}
	if status == "ready" {
		if state != "" && state != "succeeded" {
			return nil, errors.New("已有电脑与持久升级状态不一致")
		}
		if err = makePlan(ctx, tx, current, sourceImage, targetImage, targetReference, containerID, target); err != nil {
			return nil, err
		}
	}
	return service.lease(ctx, tx, userID)
}

func makePlan(ctx context.Context, tx *sql.Tx, current computer.Specification, sourceImage, targetImage, targetReference, containerID string, priorTarget []byte) error {
	if len(priorTarget) > 0 {
		prior, err := decodeSpecification(priorTarget)
		if err != nil || prior.UserID != current.UserID || prior.ComputerID != current.ComputerID || prior.Generation != current.Generation || prior.ImageReference != current.ImageReference {
			return errors.New("原实例与上次升级验收不一致")
		}
		current.OperationID, current.Attempt, current.FencingToken, current.LeaseExpiresAt = prior.OperationID, prior.Attempt, prior.FencingToken, prior.LeaseExpiresAt
	} else if err := tx.QueryRowContext(ctx, `SELECT id,attempt,fencing_token,updated_at FROM tombot.user_creation_operations
 WHERE user_id=$1 AND computer_id=$2 AND status='succeeded'`, current.UserID, current.ComputerID).Scan(&current.OperationID, &current.Attempt, &current.FencingToken, &current.LeaseExpiresAt); err != nil {
		return err
	}
	target := current
	target.ImageReference = targetReference
	target.Generation++
	target.Attempt++
	target.FencingToken++
	if err := (computer.ImageUpgrade{Current: current, Target: target, CurrentContainerID: containerID}).Validate(); err != nil {
		return err
	}
	source, err := json.Marshal(current)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(target)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.computer_image_upgrades
 (user_id,computer_id,node_id,source_image_id,target_image_id,source_generation,source_container_id,source_specification,target_specification,state)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'draining') ON CONFLICT (user_id) DO UPDATE SET
 source_image_id=$4,target_image_id=$5,source_generation=$6,source_container_id=$7,source_specification=$8,target_specification=$9,
 state='draining',attempt_count=0,lease_expires_at=NULL,next_attempt_at=clock_timestamp(),created_at=clock_timestamp(),updated_at=clock_timestamp()`,
		current.UserID, current.ComputerID, current.NodeID, sourceImage, targetImage, current.Generation, containerID, source, encoded); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE tombot.computers SET status='stopping',error_code=NULL,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, current.UserID, current.ComputerID)
	return err
}

func (service *Service) lease(ctx context.Context, tx *sql.Tx, userID string) (*migration, error) {
	var plan migration
	var source, target []byte
	err := tx.QueryRowContext(ctx, `UPDATE tombot.computer_image_upgrades SET
 lease_expires_at=clock_timestamp()+$2::bigint*interval '1 millisecond',updated_at=clock_timestamp(),
 attempt_count=attempt_count+CASE WHEN state='switching' THEN 1 ELSE 0 END
 WHERE user_id=$1 AND state IN ('draining','switching') RETURNING
 source_image_id,target_image_id,source_container_id,source_specification,target_specification,state,attempt_count,lease_expires_at`,
		userID, (2*service.timeout).Milliseconds()).Scan(&plan.sourceImage, &plan.targetImage, &plan.upgrade.CurrentContainerID, &source, &target, &plan.state, &plan.attempt, &plan.lease)
	if err != nil {
		return nil, err
	}
	plan.upgrade.Current, err = decodeSpecification(source)
	if err != nil {
		return nil, err
	}
	plan.upgrade.Target, err = decodeSpecification(target)
	if err != nil {
		return nil, err
	}
	if plan.state == "switching" {
		plan.upgrade.Target.LeaseExpiresAt = plan.lease
		plan.upgrade.Target.FencingToken++
		encoded, err := json.Marshal(plan.upgrade.Target)
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE tombot.computer_image_upgrades SET target_specification=$2 WHERE user_id=$1`, userID, encoded); err != nil {
			return nil, err
		}
	}
	return &plan, plan.upgrade.Validate()
}
