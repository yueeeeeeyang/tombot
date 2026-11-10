package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"io.yueyang/tom/internal/backup"
	"io.yueyang/tom/internal/config"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/store"
)

func TestArchiveCommandValidationAndOutput(t *testing.T) {
	base := t.TempDir()
	cfg := config.Config{DataDir: filepath.Join(base, "data"), WorkspaceDir: filepath.Join(base, "work")}
	for _, args := range [][]string{nil, {"unknown"}, {"backup", "-o"}, {"backup", "file"}, {"restore", "--input", "archive"}, {"restore", "--unknown"}} {
		if err := archiveCommand(t.Context(), cfg, args, "", io.Discard); err == nil {
			t.Fatalf("接受无效参数 %v", args)
		}
	}
	db, err := store.Open(t.Context(), filepath.Join(cfg.DataDir, "tom.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadKey(cfg.DataDir, "")
	if err != nil {
		t.Fatal(err)
	}
	clear(key)
	if err = os.Mkdir(cfg.WorkspaceDir, 0700); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(base, "snapshot")
	var output bytes.Buffer
	if err = archiveCommand(t.Context(), cfg, []string{"backup", "-o", archive}, "", &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), backup.KeyNotice("")) {
		t.Fatal("遗漏文件密钥保管提示")
	}
	if err = archiveCommand(t.Context(), cfg, []string{"restore", "--input", archive, "--target", filepath.Join(cfg.WorkspaceDir, "new")}, "", io.Discard); err == nil {
		t.Fatal("向正在使用的工作区写入还原目录")
	}
	if err = archiveCommand(t.Context(), cfg, []string{"restore", "--input", archive, "--target", filepath.Join(base, "new")}, "", io.Discard); err != nil {
		t.Fatal(err)
	}
}
