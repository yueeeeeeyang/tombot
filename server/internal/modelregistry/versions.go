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
	version                                         Version
	policyRevision, providerRevision, modelRevision int64
	currentVersion                                  *int64
	isAvailable                                     bool
}

func lockPolicy(ctx context.Context, transaction *sql.Tx, policyID string) error {
	return requireRow(transaction.QueryRowContext(ctx, `SELECT id FROM tombot.model_policies WHERE id=$1 FOR UPDATE`, policyID))
}

func readVersion(ctx context.Context, transaction *sql.Tx, policyID string, number int64, lock bool) (versionState, error) {
	var state versionState
	var encoded []byte
	var canonical Configuration
	query := `SELECT v.policy_id,v.version,v.provider_id,v.model_id,v.credential_id,v.configuration,v.status,v.published_at,
		policy.revision,p.revision,m.revision,policy.current_version,
		policy.is_enabled AND p.is_enabled AND m.is_enabled AND (v.credential_id IS NULL OR c.revoked_at IS NULL),
		p.protocol,p.base_url,m.model_key,m.context_limit,m.output_limit,m.supports_tools,m.supports_vision
		FROM tombot.model_policy_versions v JOIN tombot.model_policies policy ON policy.id=v.policy_id
		JOIN tombot.model_providers p ON p.id=v.provider_id JOIN tombot.models m ON m.provider_id=v.provider_id AND m.id=v.model_id
		LEFT JOIN tombot.model_credentials c ON c.provider_id=v.provider_id AND c.id=v.credential_id
		WHERE v.policy_id=$1 AND v.version=$2`
	if lock {
		query += ` FOR SHARE OF policy,p,m,v`
	}
	v := &state.version
	err := transaction.QueryRowContext(ctx, query, policyID, number).Scan(&v.PolicyID, &v.Number, &v.ProviderID, &v.ModelID, &v.CredentialID, &encoded, &v.Status, &v.PublishedAt,
		&state.policyRevision, &state.providerRevision, &state.modelRevision, &state.currentVersion, &state.isAvailable,
		&canonical.Protocol, &canonical.BaseURL, &canonical.ModelKey, &canonical.ContextLimit, &canonical.OutputLimit, &canonical.SupportsTools, &canonical.SupportsVision)
	if errors.Is(err, sql.ErrNoRows) {
		return state, ErrNotFound
	}
	if err != nil {
		return state, err
	}
	v.Configuration, err = decodeConfiguration(encoded)
	if err != nil {
		return state, err
	}
	configuration := v.Configuration
	if configuration.Protocol != canonical.Protocol || configuration.BaseURL != canonical.BaseURL || configuration.ModelKey != canonical.ModelKey ||
		configuration.ContextLimit != canonical.ContextLimit || configuration.OutputLimit > canonical.OutputLimit ||
		configuration.SupportsTools != canonical.SupportsTools || configuration.SupportsVision != canonical.SupportsVision {
		return state, ErrUnavailable
	}
	return state, nil
}

func decodeConfiguration(encoded []byte) (Configuration, error) {
	var configuration Configuration
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if len(encoded) > 16<<10 || decoder.Decode(&configuration) != nil {
		return Configuration{}, ErrUnavailable
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || !configuration.valid() {
		return Configuration{}, ErrUnavailable
	}
	return configuration, nil
}

// ListVersions 用单次有界查询读取草稿与历史，不为每行额外访问目录或凭据。
func (service *Service) ListVersions(ctx context.Context, actor identity.Principal, policyID string, before int64, limit int) ([]Version, error) {
	if err := service.authorizer.RequireAdmin(ctx, actor, false); err != nil {
		return nil, err
	}
	if !validID(policyID) || before < 0 || limit < 1 || limit > 100 {
		return nil, ErrInvalidInput
	}
	versions := []Version{}
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		rows, err := transaction.QueryContext(ctx, `SELECT policy_id,version,provider_id,model_id,credential_id,configuration,status,published_at FROM tombot.model_policy_versions
			WHERE policy_id=$1 AND ($2::bigint=0 OR version<$2) ORDER BY version DESC LIMIT $3`, policyID, before, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var version Version
			var encoded []byte
			if err = rows.Scan(&version.PolicyID, &version.Number, &version.ProviderID, &version.ModelID, &version.CredentialID, &encoded, &version.Status, &version.PublishedAt); err != nil {
				return err
			}
			version.Configuration, err = decodeConfiguration(encoded)
			if err != nil {
				return err
			}
			versions = append(versions, version)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return versions, nil
}

// CreateDraft 从受管目录生成完整快照，串行分配递增版本；不接受任意客户端模型参数。
func (service *Service) CreateDraft(ctx context.Context, actor identity.Principal, requestID string, request DraftRequest) (Version, error) {
	if err := service.authorize(ctx, actor, requestID); err != nil {
		return Version{}, err
	}
	if !validID(request.PolicyID) || !validID(request.ModelID) || (request.CredentialID != nil && !validID(*request.CredentialID)) {
		return Version{}, ErrInvalidInput
	}
	version := Version{PolicyID: request.PolicyID, ModelID: request.ModelID, CredentialID: request.CredentialID, Status: "draft"}
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		if err := lockPolicy(ctx, transaction, request.PolicyID); err != nil {
			return err
		}
		configuration := Configuration{Prices: request.Prices}
		maximumOutput := 0
		err := transaction.QueryRowContext(ctx, `SELECT p.id,p.protocol,p.base_url,m.model_key,m.context_limit,m.output_limit,m.supports_tools,m.supports_vision
			FROM tombot.models m JOIN tombot.model_providers p ON p.id=m.provider_id WHERE m.id=$1 FOR SHARE OF p,m`, request.ModelID).
			Scan(&version.ProviderID, &configuration.Protocol, &configuration.BaseURL, &configuration.ModelKey, &configuration.ContextLimit, &maximumOutput, &configuration.SupportsTools, &configuration.SupportsVision)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		configuration.OutputLimit = request.OutputLimit
		if !configuration.valid() || configuration.OutputLimit > maximumOutput {
			return ErrInvalidInput
		}
		version.Configuration = configuration
		if request.CredentialID != nil {
			if err = requireRow(transaction.QueryRowContext(ctx, `SELECT id FROM tombot.model_credentials WHERE id=$1 AND provider_id=$2 AND revoked_at IS NULL FOR SHARE`, *request.CredentialID, version.ProviderID)); err != nil {
				return err
			}
		}
		if err = transaction.QueryRowContext(ctx, `SELECT coalesce(max(version),0)+1 FROM tombot.model_policy_versions WHERE policy_id=$1`, request.PolicyID).Scan(&version.Number); err != nil {
			return err
		}
		encoded, err := json.Marshal(configuration)
		if err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.model_policy_versions(policy_id,version,provider_id,model_id,credential_id,configuration,status) VALUES($1,$2,$3,$4,$5,$6,'draft')`, version.PolicyID, version.Number, version.ProviderID, version.ModelID, version.CredentialID, encoded); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.model_policies SET revision=revision+1 WHERE id=$1`, version.PolicyID); err != nil {
			return err
		}
		return audit(ctx, transaction, actor, requestID, "新增模型策略草稿", version.PolicyID, map[string]any{"version": version.Number})
	})
	if err != nil {
		return Version{}, err
	}
	return version, nil
}

// Publish 在事务外验证声明能力，再核对所有修订并原子发布。回包丢失后重查同版本不会重复验证。
func (service *Service) Publish(ctx context.Context, actor identity.Principal, requestID, policyID string, number, expectedRevision int64) (Version, error) {
	if err := service.authorize(ctx, actor, requestID); err != nil {
		return Version{}, err
	}
	if !validID(policyID) || number < 1 || expectedRevision < 1 {
		return Version{}, ErrInvalidInput
	}
	var initial versionState
	var plaintext []byte
	defer func() { clear(plaintext) }()
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		var err error
		initial, err = readVersion(ctx, transaction, policyID, number, false)
		if err != nil {
			return err
		}
		if initial.version.Status == "published" {
			return nil
		}
		if !initial.isAvailable || initial.version.Status != "draft" {
			return ErrUnavailable
		}
		if initial.policyRevision != expectedRevision || (initial.currentVersion != nil && number <= *initial.currentVersion) {
			return ErrConflict
		}
		plaintext, err = service.decrypt(ctx, transaction, initial.version)
		return err
	})
	if err != nil {
		return Version{}, err
	}
	if initial.version.Status == "published" {
		return initial.version, nil
	}
	verificationContext, cancel := context.WithTimeout(ctx, service.options.VerificationTimeout)
	err = service.verifier.Verify(verificationContext, initial.version.Configuration, plaintext)
	verificationErr := verificationContext.Err()
	cancel()
	clear(plaintext)
	plaintext = nil
	if err != nil || verificationErr != nil {
		if ctx.Err() != nil {
			return Version{}, ctx.Err()
		}
		// 上游错误可能含请求头和响应正文，只持久化固定失败分类。
		if recordErr := service.transaction(ctx, func(transaction *sql.Tx) error {
			_, writeErr := transaction.ExecContext(ctx, `INSERT INTO tombot.admin_audit_events(id,actor_id,action,target_kind,target_id,request_id,result,changes)
				VALUES($1,$2,'模型策略能力验证失败','model_configuration',$3,$4,'failed',jsonb_build_object('version',$5::bigint))`, ids.New(), actor.UserID, policyID, requestID, number)
			return writeErr
		}); recordErr != nil {
			return Version{}, recordErr
		}
		return Version{}, ErrVerification
	}
	if err = service.authorize(ctx, actor, requestID); err != nil {
		return Version{}, err
	}
	var published Version
	err = service.transaction(ctx, func(transaction *sql.Tx) error {
		if err := lockPolicy(ctx, transaction, policyID); err != nil {
			return err
		}
		current, err := readVersion(ctx, transaction, policyID, number, true)
		if err != nil {
			return err
		}
		if current.version.Status == "published" {
			published = current.version
			return nil
		}
		if !current.isAvailable || current.version.Status != "draft" {
			return ErrUnavailable
		}
		if current.policyRevision != initial.policyRevision || current.providerRevision != initial.providerRevision || current.modelRevision != initial.modelRevision {
			return ErrConflict
		}
		// 再次锁定并认证凭据，防止探测期间撤销后仍把策略发布为可用。
		check, err := service.decrypt(ctx, transaction, current.version)
		clear(check)
		if err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.model_policy_versions SET status='published' WHERE policy_id=$1 AND version=$2`, policyID, number); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.model_policies SET current_version=$2,revision=revision+1 WHERE id=$1`, policyID, number); err != nil {
			return err
		}
		state, err := readVersion(ctx, transaction, policyID, number, false)
		if err != nil {
			return err
		}
		published = state.version
		return audit(ctx, transaction, actor, requestID, "发布模型策略版本", policyID, map[string]any{"version": number})
	})
	if err != nil {
		return Version{}, err
	}
	return published, nil
}

// GetVersion 返回指定版本的管理视图，不向普通用户提供供应商地址与凭据标识。
func (service *Service) GetVersion(ctx context.Context, actor identity.Principal, policyID string, number int64) (Version, error) {
	if err := service.authorizer.RequireAdmin(ctx, actor, false); err != nil {
		return Version{}, err
	}
	if !validID(policyID) || number < 1 {
		return Version{}, ErrInvalidInput
	}
	var version Version
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		state, err := readVersion(ctx, transaction, policyID, number, false)
		version = state.version
		return err
	})
	if err != nil {
		return Version{}, err
	}
	return version, nil
}

// RevokeVersion 保留历史配置并使后续使用失败；不会偷偷回退到旧版本或平台默认策略。
func (service *Service) RevokeVersion(ctx context.Context, actor identity.Principal, requestID, policyID string, number int64) error {
	if err := service.authorize(ctx, actor, requestID); err != nil {
		return err
	}
	if !validID(policyID) || number < 1 {
		return ErrInvalidInput
	}
	return service.transaction(ctx, func(transaction *sql.Tx) error {
		if err := lockPolicy(ctx, transaction, policyID); err != nil {
			return err
		}
		state, err := readVersion(ctx, transaction, policyID, number, false)
		if err != nil {
			return err
		}
		if state.version.Status == "revoked" {
			return nil
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.model_policy_versions SET status='revoked' WHERE policy_id=$1 AND version=$2`, policyID, number); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.model_policies SET revision=revision+1 WHERE id=$1`, policyID); err != nil {
			return err
		}
		return audit(ctx, transaction, actor, requestID, "撤销模型策略版本", policyID, map[string]any{"version": number})
	})
}
