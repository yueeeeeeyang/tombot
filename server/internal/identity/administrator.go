package identity

import (
	"context"
	"database/sql"
	"encoding/base32"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
)

// ErrAdministratorExists 表示平台已存在管理员，不能再走首位管理员创建路径。
var ErrAdministratorExists = errors.New("平台已存在管理员，请使用已有的管理员账号开立")

// AdministratorSetup 是一次性交付的管理员初始材料。
// 字段只包含创建者必须转交给本人的信息；动态验证器密钥不会再从服务端读出。
type AdministratorSetup struct {
	AdministratorID string     `json:"administrator_id"`
	Username        string     `json:"username"`
	DisplayName     string     `json:"display_name"`
	Secret          Credential `json:"secret"`
	Period          int        `json:"period"`
	Digits          int        `json:"digits"`
	Algorithm       string     `json:"algorithm"`
	CreatedAt       time.Time  `json:"created_at"`
}

// AdministratorsExist 供登录页判断是否还需要展示首位管理员创建入口。
// 只返回布尔值，不暴露管理员数量、用户名或状态。
func (service *Service) AdministratorsExist(ctx context.Context) (bool, error) {
	var exists bool
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		return transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.administrators)`).Scan(&exists)
	})
	return exists, err
}

// CreateFirstAdministrator 仅在平台还没有任何管理员时可用；全库互斥避免并发建出两个首位管理员。
// 口令由创建者当场设定，动态验证器密钥一次性返回，账号创建后即为有效，不再需要激活材料。
func (service *Service) CreateFirstAdministrator(ctx context.Context, username, displayName, password string, attempt Attempt) (AdministratorSetup, error) {
	return service.createAdministrator(ctx, "", username, displayName, password, attempt, true)
}

// CreateAdministrator 由已登录管理员开立新管理员，要求近期二次验证，与其它敏感动作一致。
func (service *Service) CreateAdministrator(ctx context.Context, principal Principal, username, displayName, password string, attempt Attempt) (AdministratorSetup, error) {
	if err := service.RequireAdmin(ctx, principal, true); err != nil {
		return AdministratorSetup{}, err
	}
	return service.createAdministrator(ctx, principal.UserID, username, displayName, password, attempt, false)
}

func (service *Service) createAdministrator(ctx context.Context, actorID, username, displayName, password string, attempt Attempt, isFirst bool) (AdministratorSetup, error) {
	normalized, err := NormalizeUsername(username)
	if err != nil {
		return AdministratorSetup{}, ErrInvalidInput
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" || !utf8.ValidString(displayName) || utf8.RuneCountInString(displayName) > 120 || strings.ContainsAny(displayName, "\r\n\x00") {
		return AdministratorSetup{}, ErrInvalidInput
	}
	if err := validatePassword(password); err != nil {
		return AdministratorSetup{}, err
	}
	if err := service.limitAttempt(ctx, "administrator-setup:"+attempt.Source.String(), attempt); err != nil {
		return AdministratorSetup{}, err
	}
	var encoded string
	if err := service.passwordWork(ctx, func() error { var err error; encoded, err = hashPassword(password); return err }); err != nil {
		return AdministratorSetup{}, err
	}
	secret, err := secure.Random(20)
	if err != nil {
		return AdministratorSetup{}, errors.New("生成动态验证器密钥失败")
	}
	defer clear(secret)
	administrator := AdministratorSetup{AdministratorID: ids.New(), Username: normalized, DisplayName: displayName, Period: 30, Digits: 6, Algorithm: "SHA1"}
	err = service.transaction(ctx, func(transaction *sql.Tx) error {
		if isFirst {
			if _, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('tombot.bootstrap.administrators',0))`); err != nil {
				return err
			}
			var exists bool
			if err := transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.administrators)`).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return ErrAdministratorExists
			}
		}
		err := transaction.QueryRowContext(ctx, `INSERT INTO tombot.administrators(id,username,display_name,status)
			VALUES($1,$2,$3,'active') RETURNING created_at`, administrator.AdministratorID, normalized, displayName).Scan(&administrator.CreatedAt)
		if err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.administrator_credentials(administrator_id,password_hash) VALUES($1,$2)`, administrator.AdministratorID, encoded); err != nil {
			return err
		}
		ciphertext, nonce, err := secure.Seal(service.options.EncryptionKey, secret, administrator.AdministratorID)
		if err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.administrator_mfa(administrator_id,ciphertext,nonce) VALUES($1,$2,$3)`, administrator.AdministratorID, ciphertext, nonce); err != nil {
			return err
		}
		return audit(ctx, transaction, administrator.AdministratorID, "", "创建管理员账号", "succeeded", attempt.RequestID)
	})
	if err != nil {
		return AdministratorSetup{}, err
	}
	administrator.Secret = Credential(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret))
	return administrator, nil
}

// Administrator 是可展示的管理员元数据，不含凭据、会话或动态验证材料。
type Administrator struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

// ListAdministrators 复验管理会话后分页读取管理员元数据，使用稳定标识游标。
func (service *Service) ListAdministrators(ctx context.Context, principal Principal, before string, limit int) ([]Administrator, error) {
	if err := service.RequireAdmin(ctx, principal, false); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 100 {
		return nil, ErrInvalidInput
	}
	if before != "" {
		if !validToken(before) && !validAdministratorCursor(before) {
			return nil, ErrInvalidInput
		}
	}
	values := make([]Administrator, 0, limit)
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		rows, err := transaction.QueryContext(ctx, `SELECT id,username,display_name,status,created_at FROM tombot.administrators
			WHERE ($1='' OR id<$1) ORDER BY id DESC LIMIT $2`, before, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var value Administrator
			if err = rows.Scan(&value.ID, &value.Username, &value.DisplayName, &value.Status, &value.CreatedAt); err != nil {
				return err
			}
			values = append(values, value)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return values, nil
}

// validAdministratorCursor 只接受 ULID 形式的游标，拒绝任意字符串进入比较。
func validAdministratorCursor(value string) bool {
	if len(value) != 26 {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9' || character >= 'A' && character <= 'Z') {
			return false
		}
	}
	return true
}
