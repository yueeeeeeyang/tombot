package modelregistry

import (
	"context"
	"database/sql"
	"errors"

	"io.yueyang/tom/internal/identity"
)

// GetProvider 按标识读取供应商详情，供独立页面直接加载；不返回凭据，不依赖目录分页。
// 仅管理员可读，标识无效或记录不存在时分别返回输入错误或未找到。
func (service *Service) GetProvider(ctx context.Context, actor identity.Principal, providerID string) (Provider, error) {
	if err := service.authorizer.RequireAdmin(ctx, actor, false); err != nil {
		return Provider{}, err
	}
	if !validID(providerID) {
		return Provider{}, ErrInvalidInput
	}
	var value Provider
	err := service.transaction(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT id,name,protocol,base_url,is_enabled,revision,created_at FROM tombot.model_providers WHERE id=$1`, providerID).Scan(&value.ID, &value.Name, &value.Protocol, &value.BaseURL, &value.IsEnabled, &value.Revision, &value.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return value, err
}

// GetPolicy 按标识读取策略当前状态和修订号，供详情页刷新及条件写入。
// 仅管理员可读，标识无效或记录不存在时分别返回输入错误或未找到。
func (service *Service) GetPolicy(ctx context.Context, actor identity.Principal, policyID string) (Policy, error) {
	if err := service.authorizer.RequireAdmin(ctx, actor, false); err != nil {
		return Policy{}, err
	}
	if !validID(policyID) {
		return Policy{}, ErrInvalidInput
	}
	var value Policy
	err := service.transaction(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT id,name,is_default,is_enabled,revision,current_version FROM tombot.model_policies WHERE id=$1`, policyID).Scan(&value.ID, &value.Name, &value.IsDefault, &value.IsEnabled, &value.Revision, &value.CurrentVersion)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return value, err
}
