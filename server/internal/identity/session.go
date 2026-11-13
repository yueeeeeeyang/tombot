package identity

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/oklog/ulid/v2"
	secure "io.yueyang/tom/internal/infra/crypto"
)

// Authenticate 每次查询账户、设备与会话状态；访问凭据过期、轮换或权限版本改变立即失效。
// 桌面作用域只查用户表，管理作用域只查管理员表，两类身份不会有交叉。
func (service *Service) Authenticate(ctx context.Context, token, scope string) (Principal, error) {
	subject, ok := subjectForScope(scope)
	if !validToken(token) || !ok {
		return Principal{}, ErrInvalidSession
	}
	var principal Principal
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		err := transaction.QueryRowContext(ctx, subject.render(`SELECT s.{owner},s.id,s.device_id,s.scope,s.auth_version,s.verified_at
			FROM {tokens} t JOIN {sessions} s ON (s.{owner},s.id)=(t.{owner},t.session_id)
			JOIN {accounts} a ON a.id=s.{owner} JOIN {devices} d ON (d.{owner},d.id)=(s.{owner},s.device_id)
			WHERE t.token_digest=$1 AND t.kind='access' AND t.consumed_at IS NULL AND t.expires_at>transaction_timestamp()
			AND s.scope=$2 AND s.revoked_at IS NULL AND s.expires_at>transaction_timestamp() AND d.revoked_at IS NULL
			AND a.status='active' AND a.auth_version=s.auth_version`), digest(token), scope).
			Scan(&principal.UserID, &principal.SessionID, &principal.DeviceID, &principal.Scope, &principal.AuthVersion, &principal.VerifiedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvalidSession
		}
		principal.Role = subject.role
		return err
	})
	if err != nil {
		return Principal{}, err
	}
	return principal, nil
}

// lockSession 总是先锁身份记录再锁设备及会话，串行化轮换、撤销、改密和管理员校验。
func lockSession(ctx context.Context, transaction *sql.Tx, principal Principal) (Session, time.Time, error) {
	var session Session
	var now time.Time
	subject, ok := subjectForScope(principal.Scope)
	if !ok {
		return session, now, ErrInvalidSession
	}
	if _, err := ulid.ParseStrict(principal.UserID); err != nil {
		return session, now, ErrInvalidSession
	}
	var accountID string
	err := transaction.QueryRowContext(ctx, subject.render(`SELECT id FROM {accounts} WHERE id=$1 AND status='active' FOR UPDATE`), principal.UserID).Scan(&accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return session, now, ErrInvalidSession
	}
	if err != nil {
		return session, now, err
	}
	err = transaction.QueryRowContext(ctx, subject.render(`SELECT s.{owner},s.id,s.device_id,s.scope,s.auth_version,s.verified_at,s.expires_at,transaction_timestamp()
		FROM {sessions} s JOIN {accounts} a ON a.id=s.{owner} JOIN {devices} d ON (d.{owner},d.id)=(s.{owner},s.device_id)
		WHERE s.{owner}=$1 AND s.id=$2 AND s.device_id=$3 AND s.scope=$4
		AND s.revoked_at IS NULL AND s.expires_at>transaction_timestamp() AND d.revoked_at IS NULL
		AND a.status='active' AND a.auth_version=s.auth_version FOR UPDATE OF s,d`), principal.UserID, principal.SessionID, principal.DeviceID, principal.Scope).
		Scan(&session.Principal.UserID, &session.Principal.SessionID, &session.Principal.DeviceID, &session.Principal.Scope, &session.Principal.AuthVersion, &session.Principal.VerifiedAt, &session.ExpiresAt, &now)
	if errors.Is(err, sql.ErrNoRows) {
		return session, now, ErrInvalidSession
	}
	session.Principal.Role = subject.role
	return session, now, err
}

// Refresh 原子轮换同一会话的凭据。已消费的续期值再次出现时提交撤销，再向调用方返回拒绝。
// 客户端必须串行续期；响应丢失后重新登录，不能重试旧续期值。
func (service *Service) Refresh(ctx context.Context, token, scope string, attempt Attempt) (Session, error) {
	subject, ok := subjectForScope(scope)
	if !validToken(token) || !ok {
		return Session{}, ErrInvalidSession
	}
	if err := service.limitAttempt(ctx, "refresh:"+scope+":"+secure.Hash(token), attempt); err != nil {
		return Session{}, err
	}
	var session Session
	var denied bool
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		var principal Principal
		err := transaction.QueryRowContext(ctx, subject.render(`SELECT s.{owner},s.id,s.device_id,s.scope FROM {tokens} t
			JOIN {sessions} s ON (s.{owner},s.id)=(t.{owner},t.session_id) WHERE t.token_digest=$1 AND t.kind='refresh' AND s.scope=$2`), digest(token), scope).
			Scan(&principal.UserID, &principal.SessionID, &principal.DeviceID, &principal.Scope)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvalidSession
		}
		if err != nil {
			return err
		}
		var now time.Time
		session, now, err = lockSession(ctx, transaction, principal)
		if err != nil {
			return err
		}
		var consumed sql.NullTime
		var expires time.Time
		err = transaction.QueryRowContext(ctx, subject.render(`SELECT consumed_at,expires_at FROM {tokens} WHERE token_digest=$1 AND kind='refresh' FOR UPDATE`), digest(token)).Scan(&consumed, &expires)
		if err != nil {
			return err
		}
		if consumed.Valid {
			if err = revokeSession(ctx, transaction, subject, principal); err != nil {
				return err
			}
			denied = true
			return audit(ctx, transaction, principal.UserID, principal.SessionID, "续期重放撤销", "denied", attempt.RequestID)
		}
		if !expires.After(now) {
			return ErrInvalidSession
		}
		if _, err = transaction.ExecContext(ctx, subject.render(`UPDATE {tokens} SET consumed_at=transaction_timestamp()
			WHERE {owner}=$1 AND session_id=$2 AND consumed_at IS NULL`), principal.UserID, principal.SessionID); err != nil {
			return err
		}
		if err = service.issueTokens(ctx, transaction, subject, &session, now); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, subject.render(`UPDATE {devices} SET last_seen_at=transaction_timestamp() WHERE {owner}=$1 AND id=$2`), principal.UserID, principal.DeviceID); err != nil {
			return err
		}
		return audit(ctx, transaction, principal.UserID, principal.SessionID, "续期会话", "succeeded", attempt.RequestID)
	})
	if err != nil {
		return Session{}, err
	}
	if denied {
		return Session{}, ErrInvalidSession
	}
	return session, nil
}

// Revalidate 供长连接和敏感操作重新确认身份；调用方应同时监听撤销事件并周期复核。
func (service *Service) Revalidate(ctx context.Context, principal Principal) (Principal, error) {
	var session Session
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		var err error
		session, _, err = lockSession(ctx, transaction, principal)
		return err
	})
	if err != nil {
		return Principal{}, err
	}
	return session.Principal, nil
}

// RequireAdmin 使用数据库内的当前会话状态，拒绝非管理会话或过期二次校验。
// 管理员身份不再来自用户表的角色列：管理作用域本身就意味着管理员。
func (service *Service) RequireAdmin(ctx context.Context, principal Principal, requireRecent bool) error {
	if principal.Scope != "admin" {
		return ErrForbidden
	}
	return service.transaction(ctx, func(transaction *sql.Tx) error {
		session, now, err := lockSession(ctx, transaction, principal)
		if err != nil {
			return err
		}
		if session.Principal.Scope != "admin" {
			return ErrForbidden
		}
		verified := session.Principal.VerifiedAt
		if requireRecent && (verified == nil || verified.After(now) || now.Sub(*verified) > service.options.VerificationLifetime) {
			// 与「无权」区分：前端据此弹出再验证并自动继续原操作，而不是把它当成权限拒绝。
			return ErrVerificationRequired
		}
		return nil
	})
}

func revokeSession(ctx context.Context, transaction *sql.Tx, subject subject, principal Principal) error {
	if _, err := transaction.ExecContext(ctx, subject.render(`UPDATE {sessions} SET revoked_at=transaction_timestamp() WHERE {owner}=$1 AND id=$2 AND revoked_at IS NULL`), principal.UserID, principal.SessionID); err != nil {
		return err
	}
	_, err := transaction.ExecContext(ctx, subject.render(`UPDATE {devices} SET revoked_at=transaction_timestamp() WHERE {owner}=$1 AND id=$2 AND revoked_at IS NULL`), principal.UserID, principal.DeviceID)
	return err
}

// Logout 撤销当前会话及其设备，不改变已经提交的机器人后台任务。
func (service *Service) Logout(ctx context.Context, principal Principal, requestID string) error {
	subject, ok := subjectForScope(principal.Scope)
	if !validRequestID(requestID) || !ok {
		return ErrInvalidInput
	}
	return service.transaction(ctx, func(transaction *sql.Tx) error {
		session, _, err := lockSession(ctx, transaction, principal)
		if err != nil {
			return err
		}
		if err = revokeSession(ctx, transaction, subject, session.Principal); err != nil {
			return err
		}
		return audit(ctx, transaction, principal.UserID, principal.SessionID, "退出登录", "succeeded", requestID)
	})
}

// Device 为可展示元数据，不含任何可用于认证的摘要或凭据。
type Device struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Platform   string    `json:"platform"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	IsCurrent  bool      `json:"is_current"`
}

// ListDevices 只读取当前身份仍有效的设备，使用稳定标识游标与有界分页。
func (service *Service) ListDevices(ctx context.Context, principal Principal, before string, limit int) ([]Device, error) {
	subject, ok := subjectForScope(principal.Scope)
	if !ok {
		return nil, ErrInvalidInput
	}
	if limit < 1 || limit > 100 {
		return nil, ErrInvalidInput
	}
	if before != "" {
		if _, err := ulid.ParseStrict(before); err != nil {
			return nil, ErrInvalidInput
		}
	}
	devices := []Device{}
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		if _, _, err := lockSession(ctx, transaction, principal); err != nil {
			return err
		}
		rows, err := transaction.QueryContext(ctx, subject.render(`SELECT d.id,d.name,d.platform,d.created_at,d.last_seen_at FROM {devices} d
			WHERE d.{owner}=$1 AND d.revoked_at IS NULL AND ($2='' OR d.id<$2) AND EXISTS(
			SELECT 1 FROM {sessions} s WHERE s.{owner}=d.{owner} AND s.device_id=d.id AND s.revoked_at IS NULL AND s.expires_at>transaction_timestamp())
			ORDER BY d.id DESC LIMIT $3`), principal.UserID, before, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var device Device
			if err = rows.Scan(&device.ID, &device.Name, &device.Platform, &device.CreatedAt, &device.LastSeenAt); err != nil {
				return err
			}
			device.IsCurrent = device.ID == principal.DeviceID
			devices = append(devices, device)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return devices, nil
}

// RevokeDevice 只能撤销当前身份的设备；跨身份标识与不存在标识使用相同拒绝结果。
func (service *Service) RevokeDevice(ctx context.Context, principal Principal, deviceID, requestID string) error {
	subject, ok := subjectForScope(principal.Scope)
	if _, err := ulid.ParseStrict(deviceID); err != nil || !validRequestID(requestID) || !ok {
		return ErrInvalidInput
	}
	return service.transaction(ctx, func(transaction *sql.Tx) error {
		if _, _, err := lockSession(ctx, transaction, principal); err != nil {
			return err
		}
		var id string
		err := transaction.QueryRowContext(ctx, subject.render(`UPDATE {devices} SET revoked_at=coalesce(revoked_at,transaction_timestamp()) WHERE {owner}=$1 AND id=$2 RETURNING id`), principal.UserID, deviceID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrForbidden
		}
		if err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, subject.render(`UPDATE {sessions} SET revoked_at=transaction_timestamp() WHERE {owner}=$1 AND device_id=$2 AND revoked_at IS NULL`), principal.UserID, deviceID); err != nil {
			return err
		}
		return audit(ctx, transaction, principal.UserID, principal.SessionID, "撤销设备", "succeeded", requestID)
	})
}
