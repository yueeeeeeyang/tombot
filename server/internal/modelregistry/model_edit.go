package modelregistry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"io.yueyang/tom/internal/identity"
)

// lockDisabledModel 串行化编辑、启用与删除；调用方事务结束前始终保留行锁。
func lockDisabledModel(ctx context.Context, tx *sql.Tx, modelID string, revision int64) (string, error) {
	var providerID string
	var currentRevision int64
	var enabled bool
	err := tx.QueryRowContext(ctx, `SELECT provider_id,revision,is_enabled FROM tombot.models WHERE id=$1 FOR UPDATE`, modelID).Scan(&providerID, &currentRevision, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if currentRevision != revision {
		return "", ErrConflict
	}
	if enabled {
		return "", ErrModelEnabled
	}
	return providerID, nil
}

// UpdateModel 只修改停用模型的参数，保留身份与供应商归属，修订号不符则拒绝覆盖。
// 历史策略配置独立保存；修改后需重新创建并测试启用策略版本。
func (service *Service) UpdateModel(ctx context.Context, actor identity.Principal, requestID, modelID string, revision int64, request ModelRequest) (Model, error) {
	if err := service.authorize(ctx, actor, requestID); err != nil {
		return Model{}, err
	}
	if !validID(modelID) || revision < 1 || request.ProviderID != "" || !validModelParameters(request) {
		return Model{}, ErrInvalidInput
	}
	encoded, err := json.Marshal(request.Metadata)
	if err != nil {
		return Model{}, ErrInvalidInput
	}
	updated := Model{ID: modelID, ModelKey: request.ModelKey, DisplayName: request.DisplayName, ContextLimit: request.ContextLimit, OutputLimit: request.OutputLimit, SupportsTools: request.SupportsTools, SupportsVision: request.SupportsVision, ReasoningEffort: request.ReasoningEffort, Metadata: request.Metadata}
	err = service.transaction(ctx, func(tx *sql.Tx) error {
		var err error
		updated.ProviderID, err = lockDisabledModel(ctx, tx, modelID, revision)
		if err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, `UPDATE tombot.models SET model_key=$2,display_name=$3,context_limit=$4,output_limit=$5,supports_tools=$6,supports_vision=$7,reasoning_effort=$8,metadata=$9,revision=revision+1 WHERE id=$1 RETURNING revision`, modelID, request.ModelKey, request.DisplayName, request.ContextLimit, request.OutputLimit, request.SupportsTools, request.SupportsVision, request.ReasoningEffort, encoded).Scan(&updated.Revision)
		if err != nil {
			return err
		}
		return audit(ctx, tx, actor, requestID, "编辑停用模型", modelID, map[string]any{"provider_id": updated.ProviderID, "revision": updated.Revision})
	})
	if err != nil {
		return Model{}, err
	}
	return updated, nil
}

// DeleteModel 删除停用且没有任何策略版本引用的模型；审计与删除同事务提交。
func (service *Service) DeleteModel(ctx context.Context, actor identity.Principal, requestID, modelID string, revision int64) error {
	if err := service.authorize(ctx, actor, requestID); err != nil {
		return err
	}
	if !validID(modelID) || revision < 1 {
		return ErrInvalidInput
	}
	return service.transaction(ctx, func(tx *sql.Tx) error {
		providerID, err := lockDisabledModel(ctx, tx, modelID, revision)
		if err != nil {
			return err
		}
		var referenced bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.model_policy_candidates WHERE model_id=$1)`, modelID).Scan(&referenced); err != nil {
			return err
		}
		if referenced {
			return ErrModelReferenced
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM tombot.models WHERE id=$1`, modelID); err != nil {
			return err
		}
		return audit(ctx, tx, actor, requestID, "删除停用模型", modelID, map[string]any{"provider_id": providerID, "revision": revision})
	})
}
