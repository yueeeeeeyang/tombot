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
	HostRelease    string          `json:"host_release,omitempty"`
	Stage          string          `json:"stage,omitempty"`
	InspectRelease string          `json:"inspect_release,omitempty"`
	Recovery       *RecoveryTarget `json:"recovery,omitempty"`
	OperationID    string          `json:"operation_id"`
	Finish         bool            `json:"finish"`
}

// reserveUpgrade 仅允许原证书持有者操作本节点。失联保留维护窗口，避免断电后恢复时与新分配竞争。
func reserveUpgrade(ctx context.Context, tx *sql.Tx, catalog *nodecatalog.Service, claim Claim, currentImage, requestID string) (string, error) {
	m := claim.Maintenance
	if (computer.NodeQuery{NodeID: m.OperationID}).Validate() != nil {
		return "", nodecatalog.ErrInvalidInput
	}
	image := claim.Registration.ImageID
	if m.HostRelease != "" && (!validRelease(m.HostRelease) || m.Recovery != nil || !claim.SupportsComputerUpgrades || image != currentImage) {
		return "", ErrConflict
	}
	var prior, target, priorRelease, priorStage string
	var finished bool
	err := tx.QueryRowContext(ctx, `SELECT operation_id,target_image_id,is_finished,coalesce(host_release,''),coalesce(stage,'') FROM tombot.node_upgrades WHERE node_id=$1`, claim.NodeID).Scan(&prior, &target, &finished, &priorRelease, &priorStage)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if prior == m.OperationID && priorRelease != m.HostRelease {
		return "", ErrConflict
	}
	if m.Stage != "" {
		if m.Finish || m.Recovery != nil || prior != m.OperationID || target != image {
			return "", ErrConflict
		}
		if m.Stage == "rolled_back" && m.HostRelease != "" && (!finished || priorStage == "rolled_back") {
			_, err = tx.ExecContext(ctx, `UPDATE tombot.node_upgrades SET stage='rolled_back',is_finished=true WHERE node_id=$1`, claim.NodeID)
			return "upgrade_progress", err
		}
		if finished {
			return "", ErrConflict
		}
		if m.Stage == "rollback" && m.HostRelease != "" {
			_, err = tx.ExecContext(ctx, `UPDATE tombot.node_upgrades SET stage='rollback' WHERE node_id=$1`, claim.NodeID)
			return "upgrade_progress", err
		}
		if priorStage == "rollback" {
			return "", ErrConflict
		}
		switch m.Stage {
		case "stop", "activate", "start", "finish":
			_, err = tx.ExecContext(ctx, `UPDATE tombot.node_upgrades SET stage=$2 WHERE node_id=$1 AND
				array_position(ARRAY['maintenance','stop','activate','start','finish'],coalesce(stage,'maintenance'))<=array_position(ARRAY['maintenance','stop','activate','start','finish'],$2)`, claim.NodeID, m.Stage)
			return "upgrade_progress", err
		default:
			return "", nodecatalog.ErrInvalidInput
		}
	}
	if m.Finish {
		if priorStage == "rollback" || priorStage == "rolled_back" || prior != m.OperationID || target != image || currentImage != image {
			return "", ErrConflict
		}
		if finished {
			return "upgrade_finished", nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE tombot.node_upgrades SET is_finished=true,stage='finish' WHERE node_id=$1`, claim.NodeID)
		if err != nil {
			return "", err
		}
		if m.HostRelease != "" {
			// 只唤醒已知退役故障的原资源；保留原规格和围栏，每个发布最多重核一次。
			_, err = tx.ExecContext(ctx, `WITH retry AS (
				INSERT INTO tombot.node_verification_repairs(node_id,fencing_token,release)
				SELECT node_id,fencing_token,$2 FROM tombot.node_verifications WHERE node_id=$1 AND state='quarantined'
				  AND node_error_code='agent_retirement_entry_unknown' AND lease_expires_at IS NULL
				ON CONFLICT DO NOTHING RETURNING node_id,fencing_token)
				UPDATE tombot.node_verifications v SET state='cleaning',is_valid=false,attempts=0,
					next_attempt_at=clock_timestamp(),updated_at=clock_timestamp()
				FROM retry WHERE (v.node_id,v.fencing_token)=(retry.node_id,retry.fencing_token)`, claim.NodeID, m.HostRelease)
			if err != nil {
				return "", err
			}
		}
	} else {
		if prior == m.OperationID && target == image && !finished {
			return "upgrade_held", nil
		}
		if prior != "" && (!finished || prior >= m.OperationID) || currentImage != image && currentImage != claim.Registration.PreviousImageID {
			return "", ErrConflict
		}
		// 用户编辑对两端使用同一把共享锁，维护必须在检查和撤销资格之前独占。
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('tombot.node-maintenance/'||$1,0))`, claim.NodeID); err != nil {
			return "", err
		}
		var editing bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.computer_edits WHERE stage<>'succeeded' AND (source_spec->>'node_id'=$1 OR target_spec->>'node_id'=$1) AND ($2='' OR lease_until IS NOT NULL OR next_attempt_at<>'infinity'::timestamptz))`, claim.NodeID, m.HostRelease).Scan(&editing); err != nil {
			return "", err
		}
		if editing {
			return "", maintenanceBlocker("computer_upgrade")
		}
		// 登记请求由同一事务锁串行化。行锁与分配、验收共用；撤销资格后，维护结束前不再颁发新证据。
		if err := catalog.RequireNoVerificationLeaseInTransaction(ctx, tx); err != nil {
			if errors.Is(err, nodecatalog.ErrConflict) {
				return "", maintenanceBlocker("verification_lease")
			}
			return "", err
		}
		if m.HostRelease != "" {
			if err := requireIdleHost(ctx, tx, claim.NodeID); err != nil {
				return "", err
			}
		} else if m.Recovery != nil {
			if !claim.SupportsComputerUpgrades || image != currentImage || m.Recovery.validate() != nil {
				return "", ErrConflict
			}
			if err := requeueRecovery(ctx, tx, claim.NodeID, image, *m.Recovery, requestID); err != nil {
				return "", err
			}
		} else if claim.SupportsComputerUpgrades {
			// 中央迁移与维护共用节点行锁，旧计划可收尾，下一次维护不能打断原电脑的镜像切换。
			var pending bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.computer_image_upgrades
				WHERE node_id=$1 AND state IN ('draining','switching','quarantined'))`, claim.NodeID).Scan(&pending); err != nil {
				return "", err
			}
			if pending {
				return "", maintenanceBlocker("computer_upgrade")
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.node_upgrades(node_id,operation_id,target_image_id,host_release,stage) VALUES($1,$2,$3,nullif($4,''),'maintenance')
			ON CONFLICT(node_id) DO UPDATE SET operation_id=$2,target_image_id=$3,is_finished=false,host_release=EXCLUDED.host_release,stage=EXCLUDED.stage`, claim.NodeID, m.OperationID, image, m.HostRelease); err != nil {
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

type maintenanceBlocker string

func (b maintenanceBlocker) UpgradeStep() string { return string(b) }
func (b maintenanceBlocker) Error() string       { return "等待节点维护授权：" + string(b) }
func (b maintenanceBlocker) Is(err error) bool {
	return err == ErrConflict || b == "verification_lease" && err == nodecatalog.ErrConflict
}

// requireIdleHost 在节点及会话准入锁内检查已领取的动作，结果未知时保留原运行状态。
func requireIdleHost(ctx context.Context, tx *sql.Tx, node string) error {

	var reason string
	err := tx.QueryRowContext(ctx, `SELECT CASE
		WHEN EXISTS(SELECT 1 FROM tombot.computer_sessions WHERE node_id=$1 AND status IN ('starting','active','draining','quarantined')) THEN 'active_session'
		WHEN EXISTS(SELECT 1 FROM tombot.node_verifications WHERE node_id=$1 AND lease_expires_at IS NOT NULL) THEN 'verification_lease'
		WHEN EXISTS(SELECT 1 FROM tombot.computer_image_upgrades WHERE node_id=$1 AND (lease_expires_at IS NOT NULL OR state IN ('switching','quarantined') OR state='draining' AND attempt_count>0)) THEN 'computer_upgrade'
		WHEN EXISTS(SELECT 1 FROM tombot.computers c WHERE c.node_id=$1 AND (c.status IN ('pending','starting','provisioning','quarantined')
			OR c.status='stopping' AND NOT EXISTS(SELECT 1 FROM tombot.computer_image_upgrades p WHERE (p.user_id,p.computer_id,p.node_id)=(c.user_id,c.id,c.node_id)
			AND p.state='draining' AND p.lease_expires_at IS NULL) AND NOT EXISTS (SELECT 1 FROM tombot.computer_edits e WHERE e.user_id=c.user_id AND e.stage<>'succeeded' AND e.lease_until IS NULL AND e.next_attempt_at='infinity'::timestamptz))) THEN 'computer_transition'
		ELSE '' END`, node).Scan(&reason)
	if err != nil {
		return err
	}
	if reason != "" {
		return maintenanceBlocker(reason)
	}
	return nil
}
