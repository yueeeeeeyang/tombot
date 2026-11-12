package accounts

import (
	"context"
	"database/sql"
	"errors"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/identity"
)

// Reconcile 恢复隔离开户的有界清理，保留同一实例代次和容量；确认资源全部不存在后才允许 Retry。
// 此操作不能用于已经启用的电脑，也不因节点重新可达就直接创建第二个实例。
func (service *Service) Reconcile(ctx context.Context, actor identity.Principal, operationID, requestID string) (Operation, error) {
	if err := service.authorizer.RequireAdmin(ctx, actor, true); err != nil {
		return Operation{}, err
	}
	return service.reconcile(ctx, actor.UserID, operationID, requestID, false)
}

// ReconcileBootstrap 仅供受保护的本地部署命令恢复首位管理员的隔离资源。
func (service *Service) ReconcileBootstrap(ctx context.Context, operationID, requestID string) (Operation, error) {
	return service.reconcile(ctx, "", operationID, requestID, true)
}

func (service *Service) reconcile(ctx context.Context, actorID, operationID, requestID string, initial bool) (Operation, error) {
	for _, id := range []string{operationID, requestID} {
		if _, err := ulid.ParseStrict(id); err != nil {
			return Operation{}, ErrInvalidInput
		}
	}
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		var userID, computerID, status, userStatus, computerStatus, role string
		var creator sql.NullString
		var reserved, wasReady, hasActivation bool
		err := transaction.QueryRowContext(ctx, `SELECT o.user_id,o.computer_id,o.status,u.status,c.status,u.role,o.actor_id,o.capacity_reserved,c.ready_at IS NOT NULL,
			EXISTS(SELECT 1 FROM tombot.activation_tokens WHERE user_id=u.id)
			FROM tombot.user_creation_operations o JOIN tombot.users u ON u.id=o.user_id JOIN tombot.computers c ON (c.user_id,c.id)=(o.user_id,o.computer_id)
			WHERE o.id=$1 FOR UPDATE OF o,u,c`, operationID).Scan(&userID, &computerID, &status, &userStatus, &computerStatus, &role, &creator, &reserved, &wasReady, &hasActivation)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if initial && (creator.Valid || role != "admin") {
			return ErrNotFound
		}
		if status != "quarantined" || userStatus != "provision_failed" || computerStatus != "quarantined" || !reserved || wasReady || hasActivation {
			return ErrConflict
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.computers SET status='provisioning',updated_at=transaction_timestamp() WHERE user_id=$1 AND id=$2`, userID, computerID); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.users SET status='provisioning',revision=revision+1,updated_at=transaction_timestamp() WHERE id=$1`, userID); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.user_creation_operations SET status='compensating',stage='reconciling_cleanup',reconcile_count=0,
			request_id=$2,error_code='reconcile_cleanup',lease_owner=NULL,lease_expires_at=NULL,next_attempt_at=transaction_timestamp(),
			deadline_at=transaction_timestamp()+$3::bigint*interval '1 millisecond',updated_at=transaction_timestamp() WHERE id=$1`, operationID, requestID, service.options.OperationTimeout.Milliseconds()); err != nil {
			return err
		}
		return audit(ctx, transaction, actorID, userID, "重新核实隔离开户资源", requestID, "succeeded", map[string]any{"operation_id": operationID, "computer_id": computerID})
	})
	if err != nil {
		return Operation{}, err
	}
	operation, err := service.readOperation(ctx, operationID)
	if err != nil {
		return Operation{}, err
	}
	operation, err = service.wait(ctx, operation)
	if errors.Is(err, ErrProvisionFailed) {
		// 对账的目标是清理已隔离的首次开户资源；用户仍未创建成功，必须另行重试开户。
		return operation, nil
	}
	return operation, err
}
