package identity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"time"

	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
)

// AuthorizationLifetime 是浏览器登录从发起到达成的最长时间。
// 桌面端在这段时间内等待回调；超时后授权记录不再可用，必须重新发起。
const AuthorizationLifetime = 10 * time.Minute

var (
	ErrInvalidAuthorization = errors.New("登录授权已失效，请重新登录")
	ErrAuthorizationDenied  = errors.New("登录未获批准")
)

// BrowserAuthorizationRequest 由桌面端发起：回调地址必须是本机回环，
// code_challenge 是 PKCE 校验串的 SHA-256，state 由桌面端生成并校验。
type BrowserAuthorizationRequest struct {
	RedirectURI   string
	State         string
	CodeChallenge string
	DeviceName    string
	Platform      string
}

// Authorization 是浏览器登录的一次待完成授权。
type Authorization struct {
	ID           string
	State        string
	RedirectURI  string
	DeviceName   string
	Platform     string
	ExpiresAt    time.Time
	IsApproved   bool
	ApprovedUser string
}

// challengeDigest 只保存 PKCE 挑战的摘要；数据库泄漏不能反推校验串。
func challengeDigest(challenge string) ([]byte, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(challenge)
	if err != nil || len(raw) != 32 {
		return nil, false
	}
	sum := sha256.Sum256([]byte(challenge))
	return sum[:], true
}

// ValidateRedirectURI 只接受本机回环回调，避免授权码被引导到任意站点。
func ValidateRedirectURI(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
		return false
	}
	host := parsed.Hostname()
	if host != "127.0.0.1" && host != "[::1]" && host != "::1" && host != "localhost" {
		return false
	}
	if _, err := url.ParseRequestURI(value); err != nil {
		return false
	}
	return true
}

// BeginBrowserAuthorization 记录一次待登录请求；此时不校验任何凭据。
// 桌面端随后用系统浏览器打开授权页，用户在网页里登录。
func (service *Service) BeginBrowserAuthorization(ctx context.Context, request BrowserAuthorizationRequest) (Authorization, error) {
	if !ValidateRedirectURI(request.RedirectURI) || len(request.State) < 16 || len(request.State) > 128 || !validDevice(request.DeviceName, request.Platform, "desktop") {
		return Authorization{}, ErrInvalidInput
	}
	digest, ok := challengeDigest(request.CodeChallenge)
	if !ok {
		return Authorization{}, ErrInvalidInput
	}
	authorization := Authorization{ID: ids.New(), State: request.State, RedirectURI: request.RedirectURI, DeviceName: request.DeviceName, Platform: request.Platform}
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		var expires time.Time
		err := transaction.QueryRowContext(ctx, `INSERT INTO tombot.desktop_authorizations(id,state,redirect_uri,device_name,platform,challenge_digest,expires_at)
			VALUES($1,$2,$3,$4,$5,$6,transaction_timestamp()+$7::bigint*interval '1 millisecond') RETURNING expires_at`,
			authorization.ID, authorization.State, authorization.RedirectURI, authorization.DeviceName, authorization.Platform, digest, AuthorizationLifetime.Milliseconds()).Scan(&expires)
		authorization.ExpiresAt = expires
		return err
	})
	if err != nil {
		return Authorization{}, err
	}
	return authorization, nil
}

// ApproveBrowserAuthorization 校验账号口令与动态验证，通过后生成一次性授权码。
// 与普通登录共用口令、限流与审计路径，但不在此处签发会话凭据。
func (service *Service) ApproveBrowserAuthorization(ctx context.Context, authorizationID, username, password, code string, attempt Attempt) (string, error) {
	normalized, err := NormalizeUsername(username)
	if err != nil || len(password) > 1024 {
		return "", ErrInvalidInput
	}
	if err = service.limitAttempt(ctx, "browser-login:"+normalized, attempt); err != nil {
		return "", err
	}
	record, err := service.credential(ctx, userSubject, normalized)
	if err != nil {
		return "", err
	}
	var matches bool
	err = service.passwordWork(ctx, func() error {
		var verifyErr error
		matches, verifyErr = verifyPassword(record.hash, password)
		return verifyErr
	})
	if err != nil {
		return "", err
	}
	if !matches || record.userID == "" || record.status != "active" {
		err = service.transaction(ctx, func(transaction *sql.Tx) error {
			return audit(ctx, transaction, record.userID, "", "浏览器登录", "denied", attempt.RequestID)
		})
		if err != nil {
			return "", err
		}
		return "", ErrInvalidCredentials
	}
	var authorizationCode string
	err = service.transaction(ctx, func(transaction *sql.Tx) error {
		var userID string
		var now time.Time
		err := transaction.QueryRowContext(ctx, `SELECT id,transaction_timestamp() FROM tombot.users WHERE id=$1 AND status='active' FOR UPDATE`, record.userID).Scan(&userID, &now)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvalidCredentials
		}
		if err != nil {
			return err
		}
		// 桌面登录只面向普通用户；管理员身份不在用户表中，也不持有桌面会话。
		value, err := secure.Token()
		if err != nil {
			return err
		}
		result, err := transaction.ExecContext(ctx, `UPDATE tombot.desktop_authorizations
			SET user_id=$2,code_digest=$3,approved_at=transaction_timestamp()
			WHERE id=$1 AND approved_at IS NULL AND consumed_at IS NULL AND expires_at>transaction_timestamp()`,
			authorizationID, record.userID, digest(value))
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			// 已过期、已使用或正在被并发批准：不生成新码，避免一个请求被重复授权。
			return ErrInvalidAuthorization
		}
		authorizationCode = value
		return audit(ctx, transaction, record.userID, "", "浏览器登录", "succeeded", attempt.RequestID)
	})
	if err != nil {
		return "", err
	}
	return authorizationCode, nil
}

// ExchangeBrowserAuthorization 用一次性授权码与 PKCE 校验串换取正常会话。
// 授权码与状态在校验成功前不会泄露用户身份；重复提交视为会话全部撤销。
func (service *Service) ExchangeBrowserAuthorization(ctx context.Context, code, verifier, state string, attempt Attempt) (Session, error) {
	if !validToken(code) || !validToken(verifier) || len(state) < 16 || len(state) > 128 {
		return Session{}, ErrInvalidAuthorization
	}
	if err := service.limitAttempt(ctx, "browser-exchange:"+secure.Hash(code), attempt); err != nil {
		return Session{}, err
	}
	var session Session
	var denied bool
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		var authorizationID, redirectURI, deviceName, platform, userID string
		var challenge []byte
		var consumed sql.NullTime
		var expires time.Time
		err := transaction.QueryRowContext(ctx, `SELECT id,user_id,redirect_uri,device_name,platform,challenge_digest,consumed_at,expires_at
			FROM tombot.desktop_authorizations WHERE code_digest=$1 AND state=$2 FOR UPDATE`, digest(code), state).
			Scan(&authorizationID, &userID, &redirectURI, &deviceName, &platform, &challenge, &consumed, &expires)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvalidAuthorization
		}
		if err != nil {
			return err
		}
		if consumed.Valid {
			// 授权码只能使用一次；重复提交说明码已泄漏，按被盗处理，不签发任何凭据。
			denied = true
			return audit(ctx, transaction, userID, "", "浏览器登录交换", "denied", attempt.RequestID)
		}
		verifierDigest := sha256.Sum256([]byte(verifier))
		if !expires.After(time.Now()) || len(challenge) != 32 || !constantTimeEqual(challenge, verifierDigest[:]) {
			return ErrInvalidAuthorization
		}
		var status string
		var version int64
		err = transaction.QueryRowContext(ctx, `SELECT status,auth_version FROM tombot.users WHERE id=$1 FOR UPDATE`, userID).Scan(&status, &version)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvalidAuthorization
		}
		if err != nil {
			return err
		}
		if status != "active" {
			return ErrInvalidAuthorization
		}
		var devices int
		if err = transaction.QueryRowContext(ctx, `SELECT count(*) FROM tombot.devices d WHERE user_id=$1 AND revoked_at IS NULL AND EXISTS(
			SELECT 1 FROM tombot.user_sessions s WHERE s.user_id=d.user_id AND s.device_id=d.id AND s.revoked_at IS NULL AND s.expires_at>transaction_timestamp())`, userID).Scan(&devices); err != nil {
			return err
		}
		if devices >= service.options.DeviceLimit {
			return ErrDeviceLimit
		}
		var now time.Time
		if err = transaction.QueryRowContext(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
			return err
		}
		session.Principal = Principal{UserID: userID, SessionID: ids.New(), DeviceID: ids.New(), Role: userSubject.role, Scope: "desktop", AuthVersion: version, VerifiedAt: &now}
		session.ExpiresAt = now.Add(service.options.SessionLifetime)
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.desktop_authorizations SET consumed_at=transaction_timestamp() WHERE id=$1`, authorizationID); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.devices(user_id,id,name,platform) VALUES($1,$2,$3,$4)`, userID, session.Principal.DeviceID, strings.TrimSpace(deviceName), platform); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.user_sessions(user_id,id,device_id,auth_version,scope,expires_at,verified_at)
			VALUES($1,$2,$3,$4,'desktop',$5,$6)`, userID, session.Principal.SessionID, session.Principal.DeviceID, version, session.ExpiresAt, now); err != nil {
			return err
		}
		if err = service.issueTokens(ctx, transaction, userSubject, &session, now); err != nil {
			return err
		}
		if redirectURI == "" {
			return errors.New("授权记录缺少回调地址")
		}
		return audit(ctx, transaction, userID, session.Principal.SessionID, "浏览器登录交换", "succeeded", attempt.RequestID)
	})
	if err != nil {
		return Session{}, err
	}
	if denied {
		return Session{}, ErrInvalidAuthorization
	}
	return session, nil
}

// PendingAuthorization 供授权页读取待登录请求；过期或已完成的记录一律不可展示。
func (service *Service) PendingAuthorization(ctx context.Context, authorizationID, state string) (Authorization, error) {
	var authorization Authorization
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		err := transaction.QueryRowContext(ctx, `SELECT id,state,redirect_uri,device_name,platform,expires_at,approved_at IS NOT NULL,coalesce(user_id,'')
			FROM tombot.desktop_authorizations WHERE id=$1 AND state=$2 AND consumed_at IS NULL AND expires_at>transaction_timestamp()`,
			authorizationID, state).Scan(&authorization.ID, &authorization.State, &authorization.RedirectURI, &authorization.DeviceName, &authorization.Platform, &authorization.ExpiresAt, &authorization.IsApproved, &authorization.ApprovedUser)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvalidAuthorization
		}
		return err
	})
	if err != nil {
		return Authorization{}, err
	}
	return authorization, nil
}

// constantTimeEqual 比较摘要，避免按字节短路泄露匹配长度。
func constantTimeEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for index := range left {
		difference |= left[index] ^ right[index]
	}
	return difference == 0
}
