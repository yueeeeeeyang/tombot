// Package identity 管理账户激活及用户会话，不访问机器人、消息或文件内容。
package identity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store/postgres"
)

var (
	ErrInvalidInput       = errors.New("认证参数无效")
	ErrInvalidCredentials = errors.New("用户名或认证信息无效")
	ErrInvalidSession     = errors.New("会话无效或已失效")
	ErrInvalidActivation  = errors.New("激活信息无效或已过期")
	ErrRateLimited        = errors.New("认证尝试过于频繁，请稍后再试")
	ErrForbidden          = errors.New("当前账户无权执行此操作")
	ErrDeviceLimit        = errors.New("有效设备数量已达到上限")
)

// Options 的时间和数量均来自服务端配置，构造时拒绝无界或矛盾的限额。
type Options struct {
	AccessLifetime       time.Duration
	SessionLifetime      time.Duration
	VerificationLifetime time.Duration
	RateWindow           time.Duration
	AccountAttempts      int
	SourceAttempts       int
	PasswordWorkers      int
	DeviceLimit          int
	EncryptionKey        []byte
}

// Service 使用专用身份连接；口令计算不持有数据库事务且受内存并发额度约束。
type Service struct {
	database      *postgres.Database
	options       Options
	passwordSlots chan struct{}
	dummyHash     string
}

// Principal 只由有效会话解析；HTTP 请求体不能覆盖其中的身份字段。
type Principal struct {
	UserID      string     `json:"user_id"`
	SessionID   string     `json:"session_id"`
	DeviceID    string     `json:"device_id"`
	Role        string     `json:"role"`
	Scope       string     `json:"scope"`
	AuthVersion int64      `json:"-"`
	VerifiedAt  *time.Time `json:"-"`
}

// Session 返回一次性凭据响应，持久层仅保存摘要。
type Session struct {
	Principal       Principal  `json:"principal"`
	AccessToken     Credential `json:"access_token"`
	RefreshToken    Credential `json:"refresh_token"`
	AccessExpiresAt time.Time  `json:"access_expires_at"`
	ExpiresAt       time.Time  `json:"expires_at"`
}

// Attempt 的来源必须由传输层提取可信客户端地址，不接受请求体或任意代理头。
type Attempt struct {
	Source    netip.Addr
	RequestID string
}

// LoginRequest 仅在认证入口短时使用，日志不得序列化原始输入。
type LoginRequest struct {
	Username   string
	Password   string
	Code       string
	DeviceName string
	Platform   string
	Scope      string
}

// New 校验限额与密钥并生成等成本的不存在账户校验值；数据库必须为身份用途。
func New(database *postgres.Database, options Options) (*Service, error) {
	if database == nil || database.Role() != postgres.IdentityRole || len(options.EncryptionKey) != 32 ||
		options.AccessLifetime < time.Second || options.AccessLifetime > time.Hour ||
		options.SessionLifetime <= options.AccessLifetime || options.SessionLifetime > 90*24*time.Hour ||
		options.VerificationLifetime < time.Second || options.VerificationLifetime > time.Hour ||
		options.RateWindow < time.Second || options.RateWindow > time.Hour ||
		options.AccountAttempts < 1 || options.AccountAttempts > 100 ||
		options.SourceAttempts < options.AccountAttempts || options.SourceAttempts > 1000 ||
		options.PasswordWorkers < 1 || options.PasswordWorkers > 16 || options.DeviceLimit < 1 || options.DeviceLimit > 100 {
		return nil, errors.New("身份服务配置无效")
	}
	password, err := secure.Token()
	if err != nil {
		return nil, errors.New("初始化身份服务失败")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	options.EncryptionKey = append([]byte(nil), options.EncryptionKey...)
	return &Service{database: database, options: options, passwordSlots: make(chan struct{}, options.PasswordWorkers), dummyHash: hash}, nil
}

// NormalizeUsername 统一采用 ASCII 账户名，消除大小写、首尾空白及相似 Unicode 字符歧义。
func NormalizeUsername(username string) (string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if len(username) < 3 || len(username) > 64 {
		return "", ErrInvalidInput
	}
	for _, character := range username {
		if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' || character == '-' || character == '.') {
			return "", ErrInvalidInput
		}
	}
	return username, nil
}

func (service *Service) passwordWork(ctx context.Context, operation func() error) error {
	select {
	case service.passwordSlots <- struct{}{}:
		defer func() { <-service.passwordSlots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := operation(); err != nil {
		return err
	}
	return ctx.Err()
}

func digest(value string) []byte { hash := sha256.Sum256([]byte(value)); return hash[:] }

func validToken(token string) bool {
	if len(token) != 43 {
		return false
	}
	for _, character := range token {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_') {
			return false
		}
	}
	return true
}

func validateAttempt(attempt Attempt) error {
	if !attempt.Source.IsValid() || attempt.Source.IsUnspecified() || !validRequestID(attempt.RequestID) {
		return ErrInvalidInput
	}
	return nil
}

func validRequestID(requestID string) bool {
	return len(requestID) > 0 && len(requestID) <= 120 && !strings.ContainsAny(requestID, "\r\n\x00")
}

func (service *Service) transaction(ctx context.Context, apply func(*sql.Tx) error) error {
	err := service.database.Platform(ctx, apply)
	if err == nil {
		return nil
	}
	for _, known := range []error{ErrInvalidInput, ErrInvalidCredentials, ErrInvalidSession, ErrInvalidActivation, ErrRateLimited, ErrForbidden, ErrDeviceLimit, context.Canceled, context.DeadlineExceeded} {
		// 组合错误可能还包含回滚失败，不能只提取其中的业务拒绝而丢弃基础设施故障。
		if err == known {
			return known
		}
	}
	var sqlState interface{ SQLState() string }
	if errors.As(err, &sqlState) {
		return fmt.Errorf("身份数据库操作失败（SQLSTATE %s）", sqlState.SQLState())
	}
	return errors.New("身份数据库操作失败")
}

func audit(ctx context.Context, transaction *sql.Tx, userID, sessionID, action, result, requestID string) error {
	_, err := transaction.ExecContext(ctx, `INSERT INTO tombot.authentication_events(id,user_id,session_id,action,result,request_id)
		VALUES($1,nullif($2,''),nullif($3,''),$4,$5,$6)`, ids.New(), userID, sessionID, action, result, requestID)
	return err
}

// limitAttempt 在口令计算之前原子计数，覆盖不存在账户及多实例并发；失败或成功都不退还额度。
func (service *Service) limitAttempt(ctx context.Context, subject string, attempt Attempt) error {
	if err := validateAttempt(attempt); err != nil {
		return err
	}
	limited := false
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		for _, bucket := range []struct {
			name  string
			limit int
		}{
			{"source:" + attempt.Source.Unmap().String(), service.options.SourceAttempts},
			{"subject:" + subject, service.options.AccountAttempts},
		} {
			var allowed bool
			err := transaction.QueryRowContext(ctx, `INSERT INTO tombot.auth_rate_limits(bucket_digest,failures,blocked_until,expires_at)
				VALUES($1,1,transaction_timestamp(),transaction_timestamp()+$2::bigint*interval '1 millisecond')
				ON CONFLICT(bucket_digest) DO UPDATE SET
				failures=CASE WHEN tombot.auth_rate_limits.expires_at<=transaction_timestamp() THEN 1 ELSE least(tombot.auth_rate_limits.failures+1,$3+1) END,
				blocked_until=CASE WHEN tombot.auth_rate_limits.expires_at<=transaction_timestamp() THEN transaction_timestamp()
				    WHEN tombot.auth_rate_limits.failures+1>$3 THEN tombot.auth_rate_limits.expires_at ELSE tombot.auth_rate_limits.blocked_until END,
				expires_at=CASE WHEN tombot.auth_rate_limits.expires_at<=transaction_timestamp() THEN EXCLUDED.expires_at ELSE tombot.auth_rate_limits.expires_at END
				RETURNING failures<=$3`, digest(bucket.name), service.options.RateWindow.Milliseconds(), bucket.limit).Scan(&allowed)
			if err != nil {
				return err
			}
			if !allowed {
				limited = true
				break
			}
		}
		if limited {
			return audit(ctx, transaction, "", "", "认证限流", "denied", attempt.RequestID)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if limited {
		return ErrRateLimited
	}
	return nil
}

func validDevice(name, platform, scope string) bool {
	if !utf8.ValidString(name) || strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 120 || strings.ContainsAny(name, "\r\n\x00") {
		return false
	}
	return scope == "admin" && platform == "web" || scope == "desktop" && (platform == "macos" || platform == "windows" || platform == "linux")
}
