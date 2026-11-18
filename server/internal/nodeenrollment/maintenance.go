package nodeenrollment

import (
	"context"
	"database/sql"
	"errors"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/nodecatalog"
)

var ErrMaintenanceConflict = errors.New("节点维护状态冲突，请检查在途验收或升级计划")

// MaintenanceRequest 随登记内容一起签名，旧接入请求不能被换作停机授权。
type MaintenanceRequest struct {
	OperationID string `json:"operation_id"`
	Finish      bool   `json:"finish"`
}

// reserveUpgrade 仅允许原证书持有者操作本节点。失联保留维护窗口，避免断电后恢复时与新分配竞争。
func reserveUpgrade(ctx context.Context, tx *sql.Tx, catalog *nodecatalog.Service, claim Claim, currentImage, requestID string) (string, error) {
	m := claim.Maintenance
	if (computer.NodeQuery{NodeID: m.OperationID}).Validate() != nil {
		return "", nodecatalog.ErrInvalidInput
	}
	var prior, target string
	var finished bool
	err := tx.QueryRowContext(ctx, `SELECT operation_id,target_image_id,is_finished FROM tombot.node_upgrades WHERE node_id=$1`, claim.NodeID).Scan(&prior, &target, &finished)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	image := claim.Registration.ImageID
	if m.Finish {
		if prior != m.OperationID || target != image || currentImage != image {
			return "", ErrConflict
		}
		if finished {
			return "upgrade_finished", nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE tombot.node_upgrades SET is_finished=true WHERE node_id=$1`, claim.NodeID)
		if err != nil {
			return "", err
		}
	} else {
		if prior == m.OperationID && target == image && !finished {
			return "upgrade_held", nil
		}
		if prior != "" && (!finished || prior >= m.OperationID) || currentImage != image && currentImage != claim.Registration.PreviousImageID {
			return "", ErrConflict
		}
		// 登记请求由同一事务锁串行化。行锁与分配、验收共用；撤销资格后，维护结束前不再颁发新证据。
		if err := catalog.RequireNoVerificationLeaseInTransaction(ctx, tx); err != nil {
			return "", err
		}
		if claim.SupportsComputerUpgrades {
			// 中央迁移与维护共用节点行锁，旧计划可收尾，下一次维护不能打断原电脑的镜像切换。
			var pending bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.computer_image_upgrades
				WHERE node_id=$1 AND state IN ('draining','switching','quarantined'))`, claim.NodeID).Scan(&pending); err != nil {
				return "", err
			}
			if pending {
				return "", ErrConflict
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.node_upgrades(node_id,operation_id,target_image_id) VALUES($1,$2,$3)
			ON CONFLICT(node_id) DO UPDATE SET operation_id=$2,target_image_id=$3,is_finished=false`, claim.NodeID, m.OperationID, image); err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE tombot.computer_nodes SET status=CASE WHEN status='draining' THEN status ELSE 'bootstrapping' END,verified_image_id=NULL,capability_expires_at=NULL WHERE id=$1`, claim.NodeID); err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE tombot.node_verifications SET is_valid=false WHERE node_id=$1`, claim.NodeID); err != nil {
			return "", err
		}
	}
	state, action := "upgrade_held", "开始节点维护"
	if m.Finish {
		state, action = "upgrade_finished", "结束节点维护"
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tombot.admin_audit_events(id,action,target_kind,target_id,request_id,result,changes) VALUES($1,$2,'computer_node',$3,$1,'succeeded','{}')`, requestID, action, claim.NodeID)
	return state, err
}
