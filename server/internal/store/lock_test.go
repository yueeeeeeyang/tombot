package store_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"io.yueyang/tom/internal/store"
)

func TestDataDirectoryHasSingleOwner(t *testing.T) {
	directory := t.TempDir()
	first, err := store.LockDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := store.LockDirectory(directory); err == nil {
		if closeErr := duplicate.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("重复写进程被允许")
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := store.LockDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err = next.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDataLockRejectsSymlinksAndSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, ".tom.lock")
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, lock); err != nil {
		t.Fatal(err)
	}
	if file, err := store.LockDirectory(dir); err == nil {
		t.Fatal("接受锁文件符号链接", file.Name())
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(lock, 0600); err != nil {
		t.Fatal(err)
	}
	if file, err := store.LockDirectory(dir); err == nil {
		t.Fatal("接受 FIFO 锁", file.Name())
	}
}
