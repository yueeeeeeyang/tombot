package modelregistry

import (
	"context"
	"database/sql"
	"errors"

	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
)

// CreateProvider 登记固定协议和 HTTPS 地址，并在同一事务内写入首个凭据版本。
// 凭据与供应商必须一起提交：只建出地址而没有可用凭据的供应商无法发起任何调用，
// 也不能从供应商拉取模型列表，留下这样的记录只会让后续操作失败得莫名其妙。
// 凭据为空时拒绝；禁止把凭据藏在 URL 中。
func (service *Service) CreateProvider(ctx context.Context, actor identity.Principal, requestID, name, protocol, baseURL string, credential []byte) (Provider, error) {
	if err := service.authorize(ctx, actor, requestID); err != nil {
		return Provider{}, err
	}
	if !validText(name, 120) || (protocol != "openai_compat" && protocol != "anthropic") || !validURL(baseURL) || !validCredential(credential) {
		return Provider{}, ErrInvalidInput
	}
	provider := Provider{ID: ids.New(), Name: name, Protocol: protocol, BaseURL: baseURL, IsEnabled: true, Revision: 1}
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		if err := transaction.QueryRowContext(ctx, `INSERT INTO tombot.model_providers(id,name,protocol,base_url) VALUES($1,$2,$3,$4) RETURNING created_at`, provider.ID, name, protocol, baseURL).Scan(&provider.CreatedAt); err != nil {
			return err
		}
		if _, err := insertCredential(ctx, transaction, service.keys, actor, requestID, provider.ID, credential); err != nil {
			return err
		}
		return audit(ctx, transaction, actor, requestID, "登记模型供应商", provider.ID, struct{}{})
	})
	if err != nil {
		return Provider{}, err
	}
	return provider, nil
}

// ModelRequest 为目录登记参数，标识与启用状态只能由服务端生成。
type ModelRequest struct {
	ProviderID     string `json:"provider_id"`
	ModelKey       string `json:"model_key"`
	DisplayName    string `json:"display_name"`
	ContextLimit   int    `json:"context_limit"`
	OutputLimit    int    `json:"output_limit"`
	SupportsTools  bool   `json:"supports_tools"`
	SupportsVision bool   `json:"supports_vision"`
}

// CreateModel 登记能力上限；实际能力必须在策略发布前经过验证。
func (service *Service) CreateModel(ctx context.Context, actor identity.Principal, requestID string, request ModelRequest) (Model, error) {
	if err := service.authorize(ctx, actor, requestID); err != nil {
		return Model{}, err
	}
	if !validID(request.ProviderID) || !validText(request.ModelKey, 256) || !validText(request.DisplayName, 120) || request.ContextLimit < 1 || request.ContextLimit > 100_000_000 || request.OutputLimit < 1 || request.OutputLimit > request.ContextLimit {
		return Model{}, ErrInvalidInput
	}
	model := Model{ID: ids.New(), ProviderID: request.ProviderID, ModelKey: request.ModelKey, DisplayName: request.DisplayName, ContextLimit: request.ContextLimit, OutputLimit: request.OutputLimit, SupportsTools: request.SupportsTools, SupportsVision: request.SupportsVision, IsEnabled: true, Revision: 1}
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		if _, err := transaction.ExecContext(ctx, `INSERT INTO tombot.models(id,provider_id,model_key,display_name,context_limit,output_limit,supports_tools,supports_vision) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, model.ID, model.ProviderID, model.ModelKey, model.DisplayName, model.ContextLimit, model.OutputLimit, model.SupportsTools, model.SupportsVision); err != nil {
			return err
		}
		return audit(ctx, transaction, actor, requestID, "登记供应商模型", model.ID, map[string]any{"provider_id": model.ProviderID})
	})
	if err != nil {
		return Model{}, err
	}
	return model, nil
}

// CreatePolicy 创建尚未可用的策略；发布前不作为运行候选。
func (service *Service) CreatePolicy(ctx context.Context, actor identity.Principal, requestID, name string) (Policy, error) {
	if err := service.authorize(ctx, actor, requestID); err != nil {
		return Policy{}, err
	}
	if !validText(name, 120) {
		return Policy{}, ErrInvalidInput
	}
	policy := Policy{ID: ids.New(), Name: name, IsEnabled: true, Revision: 1}
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		if _, err := transaction.ExecContext(ctx, `INSERT INTO tombot.model_policies(id,name) VALUES($1,$2)`, policy.ID, name); err != nil {
			return err
		}
		return audit(ctx, transaction, actor, requestID, "创建模型策略", policy.ID, struct{}{})
	})
	if err != nil {
		return Policy{}, err
	}
	return policy, nil
}

// SetEnabled 通过期望修订号修改启用状态；过期表单不能覆盖管理员的后续操作。
func (service *Service) SetEnabled(ctx context.Context, actor identity.Principal, requestID, kind, id string, expectedRevision int64, isEnabled bool) error {
	if err := service.authorize(ctx, actor, requestID); err != nil {
		return err
	}
	tables := map[string]string{"provider": "model_providers", "model": "models", "policy": "model_policies"}
	table, exists := tables[kind]
	if !exists || !validID(id) || expectedRevision < 1 {
		return ErrInvalidInput
	}
	return service.transaction(ctx, func(transaction *sql.Tx) error {
		var revision int64
		// 表名只取本地常量映射，所有外部值仍使用绑定参数。
		err := transaction.QueryRowContext(ctx, `UPDATE tombot.`+table+` SET is_enabled=$3,revision=revision+1 WHERE id=$1 AND revision=$2 RETURNING revision`, id, expectedRevision, isEnabled).Scan(&revision)
		if err == sql.ErrNoRows {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		return audit(ctx, transaction, actor, requestID, "修改模型配置启用状态", id, map[string]any{"kind": kind, "is_enabled": isEnabled, "revision": revision})
	})
}

// DeleteProvider 删除已停用的供应商及其模型与凭据；仍被策略版本引用时明确拒绝。
// 删除会连带清理目录行，因此要求管理员先停用，避免正在使用的供应商被一次性移除。
func (service *Service) DeleteProvider(ctx context.Context, actor identity.Principal, requestID, providerID string, expectedRevision int64) error {
	if err := service.authorize(ctx, actor, requestID); err != nil {
		return err
	}
	if !validID(providerID) || expectedRevision < 1 {
		return ErrInvalidInput
	}
	return service.transaction(ctx, func(transaction *sql.Tx) error {
		var revision int64
		var isEnabled bool
		err := transaction.QueryRowContext(ctx, `SELECT revision,is_enabled FROM tombot.model_providers WHERE id=$1 FOR UPDATE`, providerID).Scan(&revision, &isEnabled)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if revision != expectedRevision {
			return ErrConflict
		}
		// 只有停用的供应商可以删除；仍在使用的目录不能通过一次点击消失。
		if isEnabled {
			return ErrConflict
		}
		// 触发器同样会拒绝，这里先给出明确分类，避免把约束错误当成未知故障。
		var referenced bool
		if err = transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.model_policy_versions WHERE provider_id=$1)`, providerID).Scan(&referenced); err != nil {
			return err
		}
		if referenced {
			return ErrConflict
		}
		for _, statement := range []string{
			`DELETE FROM tombot.model_credentials WHERE provider_id=$1`,
			`DELETE FROM tombot.models WHERE provider_id=$1`,
			`DELETE FROM tombot.model_providers WHERE id=$1`,
		} {
			if _, err = transaction.ExecContext(ctx, statement, providerID); err != nil {
				return err
			}
		}
		return audit(ctx, transaction, actor, requestID, "删除模型供应商", providerID, map[string]any{"revision": revision})
	})
}

func validPage(before string, limit int) bool {
	return (before == "" || validID(before)) && limit >= 1 && limit <= 100
}

// ListProviders 返回稳定 ID 游标的有界目录，不附带任何凭据材料。
func (service *Service) ListProviders(ctx context.Context, actor identity.Principal, before string, limit int) ([]Provider, error) {
	if err := service.authorizer.RequireAdmin(ctx, actor, false); err != nil {
		return nil, err
	}
	if !validPage(before, limit) {
		return nil, ErrInvalidInput
	}
	providers := []Provider{}
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		rows, err := transaction.QueryContext(ctx, `SELECT id,name,protocol,base_url,is_enabled,revision,created_at FROM tombot.model_providers WHERE ($1='' OR id<$1) ORDER BY id DESC LIMIT $2`, before, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var value Provider
			if err = rows.Scan(&value.ID, &value.Name, &value.Protocol, &value.BaseURL, &value.IsEnabled, &value.Revision, &value.CreatedAt); err != nil {
				return err
			}
			providers = append(providers, value)
		}
		return rows.Err()
	})
	return providers, err
}

// ListModels 在指定供应商下分页读取模型，不跨目录无界加载。
func (service *Service) ListModels(ctx context.Context, actor identity.Principal, providerID, before string, limit int) ([]Model, error) {
	if err := service.authorizer.RequireAdmin(ctx, actor, false); err != nil {
		return nil, err
	}
	if !validID(providerID) || !validPage(before, limit) {
		return nil, ErrInvalidInput
	}
	models := []Model{}
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		rows, err := transaction.QueryContext(ctx, `SELECT id,provider_id,model_key,display_name,context_limit,output_limit,supports_tools,supports_vision,is_enabled,revision FROM tombot.models WHERE provider_id=$1 AND ($2='' OR id<$2) ORDER BY id DESC LIMIT $3`, providerID, before, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var value Model
			if err = rows.Scan(&value.ID, &value.ProviderID, &value.ModelKey, &value.DisplayName, &value.ContextLimit, &value.OutputLimit, &value.SupportsTools, &value.SupportsVision, &value.IsEnabled, &value.Revision); err != nil {
				return err
			}
			models = append(models, value)
		}
		return rows.Err()
	})
	return models, err
}

// ListPolicies 分页读取策略当前版本与修订号，供管理页面条件更新。
func (service *Service) ListPolicies(ctx context.Context, actor identity.Principal, before string, limit int) ([]Policy, error) {
	if err := service.authorizer.RequireAdmin(ctx, actor, false); err != nil {
		return nil, err
	}
	if !validPage(before, limit) {
		return nil, ErrInvalidInput
	}
	policies := []Policy{}
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		rows, err := transaction.QueryContext(ctx, `SELECT id,name,is_default,is_enabled,revision,current_version FROM tombot.model_policies WHERE ($1='' OR id<$1) ORDER BY id DESC LIMIT $2`, before, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var value Policy
			if err = rows.Scan(&value.ID, &value.Name, &value.IsDefault, &value.IsEnabled, &value.Revision, &value.CurrentVersion); err != nil {
				return err
			}
			policies = append(policies, value)
		}
		return rows.Err()
	})
	return policies, err
}
