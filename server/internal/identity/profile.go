package identity

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"image"
	_ "image/jpeg"
	"image/png"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const MaxUserAvatarBytes = 2 << 20
const maxUserAvatarDimension = 512

var ErrInvalidAvatar = errors.New("头像须为 PNG 或 JPEG，文件不超过 2 MiB，宽高均不超过 512 像素")

// normalizeUserAvatar 限制解码内存并重新编码，移除元数据和尾随内容，不接受远程地址。
func normalizeUserAvatar(content []byte) ([]byte, error) {
	if len(content) == 0 || len(content) > MaxUserAvatarBytes {
		return nil, ErrInvalidAvatar
	}
	configuration, format, err := image.DecodeConfig(bytes.NewReader(content))
	if err != nil || (format != "png" && format != "jpeg") || configuration.Width < 1 || configuration.Height < 1 || configuration.Width > maxUserAvatarDimension || configuration.Height > maxUserAvatarDimension {
		return nil, ErrInvalidAvatar
	}
	picture, _, err := image.Decode(bytes.NewReader(content))
	if err != nil {
		return nil, ErrInvalidAvatar
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, picture); err != nil {
		return nil, err
	}
	if encoded.Len() > MaxUserAvatarBytes {
		return nil, ErrInvalidAvatar
	}
	return encoded.Bytes(), nil
}

// ProfileUpdate 只包含本次变更的字段，未提供的字段保留数据库当前值。
type ProfileUpdate struct {
	DisplayName *string
	Avatar      []byte
}

// UserProfile 返回事务提交后的规范昵称，不包含头像原始内容或认证材料。
type UserProfile struct {
	DisplayName string `json:"display_name"`
}

// UpdateProfile 原子保存指定字段并返回规范资料；缺少变更、无效输入或失效会话返回错误。
// 更新与改密、撤销共用身份锁，分开保存昵称与头像时不会以旧页面值覆盖另一字段。
func (service *Service) UpdateProfile(ctx context.Context, principal Principal, update ProfileUpdate, requestID string) (UserProfile, error) {
	if principal.Scope != "web" {
		return UserProfile{}, ErrInvalidSession
	}
	if !validRequestID(requestID) || (update.DisplayName == nil && update.Avatar == nil) {
		return UserProfile{}, ErrInvalidInput
	}
	var displayName *string
	if update.DisplayName != nil {
		if !utf8.ValidString(*update.DisplayName) {
			return UserProfile{}, ErrInvalidInput
		}
		normalized := norm.NFC.String(strings.TrimSpace(*update.DisplayName))
		if normalized == "" || utf8.RuneCountInString(normalized) > 120 || strings.ContainsFunc(normalized, unicode.IsControl) {
			return UserProfile{}, ErrInvalidInput
		}
		displayName = &normalized
	}
	var encoded []byte
	if update.Avatar != nil {
		var err error
		encoded, err = normalizeUserAvatar(update.Avatar)
		if err != nil {
			return UserProfile{}, err
		}
	}
	var profile UserProfile
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		if _, _, err := lockSession(ctx, transaction, principal); err != nil {
			return err
		}
		if err := transaction.QueryRowContext(ctx, `UPDATE tombot.users SET display_name=coalesce($2,display_name),revision=revision+1,updated_at=transaction_timestamp() WHERE id=$1 RETURNING display_name`, principal.UserID, displayName).Scan(&profile.DisplayName); err != nil {
			return err
		}
		if encoded != nil {
			if _, err := transaction.ExecContext(ctx, `INSERT INTO tombot.user_avatars(user_id,content) VALUES($1,$2)
                ON CONFLICT(user_id) DO UPDATE SET content=excluded.content,updated_at=transaction_timestamp()`, principal.UserID, encoded); err != nil {
				return err
			}
		}
		return audit(ctx, transaction, principal.UserID, principal.SessionID, "修改个人资料", "succeeded", requestID)
	})
	if err != nil {
		return UserProfile{}, err
	}
	return profile, nil
}

// UserAvatar 只返回当前会话用户的头像；nil 表示使用默认头像，失效会话或数据库故障返回错误。
func (service *Service) UserAvatar(ctx context.Context, principal Principal) ([]byte, error) {
	if principal.Scope != "web" {
		return nil, ErrInvalidSession
	}
	var content []byte
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		if _, _, err := lockSession(ctx, transaction, principal); err != nil {
			return err
		}
		err := transaction.QueryRowContext(ctx, `SELECT content FROM tombot.user_avatars WHERE user_id=$1`, principal.UserID).Scan(&content)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return content, err
}
