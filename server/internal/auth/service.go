package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/domain"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store"
)

type credentials struct {
	PasswordHash string `json:"password_hash"`
}

// Device 是可公开的设备元数据，永不包含原始设备凭据。
type Device struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Platform   string     `json:"platform"`
	Hint       string     `json:"token_hint"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	IsCurrent  bool       `json:"is_current"`
}

// Session 只在登录成功时返回一次；不得序列化到日志。
type Session struct {
	DeviceID    string `json:"device_id"`
	DeviceToken string `json:"device_token"`
}

// Service 串行化口令派生与初始化，避免并发初始化覆盖及内存耗尽。
type Service struct {
	db           *sql.DB
	path         string
	mu           sync.Mutex
	passwordHash string
	setupHash    string
	failures     int
	blockedUntil time.Time
	now          func() time.Time
}

// New 读取认证文件；损坏文件不能被当成未初始化状态。
func New(db *sql.DB, dir string) (*Service, error) {
	s := &Service{db: db, path: filepath.Join(dir, "auth.json"), now: time.Now}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.loadSetup(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// IssueSetupCode 轮换一次性初始化码；只向本地控制台返回，不持久化明文。
func (s *Service) IssueSetupCode() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.passwordHash != "" {
		return "", nil
	}
	code, err := secure.Token()
	if err != nil {
		return "", err
	}
	s.setupHash = secure.Hash(code)
	return code, nil
}

// Initialized 返回是否已完成管理员初始化。
func (s *Service) Initialized() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.passwordHash != "" }

// AuthorizeSetup 验证初始化授权，可用于初始化期间的依赖修复。
func (s *Service) AuthorizeSetup(code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.passwordHash != "" || s.setupHash == "" || subtle.ConstantTimeCompare([]byte(s.setupHash), []byte(secure.Hash(code))) != 1 {
		return domain.NewError(401, "SETUP_CODE_INVALID", "The setup code is invalid.")
	}
	return nil
}

// Setup 消费初始化码并原子保存口令；重复初始化、弱口令及错误码均拒绝。
func (s *Service) Setup(password, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.loadSetup(ctx); err != nil {
		return err
	}
	if s.passwordHash != "" {
		return domain.NewError(409, "ALREADY_INITIALIZED", "The server has already been initialized.")
	}
	if s.setupHash == "" || subtle.ConstantTimeCompare([]byte(s.setupHash), []byte(secure.Hash(code))) != 1 {
		return domain.NewError(401, "SETUP_CODE_INVALID", "The setup code is invalid.")
	}
	if utf8.RuneCountInString(password) < 12 || len(password) > 1024 {
		return domain.NewError(400, "PASSWORD_INVALID", "Use a password of at least 12 characters and at most 1024 bytes.")
	}
	hash, err := secure.HashPassword(password)
	if err != nil {
		return err
	}
	return s.commitSetup(ctx, hash)
}

// Login 校验口令并签发设备凭据；失败五次后按全局单用户状态退避。
func (s *Service) Login(ctx context.Context, password, name, platform string, push *PushRegistration) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if now.Before(s.blockedUntil) {
		return Session{}, domain.NewError(429, "AUTH_RATE_LIMITED", "Too many attempts. Try again later.")
	}
	if len(password) > 1024 {
		return Session{}, domain.NewError(400, "PASSWORD_INVALID", "Password exceeds the size limit.")
	}
	if s.passwordHash == "" {
		return Session{}, domain.NewError(409, "NOT_INITIALIZED", "Initialize the server first.")
	}
	if !secure.VerifyPassword(s.passwordHash, password) {
		s.failures++
		if s.failures >= 5 {
			shift := min(s.failures-5, 8)
			s.blockedUntil = now.Add(time.Second * time.Duration(1<<shift))
		}
		if err := store.Transact(ctx, s.db, func(tx *sql.Tx) error {
			return store.Audit(ctx, tx, "anonymous", "auth.login.denied", "administrator", map[string]bool{"backoff": now.Before(s.blockedUntil)})
		}); err != nil {
			return Session{}, err
		}
		return Session{}, domain.NewError(401, "AUTH_INVALID", "Authentication failed.")
	}
	if strings.TrimSpace(name) == "" || len(name) > 120 {
		return Session{}, domain.NewError(400, "DEVICE_INVALID", "Provide a device name of at most 120 bytes.")
	}
	switch platform {
	case "macos", "windows", "linux", "ios", "android":
	default:
		return Session{}, domain.NewError(400, "DEVICE_INVALID", "Unsupported device platform.")
	}
	random, err := secure.Token()
	if err != nil {
		return Session{}, err
	}
	token := "tomd_" + random
	id := ids.New()
	var registered any
	if push != nil {
		registered, err = encryptedPush(id, push.Token, push.Key)
		if err != nil {
			return Session{}, err
		}
	}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO devices(id,name,platform,token_hash,token_hint,last_seen_at,created_at,push_token) VALUES(?,?,?,?,?,?,?,?)`, id, name, platform, secure.Hash(token), token[:4]+"…"+token[len(token)-4:], now.UnixMilli(), now.UnixMilli(), registered)
		if err != nil {
			return err
		}
		return store.Audit(ctx, tx, "device:"+id, "auth.login", id, map[string]any{"platform": platform, "push_registered": push != nil})
	})
	if err != nil {
		return Session{}, err
	}
	s.failures = 0
	s.blockedUntil = time.Time{}
	return Session{DeviceID: id, DeviceToken: token}, nil
}

// Authenticate 拒绝撤销或过期设备；数据库异常不能转换为认证成功。
func (s *Service) Authenticate(ctx context.Context, token string) (string, error) {
	if !strings.HasPrefix(token, "tomd_") || len(token) != 48 {
		return "", domain.NewError(401, "AUTH_REQUIRED", "A valid device token is required.")
	}
	var id string
	now := s.now().UnixMilli()
	err := s.db.QueryRowContext(ctx, `SELECT id FROM devices WHERE token_hash=? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>?)`, secure.Hash(token), now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", domain.NewError(401, "AUTH_REQUIRED", "A valid device token is required.")
	}
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, "UPDATE devices SET last_seen_at=? WHERE id=? AND (last_seen_at IS NULL OR last_seen_at<?)", now, id, now-60000)
	return id, err
}

// List 返回最多一页设备，按唯一标识降序；current 标记当前客户端。
func (s *Service) List(ctx context.Context, current, cursor string, limit int) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,platform,token_hint,created_at,last_seen_at FROM devices WHERE revoked_at IS NULL AND (?='' OR id<?) ORDER BY id DESC LIMIT ?`, cursor, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	devices := []Device{}
	for rows.Next() {
		var d Device
		var created int64
		var seen sql.NullInt64
		if err = rows.Scan(&d.ID, &d.Name, &d.Platform, &d.Hint, &created, &seen); err != nil {
			return nil, err
		}
		d.CreatedAt = time.UnixMilli(created).UTC()
		if seen.Valid {
			t := time.UnixMilli(seen.Int64).UTC()
			d.LastSeenAt = &t
		}
		d.IsCurrent = d.ID == current
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

// Revoke 在同一事务内撤销指定设备并记录发起设备。重复撤销不会重复计入审计。
func (s *Service) Revoke(ctx context.Context, id, actorID string) error {
	if _, err := ulid.ParseStrict(id); err != nil {
		return domain.NewError(400, "DEVICE_INVALID", "Provide a valid device identifier.")
	}
	return store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		changed, err := tx.ExecContext(ctx, "UPDATE devices SET revoked_at=? WHERE id=? AND revoked_at IS NULL", s.now().UnixMilli(), id)
		if err != nil {
			return err
		}
		count, err := changed.RowsAffected()
		if err != nil || count == 0 {
			return err
		}
		return store.Audit(ctx, tx, "device:"+actorID, "auth.device.revoke", id, map[string]bool{"self": id == actorID})
	})
}

// PushRegistration 只在加密并登记登录设备的过程中传递推送凭据。
type PushRegistration struct {
	Token string
	Key   []byte
}

func encryptedPush(id, token string, key []byte) (string, error) {
	if len(token) > 4096 {
		return "", domain.NewError(400, "PUSH_TOKEN_INVALID", "Push token exceeds the size limit.")
	}
	ciphertext, nonce, err := secure.Seal(key, []byte(token), id)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(struct{ Ciphertext, Nonce []byte }{ciphertext, nonce})
	return string(encoded), err
}

// UpdatePushToken 只对有效设备写入密文，审计和凭据更新共同提交。
func (s *Service) UpdatePushToken(ctx context.Context, id, token string, key []byte) error {
	encoded, err := encryptedPush(id, token, key)
	if err != nil {
		return err
	}
	return store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		changed, err := tx.ExecContext(ctx, "UPDATE devices SET push_token=? WHERE id=? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>?)", encoded, id, s.now().UnixMilli())
		if err != nil {
			return err
		}
		count, err := changed.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return domain.NewError(401, "AUTH_REQUIRED", "A valid device token is required.")
		}
		return store.Audit(ctx, tx, "device:"+id, "auth.device.push", id, map[string]bool{"registered": token != ""})
	})
}
