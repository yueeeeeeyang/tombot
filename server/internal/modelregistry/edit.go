package modelregistry

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"

	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/provider"
)

// UpdateProvider 只编辑停用供应商，保留目录身份和历史策略快照。
// 凭据为空表示保留；更换 HTTPS 来源必须提供新凭据并撤销旧凭据，避免跨主机泄露。
func (service *Service) UpdateProvider(ctx context.Context, actor identity.Principal, requestID, providerID string, revision int64, name, protocol, baseURL string, credential []byte) (Provider, error) {
	if err := service.authorize(ctx, actor, requestID); err != nil {
		return Provider{}, err
	}
	if !validID(providerID) || revision < 1 || !validText(name, 120) || provider.GenerationPath(protocol) == "" || !validURL(baseURL) || len(credential) > 0 && !validCredential(credential) {
		return Provider{}, ErrInvalidInput
	}
	var updated Provider
	err := service.transaction(ctx, func(tx *sql.Tx) error {
		var previousURL string
		var currentRevision int64
		var isEnabled bool
		err := tx.QueryRowContext(ctx, `SELECT base_url,revision,is_enabled FROM tombot.model_providers WHERE id=$1 FOR UPDATE`, providerID).Scan(&previousURL, &currentRevision, &isEnabled)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if revision != currentRevision {
			return ErrConflict
		}
		if isEnabled {
			return ErrProviderEnabled
		}
		hasOriginChanged := !sameOrigin(previousURL, baseURL)
		if hasOriginChanged && len(credential) == 0 {
			return ErrNewCredentialRequired
		}
		if hasOriginChanged {
			if _, err = tx.ExecContext(ctx, `UPDATE tombot.model_credentials SET revoked_at=transaction_timestamp() WHERE provider_id=$1 AND revoked_at IS NULL`, providerID); err != nil {
				return err
			}
		}
		if len(credential) > 0 {
			if _, err = insertCredential(ctx, tx, service.keys, actor, requestID, providerID, credential); err != nil {
				return err
			}
		}
		err = tx.QueryRowContext(ctx, `UPDATE tombot.model_providers SET name=$2,protocol=$3,base_url=$4,revision=revision+1 WHERE id=$1 RETURNING id,name,protocol,base_url,is_enabled,revision,created_at`, providerID, name, protocol, baseURL).Scan(&updated.ID, &updated.Name, &updated.Protocol, &updated.BaseURL, &updated.IsEnabled, &updated.Revision, &updated.CreatedAt)
		if err != nil {
			return err
		}
		return audit(ctx, tx, actor, requestID, "编辑停用模型供应商", providerID, map[string]any{"revision": updated.Revision, "protocol": protocol, "origin_changed": hasOriginChanged, "credential_updated": len(credential) > 0})
	})
	if err != nil {
		return Provider{}, err
	}
	return updated, nil
}

func sameOrigin(left, right string) bool {
	first, firstError := url.Parse(left)
	second, secondError := url.Parse(right)
	if firstError != nil || secondError != nil {
		return false
	}
	port := func(address *url.URL) string {
		if address.Port() == "" {
			return "443"
		}
		return address.Port()
	}
	return first.Scheme == "https" && second.Scheme == "https" && strings.EqualFold(first.Hostname(), second.Hostname()) && port(first) == port(second)
}
