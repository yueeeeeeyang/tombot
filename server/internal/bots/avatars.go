package bots

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"image/png"

	"io.yueyang/tom/internal/identity"
)

const MaxAvatarBytes = 72 << 10

func normalizeAvatar(content []byte) ([]byte, error) {
	if len(content) < 1 || len(content) > MaxAvatarBytes {
		return nil, ErrInvalidInput
	}
	configuration, err := png.DecodeConfig(bytes.NewReader(content))
	if err != nil || configuration.Width < 1 || configuration.Height < 1 || configuration.Width > 128 || configuration.Height > 128 {
		return nil, ErrInvalidInput
	}
	picture, err := png.Decode(bytes.NewReader(content))
	if err != nil {
		return nil, ErrInvalidInput
	}
	var encoded bytes.Buffer
	if err = png.Encode(&encoded, picture); err != nil {
		return nil, err
	}
	if encoded.Len() > MaxAvatarBytes {
		return nil, ErrInvalidInput
	}
	return encoded.Bytes(), nil
}

// PutAvatar 只接收小尺寸 PNG 并重新编码，剥离元数据与尾随内容；头像更新参与机器人修订。
func (service *Service) PutAvatar(ctx context.Context, principal identity.Principal, id string, revision int64, content []byte) (Bot, error) {
	if !validID(id) || revision < 1 {
		return Bot{}, ErrInvalidInput
	}
	encoded, err := normalizeAvatar(content)
	if err != nil {
		return Bot{}, err
	}
	digest := sha256.Sum256(encoded)
	return service.changeAvatar(ctx, principal, id, revision, func(transaction *sql.Tx) error {
		_, err := transaction.ExecContext(ctx, `INSERT INTO tombot.bot_avatars(user_id,bot_id,content,sha256) VALUES($1,$2,$3,$4)
			ON CONFLICT(user_id,bot_id) DO UPDATE SET content=excluded.content,sha256=excluded.sha256,updated_at=clock_timestamp()`, principal.UserID, id, encoded, hex.EncodeToString(digest[:]))
		return err
	})
}

// RemoveAvatar 删除自定义图像并恢复客户端展示标识，保留机器人的业务身份。
func (service *Service) RemoveAvatar(ctx context.Context, principal identity.Principal, id string, revision int64) (Bot, error) {
	if !validID(id) || revision < 1 {
		return Bot{}, ErrInvalidInput
	}
	return service.changeAvatar(ctx, principal, id, revision, func(transaction *sql.Tx) error {
		_, err := transaction.ExecContext(ctx, `DELETE FROM tombot.bot_avatars WHERE user_id=$1 AND bot_id=$2`, principal.UserID, id)
		return err
	})
}

func (service *Service) changeAvatar(ctx context.Context, principal identity.Principal, id string, revision int64, apply func(*sql.Tx) error) (Bot, error) {
	var bot Bot
	err := service.transaction(ctx, principal, true, func(transaction *sql.Tx) error {
		current, err := readBot(ctx, transaction, principal.UserID, id)
		if err != nil {
			return err
		}
		if current.Revision != revision {
			return ErrConflict
		}
		if current.IsArchived {
			return ErrArchived
		}
		if err = apply(transaction); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `UPDATE tombot.bots SET revision=revision+1,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, principal.UserID, id); err != nil {
			return err
		}
		bot, err = readBot(ctx, transaction, principal.UserID, id)
		if err != nil {
			return err
		}
		return emitChange(ctx, transaction, principal.UserID, "bot.updated", id, bot.Revision)
	})
	if err != nil {
		return Bot{}, err
	}
	return bot, nil
}

// GetAvatar 验证所属关系并返回规范 PNG；没有自定义头像时返回不存在，不读取外部地址。
func (service *Service) GetAvatar(ctx context.Context, principal identity.Principal, id string) (Avatar, error) {
	if !validID(id) {
		return Avatar{}, ErrInvalidInput
	}
	var avatar Avatar
	err := service.transaction(ctx, principal, false, func(transaction *sql.Tx) error {
		return transaction.QueryRowContext(ctx, `SELECT content,sha256 FROM tombot.bot_avatars WHERE user_id=$1 AND bot_id=$2`, principal.UserID, id).Scan(&avatar.Content, &avatar.SHA256)
	})
	if err != nil {
		return Avatar{}, err
	}
	digest := sha256.Sum256(avatar.Content)
	if hex.EncodeToString(digest[:]) != avatar.SHA256 {
		return Avatar{}, errors.New("头像内容完整性检查失败")
	}
	return avatar, nil
}
