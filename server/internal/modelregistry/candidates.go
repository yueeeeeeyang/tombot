package modelregistry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// MaxCandidates 限制单版本候选数，也限制一次任务的最大故障切换范围。
const MaxCandidates = 16

func validRoutingMode(mode string) bool {
	return mode == "priority" || mode == "weighted" || mode == "round_robin"
}

type candidateState struct {
	candidate                       Candidate
	providerRevision, modelRevision int64
	available                       bool
}

// readCandidates 单次读取有界候选及当前目录状态；目录变化只使相应候选不可用，不覆盖快照。
func readCandidates(ctx context.Context, tx *sql.Tx, policyID string, number int64, lock bool) ([]candidateState, error) {
	query := `SELECT c.position,c.provider_id,c.model_id,c.credential_id,c.configuration,c.priority,c.weight,
 p.revision,m.revision,p.is_enabled AND m.is_enabled AND (c.credential_id IS NULL OR k.revoked_at IS NULL AND k.destroyed_at IS NULL),
 p.protocol,p.base_url,m.model_key,m.context_limit,m.output_limit,m.supports_tools,m.supports_vision,m.reasoning_effort,m.metadata
 FROM tombot.model_policy_candidates c JOIN tombot.model_providers p ON p.id=c.provider_id
 JOIN tombot.models m ON m.id=c.model_id AND m.provider_id=c.provider_id
 LEFT JOIN tombot.model_credentials k ON k.id=c.credential_id AND k.provider_id=c.provider_id
 WHERE c.policy_id=$1 AND c.version=$2 ORDER BY c.position LIMIT 17`
	if lock {
		query += ` FOR SHARE OF p,m`
	}
	rows, err := tx.QueryContext(ctx, query, policyID, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := []candidateState{}
	for rows.Next() {
		var state candidateState
		var canonical Configuration
		var encoded, metadata []byte
		c := &state.candidate
		if err = rows.Scan(&c.Position, &c.ProviderID, &c.ModelID, &c.CredentialID, &encoded, &c.Priority, &c.Weight,
			&state.providerRevision, &state.modelRevision, &state.available, &canonical.Protocol, &canonical.BaseURL, &canonical.ModelKey,
			&canonical.ContextLimit, &canonical.OutputLimit, &canonical.SupportsTools, &canonical.SupportsVision, &canonical.ReasoningEffort, &metadata); err != nil {
			return nil, err
		}
		c.Configuration, err = decodeConfiguration(encoded)
		if err != nil {
			return nil, err
		}
		snapshot := c.Configuration
		var declared ModelMetadata
		if json.Unmarshal(metadata, &declared) != nil {
			return nil, ErrUnavailable
		}
		state.available = state.available && snapshot.Protocol == canonical.Protocol && snapshot.BaseURL == canonical.BaseURL && snapshot.ModelKey == canonical.ModelKey &&
			snapshot.ContextLimit == canonical.ContextLimit && snapshot.OutputLimit <= canonical.OutputLimit && snapshot.SupportsTools == canonical.SupportsTools &&
			snapshot.SupportsVision == canonical.SupportsVision && declared.accepts(snapshot.ReasoningEffort, snapshot.SupportsVision)
		states = append(states, state)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(states) < 1 || len(states) > MaxCandidates {
		return nil, ErrUnavailable
	}
	return states, nil
}

// snapshotCandidate 在停用事务中继承当前参数；覆盖只能收紧输出，思考级别必须属于模型声明。
func snapshotCandidate(ctx context.Context, tx *sql.Tx, request CandidateRequest, position int) (Candidate, error) {
	c := Candidate{Position: position, ModelID: request.ModelID, CredentialID: request.CredentialID, Priority: request.Priority, Weight: request.Weight}
	config := &c.Configuration
	config.Prices = request.Prices
	var enabled bool
	var metadata []byte
	err := tx.QueryRowContext(ctx, `SELECT p.id,p.protocol,p.base_url,m.model_key,m.context_limit,m.output_limit,m.supports_tools,m.supports_vision,m.reasoning_effort,m.metadata,p.is_enabled AND m.is_enabled
 FROM tombot.models m JOIN tombot.model_providers p ON p.id=m.provider_id WHERE m.id=$1 FOR SHARE OF p,m`, request.ModelID).
		Scan(&c.ProviderID, &config.Protocol, &config.BaseURL, &config.ModelKey, &config.ContextLimit, &config.OutputLimit, &config.SupportsTools, &config.SupportsVision, &config.ReasoningEffort, &metadata, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	if !enabled {
		return c, ErrUnavailable
	}
	if request.OutputLimit != nil {
		if *request.OutputLimit < 1 || *request.OutputLimit > config.OutputLimit {
			return c, ErrInvalidInput
		}
		config.OutputLimit = *request.OutputLimit
	}
	var declared ModelMetadata
	if json.Unmarshal(metadata, &declared) != nil {
		return c, ErrUnavailable
	}
	if request.ReasoningEffort != nil {
		config.ReasoningEffort = *request.ReasoningEffort
	}
	if !config.valid() || !declared.accepts(config.ReasoningEffort, config.SupportsVision) {
		return c, ErrInvalidInput
	}
	if c.CredentialID == nil {
		var id string
		err = tx.QueryRowContext(ctx, `SELECT id FROM tombot.model_credentials WHERE provider_id=$1 AND revoked_at IS NULL AND destroyed_at IS NULL ORDER BY version DESC LIMIT 1 FOR SHARE`, c.ProviderID).Scan(&id)
		if err == nil {
			c.CredentialID = &id
		} else if errors.Is(err, sql.ErrNoRows) {
			return c, ErrCredential
		} else {
			return c, err
		}
	} else if err = requireRow(tx.QueryRowContext(ctx, `SELECT id FROM tombot.model_credentials WHERE id=$1 AND provider_id=$2 AND revoked_at IS NULL AND destroyed_at IS NULL FOR SHARE`, *c.CredentialID, c.ProviderID)); err != nil {
		return c, err
	}
	return c, nil
}
