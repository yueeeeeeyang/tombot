package accounts

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"golang.org/x/sync/errgroup"
	"io.yueyang/tom/internal/computer"
)

type claim struct {
	specification    computer.Specification
	status           string
	userStatus       string
	actorID          string
	requestID        string
	deadline         time.Time
	reconciliations  int
	capacityReserved bool
	errorCode        string
}

// Run 执行有界工作池；数据库或持久状态错误向启动入口返回，不静默丢弃操作。
// 请求取消不控制本上下文，服务退出后未完成操作由下一实例在租约到期后恢复。
func (service *Service) Run(ctx context.Context) error {
	workers, ctx := errgroup.WithContext(ctx)
	for range service.options.Workers {
		workers.Go(func() error {
			ticker := time.NewTicker(service.options.PollInterval)
			defer ticker.Stop()
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				claimed, err := service.ProcessNext(ctx)
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if err != nil && !errors.Is(err, ErrLeaseLost) {
					return err
				}
				if claimed && err == nil {
					continue
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-ticker.C:
				}
			}
		})
	}
	return workers.Wait()
}

func (service *Service) claimNext(ctx context.Context) (*claim, error) {
	var claimed *claim
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		var next claim
		var actorID, errorCode sql.NullString
		specification := &next.specification
		err := transaction.QueryRowContext(ctx, `SELECT o.id,o.user_id,o.computer_id,o.status,o.actor_id,o.request_id,o.deadline_at,o.reconcile_count,o.capacity_reserved,o.error_code,
			u.status,c.node_id,i.reference,c.volume_ref,c.generation,c.cpu_millis,c.memory_bytes,c.disk_bytes,c.process_limit,o.attempt
			FROM tombot.user_creation_operations o JOIN tombot.users u ON u.id=o.user_id
			JOIN tombot.computers c ON (c.user_id,c.id)=(o.user_id,o.computer_id) JOIN tombot.computer_images i ON i.id=c.image_id
			WHERE o.status IN ('pending','running','reconciling','compensating') AND o.next_attempt_at<=transaction_timestamp()
			AND (o.lease_expires_at IS NULL OR o.lease_expires_at<=transaction_timestamp())
			ORDER BY o.next_attempt_at,o.created_at,o.id LIMIT 1 FOR UPDATE OF o SKIP LOCKED`).Scan(
			&specification.OperationID, &specification.UserID, &specification.ComputerID, &next.status, &actorID, &next.requestID, &next.deadline, &next.reconciliations, &next.capacityReserved, &errorCode,
			&next.userStatus, &specification.NodeID, &specification.ImageReference, &specification.VolumeReference, &specification.Generation,
			&specification.Resources.CPUMillis, &specification.Resources.MemoryBytes, &specification.Resources.DiskBytes, &specification.Resources.ProcessLimit, &specification.Attempt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		next.actorID = actorID.String
		next.errorCode = errorCode.String
		err = transaction.QueryRowContext(ctx, `UPDATE tombot.user_creation_operations SET
			status=CASE WHEN status='compensating' THEN status ELSE 'running' END,stage='inspecting',lease_owner=$2,
			lease_expires_at=transaction_timestamp()+$3::bigint*interval '1 millisecond',fencing_token=fencing_token+1,updated_at=transaction_timestamp()
			WHERE id=$1 RETURNING fencing_token,lease_expires_at`, specification.OperationID, service.options.WorkerID, service.options.LeaseDuration.Milliseconds()).Scan(&specification.FencingToken, &specification.LeaseExpiresAt)
		if err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.computers SET status='provisioning',updated_at=transaction_timestamp()
			WHERE user_id=$1 AND id=$2 AND status='pending'`, specification.UserID, specification.ComputerID); err != nil {
			return err
		}
		claimed = &next
		return nil
	})
	return claimed, err
}

// ProcessNext 领取一个操作并先核实节点状态。结果不确定时只进入对账，不盲目新建或释放容量。
func (service *Service) ProcessNext(ctx context.Context) (bool, error) {
	claimed, err := service.claimNext(ctx)
	if err != nil || claimed == nil {
		return false, err
	}
	if claimed.userStatus != "provisioning" || !claimed.capacityReserved {
		return true, service.quarantine(ctx, claimed, "user_state_mismatch")
	}
	if err = claimed.specification.Validate(); err != nil {
		return true, service.quarantine(ctx, claimed, "specification_invalid")
	}
	observation, err := service.call(ctx, claimed.specification, service.runtime.Inspect)
	if err != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		return true, service.uncertain(ctx, claimed, "inspection_failed")
	}
	if !observation.Matches(claimed.specification) {
		return true, service.quarantine(ctx, claimed, "node_identity_mismatch")
	}
	if observation.State == "quarantined" {
		return true, service.quarantine(ctx, claimed, "node_quarantined")
	}
	if claimed.status == "compensating" {
		return true, service.compensate(ctx, claimed, observation)
	}
	if observation.State == "ready" {
		if !observation.Capabilities.Ready() {
			return true, service.schedule(ctx, claimed, "compensating", "capability_check_failed")
		}
		return true, service.succeed(ctx, claimed, observation)
	}
	if time.Now().After(claimed.deadline) || claimed.reconciliations >= service.options.MaxReconciliations {
		return true, service.schedule(ctx, claimed, "compensating", "provision_deadline_exceeded")
	}
	observation, err = service.call(ctx, claimed.specification, service.runtime.Ensure)
	if err != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		var failure *computer.Failure
		if errors.As(err, &failure) && failure.IsPermanent {
			return true, service.schedule(ctx, claimed, "compensating", "node_creation_failed")
		}
		return true, service.uncertain(ctx, claimed, "creation_result_unknown")
	}
	if !observation.Matches(claimed.specification) {
		return true, service.quarantine(ctx, claimed, "node_identity_mismatch")
	}
	if observation.State == "quarantined" {
		return true, service.quarantine(ctx, claimed, "node_quarantined")
	}
	if observation.State == "ready" {
		if !observation.Capabilities.Ready() {
			return true, service.schedule(ctx, claimed, "compensating", "capability_check_failed")
		}
		return true, service.succeed(ctx, claimed, observation)
	}
	return true, service.uncertain(ctx, claimed, "computer_not_ready")
}

func (service *Service) call(ctx context.Context, specification computer.Specification, operation func(context.Context, computer.Specification) (computer.Observation, error)) (computer.Observation, error) {
	ctx, cancel := context.WithTimeout(ctx, service.options.CallTimeout)
	defer cancel()
	return operation(ctx, specification)
}

func (service *Service) uncertain(ctx context.Context, claimed *claim, code string) error {
	if claimed.reconciliations >= service.options.MaxReconciliations || time.Now().After(claimed.deadline) {
		return service.quarantine(ctx, claimed, code)
	}
	status := "reconciling"
	if claimed.status == "compensating" {
		status = "compensating"
	}
	return service.schedule(ctx, claimed, status, code)
}

func (service *Service) compensate(ctx context.Context, claimed *claim, observation computer.Observation) error {
	if observation.State == "absent" {
		return service.failCleanly(ctx, claimed)
	}
	observation, err := service.call(ctx, claimed.specification, service.runtime.Remove)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return service.uncertain(ctx, claimed, "cleanup_result_unknown")
	}
	if !observation.Matches(claimed.specification) {
		return service.quarantine(ctx, claimed, "node_identity_mismatch")
	}
	if observation.State != "absent" {
		return service.uncertain(ctx, claimed, "cleanup_not_confirmed")
	}
	return service.failCleanly(ctx, claimed)
}

func (service *Service) lockClaim(ctx context.Context, transaction *sql.Tx, claimed *claim) error {
	var id string
	err := transaction.QueryRowContext(ctx, `SELECT id FROM tombot.user_creation_operations WHERE id=$1 AND lease_owner=$2 AND fencing_token=$3
		AND lease_expires_at>transaction_timestamp() AND status IN ('running','compensating') FOR UPDATE`, claimed.specification.OperationID, service.options.WorkerID, claimed.specification.FencingToken).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	return err
}

func (service *Service) schedule(ctx context.Context, claimed *claim, status, code string) error {
	return service.transaction(ctx, func(transaction *sql.Tx) error {
		if err := service.lockClaim(ctx, transaction, claimed); err != nil {
			return err
		}
		delay := min(service.options.RetryDelay*time.Duration(1<<min(claimed.reconciliations, 6)), time.Minute)
		_, err := transaction.ExecContext(ctx, `UPDATE tombot.user_creation_operations SET status=$2,stage=$2,error_code=$3,reconcile_count=reconcile_count+1,
			lease_owner=NULL,lease_expires_at=NULL,next_attempt_at=transaction_timestamp()+$4::bigint*interval '1 millisecond',updated_at=transaction_timestamp() WHERE id=$1`, claimed.specification.OperationID, status, code, delay.Milliseconds())
		if err != nil {
			return err
		}
		return audit(ctx, transaction, claimed.actorID, claimed.specification.UserID, "等待开户对账", claimed.requestID, "unknown", map[string]any{"operation_id": claimed.specification.OperationID, "stage": status, "reason": code})
	})
}
