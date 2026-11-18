package nodeenrollment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/nodeapi"
	"io.yueyang/tom/internal/nodecatalog"
	"io.yueyang/tom/internal/store/postgres"
)

const enrollmentLifetime = 24 * time.Hour

type Service struct {
	control      *postgres.Database
	authorizer   nodecatalog.Authorizer
	maximumNodes int
}

type Grant struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func New(control *postgres.Database, authorizer nodecatalog.Authorizer, maximumNodes int) (*Service, error) {
	if control == nil || control.Role() != postgres.ControlRole || authorizer == nil || maximumNodes < 1 || maximumNodes > 1000 {
		return nil, errors.New("节点接入服务配置无效")
	}
	return &Service{control, authorizer, maximumNodes}, nil
}

// Issue 由近期验证的管理员生成一次性凭据；数据库只保存摘要，原值仅在本次响应返回。
func (service *Service) Issue(ctx context.Context, actor identity.Principal, requestID string) (Grant, error) {
	if err := service.authorizer.RequireAdmin(ctx, actor, true); err != nil {
		return Grant{}, err
	}
	if (computer.NodeQuery{NodeID: requestID}).Validate() != nil {
		return Grant{}, nodecatalog.ErrInvalidInput
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Grant{}, errors.New("生成节点接入凭据失败")
	}
	defer clear(raw)
	digest := sha256.Sum256(raw)
	grant := Grant{ID: ids.New(), Token: base64.RawURLEncoding.EncodeToString(raw)}
	err := service.control.Platform(ctx, func(tx *sql.Tx) error {
		// 有界清理不影响已接入节点；重启依赖节点签名，不依赖已消费的凭据。
		if _, err := tx.ExecContext(ctx, `DELETE FROM tombot.node_enrollment_tokens WHERE id IN (SELECT id FROM tombot.node_enrollment_tokens WHERE expires_at<clock_timestamp() ORDER BY expires_at LIMIT 100)`); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `INSERT INTO tombot.node_enrollment_tokens(id,token_hash,created_by,expires_at) VALUES($1,$2,$3,clock_timestamp()+$4::interval) RETURNING expires_at`, grant.ID, digest[:], actor.UserID, enrollmentLifetime.String()).Scan(&grant.ExpiresAt); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO tombot.admin_audit_events(id,actor_id,action,target_kind,target_id,request_id,result,changes) VALUES($1,$2,'签发节点接入凭据','node_enrollment',$3,$4,'succeeded','{}')`, ids.New(), actor.UserID, grant.ID, requestID)
		return err
	})
	if err != nil {
		return Grant{}, transactionError(err)
	}
	return grant, nil
}

// Register 首次消费凭据，重启只验证已绑定证书；连接、目录与凭据消费在同一事务提交。
func (service *Service) Register(ctx context.Context, request Request, token, requestID string) (Receipt, error) {
	fingerprint, err := request.Verify(time.Now())
	if err != nil {
		return Receipt{}, err
	}
	claim := request.Claim
	catalog, err := nodecatalog.New(service.control, claim.NodeID)
	if err != nil {
		return Receipt{}, err
	}
	state := "registered"
	err = service.control.Platform(ctx, func(tx *sql.Tx) error {
		// 首次接入争用总量与多个唯一身份，固定全局顺序避免并发突破容量或重复消费。
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('tombot.node-enrollment',0))`); err != nil {
			return err
		}
		var url, pin, imageID string
		err := tx.QueryRowContext(ctx, `SELECT url,certificate_fingerprint,image_id FROM tombot.node_connections WHERE node_id=$1`, claim.NodeID).Scan(&url, &pin, &imageID)
		if err == nil {
			if pin != fingerprint {
				return ErrUnauthorized
			}
			if url != claim.URL {
				return ErrConflict
			}
			if claim.Maintenance != nil {
				state, err = reserveUpgrade(ctx, tx, catalog, claim, imageID, requestID)
				if err != nil || !claim.SupportsComputerUpgrades {
					return err
				}
				_, err = tx.ExecContext(ctx, `UPDATE tombot.node_connections SET supports_computer_upgrades=true WHERE node_id=$1 AND NOT supports_computer_upgrades`, claim.NodeID)
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE tombot.node_connections SET supports_computer_upgrades=$2 WHERE node_id=$1 AND supports_computer_upgrades<>$2`, claim.NodeID, claim.SupportsComputerUpgrades); err != nil {
				return err
			}
			if imageID != claim.Registration.ImageID {
				var conflicting bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.node_upgrades WHERE node_id=$1 AND NOT is_finished AND target_image_id<>$2)`, claim.NodeID, claim.Registration.ImageID).Scan(&conflicting); err != nil {
					return err
				}
				if conflicting || claim.Registration.PreviousImageID != imageID {
					return ErrConflict
				}
				if err := catalog.UpgradeImageInTransaction(ctx, tx, claim.Registration); err != nil {
					return err
				}
				_, err := tx.ExecContext(ctx, `UPDATE tombot.node_connections SET image_id=$2 WHERE node_id=$1`, claim.NodeID, claim.Registration.ImageID)
				return err
			}
			return catalog.ReconnectInTransaction(ctx, tx, claim.Registration)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if claim.Maintenance != nil || claim.Registration.PreviousImageID != "" {
			return ErrConflict
		}
		raw, err := parseToken(token)
		if err != nil {
			return ErrUnauthorized
		}
		digest := sha256.Sum256(raw)
		clear(raw)
		var grantID string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM tombot.node_enrollment_tokens WHERE token_hash=$1 AND consumed_node_id IS NULL AND expires_at>clock_timestamp() FOR UPDATE`, digest[:]).Scan(&grantID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrUnauthorized
			}
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tombot.node_connections`).Scan(&count); err != nil {
			return err
		}
		if count >= service.maximumNodes {
			return ErrCapacity
		}
		if _, err := catalog.RegisterInTransaction(ctx, tx, claim.Registration, requestID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tombot.node_connections(node_id,url,certificate_fingerprint,image_id,supports_computer_upgrades) VALUES($1,$2,$3,$4,$5)`, claim.NodeID, claim.URL, fingerprint, claim.Registration.ImageID, claim.SupportsComputerUpgrades); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tombot.computer_nodes SET status=CASE WHEN status='ready' THEN 'bootstrapping' ELSE status END,control_expires_at=NULL,capability_expires_at=NULL WHERE id=$1`, claim.NodeID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tombot.node_enrollment_tokens SET consumed_node_id=$1 WHERE id=$2`, claim.NodeID, grantID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO tombot.admin_audit_events(id,action,target_kind,target_id,request_id,result,changes) VALUES($1,'节点自动接入','computer_node',$2,$3,'succeeded','{}')`, ids.New(), claim.NodeID, requestID)
		return err
	})
	if err != nil {
		return Receipt{}, transactionError(err)
	}
	return Receipt{NodeID: claim.NodeID, ImageID: claim.Registration.ImageID, State: state}, nil
}

// Resolve 只返回已授权且不可变的节点连接，不接受调用方提供的目标地址。
func (service *Service) Resolve(ctx context.Context, nodeID string) (nodeapi.Endpoint, error) {
	var endpoint nodeapi.Endpoint
	if (computer.NodeQuery{NodeID: nodeID}).Validate() != nil {
		return endpoint, nodecatalog.ErrInvalidInput
	}
	err := service.control.Platform(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT node_id,url,certificate_fingerprint,image_id FROM tombot.node_connections WHERE node_id=$1`, nodeID).Scan(&endpoint.NodeID, &endpoint.URL, &endpoint.Fingerprint, &endpoint.ImageID)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return endpoint, nodecatalog.ErrNotFound
	}
	return endpoint, transactionError(err)
}

// List 使用节点主键分页，为有限的节点监督器提供已授权的接入目录。
func (service *Service) List(ctx context.Context, after string, limit int) ([]nodeapi.Endpoint, error) {
	if limit < 1 || limit > 100 || after != "" && (computer.NodeQuery{NodeID: after}).Validate() != nil {
		return nil, nodecatalog.ErrInvalidInput
	}
	endpoints := []nodeapi.Endpoint{}
	err := service.control.Platform(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT node_id,url,certificate_fingerprint,image_id FROM tombot.node_connections WHERE node_id>$1 ORDER BY node_id LIMIT $2`, after, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var endpoint nodeapi.Endpoint
			if err := rows.Scan(&endpoint.NodeID, &endpoint.URL, &endpoint.Fingerprint, &endpoint.ImageID); err != nil {
				return err
			}
			endpoints = append(endpoints, endpoint)
		}
		return rows.Err()
	})
	return endpoints, transactionError(err)
}

func transactionError(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{ErrUnauthorized, ErrConflict, ErrCapacity, nodecatalog.ErrConflict, nodecatalog.ErrInvalidInput, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known
		}
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) && (state.SQLState() == "23505" || state.SQLState() == "40001" || state.SQLState() == "40P01") {
		return ErrConflict
	}
	return errors.New("节点接入事务未确认，请保留身份重试")
}
