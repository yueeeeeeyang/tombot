package modelregistry

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"

	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
)

type versionState struct {
	version        Version
	policyRevision int64
	isAvailable    bool
	candidates     []candidateState
}

func lockPolicy(ctx context.Context, transaction *sql.Tx, policyID string) error {
	return requireRow(transaction.QueryRowContext(ctx, `SELECT id FROM tombot.model_policies WHERE id=$1 FOR UPDATE`, policyID))
}
func readVersion(ctx context.Context, tx *sql.Tx, policyID string, number int64, lock bool) (versionState, error) {
	var state versionState
	query := `SELECT v.policy_id,v.version,v.routing_mode,coalesce(p.current_version=v.version,false),v.verified_at,p.revision,p.is_enabled
 FROM tombot.model_policy_versions v JOIN tombot.model_policies p ON p.id=v.policy_id WHERE v.policy_id=$1 AND v.version=$2`
	if lock {
		query += ` FOR SHARE OF p,v`
	}
	v := &state.version
	err := tx.QueryRowContext(ctx, query, policyID, number).Scan(&v.PolicyID, &v.Number, &v.RoutingMode, &v.IsEnabled, &v.VerifiedAt, &state.policyRevision, &state.isAvailable)
	if errors.Is(err, sql.ErrNoRows) {
		return state, ErrNotFound
	}
	if err != nil {
		return state, err
	}
	state.candidates, err = readCandidates(ctx, tx, policyID, number, lock)
	for _, c := range state.candidates {
		v.Candidates = append(v.Candidates, c.candidate)
	}
	return state, err
}
func decodeConfiguration(encoded []byte) (Configuration, error) {
	var config Configuration
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if len(encoded) > 16<<10 || decoder.Decode(&config) != nil {
		return config, ErrUnavailable
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || !config.valid() {
		return Configuration{}, ErrUnavailable
	}
	return config, nil
}

// ListVersions 聚合候选快照，单次有界查询避免按版本逐行访问目录。
func (service *Service) ListVersions(ctx context.Context, actor identity.Principal, policyID string, before int64, limit int) ([]Version, error) {
	if err := service.authorizer.RequireAdmin(ctx, actor, false); err != nil {
		return nil, err
	}
	if !validID(policyID) || before < 0 || limit < 1 || limit > 100 {
		return nil, ErrInvalidInput
	}
	versions := []Version{}
	err := service.transaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `WITH page AS (SELECT v.policy_id,v.version,v.routing_mode,v.verified_at,coalesce(p.current_version=v.version,false) AS is_enabled
   FROM tombot.model_policy_versions v JOIN tombot.model_policies p ON p.id=v.policy_id
   WHERE v.policy_id=$1 AND ($2::bigint=0 OR v.version<$2) ORDER BY v.version DESC LIMIT $3)
   SELECT p.policy_id,p.version,p.routing_mode,p.is_enabled,p.verified_at,
   jsonb_agg(to_jsonb(c)-ARRAY['policy_id','version'] ORDER BY c.position)
   FROM page p JOIN tombot.model_policy_candidates c USING(policy_id,version)
   GROUP BY p.policy_id,p.version,p.routing_mode,p.is_enabled,p.verified_at ORDER BY p.version DESC`, policyID, before, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v Version
			var encoded []byte
			if err = rows.Scan(&v.PolicyID, &v.Number, &v.RoutingMode, &v.IsEnabled, &v.VerifiedAt, &encoded); err != nil {
				return err
			}
			if json.Unmarshal(encoded, &v.Candidates) != nil || len(v.Candidates) < 1 || len(v.Candidates) > MaxCandidates {
				return ErrUnavailable
			}
			for _, c := range v.Candidates {
				if !c.Configuration.valid() {
					return ErrUnavailable
				}
			}
			versions = append(versions, v)
		}
		return rows.Err()
	})
	return versions, err
}

// CreateVersion 原子创建停用版本和候选快照；参数和凭据默认继承，历史配置不可原地修改。
func (service *Service) CreateVersion(ctx context.Context, actor identity.Principal, requestID string, request VersionRequest) (Version, error) {
	if err := service.authorize(ctx, actor, requestID); err != nil {
		return Version{}, err
	}
	if !validID(request.PolicyID) || !validRoutingMode(request.RoutingMode) || len(request.Candidates) < 1 || len(request.Candidates) > MaxCandidates {
		return Version{}, ErrInvalidInput
	}
	seen := map[string]bool{}
	for _, c := range request.Candidates {
		if !validID(c.ModelID) || seen[c.ModelID] || c.Priority < 1 || c.Priority > 1000 || c.Weight < 1 || c.Weight > 1000 || c.CredentialID != nil && !validID(*c.CredentialID) {
			return Version{}, ErrInvalidInput
		}
		seen[c.ModelID] = true
	}
	v := Version{PolicyID: request.PolicyID, RoutingMode: request.RoutingMode, Candidates: []Candidate{}}
	err := service.transaction(ctx, func(tx *sql.Tx) error {
		if err := lockPolicy(ctx, tx, v.PolicyID); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(version),0)+1 FROM tombot.model_policy_versions WHERE policy_id=$1`, v.PolicyID).Scan(&v.Number); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tombot.model_policy_versions(policy_id,version,routing_mode) VALUES($1,$2,$3)`, v.PolicyID, v.Number, v.RoutingMode); err != nil {
			return err
		}
		for i, request := range request.Candidates {
			c, err := snapshotCandidate(ctx, tx, request, i+1)
			if err != nil {
				return err
			}
			encoded, err := json.Marshal(c.Configuration)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.model_policy_candidates(policy_id,version,position,provider_id,model_id,credential_id,configuration,priority,weight)
    VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, v.PolicyID, v.Number, c.Position, c.ProviderID, c.ModelID, c.CredentialID, encoded, c.Priority, c.Weight); err != nil {
				return err
			}
			v.Candidates = append(v.Candidates, c)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tombot.model_policies SET revision=revision+1 WHERE id=$1`, v.PolicyID); err != nil {
			return err
		}
		return audit(ctx, tx, actor, requestID, "新增多模型策略版本", v.PolicyID, map[string]any{"version": v.Number, "candidates": len(v.Candidates), "routing_mode": v.RoutingMode})
	})
	if err != nil {
		return Version{}, err
	}
	return v, nil
}

// EnableVersion 首次启用前逐个验证候选；测试不持事务，提交时复验修订并原子替换唯一启用版本。
// 已验证版本只检查当前可用性；修订冲突、能力验证失败或目录变动均保持原版本不变。
func (service *Service) EnableVersion(ctx context.Context, actor identity.Principal, requestID, policyID string, number, expectedRevision int64) (Version, error) {
	if err := service.authorize(ctx, actor, requestID); err != nil {
		return Version{}, err
	}
	if !validID(policyID) || number < 1 || expectedRevision < 1 {
		return Version{}, ErrInvalidInput
	}
	var initial versionState
	var credentials [][]byte
	defer func() {
		for _, value := range credentials {
			clear(value)
		}
	}()
	err := service.transaction(ctx, func(tx *sql.Tx) error {
		var err error
		initial, err = readVersion(ctx, tx, policyID, number, true)
		if err != nil {
			return err
		}
		if initial.policyRevision != expectedRevision {
			return ErrConflict
		}
		if !initial.isAvailable || !hasAvailableCandidate(initial) {
			return ErrUnavailable
		}
		if initial.version.VerifiedAt != nil {
			return nil
		}
		for _, c := range initial.candidates {
			if !c.available {
				return ErrUnavailable
			}
			plaintext, err := service.decrypt(ctx, tx, c.candidate)
			if err != nil {
				return err
			}
			credentials = append(credentials, plaintext)
		}
		return nil
	})
	if err != nil {
		return Version{}, err
	}
	if initial.version.VerifiedAt == nil {
		bounded, cancel := context.WithTimeout(ctx, service.options.VerificationTimeout)
		defer cancel()
		for i, c := range initial.candidates {
			err = service.verifier.Verify(bounded, c.candidate.Configuration, credentials[i])
			clear(credentials[i])
			if err != nil || bounded.Err() != nil {
				if ctx.Err() != nil {
					return Version{}, ctx.Err()
				}
				if recordErr := service.transaction(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `INSERT INTO tombot.admin_audit_events(id,actor_id,action,target_kind,target_id,request_id,result,changes)
      VALUES($1,$2,'模型策略能力验证失败','model_configuration',$3,$4,'failed',jsonb_build_object('version',$5::bigint,'model_id',$6::text))`, ids.New(), actor.UserID, policyID, requestID, number, c.candidate.ModelID)
					return err
				}); recordErr != nil {
					return Version{}, recordErr
				}
				return Version{}, ErrVerification
			}
		}
	}
	if err = service.authorize(ctx, actor, requestID); err != nil {
		return Version{}, err
	}
	var enabled Version
	err = service.transaction(ctx, func(tx *sql.Tx) error {
		if err := lockPolicy(ctx, tx, policyID); err != nil {
			return err
		}
		current, err := readVersion(ctx, tx, policyID, number, true)
		if err != nil {
			return err
		}
		if current.policyRevision != initial.policyRevision {
			return ErrConflict
		}
		if !current.isAvailable || !hasAvailableCandidate(current) {
			return ErrUnavailable
		}
		if current.version.IsEnabled {
			enabled = current.version
			return nil
		}
		if initial.version.VerifiedAt == nil {
			for i, c := range current.candidates {
				if !c.available {
					return ErrUnavailable
				}
				if c.providerRevision != initial.candidates[i].providerRevision || c.modelRevision != initial.candidates[i].modelRevision {
					return ErrConflict
				}
				check, err := service.decrypt(ctx, tx, c.candidate)
				clear(check)
				if err != nil {
					return err
				}
			}
			if _, err = tx.ExecContext(ctx, `UPDATE tombot.model_policy_versions SET verified_at=clock_timestamp() WHERE policy_id=$1 AND version=$2`, policyID, number); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE tombot.model_policies SET current_version=$2,revision=revision+1 WHERE id=$1`, policyID, number); err != nil {
			return err
		}
		state, err := readVersion(ctx, tx, policyID, number, false)
		if err != nil {
			return err
		}
		enabled = state.version
		return audit(ctx, tx, actor, requestID, "启用模型策略版本", policyID, map[string]any{"version": number})
	})
	if err != nil {
		return Version{}, err
	}
	return enabled, nil
}

func hasAvailableCandidate(state versionState) bool {
	for _, c := range state.candidates {
		if c.available {
			return true
		}
	}
	return false
}

// GetVersion 返回完整历史快照；目录状态变化不会隐藏管理员的历史记录。
func (service *Service) GetVersion(ctx context.Context, actor identity.Principal, policyID string, number int64) (Version, error) {
	if err := service.authorizer.RequireAdmin(ctx, actor, false); err != nil {
		return Version{}, err
	}
	if !validID(policyID) || number < 1 {
		return Version{}, ErrInvalidInput
	}
	var v Version
	err := service.transaction(ctx, func(tx *sql.Tx) error {
		state, err := readVersion(ctx, tx, policyID, number, false)
		v = state.version
		return err
	})
	return v, err
}

// DisableVersion 清除所选版本；已停用版本重复操作不影响其他版本，已有运行保持原配置。
func (service *Service) DisableVersion(ctx context.Context, actor identity.Principal, requestID, policyID string, number, expectedRevision int64) error {
	if err := service.authorize(ctx, actor, requestID); err != nil {
		return err
	}
	if !validID(policyID) || number < 1 || expectedRevision < 1 {
		return ErrInvalidInput
	}
	return service.transaction(ctx, func(tx *sql.Tx) error {
		if err := lockPolicy(ctx, tx, policyID); err != nil {
			return err
		}
		state, err := readVersion(ctx, tx, policyID, number, true)
		if err != nil {
			return err
		}
		if state.policyRevision != expectedRevision {
			return ErrConflict
		}
		if !state.version.IsEnabled {
			return nil
		}
		if _, err = tx.ExecContext(ctx, `UPDATE tombot.model_policies SET revision=revision+1,current_version=NULL WHERE id=$1`, policyID); err != nil {
			return err
		}
		return audit(ctx, tx, actor, requestID, "停用模型策略版本", policyID, map[string]any{"version": number})
	})
}
