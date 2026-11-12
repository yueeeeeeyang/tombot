package nodecatalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

type verificationNode struct {
	runtimeID  sql.NullString
	generation int64
	status     string
	expires    sql.NullTime
	now        time.Time
}

// lockVerificationNode 始终先锁节点再锁验收行，与心跳失效触发器采用相同顺序。
func lockVerificationNode(ctx context.Context, tx *sql.Tx, nodeID string) (verificationNode, error) {
	var node verificationNode
	err := tx.QueryRowContext(ctx, `SELECT runtime_id,generation,status,control_expires_at FROM tombot.computer_nodes WHERE id=$1 FOR UPDATE`, nodeID).
		Scan(&node.runtimeID, &node.generation, &node.status, &node.expires)
	if errors.Is(err, sql.ErrNoRows) {
		return node, ErrNotFound
	}
	if err == nil {
		// 取得锁之后才读取时钟，不能使用等待锁之前的时间继续授予租约。
		err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&node.now)
	}
	return node, err
}

func (node verificationNode) online() bool {
	return node.runtimeID.Valid && node.expires.Valid && node.expires.Time.After(node.now)
}

func readVerification(ctx context.Context, tx *sql.Tx, nodeID string) (*verification, sql.NullTime, time.Time, int64, error) {
	current := &verification{}
	var specification, capabilities []byte
	var verifiedAt, lease sql.NullTime
	var code sql.NullString
	var next time.Time
	var token int64
	err := tx.QueryRowContext(ctx, `SELECT image_id,specification,runtime_id,runtime_generation,is_valid,state,fencing_token,lease_expires_at,attempts,capabilities,verified_at,error_code,next_attempt_at
		FROM tombot.node_verifications WHERE node_id=$1 FOR UPDATE`, nodeID).
		Scan(&current.imageID, &specification, &current.runtimeID, &current.runtimeGeneration, &current.isValid, &current.state, &token, &lease, &current.attempts, &capabilities, &verifiedAt, &code, &next)
	if err != nil {
		return nil, lease, next, token, err
	}
	if json.Unmarshal(specification, &current.specification) != nil || current.specification.Validate() != nil || current.specification.NodeID != nodeID || current.specification.FencingToken != token {
		return nil, lease, next, token, ErrConflict
	}
	var reference string
	var isBusinessResource bool
	err = tx.QueryRowContext(ctx, `SELECT reference,EXISTS(SELECT 1 FROM tombot.users WHERE id=$2) OR EXISTS(SELECT 1 FROM tombot.computers WHERE id=$3)
		FROM tombot.computer_images WHERE id=$1`, current.imageID, current.specification.UserID, current.specification.ComputerID).Scan(&reference, &isBusinessResource)
	if err != nil {
		return nil, lease, next, token, err
	}
	if isBusinessResource || reference != current.specification.ImageReference {
		return nil, lease, next, token, ErrConflict
	}
	if verifiedAt.Valid {
		current.verifiedAt = &verifiedAt.Time
		current.capabilities = new(computer.Capabilities)
		if json.Unmarshal(capabilities, current.capabilities) != nil || !current.capabilities.Ready() {
			return nil, lease, next, token, ErrConflict
		}
	}
	current.errorCode = code.String
	return current, lease, next, token, nil
}

// claim 在旧资源确认清理前一直沿用其身份；仅终态允许开始下一代验收。
func (verifier *Verifier) claim(parent context.Context) (*verification, error) {
	ctx, cancel := context.WithTimeout(parent, verifier.options.CallTimeout)
	defer cancel()
	var claimed *verification
	err := verifier.service.transaction(ctx, func(tx *sql.Tx) error {
		node, err := lockVerificationNode(ctx, tx, verifier.service.nodeID)
		if err != nil || !node.online() {
			return err
		}
		current, lease, next, token, err := readVerification(ctx, tx, verifier.service.nodeID)
		missing := errors.Is(err, sql.ErrNoRows)
		if err != nil && !missing {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&node.now); err != nil {
			return err
		}
		if !node.online() {
			return nil
		}
		if !missing && (current.state == "quarantined" || lease.Valid && lease.Time.After(node.now)) {
			return nil
		}
		fresh := missing || current.state == "failed" || current.state == "succeeded"
		if fresh {
			if node.status == "draining" || !missing && next.After(node.now) && current.imageID == verifier.options.ImageID && current.specification.Resources == verifier.options.Resources && current.isValid {
				return nil
			}
			var reference string
			if err := tx.QueryRowContext(ctx, `SELECT reference FROM tombot.computer_images WHERE id=$1 AND is_enabled FOR SHARE`, verifier.options.ImageID).Scan(&reference); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrNotFound
				}
				return err
			}
			if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&node.now); err != nil {
				return err
			}
			if !node.online() {
				return nil
			}
			specification := computer.Specification{OperationID: ids.New(), UserID: ids.New(), ComputerID: ids.New(), NodeID: verifier.service.nodeID, ImageReference: reference, Generation: 1, Attempt: 1, Resources: verifier.options.Resources}
			if !missing {
				if current.specification.Generation == math.MaxInt64 || current.specification.Attempt >= math.MaxInt32 {
					return ErrConflict
				}
				specification.UserID, specification.ComputerID = current.specification.UserID, current.specification.ComputerID
				// 正式节点将操作 ID 绑定到逻辑电脑，清理后只允许代次、尝试号和控制令牌一起前进。
				specification.OperationID = current.specification.OperationID
				specification.Generation = current.specification.Generation + 1
				specification.Attempt = current.specification.Attempt + 1
			}
			specification.VolumeReference = "tombot-user-" + specification.UserID
			current = &verification{specification: specification, imageID: verifier.options.ImageID, runtimeID: node.runtimeID.String, runtimeGeneration: node.generation, isValid: true, state: "checking"}
			if _, err := tx.ExecContext(ctx, `UPDATE tombot.computer_nodes SET status='bootstrapping',verified_image_id=NULL,capability_expires_at=NULL WHERE id=$1`, verifier.service.nodeID); err != nil {
				return err
			}
		} else if next.After(node.now) {
			return nil
		}
		if token == math.MaxInt64 {
			return ErrConflict
		}
		current.isValid = current.isValid && node.runtimeID.String == current.runtimeID && node.generation == current.runtimeGeneration && node.status != "draining"
		current.specification.FencingToken = token + 1
		current.specification.LeaseExpiresAt = node.now.Add(verifier.options.LeaseDuration)
		current.started = node.now
		current.attempts++
		if err := current.specification.Validate(); err != nil {
			return ErrConflict
		}
		encoded, err := json.Marshal(current.specification)
		if err != nil {
			return err
		}
		if fresh {
			_, err = tx.ExecContext(ctx, `INSERT INTO tombot.node_verifications(node_id,image_id,specification,runtime_id,runtime_generation,is_valid,state,fencing_token,lease_expires_at,attempts)
				VALUES($1,$2,$3,$4,$5,true,'checking',$6,$7,$8) ON CONFLICT(node_id) DO UPDATE SET
				image_id=EXCLUDED.image_id,specification=EXCLUDED.specification,runtime_id=EXCLUDED.runtime_id,runtime_generation=EXCLUDED.runtime_generation,is_valid=true,state='checking',
				fencing_token=EXCLUDED.fencing_token,lease_expires_at=EXCLUDED.lease_expires_at,attempts=EXCLUDED.attempts,capabilities=NULL,verified_at=NULL,error_code=NULL,next_attempt_at=clock_timestamp(),created_at=clock_timestamp(),updated_at=clock_timestamp()`,
				verifier.service.nodeID, current.imageID, encoded, current.runtimeID, current.runtimeGeneration, current.specification.FencingToken, current.specification.LeaseExpiresAt, current.attempts)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE tombot.node_verifications SET specification=$2,fencing_token=$3,lease_expires_at=$4,attempts=$5,is_valid=$6,updated_at=clock_timestamp() WHERE node_id=$1`,
				verifier.service.nodeID, encoded, current.specification.FencingToken, current.specification.LeaseExpiresAt, current.attempts, current.isValid)
		}
		if err == nil {
			claimed = current
		}
		return err
	})
	return claimed, err
}

// lockClaim 拒绝已经过期或被其他工作者接管的回包；未知提交不能覆盖新控制者。
func (verifier *Verifier) lockClaim(ctx context.Context, tx *sql.Tx, claimed *verification) (verificationNode, *verification, error) {
	node, err := lockVerificationNode(ctx, tx, verifier.service.nodeID)
	if err != nil {
		return node, nil, err
	}
	current, lease, _, token, err := readVerification(ctx, tx, verifier.service.nodeID)
	if err != nil {
		return node, nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&node.now); err != nil {
		return node, nil, err
	}
	if token != claimed.specification.FencingToken || current.specification.OperationID != claimed.specification.OperationID || !lease.Valid || !lease.Time.After(node.now) || current.state != claimed.state {
		return node, current, ErrVerificationLease
	}
	return node, current, nil
}

// advance 原子记录证据或失败原因后释放本轮租约；能力通过也必须先进入清理阶段。
func (verifier *Verifier) advance(parent context.Context, claimed *verification, state, code string) error {
	ctx, cancel := context.WithTimeout(parent, verifier.options.CallTimeout)
	defer cancel()
	err := verifier.service.transaction(ctx, func(tx *sql.Tx) error {
		node, current, err := verifier.lockClaim(ctx, tx, claimed)
		if err != nil {
			return err
		}
		var capabilities []byte
		if claimed.capabilities != nil {
			capabilities, err = json.Marshal(claimed.capabilities)
			if err != nil {
				return err
			}
		}
		attempts := current.attempts
		delay := verifier.options.RetryDelay
		if current.state != state {
			attempts, delay = 0, 0
		}
		result, err := tx.ExecContext(ctx, `UPDATE tombot.node_verifications SET state=$2,error_code=NULLIF($3,''),capabilities=$4,verified_at=$5,lease_expires_at=NULL,attempts=$6,next_attempt_at=$7,updated_at=clock_timestamp() WHERE node_id=$1 AND lease_expires_at>clock_timestamp()`,
			verifier.service.nodeID, state, code, capabilities, claimed.verifiedAt, attempts, node.now.Add(delay))
		return verificationUpdated(result, err)
	})
	if err == nil && state == "quarantined" {
		verifier.logger.ErrorContext(parent, "节点验收资源状态无法确认，保留隔离记录并禁止新建替代电脑", "node_id", verifier.service.nodeID, "operation_id", claimed.specification.OperationID, "reason", code)
	}
	return err
}

// finish 再核对实际控制进程与持久账本；旧心跳、过期证据、排空及镜像停用均不恢复就绪。
func (verifier *Verifier) finish(parent context.Context, claimed *verification) error {
	ctx, cancel := context.WithTimeout(parent, verifier.options.CallTimeout)
	heartbeat, heartbeatErr := verifier.node.Heartbeat(ctx, computer.NodeQuery{NodeID: verifier.service.nodeID})
	cancel()
	ctx, cancel = context.WithTimeout(parent, verifier.options.CallTimeout)
	defer cancel()
	var published bool
	err := verifier.service.transaction(ctx, func(tx *sql.Tx) error {
		node, current, err := verifier.lockClaim(ctx, tx, claimed)
		if err != nil {
			return err
		}
		confirmed := heartbeatErr == nil && heartbeat.Validate(computer.NodeQuery{NodeID: verifier.service.nodeID}) == nil && heartbeat.InstanceID == current.runtimeID && heartbeat.Generation == current.runtimeGeneration
		ready := confirmed && current.isValid && current.imageID == verifier.options.ImageID && current.specification.Resources == verifier.options.Resources && node.online() && node.status != "draining" && node.runtimeID.String == current.runtimeID && node.generation == current.runtimeGeneration && current.verifiedAt != nil && !current.verifiedAt.After(node.now) && current.capabilities != nil && current.capabilities.Ready() && current.verifiedAt.Add(verifier.options.Lifetime).After(node.now)
		if ready {
			var enabled bool
			if err := tx.QueryRowContext(ctx, `SELECT is_enabled FROM tombot.computer_images WHERE id=$1 FOR SHARE`, current.imageID).Scan(&enabled); err != nil {
				return err
			}
			ready = enabled
		}
		state, code := "failed", current.errorCode
		if code == "" {
			code = "verification_invalidated"
		}
		if ready {
			state, code = "succeeded", ""
			result, err := tx.ExecContext(ctx, `UPDATE tombot.computer_nodes SET status='ready',verified_image_id=$2,capability_expires_at=$3 WHERE id=$1 AND control_expires_at>clock_timestamp() AND $3::timestamptz>clock_timestamp()`, verifier.service.nodeID, current.imageID, current.verifiedAt.Add(verifier.options.Lifetime))
			if err != nil {
				return err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if count != 1 {
				return ErrVerificationLease
			}
		}
		result, err := tx.ExecContext(ctx, `UPDATE tombot.node_verifications SET state=$2,error_code=NULLIF($3,''),lease_expires_at=NULL,next_attempt_at=$4,updated_at=clock_timestamp() WHERE node_id=$1 AND lease_expires_at>clock_timestamp()`, verifier.service.nodeID, state, code, node.now.Add(verifier.options.Interval))
		if err = verificationUpdated(result, err); err != nil {
			return err
		}
		published = ready
		return audit(ctx, tx, ids.New(), "完成节点电脑验收", "computer_node", verifier.service.nodeID, map[string]any{"state": state, "image_id": current.imageID, "operation_id": current.specification.OperationID, "error_code": code})
	})
	if err == nil {
		if published {
			verifier.logger.InfoContext(parent, "节点电脑能力已验收且临时资源已清理，开放固定镜像准入", "node_id", verifier.service.nodeID, "image_id", claimed.imageID)
		} else {
			verifier.logger.WarnContext(parent, "节点电脑验收未通过，临时资源已清理且保持禁止开户", "node_id", verifier.service.nodeID, "image_id", claimed.imageID)
		}
	}
	return err
}

func verificationUpdated(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrVerificationLease
	}
	return nil
}
