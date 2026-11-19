package nodeenrollment

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

// RecoveryTarget 由节点核实本机原实例后签名，只对该发布与该计划的当前控制版本有效。
type RecoveryTarget struct {
	ComputerID  string `json:"computer_id"`
	ContainerID string `json:"source_container_id"`
	Fence       int64  `json:"target_fence"`
	Release     string `json:"release"`
}

func validRelease(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}

func (target RecoveryTarget) validate() error {
	if (computer.NodeQuery{NodeID: target.ComputerID}).Validate() != nil || !validRelease(target.ContainerID) || !validRelease(target.Release) || target.Fence <= 0 {
		return ErrConflict
	}
	return nil
}

// RecoveryInspection 包含一项待核验的旧实例；控制服务每次最多返回一台电脑。
type RecoveryInspection struct {
	State     string                  `json:"state"`
	Source    *computer.Specification `json:"source,omitempty"`
	Target    RecoveryTarget          `json:"target,omitzero"`
	ErrorCode string                  `json:"error_code,omitempty"`
}

func inspectRecovery(ctx context.Context, tx *sql.Tx, nodeID, imageID, release string) (*RecoveryInspection, error) {
	result := new(RecoveryInspection)
	var source []byte
	err := tx.QueryRowContext(ctx, `SELECT CASE
 WHEN p.target_image_id<>$2 OR c.status<>'stopping' OR (c.node_id,c.image_id,c.generation,c.container_id)
      IS DISTINCT FROM (p.node_id,p.source_image_id,p.source_generation,p.source_container_id) THEN 'blocked'
 WHEN p.state IN ('draining','switching') THEN 'waiting'
 WHEN p.attempt_count=0 OR p.lease_expires_at IS NOT NULL OR p.recovery_release=$3 OR
      (p.last_failure_code IS NOT NULL AND p.last_failure_code<>'computer_image_upgrade_source_mismatch') THEN 'blocked'
 WHEN EXISTS (SELECT 1 FROM tombot.node_verifications v WHERE v.node_id=p.node_id AND v.lease_expires_at>clock_timestamp()) THEN 'waiting'
 ELSE 'retry' END,p.source_specification,p.computer_id,p.source_container_id,
 coalesce((p.target_specification->>'fencing_token')::bigint,0),coalesce(p.last_failure_code,'')
 FROM tombot.computer_image_upgrades p JOIN tombot.computers c ON (c.user_id,c.id)=(p.user_id,p.computer_id)
 WHERE p.node_id=$1 AND p.state IN ('draining','switching','quarantined')
 ORDER BY (p.state='quarantined'),p.computer_id LIMIT 1`, nodeID, imageID, release).Scan(&result.State, &source, &result.Target.ComputerID, &result.Target.ContainerID, &result.Target.Fence, &result.ErrorCode)
	if errors.Is(err, sql.ErrNoRows) {
		// 刚登记新镜像时，工作进程可能还没为原电脑建立迁移计划。
		err = tx.QueryRowContext(ctx, `SELECT id FROM tombot.computers WHERE node_id=$1 AND image_id<>$2 AND status='ready' LIMIT 1`, nodeID, imageID).Scan(&result.Target.ComputerID)
		if errors.Is(err, sql.ErrNoRows) {
			return &RecoveryInspection{State: "clear"}, nil
		}
		if err != nil {
			return nil, err
		}
		result.State = "waiting"
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	result.Target.Release = release
	if result.State == "retry" {
		var spec computer.Specification
		if json.Unmarshal(source, &spec) != nil || spec.Validate() != nil || spec.NodeID != nodeID || spec.ComputerID != result.Target.ComputerID || result.Target.validate() != nil {
			return nil, ErrConflict
		}
		result.Source = &spec
	}
	return result, nil
}

// requeueRecovery 与维护窗口一起提交；切换工作只有在窗口交还、节点重新验收后才能领取。
func requeueRecovery(ctx context.Context, tx *sql.Tx, nodeID, imageID string, target RecoveryTarget, requestID string) error {
	var userID string
	err := tx.QueryRowContext(ctx, `SELECT h.user_id FROM tombot.computer_execution_heads h JOIN tombot.computer_image_upgrades p ON p.user_id=h.user_id
 WHERE p.computer_id=$1 AND p.node_id=$2 FOR UPDATE OF h`, target.ComputerID, nodeID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	var accepted bool
	if err := tx.QueryRowContext(ctx, `SELECT tombot.retry_node_upgrade_computer($1,$2,$3,$4,$5,$6)`, nodeID, imageID, target.ComputerID, target.ContainerID, target.Fence, target.Release).Scan(&accepted); err != nil {
		return err
	}
	if !accepted {
		return ErrConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tombot.admin_audit_events(id,action,target_kind,target_id,request_id,result,changes)
 VALUES($1,'标准节点升级恢复隔离电脑','computer',$2,$3,'succeeded',jsonb_build_object('node_id',$4::text,'target_fence',$5::bigint))`, ids.New(), target.ComputerID, requestID, nodeID, target.Fence)
	return err
}
