package nodecatalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store/postgres"
)

// Administration 只管理目录配置和查询运行证据，不接受客户端声明的就绪状态。
type Administration struct {
	control    *postgres.Database
	authorizer Authorizer
}

func NewAdministration(control *postgres.Database, authorizer Authorizer) (*Administration, error) {
	if control == nil || control.Role() != postgres.ControlRole || authorizer == nil {
		return nil, errors.New("节点管理依赖无效")
	}
	return &Administration{control, authorizer}, nil
}

type NodeVerification struct {
	State          string                 `json:"state"`
	IsValid        bool                   `json:"is_valid"`
	Attempts       int                    `json:"attempts"`
	FencingToken   int64                  `json:"fencing_token"`
	ErrorCode      *string                `json:"error_code"`
	NodeErrorCode  *string                `json:"node_error_code"`
	UpdatedAt      time.Time              `json:"updated_at"`
	NextAttemptAt  time.Time              `json:"next_attempt_at"`
	VerifiedAt     *time.Time             `json:"verified_at"`
	LeaseExpiresAt *time.Time             `json:"lease_expires_at"`
	Capabilities   *computer.Capabilities `json:"capabilities"`
	Resources      computer.Resources     `json:"resources"`
}

type NodeDiagnosis struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// NodeDetail 仅返回管理员运维所需的目录、公开连接地址与能力证据，不包含接入凭据或私钥。
type NodeDetail struct {
	UpgradeStep string `json:"upgrade_step,omitempty"`
	Node
	ConfigurationVersion   int64             `json:"configuration_version"`
	IsDraining             bool              `json:"is_draining"`
	CreatedAt              time.Time         `json:"created_at"`
	URL                    *string           `json:"url"`
	CertificateFingerprint *string           `json:"certificate_fingerprint"`
	RuntimeID              *string           `json:"runtime_id"`
	ControlExpiresAt       *time.Time        `json:"control_expires_at"`
	ControlSequence        int64             `json:"control_sequence"`
	Image                  *Image            `json:"image"`
	Verification           *NodeVerification `json:"verification"`
	Diagnosis              NodeDiagnosis     `json:"diagnosis"`
	CanRetryVerification   bool              `json:"can_retry_verification"`
}

type NodeUpdate struct {
	Name                 string `json:"name"`
	Capacity             int    `json:"capacity"`
	IsDraining           bool   `json:"is_draining"`
	ConfigurationVersion int64  `json:"configuration_version"`
}

// GetNode 在一次语句快照内读取节点与验收，查询失败不能解释为未运行或无资源。
func (service *Administration) GetNode(ctx context.Context, actor identity.Principal, nodeID string) (NodeDetail, error) {
	if err := service.authorizer.RequireAdmin(ctx, actor, false); err != nil {
		return NodeDetail{}, err
	}
	if !validID(nodeID) {
		return NodeDetail{}, ErrInvalidInput
	}
	var detail NodeDetail
	catalog := &Service{control: service.control, nodeID: nodeID}
	err := catalog.transaction(ctx, func(tx *sql.Tx) error {
		var err error
		detail, err = readNodeDetail(ctx, tx, nodeID)
		return err
	})
	return detail, err
}

func readNodeDetail(ctx context.Context, tx *sql.Tx, nodeID string) (NodeDetail, error) {
	var detail NodeDetail
	var imageID, reference *string
	var imageEnabled *bool
	var verification, capabilities, specification []byte
	var now time.Time
	err := tx.QueryRowContext(ctx, `SELECT n.id,n.name,tombot.computer_node_status(n.status,n.runtime_id,n.control_expires_at,n.capability_expires_at),
		n.capacity,n.reserved,n.generation,n.last_seen_at,n.verified_image_id,n.capability_expires_at,n.configuration_version,n.status='draining',n.created_at,
		c.url,c.certificate_fingerprint,n.runtime_id,n.control_expires_at,n.control_sequence,i.id,i.reference,i.is_enabled,
		CASE WHEN v.node_id IS NOT NULL THEN jsonb_build_object('state',v.state,'is_valid',v.is_valid,'attempts',v.attempts,'fencing_token',v.fencing_token,
		'error_code',v.error_code,'node_error_code',v.node_error_code,'updated_at',v.updated_at,'next_attempt_at',v.next_attempt_at,'verified_at',v.verified_at,'lease_expires_at',v.lease_expires_at) END,
		v.capabilities,v.specification,clock_timestamp(),
		CASE WHEN EXISTS(SELECT 1 FROM tombot.node_upgrades WHERE node_id=n.id AND NOT is_finished)
			OR EXISTS(SELECT 1 FROM tombot.computer_image_upgrades WHERE node_id=n.id AND state IN ('draining','switching','quarantined'))
			THEN coalesce(tombot.node_upgrade_diagnosis(n.id),'') ELSE '' END
		FROM tombot.computer_nodes n LEFT JOIN tombot.node_connections c ON c.node_id=n.id
		LEFT JOIN tombot.computer_images i ON i.id=c.image_id LEFT JOIN tombot.node_verifications v ON v.node_id=n.id WHERE n.id=$1`, nodeID).
		Scan(&detail.ID, &detail.Name, &detail.Status, &detail.Capacity, &detail.Reserved, &detail.Generation, &detail.LastSeenAt, &detail.VerifiedImageID, &detail.CapabilityExpiresAt,
			&detail.ConfigurationVersion, &detail.IsDraining, &detail.CreatedAt, &detail.URL, &detail.CertificateFingerprint, &detail.RuntimeID, &detail.ControlExpiresAt, &detail.ControlSequence,
			&imageID, &reference, &imageEnabled, &verification, &capabilities, &specification, &now, &detail.UpgradeStep)
	if errors.Is(err, sql.ErrNoRows) {
		return detail, ErrNotFound
	}
	if err != nil {
		return detail, err
	}
	if imageID != nil && reference != nil && imageEnabled != nil {
		detail.Image = &Image{ID: *imageID, Reference: *reference, IsEnabled: *imageEnabled}
	}
	if len(verification) > 0 {
		detail.Verification = new(NodeVerification)
		var spec computer.Specification
		if json.Unmarshal(verification, detail.Verification) != nil || json.Unmarshal(specification, &spec) != nil || spec.Validate() != nil {
			return NodeDetail{}, ErrConflict
		}
		detail.Verification.Resources = spec.Resources
		if len(capabilities) > 0 {
			detail.Verification.Capabilities = new(computer.Capabilities)
			if json.Unmarshal(capabilities, detail.Verification.Capabilities) != nil {
				return NodeDetail{}, ErrConflict
			}
		}
		detail.VerificationStatus = detail.Verification.State
		detail.VerificationError = detail.Verification.ErrorCode
	}
	detail.Diagnosis = diagnoseNode(detail, now)
	v := detail.Verification
	detail.CanRetryVerification = !detail.IsDraining && detail.ControlExpiresAt != nil && detail.ControlExpiresAt.After(now) && v != nil && (v.State == "quarantined" || v.State == "failed" && v.NextAttemptAt.After(now)) && (v.LeaseExpiresAt == nil || !v.LeaseExpiresAt.After(now))
	return detail, nil
}

// UpdateNode 与容量预留使用相同节点行锁；并发编辑必须匹配配置版本，不能覆盖新配置。
func (service *Administration) UpdateNode(ctx context.Context, actor identity.Principal, nodeID string, update NodeUpdate, requestID string) (NodeDetail, error) {
	if err := service.authorizer.RequireAdmin(ctx, actor, true); err != nil {
		return NodeDetail{}, err
	}
	if !validID(nodeID) || !validID(requestID) || update.ConfigurationVersion < 1 || !validNodeSettings(update.Name, update.Capacity) {
		return NodeDetail{}, ErrInvalidInput
	}
	var result NodeDetail
	catalog := &Service{control: service.control, nodeID: nodeID}
	err := catalog.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := lockVerificationNode(ctx, tx, nodeID); err != nil {
			return err
		}
		before, err := readNodeDetail(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		if before.ConfigurationVersion != update.ConfigurationVersion || update.Capacity < before.Reserved {
			return ErrConflict
		}
		if before.Name == update.Name && before.Capacity == update.Capacity && before.IsDraining == update.IsDraining {
			result = before
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE tombot.computer_nodes SET name=$2,capacity=$3,configuration_version=configuration_version+1,
		status=CASE WHEN $4 THEN 'draining' WHEN status='draining' THEN 'bootstrapping' ELSE status END,
		verified_image_id=CASE WHEN $5 THEN NULL ELSE verified_image_id END,capability_expires_at=CASE WHEN $5 THEN NULL ELSE capability_expires_at END WHERE id=$1`, nodeID, update.Name, update.Capacity, update.IsDraining, before.IsDraining != update.IsDraining)
		if err != nil {
			return err
		}
		if before.IsDraining != update.IsDraining {
			if _, err = tx.ExecContext(ctx, `UPDATE tombot.node_verifications SET is_valid=false WHERE node_id=$1`, nodeID); err != nil {
				return err
			}
		}
		if err = adminNodeAudit(ctx, tx, actor, requestID, nodeID, "修改节点配置", map[string]any{"name": update.Name, "capacity": update.Capacity, "is_draining": update.IsDraining, "configuration_version": before.ConfigurationVersion + 1}); err != nil {
			return err
		}
		result, err = readNodeDetail(ctx, tx, nodeID)
		return err
	})
	return result, err
}

// RetryNodeVerification 先核对并清理原隔离资源，保留身份与 fencing 账本；失败终态只提前下一次正常验收。
func (service *Administration) RetryNodeVerification(ctx context.Context, actor identity.Principal, nodeID string, token int64, requestID string) (NodeDetail, error) {
	if err := service.authorizer.RequireAdmin(ctx, actor, true); err != nil {
		return NodeDetail{}, err
	}
	if !validID(nodeID) || !validID(requestID) || token < 1 {
		return NodeDetail{}, ErrInvalidInput
	}
	var result NodeDetail
	catalog := &Service{control: service.control, nodeID: nodeID}
	err := catalog.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := lockVerificationNode(ctx, tx, nodeID); err != nil {
			return err
		}
		before, err := readNodeDetail(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		if !before.CanRetryVerification || before.Verification.FencingToken != token {
			return ErrConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE tombot.node_verifications SET
		state=CASE WHEN state='quarantined' THEN 'cleaning' ELSE state END,
		is_valid=CASE WHEN state='failed' THEN false ELSE is_valid END,attempts=0,lease_expires_at=NULL,next_attempt_at=clock_timestamp(),updated_at=clock_timestamp() WHERE node_id=$1`, nodeID)
		if err != nil {
			return err
		}
		if err = adminNodeAudit(ctx, tx, actor, requestID, nodeID, "重新核对节点验收", map[string]any{"fencing_token": token, "previous_state": before.Verification.State}); err != nil {
			return err
		}
		result, err = readNodeDetail(ctx, tx, nodeID)
		return err
	})
	return result, err
}

func adminNodeAudit(ctx context.Context, tx *sql.Tx, actor identity.Principal, requestID, nodeID, action string, changes map[string]any) error {
	encoded, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tombot.admin_audit_events(id,actor_id,action,target_kind,target_id,request_id,result,changes) VALUES($1,$2,$3,'computer_node',$4,$5,'succeeded',$6)`, ids.New(), actor.UserID, action, nodeID, requestID, encoded)
	return err
}

func diagnoseNode(node NodeDetail, now time.Time) NodeDiagnosis {
	if node.IsDraining {
		return NodeDiagnosis{"draining", "已停止接收新电脑，已有电脑不受此设置影响。"}
	}
	if node.LastSeenAt == nil {
		return NodeDiagnosis{"waiting_heartbeat", "尚未确认首次心跳，请检查节点服务、访问地址、网络连通和证书配置。"}
	}
	if node.ControlExpiresAt == nil || !node.ControlExpiresAt.After(now) {
		return NodeDiagnosis{"offline", "节点心跳已过期，请检查节点服务和网络连接。"}
	}
	v := node.Verification
	if v == nil {
		return NodeDiagnosis{"waiting_verification", "节点在线，正在等待首次电脑能力验收。"}
	}
	if v.State == "quarantined" {
		return NodeDiagnosis{"quarantined", "验收资源状态连续核对失败，已隔离并停止自动重试。修复节点故障后可重新核对原资源。"}
	}
	if v.State == "failed" {
		return NodeDiagnosis{"verification_failed", "电脑能力验收未通过，临时资源已回收；节点暂不可创建电脑。"}
	}
	if v.State == "cleaning" {
		return NodeDiagnosis{"cleaning", "正在清理验收电脑；清理和最终复核完成后才能就绪。"}
	}
	if v.State == "checking" {
		return NodeDiagnosis{"checking", "节点在线，正在创建并检查独立的验收电脑。"}
	}
	if node.Status == "ready" {
		return NodeDiagnosis{"ready", "心跳与电脑能力验收均有效，可以接收新电脑。"}
	}
	return NodeDiagnosis{"verification_expired", "原能力证据已过期或运行实例发生变化，等待重新验收。"}
}
