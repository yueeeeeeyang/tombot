package nodecatalog

import (
	"context"
	"database/sql"
	"errors"

	"io.yueyang/tom/internal/infra/ids"
)

// UpgradeImageInTransaction 在已验证原节点证书的登记事务中切换分配镜像，已有电脑继续引用原镜像。
// 切换先撤销旧能力证据，后续验收必须先清理旧验收资源。
func (service *Service) UpgradeImageInTransaction(ctx context.Context, tx *sql.Tx, registration Registration) error {
	if tx == nil || registration.Validate() != nil || registration.PreviousImageID == "" {
		return ErrInvalidInput
	}
	if err := service.RequireNoVerificationLeaseInTransaction(ctx, tx); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO tombot.computer_images(id,reference,is_enabled) VALUES($1,$2,true) ON CONFLICT DO NOTHING`, registration.ImageID, registration.ImageReference)
	if err != nil {
		return err
	}
	created, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if created != 1 {
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tombot.computer_nodes SET status=CASE WHEN status='draining' THEN status ELSE 'bootstrapping' END,verified_image_id=NULL,capability_expires_at=NULL WHERE id=$1`, service.nodeID); err != nil {
		return err
	}
	// 保留 specification 和 fencing_token；旧的隔离资源也必须按原身份调用 Remove，不能直接忘记。
	if _, err = tx.ExecContext(ctx, `UPDATE tombot.node_verifications SET is_valid=false,state=CASE WHEN state IN ('succeeded','failed') THEN state ELSE 'cleaning' END,capabilities=NULL,verified_at=NULL,attempts=0,lease_expires_at=NULL,next_attempt_at=clock_timestamp(),updated_at=clock_timestamp() WHERE node_id=$1`, service.nodeID); err != nil {
		return err
	}
	return audit(ctx, tx, ids.New(), "升级节点电脑镜像", "computer_node", service.nodeID, map[string]any{"previous_image_id": registration.PreviousImageID, "image_id": registration.ImageID})
}

// RequireNoVerificationLeaseInTransaction 在节点锁内检查验收租约，调用方须在同一事务撤销分配资格。
func (service *Service) RequireNoVerificationLeaseInTransaction(ctx context.Context, tx *sql.Tx) error {
	var nodeID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM tombot.computer_nodes WHERE id=$1 FOR UPDATE`, service.nodeID).Scan(&nodeID); err != nil {
		return err
	}
	var leased bool
	err := tx.QueryRowContext(ctx, `SELECT coalesce(lease_expires_at>clock_timestamp(),false) FROM tombot.node_verifications WHERE node_id=$1 FOR UPDATE`, service.nodeID).Scan(&leased)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if leased {
		return ErrConflict
	}
	return nil
}
