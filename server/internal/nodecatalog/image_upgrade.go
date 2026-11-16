package nodecatalog

import (
	"context"
	"database/sql"
	"errors"
	"io.yueyang/tom/internal/infra/ids"
)

// UpgradeImageInTransaction 只供已验证原节点证书的登记事务调用；不修改旧镜像或已有电脑。
// 空闲节点撤销旧能力证据，并先清理旧验收资源，随后才能验证新镜像。
func (service *Service) UpgradeImageInTransaction(ctx context.Context, tx *sql.Tx, registration Registration) error {
	if tx == nil || registration.Validate() != nil || registration.PreviousImageID == "" {
		return ErrInvalidInput
	}
	var reserved int
	if err := tx.QueryRowContext(ctx, `SELECT reserved FROM tombot.computer_nodes WHERE id=$1 FOR UPDATE`, service.nodeID).Scan(&reserved); err != nil {
		return err
	}
	if reserved != 0 {
		return ErrConflict
	}
	var leased bool
	err := tx.QueryRowContext(ctx, `SELECT coalesce(lease_expires_at>clock_timestamp(),false) FROM tombot.node_verifications WHERE node_id=$1 FOR UPDATE`, service.nodeID).Scan(&leased)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if leased {
		return ErrConflict
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
