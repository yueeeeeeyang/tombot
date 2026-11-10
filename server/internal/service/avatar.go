package service

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"image"
	_ "image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store"
)

// SetAvatar 限制压缩大小与像素数量，并重编码为 PNG 去除可执行内容和元数据。
func (s *Bots) SetAvatar(ctx context.Context, id, actor string, reader io.Reader) (path string, result error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bot, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}
	encoded, err := io.ReadAll(io.LimitReader(reader, (4<<20)+1))
	if err != nil {
		return "", err
	}
	if len(encoded) > 4<<20 {
		return "", domain.NewError(413, "AVATAR_TOO_LARGE", "Avatar must be at most 4 MiB.")
	}
	configuration, _, err := image.DecodeConfig(bytes.NewReader(encoded))
	if err != nil || configuration.Width < 1 || configuration.Height < 1 || configuration.Width > 2048 || configuration.Height > 2048 {
		return "", badInput("Use a PNG or JPEG avatar up to 2048 pixels on each side.")
	}
	picture, _, err := image.Decode(bytes.NewReader(encoded))
	if err != nil {
		return "", badInput("Avatar cannot be decoded.")
	}
	relative := filepath.Join("bots", bot.ID, ".tom", "avatar-"+ids.New()+".png")
	root, err := os.OpenRoot(s.workspace)
	if err != nil {
		return "", err
	}
	defer func() { result = errors.Join(result, root.Close()) }()
	file, err := root.OpenFile(relative, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	encodeErr := png.Encode(file, picture)
	if err = errors.Join(encodeErr, file.Close()); err != nil {
		return "", errors.Join(err, root.Remove(relative))
	}
	committed := false
	defer func() {
		if !committed {
			result = errors.Join(result, root.Remove(relative))
		}
	}()
	path = "/api/v1/bots/" + id + "/avatar"
	result = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		if err := store.RequireChanged(tx.ExecContext(ctx, "UPDATE bots SET avatar_path=?,updated_at=? WHERE id=?", filepath.ToSlash(relative), time.Now().UnixMilli(), id)); err != nil {
			return err
		}
		if err := store.Audit(ctx, tx, actor, "bot.avatar", id, map[string]string{}); err != nil {
			return err
		}
		_, err := bus.Append(ctx, tx, "bot.updated", "", map[string]string{"bot_id": id})
		return err
	})
	if result != nil {
		return "", result
	}
	committed = true
	s.events.Notify()
	if bot.AvatarPath != nil {
		if err = root.Remove(*bot.AvatarPath); err != nil && !os.IsNotExist(err) {
			return path, err
		}
	}
	return path, nil
}

// Avatar 返回受工作区根约束的文件；调用方负责关闭读取句柄。
func (s *Bots) Avatar(ctx context.Context, id string) (*os.File, error) {
	// 打开句柄后旧文件可以安全回收，查版本到打开之间必须与替换互斥。
	s.mu.Lock()
	defer s.mu.Unlock()
	bot, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if bot.AvatarPath == nil {
		return nil, domain.NewError(404, "NOT_FOUND", "No avatar has been uploaded.")
	}
	root, err := os.OpenRoot(s.workspace)
	if err != nil {
		return nil, err
	}
	file, err := root.Open(*bot.AvatarPath)
	closeErr := root.Close()
	if err != nil || closeErr != nil {
		if file != nil {
			return nil, errors.Join(err, closeErr, file.Close())
		}
		return nil, errors.Join(err, closeErr)
	}
	return file, nil
}
