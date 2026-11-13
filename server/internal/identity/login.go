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

// credential 按作用域读取对应用户名；管理员与普通用户是两套表，不存在跨表同名歧义。
func (service *Service) credential(ctx context.Context, subject subject, username string) (credentialRecord, error) {
	record := credentialRecord{hash: service.dummyHash, role: subject.role}
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		err := transaction.QueryRowContext(ctx, subject.render(`SELECT a.id,a.status,a.auth_version,c.password_hash
			FROM {accounts} a JOIN {credentials} c ON c.{owner}=a.id WHERE a.username=$1`), username).
			Scan(&record.userID, &record.status, &record.version, &record.hash)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return record, err
}

// Login 验证账号、口令、设备与权限。
// 管理入口只查管理员表，桌面入口只查用户表：管理员不会出现在用户登录链路中，
// 普通用户也无法通过管理入口获得管理会话。
func (service *Service) Login(ctx context.Context, request LoginRequest, attempt Attempt) (Session, error) {
	subject, ok := subjectForScope(request.Scope)
	if !ok {
		return Session{}, ErrInvalidInput
	}
	username, err := NormalizeUsername(request.Username)
	if err != nil || len(request.Password) > 1024 || !validDevice(request.DeviceName, request.Platform, request.Scope) {
		return Session{}, ErrInvalidInput
	}
	if err = service.limitAttempt(ctx, "login:"+request.Scope+":"+username, attempt); err != nil {
		return Session{}, err
	}
	record, err := service.credential(ctx, subject, username)
	if err != nil {
		return Session{}, err
	}
	var matches bool
	err = service.passwordWork(ctx, func() error { var err error; matches, err = verifyPassword(record.hash, request.Password); return err })
	if err != nil {
		return Session{}, err
	}
	if !matches || record.userID == "" || record.status != "active" {
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
		err := transaction.QueryRowContext(ctx, subject.render(`SELECT a.status,a.auth_version,c.password_hash,transaction_timestamp()
			FROM {accounts} a JOIN {credentials} c ON c.{owner}=a.id WHERE a.id=$1 FOR UPDATE OF a`), record.userID).
			Scan(&current.status, &current.version, &current.hash, &now)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvalidCredentials
		}
		if err != nil {
			return err
		}
		if current.status != "active" || current.version != record.version || current.hash != record.hash {
			return ErrInvalidCredentials
		}
		// 管理会话必须校验动态验证；普通用户不持有该材料，也不参与管理动作。
		if request.Scope == "admin" {
			if err = service.verifyMFA(ctx, transaction, administratorSubject, record.userID, request.Code, now); errors.Is(err, ErrInvalidCredentials) {
				denied = err
				return audit(ctx, transaction, record.userID, "", "登录", "denied", attempt.RequestID)
			} else if err != nil {
				return err
			}
		}
		var devices int
		if err = transaction.QueryRowContext(ctx, subject.render(`SELECT count(*) FROM {devices} d WHERE d.{owner}=$1 AND d.revoked_at IS NULL AND EXISTS(
			SELECT 1 FROM {sessions} s WHERE s.{owner}=d.{owner} AND s.device_id=d.id AND s.revoked_at IS NULL AND s.expires_at>transaction_timestamp())`), record.userID).Scan(&devices); err != nil {
			return err
		}
		if devices >= service.options.DeviceLimit {
			return ErrDeviceLimit
		}
		session.Principal = Principal{UserID: record.userID, SessionID: ids.New(), DeviceID: ids.New(), Role: subject.role, Scope: request.Scope, AuthVersion: record.version}
		session.Principal.VerifiedAt = &now
		session.ExpiresAt = now.Add(service.options.SessionLifetime)
		if _, err = transaction.ExecContext(ctx, subject.render(`INSERT INTO {devices}({owner},id,name,platform) VALUES($1,$2,$3,$4)`), record.userID, session.Principal.DeviceID, strings.TrimSpace(request.DeviceName), request.Platform); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, subject.render(`INSERT INTO {sessions}({owner},id,device_id,auth_version,scope,expires_at,verified_at)
			VALUES($1,$2,$3,$4,$5,$6,$7)`), record.userID, session.Principal.SessionID, session.Principal.DeviceID, record.version, request.Scope, session.ExpiresAt, now); err != nil {
			return err
		}
		if err = service.issueTokens(ctx, transaction, subject, &session, now); err != nil {
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

func (service *Service) issueTokens(ctx context.Context, transaction *sql.Tx, subject subject, session *Session, now time.Time) error {
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
	_, err = transaction.ExecContext(ctx, subject.render(`INSERT INTO {tokens}({owner},id,session_id,kind,token_digest,expires_at)
		VALUES($1,$2,$3,'access',$4,$5),($1,$6,$3,'refresh',$7,$8)`), session.Principal.UserID, ids.New(), session.Principal.SessionID, digest(access), session.AccessExpiresAt, ids.New(), digest(refresh), session.ExpiresAt)
	return err
}
