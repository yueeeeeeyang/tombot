package service

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"strings"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/store"
	"io.yueyang/tom/internal/workspace"
)

// Files 处理用户工作区请求的目标选择与审计，底层目录隔离由 workspace 统一执行。
type Files struct {
	db       *sql.DB
	bots     *Bots
	storage  *workspace.Files
	events   *bus.Bus
	memories *Memories
}

func NewFiles(db *sql.DB, bots *Bots, storage *workspace.Files, events *bus.Bus, memories *Memories) *Files {
	return &Files{db: db, bots: bots, storage: storage, events: events, memories: memories}
}

func (s *Files) target(ctx context.Context, botID, scope, path string) (string, string, error) {
	if scope == "" {
		if botID == "" {
			scope = "shared"
		} else {
			scope = "bot"
		}
	}
	if scope != "shared" && scope != "bot" {
		return "", "", badInput("Scope must be bot or shared.")
	}
	if scope == "bot" {
		if _, err := s.bots.Get(ctx, botID); err != nil {
			return "", "", err
		}
		return botID, path, nil
	}
	if strings.HasPrefix(path, "/") {
		return "", "", badInput("Shared paths must be relative to the shared workspace.")
	}
	if path == "shared" {
		path = ""
	} else {
		path = strings.TrimPrefix(path, "shared/")
	}
	// shared 根不会打开 Bot 目录，此标识仅供统一路径校验。
	return "user", "shared/" + strings.TrimPrefix(path, "/"), nil
}
func (s *Files) Tree(ctx context.Context, botID, scope, path string, depth int) (workspace.Listing, error) {
	botID, path, err := s.target(ctx, botID, scope, path)
	if err != nil {
		return workspace.Listing{}, err
	}
	timeout, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return s.storage.List(timeout, botID, path, depth)
}
func (s *Files) Read(ctx context.Context, botID, scope, path string) (workspace.TextFile, error) {
	botID, path, err := s.target(ctx, botID, scope, path)
	if err != nil {
		return workspace.TextFile{}, err
	}
	timeout, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return s.storage.Text(timeout, botID, path)
}

type FileWriteInput struct {
	BotID          string  `json:"bot_id"`
	Scope          string  `json:"scope"`
	Path           string  `json:"path"`
	Content        string  `json:"content"`
	CreateDirs     bool    `json:"create_dirs"`
	ExpectedSHA256 *string `json:"expected_sha256"`
}

func (s *Files) Write(ctx context.Context, input FileWriteInput, actor string) (workspace.WriteResult, error) {
	if input.BotID != "" {
		release, err := s.bots.beginFileWrite(ctx, input.BotID)
		if err != nil {
			return workspace.WriteResult{}, err
		}
		defer release()
	}
	botID, path, err := s.target(ctx, input.BotID, input.Scope, input.Path)
	if err != nil {
		return workspace.WriteResult{}, err
	}
	timeout, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	before := func(ctx context.Context, path, hash string) error {
		return store.Transact(ctx, s.db, func(tx *sql.Tx) error {
			return store.Audit(ctx, tx, actor, "fs.write", botID, map[string]string{"path": path, "old_sha256": hash})
		})
	}
	var result workspace.WriteResult
	if input.ExpectedSHA256 != nil {
		result, err = s.storage.UpdateText(timeout, botID, path, input.Content, *input.ExpectedSHA256, before)
	} else {
		result, err = s.storage.Write(timeout, botID, path, input.Content, input.CreateDirs, before)
	}
	if err != nil {
		var public *domain.Error
		if errors.As(err, &public) && public.Code == "FILE_TOO_LARGE" {
			return result, domain.NewError(413, "FILE_TOO_LARGE", "Text edits are limited to one MiB.")
		}
		return result, err
	}
	persist, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err = s.memories.FileChanged(persist, botID, result.Path); err != nil {
		return result, err
	}
	return result, s.changed(persist, input.BotID, result.Path, "fs.write.complete", actor, result.SHA256)
}

func (s *Files) changed(ctx context.Context, botID, path, action, actor, hash string) (err error) {
	defer func() {
		if err != nil {
			err = errors.Join(domain.NewError(500, "FILE_SAVED_EVENT_FAILED", "The file was saved but its completion record failed. Refresh before repeating the action."), err)
		}
	}()
	conversation := ""
	if botID != "" {
		bot, err := s.bots.Get(ctx, botID)
		if err != nil {
			return err
		}
		conversation = bot.ConversationID
	}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		if err := store.Audit(ctx, tx, actor, action, botID, map[string]string{"path": path, "sha256": hash}); err != nil {
			return err
		}
		_, err := bus.Append(ctx, tx, "artifact.created", conversation, map[string]string{"bot_id": botID, "path": path, "kind": "file"})
		return err
	})
	if err == nil {
		s.events.Notify()
	}
	return err
}

func (s *Files) Upload(ctx context.Context, botID, filename string, source io.Reader, actor string) (workspace.WriteResult, error) {
	if botID == "" {
		return s.uploadShared(ctx, filename, source, actor, "fs.upload.start", "fs.upload.complete")
	}
	release, err := s.bots.beginFileWrite(ctx, botID)
	if err != nil {
		return workspace.WriteResult{}, err
	}
	defer release()
	result, err := s.storage.Upload(ctx, botID, filename, source, func(ctx context.Context, path, hash string) error {
		return store.Transact(ctx, s.db, func(tx *sql.Tx) error {
			return store.Audit(ctx, tx, actor, "fs.upload.start", botID, map[string]string{"path": path})
		})
	})
	if err != nil {
		return result, err
	}
	persist, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return result, s.changed(persist, botID, result.Path, "fs.upload.complete", actor, result.SHA256)
}

func (s *Files) Download(ctx context.Context, botID, scope, path, actor string) (workspace.Download, error) {
	botID, path, err := s.target(ctx, botID, scope, path)
	if err != nil {
		return workspace.Download{}, err
	}
	file, err := s.storage.Download(ctx, botID, path)
	if err != nil {
		return file, err
	}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		return store.Audit(ctx, tx, actor, "fs.download", botID, map[string]any{"path": file.Path, "bytes": file.Size})
	})
	if err != nil {
		return workspace.Download{}, errors.Join(err, file.File.Close())
	}
	return file, nil
}

// SaveBrowserDownload 根据标签归属选择收件目录，独立用户标签进入共享目录；不会覆盖同名文件。
func (s *Files) SaveBrowserDownload(ctx context.Context, botID, filename string, source io.Reader, actor string) (workspace.WriteResult, error) {
	if botID != "" {
		return s.Upload(ctx, botID, filename, source, actor)
	}
	return s.uploadShared(ctx, filename, source, actor, "browser.download.save", "browser.download.saved")
}

func (s *Files) uploadShared(ctx context.Context, filename string, source io.Reader, actor, started, completed string) (workspace.WriteResult, error) {
	result, err := s.storage.UploadShared(ctx, filename, source, func(ctx context.Context, path, hash string) error {
		return store.Transact(ctx, s.db, func(tx *sql.Tx) error {
			return store.Audit(ctx, tx, actor, started, "shared", map[string]string{"path": path})
		})
	})
	if err != nil {
		return result, err
	}
	persist, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return result, s.changed(persist, "", result.Path, completed, actor, result.SHA256)
}
