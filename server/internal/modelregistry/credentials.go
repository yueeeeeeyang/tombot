package modelregistry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	"io.yueyang/tom/internal/identity"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
)

// Keyring 仅接收部署注入的主密钥集合；新密文使用当前版本，旧版本仍可解密在途引用。
type Keyring struct {
	active string
	keys   map[string][]byte
}

// NewKeyring 复制主密钥，拒绝缺失当前版本、非 256 位密钥或无界集合；不生成替代密钥。
func NewKeyring(active string, keys map[string][]byte) (*Keyring, error) {
	if len(keys) == 0 || len(keys) > 16 || len(keys[active]) != 32 {
		return nil, ErrCredential
	}
	result := &Keyring{active: active, keys: make(map[string][]byte, len(keys))}
	pattern := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,119}$`)
	for reference, key := range keys {
		if !pattern.MatchString(reference) || len(key) != 32 {
			return nil, ErrCredential
		}
		result.keys[reference] = append([]byte(nil), key...)
	}
	return result, nil
}

// String 与 GoString 防止结构化调试格式意外打印主密钥。
func (*Keyring) String() string        { return "模型主密钥集合（已隐藏）" }
func (keys *Keyring) GoString() string { return keys.String() }

func credentialBinding(providerID, credentialID, keyReference string, version int64) string {
	return fmt.Sprintf("tombot.model.credential/v1/%s/%s/%d/%s", providerID, credentialID, version, keyReference)
}

// validCredential 只接受可打印 ASCII：供应商令牌都是这种形态，控制字符不应进入密文。
func validCredential(plaintext []byte) bool {
	if len(plaintext) < 1 || len(plaintext) > 8192 {
		return false
	}
	for _, value := range plaintext {
		if value < 33 || value > 126 {
			return false
		}
	}
	return true
}

// insertCredential 在调用方事务中写入一个凭据版本；调用方负责清理明文。
// 供应商登记与后续新增凭据共用这一实现，避免两处加密或版本计算出现差异。
func insertCredential(ctx context.Context, transaction *sql.Tx, keys *Keyring, actor identity.Principal, requestID, providerID string, plaintext []byte) (Credential, error) {
	credential := Credential{ID: ids.New(), ProviderID: providerID}
	if err := transaction.QueryRowContext(ctx, `SELECT coalesce(max(version),0)+1 FROM tombot.model_credentials WHERE provider_id=$1`, providerID).Scan(&credential.Version); err != nil {
		return Credential{}, err
	}
	keyReference := keys.active
	ciphertext, nonce, err := secure.Seal(keys.keys[keyReference], plaintext, credentialBinding(providerID, credential.ID, keyReference, credential.Version))
	if err != nil {
		return Credential{}, ErrCredential
	}
	if err = transaction.QueryRowContext(ctx, `INSERT INTO tombot.model_credentials(id,provider_id,version,key_reference,nonce,ciphertext)
		VALUES($1,$2,$3,$4,$5,$6) RETURNING created_at`, credential.ID, providerID, credential.Version, keyReference, nonce, ciphertext).Scan(&credential.CreatedAt); err != nil {
		return Credential{}, err
	}
	if err = audit(ctx, transaction, actor, requestID, "新增模型凭据版本", credential.ID, map[string]any{"provider_id": providerID, "version": credential.Version}); err != nil {
		return Credential{}, err
	}
	return credential, nil
}

// AddCredential 创建独立凭据版本；调用方负责清理输入，返回值不包含凭据或可还原材料。
// 凭据版本管理已从管理平台移除，改由登记供应商时一并设置；该入口保留给部署脚本与测试。
func (service *Service) AddCredential(ctx context.Context, actor identity.Principal, requestID, providerID string, plaintext []byte) (Credential, error) {
	if err := service.authorize(ctx, actor, requestID); err != nil {
		return Credential{}, err
	}
	if !validID(providerID) || !validCredential(plaintext) {
		return Credential{}, ErrInvalidInput
	}
	var credential Credential
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		if err := requireRow(transaction.QueryRowContext(ctx, `SELECT id FROM tombot.model_providers WHERE id=$1 FOR UPDATE`, providerID)); err != nil {
			return err
		}
		var err error
		credential, err = insertCredential(ctx, transaction, service.keys, actor, requestID, providerID, plaintext)
		return err
	})
	if err != nil {
		return Credential{}, err
	}
	return credential, nil
}

// RevokeCredential 幂等撤销单个版本，后续发布和使用均拒绝该版本，不自动切换到其他凭据。
func (service *Service) RevokeCredential(ctx context.Context, actor identity.Principal, requestID, credentialID string) error {
	if err := service.authorize(ctx, actor, requestID); err != nil {
		return err
	}
	if !validID(credentialID) {
		return ErrInvalidInput
	}
	return service.transaction(ctx, func(transaction *sql.Tx) error {
		var revoked bool
		err := transaction.QueryRowContext(ctx, `SELECT revoked_at IS NOT NULL FROM tombot.model_credentials WHERE id=$1 FOR UPDATE`, credentialID).Scan(&revoked)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if revoked {
			return nil
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.model_credentials SET revoked_at=transaction_timestamp() WHERE id=$1`, credentialID); err != nil {
			return err
		}
		return audit(ctx, transaction, actor, requestID, "撤销模型凭据版本", credentialID, struct{}{})
	})
}

// DestroyCredential 仅销毁已撤销且所有策略引用都已撤销的密文；历史标识和版本仍保留。
// 该动作不可由应用回滚撤销，历史备份中的材料需按部署保留政策另外处理。
func (service *Service) DestroyCredential(ctx context.Context, actor identity.Principal, requestID, credentialID string) error {
	if err := service.authorize(ctx, actor, requestID); err != nil {
		return err
	}
	if !validID(credentialID) {
		return ErrInvalidInput
	}
	return service.transaction(ctx, func(transaction *sql.Tx) error {
		var revoked, destroyed bool
		err := transaction.QueryRowContext(ctx, `SELECT revoked_at IS NOT NULL,destroyed_at IS NOT NULL FROM tombot.model_credentials WHERE id=$1 FOR UPDATE`, credentialID).Scan(&revoked, &destroyed)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if destroyed {
			return nil
		}
		if !revoked {
			return ErrConflict
		}
		var referenced bool
		if err = transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.model_policy_versions WHERE credential_id=$1 AND status<>'revoked')`, credentialID).Scan(&referenced); err != nil {
			return err
		}
		if referenced {
			return ErrConflict
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.model_credentials SET destroyed_at=transaction_timestamp(),nonce=NULL,ciphertext=NULL WHERE id=$1`, credentialID); err != nil {
			return err
		}
		return audit(ctx, transaction, actor, requestID, "销毁已撤销模型凭据", credentialID, struct{}{})
	})
}

func (service *Service) decrypt(ctx context.Context, transaction *sql.Tx, version Version) ([]byte, error) {
	if version.CredentialID == nil {
		return nil, nil
	}
	var keyReference string
	var number int64
	var nonce, ciphertext []byte
	if err := transaction.QueryRowContext(ctx, `SELECT version,key_reference,nonce,ciphertext FROM tombot.model_credentials
		WHERE provider_id=$1 AND id=$2 AND revoked_at IS NULL FOR SHARE`, version.ProviderID, *version.CredentialID).Scan(&number, &keyReference, &nonce, &ciphertext); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCredential
		}
		return nil, err
	}
	key, exists := service.keys.keys[keyReference]
	if !exists {
		return nil, ErrCredential
	}
	plaintext, err := secure.Open(key, ciphertext, nonce, credentialBinding(version.ProviderID, *version.CredentialID, keyReference, number))
	if err != nil {
		return nil, ErrCredential
	}
	return plaintext, nil
}

// ListCredentials 按供应商分页读取安全元数据；缺少当前管理员身份时拒绝读取。
func (service *Service) ListCredentials(ctx context.Context, actor identity.Principal, providerID string, before int64, limit int) ([]Credential, error) {
	if err := service.authorizer.RequireAdmin(ctx, actor, false); err != nil {
		return nil, err
	}
	if !validID(providerID) || before < 0 || limit < 1 || limit > 100 {
		return nil, ErrInvalidInput
	}
	credentials := []Credential{}
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		rows, err := transaction.QueryContext(ctx, `SELECT id,provider_id,version,revoked_at IS NOT NULL,destroyed_at IS NOT NULL,created_at FROM tombot.model_credentials
			WHERE provider_id=$1 AND ($2::bigint=0 OR version<$2) ORDER BY version DESC LIMIT $3`, providerID, before, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var value Credential
			if err = rows.Scan(&value.ID, &value.ProviderID, &value.Version, &value.IsRevoked, &value.IsDestroyed, &value.CreatedAt); err != nil {
				return err
			}
			credentials = append(credentials, value)
		}
		return rows.Err()
	})
	return credentials, err
}
