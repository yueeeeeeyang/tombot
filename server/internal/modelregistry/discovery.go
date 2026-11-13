package modelregistry

import (
	"context"
	"database/sql"
	"errors"

	"io.yueyang/tom/internal/identity"
)

// DiscoveredModel 是供应商模型列表返回的一项，只含标识与显示名。
// 上下文与输出上限不在上游列表里，登记时必须由管理员显式填写，不能推断。
type DiscoveredModel struct {
	ModelKey    string `json:"model_key"`
	DisplayName string `json:"display_name"`
}

// Discoverer 通过受控出口读取供应商模型列表；实现不得跟随跳转或回传上游响应正文。
type Discoverer interface {
	Discover(context.Context, string, string, []byte) ([]DiscoveredModel, error)
}

// ListRemoteModels 读取指定供应商当前可用的模型标识，供管理员挑选后登记到目录。
// 该操作只读，不改变目录；登记仍走 CreateModel 的能力与上限校验。
// 使用该供应商最新未撤销的凭据；没有可用凭据时明确拒绝，而不是返回空列表。
func (service *Service) ListRemoteModels(ctx context.Context, actor identity.Principal, providerID string) ([]DiscoveredModel, error) {
	if err := service.authorizer.RequireAdmin(ctx, actor, false); err != nil {
		return nil, err
	}
	if !validID(providerID) || service.discoverer == nil {
		return nil, ErrInvalidInput
	}
	var protocol, baseURL string
	var credential []byte
	defer func() { clear(credential) }()
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		err := transaction.QueryRowContext(ctx, `SELECT protocol,base_url FROM tombot.model_providers WHERE id=$1 AND is_enabled`, providerID).Scan(&protocol, &baseURL)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var credentialID string
		err = transaction.QueryRowContext(ctx, `SELECT id FROM tombot.model_credentials
			WHERE provider_id=$1 AND revoked_at IS NULL AND destroyed_at IS NULL ORDER BY version DESC LIMIT 1`, providerID).Scan(&credentialID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCredential
		}
		if err != nil {
			return err
		}
		credential, err = service.decrypt(ctx, transaction, Version{ProviderID: providerID, CredentialID: &credentialID})
		return err
	})
	if err != nil {
		return nil, err
	}
	return service.discoverer.Discover(ctx, protocol, baseURL, credential)
}
