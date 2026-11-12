package accounts

import (
	"context"
	"database/sql"
	"errors"

	"io.yueyang/tom/internal/computer"
)

func requireUpdated(row *sql.Row) error {
	var id string
	err := row.Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	return err
}

func (service *Service) succeed(ctx context.Context, claimed *claim, observation computer.Observation) error {
	if !observation.Matches(claimed.specification) || observation.State != "ready" || !observation.Capabilities.Ready() {
		return ErrInvalidInput
	}
	return service.transaction(ctx, func(transaction *sql.Tx) error {
		if err := service.lockClaim(ctx, transaction, claimed); err != nil {
			return err
		}
		specification := claimed.specification
		if err := requireUpdated(transaction.QueryRowContext(ctx, `UPDATE tombot.computers SET status='ready',container_id=$4,ready_at=transaction_timestamp(),error_code=NULL,updated_at=transaction_timestamp()
			WHERE user_id=$1 AND id=$2 AND generation=$3 AND status IN ('pending','provisioning') RETURNING id`, specification.UserID, specification.ComputerID, specification.Generation, observation.ContainerID)); err != nil {
			return err
		}
		if err := requireUpdated(transaction.QueryRowContext(ctx, `UPDATE tombot.users SET status='pending_activation',revision=revision+1,updated_at=transaction_timestamp()
			WHERE id=$1 AND status='provisioning' RETURNING id`, specification.UserID)); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `UPDATE tombot.user_creation_operations SET status='succeeded',stage='completed',error_code=NULL,
			lease_owner=NULL,lease_expires_at=NULL,updated_at=transaction_timestamp() WHERE id=$1`, specification.OperationID); err != nil {
			return err
		}
		return audit(ctx, transaction, claimed.actorID, specification.UserID, "用户电脑创建完成", claimed.requestID, "succeeded", map[string]any{"operation_id": specification.OperationID, "computer_id": specification.ComputerID, "generation": specification.Generation})
	})
}

func (service *Service) failCleanly(ctx context.Context, claimed *claim) error {
	return service.transaction(ctx, func(transaction *sql.Tx) error {
		if err := service.lockClaim(ctx, transaction, claimed); err != nil {
			return err
		}
		specification := claimed.specification
		if err := requireUpdated(transaction.QueryRowContext(ctx, `UPDATE tombot.computers SET status='error',container_id=NULL,ready_at=NULL,error_code=$4,updated_at=transaction_timestamp()
			WHERE user_id=$1 AND id=$2 AND generation=$3 AND status IN ('pending','provisioning') RETURNING id`, specification.UserID, specification.ComputerID, specification.Generation, claimed.errorCode)); err != nil {
			return err
		}
		if err := requireUpdated(transaction.QueryRowContext(ctx, `UPDATE tombot.users SET status='provision_failed',revision=revision+1,updated_at=transaction_timestamp()
			WHERE id=$1 AND status='provisioning' RETURNING id`, specification.UserID)); err != nil {
			return err
		}
		if !claimed.capacityReserved {
			return ErrLeaseLost
		}
		if err := requireUpdated(transaction.QueryRowContext(ctx, `UPDATE tombot.computer_nodes SET reserved=reserved-1 WHERE id=$1 AND reserved>0 RETURNING id`, specification.NodeID)); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `UPDATE tombot.user_creation_operations SET status='failed',stage='compensated',capacity_reserved=false,
			lease_owner=NULL,lease_expires_at=NULL,updated_at=transaction_timestamp() WHERE id=$1`, specification.OperationID); err != nil {
			return err
		}
		return audit(ctx, transaction, claimed.actorID, specification.UserID, "用户创建失败且资源已清理", claimed.requestID, "failed", map[string]any{"operation_id": specification.OperationID, "reason": claimed.errorCode})
	})
}

func (service *Service) quarantine(ctx context.Context, claimed *claim, code string) error {
	return service.transaction(ctx, func(transaction *sql.Tx) error {
		if err := service.lockClaim(ctx, transaction, claimed); err != nil {
			return err
		}
		specification := claimed.specification
		if _, err := transaction.ExecContext(ctx, `UPDATE tombot.computers SET status='quarantined',error_code=$3,updated_at=transaction_timestamp()
			WHERE user_id=$1 AND id=$2 AND EXISTS(SELECT 1 FROM tombot.users WHERE id=$1 AND status IN ('provisioning','provision_failed'))`, specification.UserID, specification.ComputerID, code); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `UPDATE tombot.users SET status='provision_failed',revision=revision+1,updated_at=transaction_timestamp() WHERE id=$1 AND status='provisioning'`, specification.UserID); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `UPDATE tombot.user_creation_operations SET status='quarantined',stage='quarantined',error_code=$2,
			lease_owner=NULL,lease_expires_at=NULL,updated_at=transaction_timestamp() WHERE id=$1`, specification.OperationID, code); err != nil {
			return err
		}
		return audit(ctx, transaction, claimed.actorID, specification.UserID, "用户电脑已隔离", claimed.requestID, "unknown", map[string]any{"operation_id": specification.OperationID, "reason": code})
	})
}
