//go:build linux && browserintegration

package sharedbrowser

import (
	"bytes"
	"errors"
	"os"
	"syscall"
	"testing"
)

func TestUploadStoreImmutableContentQuotaAndCleanup(t *testing.T) {
	store, err := NewUploadStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	files := []UploadFile{{Name: "中文.txt", Content: []byte("original")}}
	id, paths, err := store.Stage(t.Context(), files)
	if err != nil || len(paths) != 1 {
		t.Fatal(err)
	}
	files[0].Content[0] = 'X'
	content, err := os.ReadFile(paths[0])
	if err != nil || string(content) != "original" {
		t.Fatal("副本未固定输入内容", err)
	}
	stat, err := os.Stat(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	identity := stat.Sys().(*syscall.Stat_t)
	if stat.Mode().Perm() != 0440 || identity.Uid != 0 || identity.Gid != 20000 {
		t.Fatal("文件副本仍可被 Bot 修改")
	}
	if err := store.Release([]string{id}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("关闭页面未清理副本", err)
	}
	chunk := []UploadFile{{Name: "large.bin", Content: bytes.Repeat([]byte{1}, MaximumUploadBytes)}}
	for range maximumUploadStorage / MaximumUploadBytes {
		if _, _, err := store.Stage(t.Context(), chunk); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := store.Stage(t.Context(), chunk); !errors.Is(err, ErrCapacity) {
		t.Fatal("上传存储超限仍成功", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("服务关闭遗留副本", err)
	}
	if _, _, err := store.Stage(t.Context(), files); !errors.Is(err, ErrUnavailable) {
		t.Fatal("已关闭存储重新创建文件", err)
	}
}

func TestUploadStoreCleanupFailureIsNotIgnored(t *testing.T) {
	store, err := NewUploadStore()
	if err != nil {
		t.Fatal(err)
	}
	id, paths, err := store.Stage(t.Context(), []UploadFile{{Name: "file", Content: []byte("body")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(paths[0]); err != nil {
		t.Fatal(err)
	}
	if err := store.Release([]string{id}); err == nil {
		t.Fatal("遗失的副本被当作正常清理")
	}
	if _, _, err := store.Stage(t.Context(), []UploadFile{{Name: "next"}}); !errors.Is(err, ErrUnavailable) {
		t.Fatal("清理失败后继续接收上传", err)
	}
	if err := store.Close(); err == nil {
		t.Fatal("关闭丢失清理失败")
	}
	if err := os.RemoveAll(store.directory); err != nil {
		t.Fatal(err)
	}
}
