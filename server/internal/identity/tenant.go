package identity

import (
	"context"
	"database/sql"
	"errors"

	"github.com/oklog/ulid/v2"
)

// CheckTenantSession 在业务租户事务的读取点复核桌面身份，供各用户领域统一调用。
// 身份须先由认证入口解析；此方法不接受或替代访问凭据验证，也不赋予管理会话内容读取权。
func CheckTenantSession(ctx context.Context, transaction *sql.Tx, principal Principal) error {
	if transaction == nil || principal.Scope != "desktop" || principal.AuthVersion < 1 {
		return ErrInvalidSession
	}
	for _, id := range []string{principal.UserID, principal.SessionID, principal.DeviceID} {
		if _, err := ulid.ParseStrict(id); err != nil {
			return ErrInvalidSession
		}
	}
	var valid bool
	err := transaction.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM tombot.users u JOIN tombot.user_sessions s ON s.user_id=u.id
		JOIN tombot.devices d ON (d.user_id,d.id)=(s.user_id,s.device_id)
		WHERE u.id=$1 AND s.id=$2 AND d.id=$3 AND s.scope='desktop'
		AND u.status='active' AND u.auth_version=$4 AND s.auth_version=u.auth_version
		AND s.revoked_at IS NULL AND d.revoked_at IS NULL AND s.expires_at>transaction_timestamp())`,
		principal.UserID, principal.SessionID, principal.DeviceID, principal.AuthVersion).Scan(&valid)
	if err != nil {
		return errors.New("复核业务会话失败")
	}
	if !valid {
		return ErrInvalidSession
	}
	return nil
}
