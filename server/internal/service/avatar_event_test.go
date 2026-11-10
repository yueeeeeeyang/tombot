package service

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/testkit"
)

func TestAvatarReadersKeepValidHandleDuringReplacement(t *testing.T) {
	ctx := context.Background()
	db := testkit.Database(t)
	bots := NewBots(db, bus.New(db), t.TempDir())
	bot, err := bots.Create(ctx, BotInput{Template: "researcher"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	replace := func() error {
		_, err := bots.SetAvatar(ctx, bot.ID, "user", bytes.NewReader(picture.Bytes()))
		return err
	}
	if err := replace(); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 3 {
		workers.Go(func() {
			for range 20 {
				file, err := bots.Avatar(ctx, bot.ID)
				if err != nil {
					t.Error(err)
					return
				}
				_, err = png.Decode(file)
				if err := errors.Join(err, file.Close()); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	for range 20 {
		if err := replace(); err != nil {
			t.Error(err)
			break
		}
	}
	workers.Wait()
}

func TestAvatarEventFailureKeepsPreviousFileAndRevision(t *testing.T) {
	ctx := context.Background()
	db := testkit.Database(t)
	workspace := t.TempDir()
	events := bus.New(db)
	bots := NewBots(db, events, workspace)
	bot, err := bots.Create(ctx, BotInput{Template: "researcher"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	if _, err = bots.SetAvatar(ctx, bot.ID, "user", bytes.NewReader(picture.Bytes())); err != nil {
		t.Fatal(err)
	}
	first, err := bots.Get(ctx, bot.ID)
	if err != nil || first.AvatarPath == nil {
		t.Fatal("缺少头像版本", err)
	}
	if _, err = db.Exec(`CREATE TRIGGER fail_avatar_event BEFORE INSERT ON events WHEN NEW.kind='bot.updated' BEGIN SELECT RAISE(ABORT,'event unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	wake := events.Wake()
	if _, err = bots.SetAvatar(ctx, bot.ID, "user", bytes.NewReader(picture.Bytes())); err == nil {
		t.Fatal("事件失败仍确认上传成功")
	}
	select {
	case <-wake:
		t.Fatal("回滚后错误通知客户端")
	default:
	}
	after, err := bots.Get(ctx, bot.ID)
	if err != nil || after.AvatarPath == nil || *after.AvatarPath != *first.AvatarPath || after.UpdatedAt != first.UpdatedAt {
		t.Fatal("失败写入改变头像", err)
	}
	files, err := filepath.Glob(filepath.Join(workspace, bot.WorkspaceDir, ".tom", "avatar-*.png"))
	if err != nil || len(files) != 1 {
		t.Fatal("留下未发布的头像", files, err)
	}
	if _, err = db.Exec(`DROP TRIGGER fail_avatar_event`); err != nil {
		t.Fatal(err)
	}
	if _, err = bots.SetAvatar(ctx, bot.ID, "user", bytes.NewReader(picture.Bytes())); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("成功替换没有唤醒订阅者")
	}
	if _, err = os.Stat(filepath.Join(workspace, *first.AvatarPath)); !os.IsNotExist(err) {
		t.Fatal("旧头像未回收", err)
	}
	var updates, audits int
	if err = db.QueryRow(`SELECT (SELECT COUNT(*) FROM events WHERE kind='bot.updated' AND json_extract(payload_json,'$.bot_id')=?),(SELECT COUNT(*) FROM audit_log WHERE action='bot.avatar')`, bot.ID).Scan(&updates, &audits); err != nil || updates != 2 || audits != 2 {
		t.Fatal("事件与审计未原子提交", updates, audits, err)
	}
}
