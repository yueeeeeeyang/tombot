package service

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/testkit"
	"io.yueyang/tom/internal/workspace"
)

type gatedUpload struct {
	entered chan struct{}
	proceed chan struct{}
	started bool
	source  io.Reader
}

func (reader *gatedUpload) Read(buffer []byte) (int, error) {
	if !reader.started {
		reader.started = true
		close(reader.entered)
		<-reader.proceed
	}
	return reader.source.Read(buffer)
}

func TestUploadProtectsBotDeletionAndPublishesArtifact(t *testing.T) {
	ctx := context.Background()
	db := testkit.Database(t)
	events := bus.New(db)
	directory := t.TempDir()
	bots := NewBots(db, events, directory)
	bot, err := bots.Create(ctx, BotInput{Template: "writer"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	storage, err := workspace.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Error(err)
		}
	})
	files := NewFiles(db, bots, storage, events, NewMemories(db, bots, storage, events))
	reader := &gatedUpload{entered: make(chan struct{}), proceed: make(chan struct{}), source: strings.NewReader("报告")}
	done := make(chan error, 1)
	go func() { _, err := files.Upload(ctx, bot.ID, "report.txt", reader, "user"); done <- err }()
	select {
	case <-reader.entered:
	case err := <-done:
		t.Fatal("上传提前停止", err)
	case <-time.After(3 * time.Second):
		t.Fatal("上传未进入读取")
	}
	deleteErr := bots.Delete(ctx, bot.ID, bot.Name, "user", true)
	close(reader.proceed)
	var public *domain.Error
	if !errors.As(deleteErr, &public) || public.Code != "BOT_FILES_BUSY" {
		t.Fatal("上传期间可以删除 Bot", deleteErr)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	var artifacts int
	if err = db.QueryRow("SELECT COUNT(*) FROM events WHERE kind='artifact.created' AND conversation_id=?", bot.ConversationID).Scan(&artifacts); err != nil || artifacts != 1 {
		t.Fatal("上传完成没有产物事件", err)
	}
	if err = bots.Delete(ctx, bot.ID, bot.Name, "user", true); err != nil {
		t.Fatal("上传完成仍占用 Bot", err)
	}
}

func TestUploadAuditFailureAndCompletionFailure(t *testing.T) {
	for _, phase := range []string{"start", "complete"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			db := testkit.Database(t)
			events := bus.New(db)
			directory := t.TempDir()
			bots := NewBots(db, events, directory)
			bot, err := bots.Create(ctx, BotInput{Template: "writer"}, "user")
			if err != nil {
				t.Fatal(err)
			}
			storage, err := workspace.Open(directory)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := storage.Close(); err != nil {
					t.Error(err)
				}
			})
			if _, err = db.Exec(`CREATE TRIGGER reject_upload_audit BEFORE INSERT ON audit_log WHEN NEW.action='fs.upload.` + phase + `' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			files := NewFiles(db, bots, storage, events, NewMemories(db, bots, storage, events))
			result, err := files.Upload(ctx, bot.ID, "report.txt", strings.NewReader("报告"), "user")
			if err == nil {
				t.Fatal("审计失败未返回")
			}
			if phase == "start" {
				entries, readErr := os.ReadDir(filepath.Join(directory, "bots", bot.ID, "inbox"))
				if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
					t.Fatal(readErr)
				}
				if len(entries) != 0 {
					t.Fatal("审计失败仍写入文件")
				}
			} else {
				var public *domain.Error
				if !errors.As(err, &public) || public.Code != "FILE_SAVED_EVENT_FAILED" || result.Path == "" {
					t.Fatal("完成记录失败未说明已保存", err)
				}
				if _, err = files.Read(ctx, bot.ID, "bot", result.Path); err != nil {
					t.Fatal("已保存文件丢失", err)
				}
			}
		})
	}
}
