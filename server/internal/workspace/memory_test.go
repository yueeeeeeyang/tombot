package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"io.yueyang/tom/internal/domain"
)

func TestMemoryFilesRejectLinksSpecialFilesAndCapacity(t *testing.T) {
	for _, scenario := range []string{"symlink", "fifo", "directory", "file_size", "file_count", "total_bytes"} {
		t.Run(scenario, func(t *testing.T) {
			files, root := fixture(t)
			directory := filepath.Join(root, "bots/first/.tom/memory")
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(directory, "entry.md")
			switch scenario {
			case "symlink":
				if err := os.WriteFile(filepath.Join(root, "bots/second/private.md"), []byte("其他 Bot 内容"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(root, "bots/second/private.md"), target); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(target, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
			case "file_size":
				if err := os.WriteFile(target, []byte(strings.Repeat("x", domain.MaxMemoryFileBytes+1)), 0600); err != nil {
					t.Fatal(err)
				}
			case "file_count":
				for index := 0; index <= domain.MaxMemoryFiles; index++ {
					if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("%03d.md", index)), []byte("记录"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "total_bytes":
				for index := 0; index < 9; index++ {
					if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("%d.md", index)), []byte(strings.Repeat("x", domain.MaxMemoryFileBytes)), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := files.MemoryFiles(context.Background(), "first"); err == nil {
				t.Fatal("无效记忆目录被索引", scenario)
			}
		})
	}
}

func TestMemoryCreateConditionAndDirectorySymlinkGuard(t *testing.T) {
	files, root := fixture(t)
	directory := filepath.Join(root, "bots/first/.tom/memory")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	first, err := files.WriteMemory(context.Background(), "first", ".tom/memory/entry.md", "原文", "", audited)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = files.WriteMemory(context.Background(), "first", first.Path, "覆盖", "", audited); err == nil {
		t.Fatal("创建覆盖已有文件")
	}
	if err = files.DeleteMemory(context.Background(), "first", first.Path, first.SHA256, nil); err == nil {
		t.Fatal("无审计删除")
	}
	if err = os.Rename(directory, directory+"-saved"); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(directory+"-saved", directory); err != nil {
		t.Fatal(err)
	}
	if _, err = files.ReadMemory(context.Background(), "first", first.Path); err == nil {
		t.Fatal("允许链接记忆根目录")
	}
}
