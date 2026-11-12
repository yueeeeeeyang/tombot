package accounts

import (
	"context"
	"database/sql"
	"errors"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/identity"
)

// Retry 仅重试已经确认清理完成的失败操作，保留用户和电脑标识，增加实例代次与尝试号。
// 隔离操作不能绕过对账直接重建；未完成操作应查询同一操作而不是再次申请资源。
func (service *Service) Retry(ctx context.Context, actor identity.Principal, operationID, requestID string) (Operation, error) {
	if err := service.authorizer.RequireAdmin(ctx, actor, true); err != nil {
		return Operation{}, err
	}
	return service.retry(ctx, actor.UserID, operationID, requestID, false)
}

// RetryBootstrap 供受保护的部署命令恢复首次开户失败，不要求尚未激活的管理员先登录。
func (service *Service) RetryBootstrap(ctx context.Context, operationID, requestID string) (Operation, error) {
	return service.retry(ctx, "", operationID, requestID, true)
}

func (service *Service) retry(ctx context.Context, actorID, operationID, requestID string, initial bool) (Operation, error) {
	for _, id := range []string{operationID, requestID} {
		if _, err := ulid.ParseStrict(id); err != nil {
			return Operation{}, ErrInvalidInput
		}
	}
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		var status, userID, computerID, nodeID, imageID, userStatus, computerStatus, role string
		var creator sql.NullString
		var reserved bool
		err := transaction.QueryRowContext(ctx, `SELECT o.status,o.user_id,o.computer_id,o.capacity_reserved,c.node_id,c.image_id,u.status,c.status,u.role,o.actor_id
			FROM tombot.user_creation_operations o JOIN tombot.computers c ON (c.user_id,c.id)=(o.user_id,o.computer_id) JOIN tombot.users u ON u.id=o.user_id
			WHERE o.id=$1 FOR UPDATE OF o`, operationID).Scan(&status, &userID, &computerID, &reserved, &nodeID, &imageID, &userStatus, &computerStatus, &role, &creator)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if initial && (creator.Valid || role != "admin") {
			return ErrNotFound
		}
		if status == "quarantined" {
			return ErrQuarantined
		}
		if status != "failed" || reserved || userStatus != "provision_failed" || computerStatus != "error" {
			return ErrConflict
		}
		var enabled bool
		if err = transaction.QueryRowContext(ctx, `SELECT is_enabled FROM tombot.computer_images WHERE id=$1`, imageID).Scan(&enabled); err != nil {
			return err
		}
		if !enabled {
			return ErrImageUnavailable
		}
		var returned string
		err = transaction.QueryRowContext(ctx, `SELECT tombot.reserve_computer_node($1,$2)`, nodeID, imageID).Scan(&returned)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoCapacity
		}
		if err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.computers SET status='pending',generation=generation+1,error_code=NULL,ready_at=NULL,container_id=NULL,updated_at=transaction_timestamp() WHERE user_id=$1 AND id=$2`, userID, computerID); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.users SET status='provisioning',revision=revision+1,updated_at=transaction_timestamp() WHERE id=$1`, userID); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.user_creation_operations SET status='pending',stage='reserved',attempt=attempt+1,reconcile_count=0,capacity_reserved=true,
			request_id=$2,error_code=NULL,lease_owner=NULL,lease_expires_at=NULL,next_attempt_at=transaction_timestamp(),deadline_at=transaction_timestamp()+$3::bigint*interval '1 millisecond',updated_at=transaction_timestamp() WHERE id=$1`, operationID, requestID, service.options.OperationTimeout.Milliseconds()); err != nil {
			return err
		}
		return audit(ctx, transaction, actorID, userID, "重新创建用户电脑", requestID, "succeeded", map[string]any{"operation_id": operationID, "computer_id": computerID})
	})
	if err != nil {
		return Operation{}, err
	}
	operation, err := service.readOperation(ctx, operationID)
	if err != nil {
		return Operation{}, err
	}
	return service.wait(ctx, operation)
}
