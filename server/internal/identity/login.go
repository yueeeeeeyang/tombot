package identity

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
)

type credentialRecord struct {
	userID  string
	role    string
	status  string
	version int64
	hash    string
}

func (service *Service) credential(ctx context.Context, username string) (credentialRecord, error) {
	record := credentialRecord{hash: service.dummyHash}
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		err := transaction.QueryRowContext(ctx, `SELECT u.id,u.role,u.status,u.auth_version,c.password_hash FROM tombot.users u
			JOIN tombot.user_credentials c ON c.user_id=u.id WHERE u.username=$1`, username).Scan(&record.userID, &record.role, &record.status, &record.version, &record.hash)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return record, err
}

// Login 验证账号、口令、设备与权限；管理员必须校验动态验证信息，普通用户不能签发管理会话。
func (service *Service) Login(ctx context.Context, request LoginRequest, attempt Attempt) (Session, error) {
	username, err := NormalizeUsername(request.Username)
	if err != nil || len(request.Password) > 1024 || !validDevice(request.DeviceName, request.Platform, request.Scope) {
		return Session{}, ErrInvalidInput
	}
	if err = service.limitAttempt(ctx, "login:"+username, attempt); err != nil {
		return Session{}, err
	}
	record, err := service.credential(ctx, username)
	if err != nil {
		return Session{}, err
	}
	var matches bool
	err = service.passwordWork(ctx, func() error { var err error; matches, err = verifyPassword(record.hash, request.Password); return err })
	if err != nil {
		return Session{}, err
	}
	if !matches || record.userID == "" || record.status != "active" || request.Scope == "admin" && record.role != "admin" {
		err = service.transaction(ctx, func(transaction *sql.Tx) error {
			return audit(ctx, transaction, record.userID, "", "登录", "denied", attempt.RequestID)
		})
		if err != nil {
			return Session{}, err
		}
		return Session{}, ErrInvalidCredentials
	}
	var session Session
	var denied error
	err = service.transaction(ctx, func(transaction *sql.Tx) error {
		var current credentialRecord
		var now time.Time
		err := transaction.QueryRowContext(ctx, `SELECT u.role,u.status,u.auth_version,c.password_hash,transaction_timestamp()
			FROM tombot.users u JOIN tombot.user_credentials c ON c.user_id=u.id WHERE u.id=$1 FOR UPDATE OF u`, record.userID).
			Scan(&current.role, &current.status, &current.version, &current.hash, &now)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvalidCredentials
		}
		if err != nil {
			return err
		}
		if current.status != "active" || current.version != record.version || current.hash != record.hash || current.role != record.role {
			return ErrInvalidCredentials
		}
		if current.role == "admin" {
			if err = service.verifyMFA(ctx, transaction, record.userID, request.Code, now); errors.Is(err, ErrInvalidCredentials) {
				denied = err
				return audit(ctx, transaction, record.userID, "", "登录", "denied", attempt.RequestID)
			} else if err != nil {
				return err
			}
		}
		var devices int
		if err = transaction.QueryRowContext(ctx, `SELECT count(*) FROM tombot.devices d WHERE user_id=$1 AND revoked_at IS NULL AND EXISTS(
			SELECT 1 FROM tombot.user_sessions s WHERE s.user_id=d.user_id AND s.device_id=d.id AND s.revoked_at IS NULL AND s.expires_at>transaction_timestamp())`, record.userID).Scan(&devices); err != nil {
			return err
		}
		if devices >= service.options.DeviceLimit {
			return ErrDeviceLimit
		}
		session.Principal = Principal{UserID: record.userID, SessionID: ids.New(), DeviceID: ids.New(), Role: record.role, Scope: request.Scope, AuthVersion: record.version}
		session.Principal.VerifiedAt = &now
		session.ExpiresAt = now.Add(service.options.SessionLifetime)
		if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.devices(user_id,id,name,platform) VALUES($1,$2,$3,$4)`, record.userID, session.Principal.DeviceID, strings.TrimSpace(request.DeviceName), request.Platform); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.user_sessions(user_id,id,device_id,auth_version,scope,expires_at,verified_at)
			VALUES($1,$2,$3,$4,$5,$6,$7)`, record.userID, session.Principal.SessionID, session.Principal.DeviceID, record.version, request.Scope, session.ExpiresAt, now); err != nil {
			return err
		}
		if err = service.issueTokens(ctx, transaction, &session, now); err != nil {
			return err
		}
		return audit(ctx, transaction, record.userID, session.Principal.SessionID, "登录", "succeeded", attempt.RequestID)
	})
	if err != nil {
		return Session{}, err
	}
	if denied != nil {
		return Session{}, denied
	}
	return session, nil
}

func (service *Service) issueTokens(ctx context.Context, transaction *sql.Tx, session *Session, now time.Time) error {
	access, err := secure.Token()
	if err != nil {
		return err
	}
	refresh, err := secure.Token()
	if err != nil {
		return err
	}
	session.AccessToken = Credential(access)
	session.RefreshToken = Credential(refresh)
	session.AccessExpiresAt = now.Add(service.options.AccessLifetime)
	if session.AccessExpiresAt.After(session.ExpiresAt) {
		session.AccessExpiresAt = session.ExpiresAt
	}
	_, err = transaction.ExecContext(ctx, `INSERT INTO tombot.session_tokens(user_id,id,session_id,kind,token_digest,expires_at)
		VALUES($1,$2,$3,'access',$4,$5),($1,$6,$3,'refresh',$7,$8)`, session.Principal.UserID, ids.New(), session.Principal.SessionID, digest(access), session.AccessExpiresAt, ids.New(), digest(refresh), session.ExpiresAt)
	return err
}
