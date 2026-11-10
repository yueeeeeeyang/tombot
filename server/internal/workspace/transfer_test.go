package workspace

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

type zeroReader struct{}

func (zeroReader) Read(buffer []byte) (int, error) { clear(buffer); return len(buffer), nil }

type brokenReader struct{}

func (brokenReader) Read(buffer []byte) (int, error) { return 0, errors.New("上传中断") }

func TestSharedDownloadPublicationUsesSharedRootAndUniqueNames(t *testing.T) {
	f, root := fixture(t)
	ctx := context.Background()
	paths := map[string]bool{}
	for range 2 {
		result, err := f.UploadShared(ctx, "下载.txt", strings.NewReader("共享下载"), audited)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(result.Path, "shared/inbox/") || paths[result.Path] {
			t.Fatal("共享文件路径不隔离或覆盖同名文件")
		}
		paths[result.Path] = true
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(result.Path)))
		if err != nil || string(content) != "共享下载" {
			t.Fatal("共享下载未落盘", err)
		}
	}
	if _, err := f.UploadShared(ctx, "../escape", strings.NewReader("invalid"), audited); err == nil {
		t.Fatal("共享文件名允许路径穿越")
	}
	if _, err := f.UploadShared(ctx, "failed.bin", brokenReader{}, audited); err == nil {
		t.Fatal("失败流发布成功")
	}
	entries, err := os.ReadDir(filepath.Join(root, "shared", "inbox"))
	if err != nil || len(entries) != 2 {
		t.Fatal("失败共享下载留下文件", err)
	}
}

func TestUploadBinaryUniqueAndDownload(t *testing.T) {
	f, root := fixture(t)
	ctx := context.Background()
	content := []byte{0, 255, 128, 10, 12}
	paths := make(chan string, 4)
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			result, err := f.Upload(ctx, "first", "报告.bin", bytes.NewReader(content), audited)
			if err != nil {
				t.Error(err)
				return
			}
			if result.Bytes != len(content) || result.SHA256 != digest(content) || !strings.HasPrefix(result.Path, "inbox/") {
				t.Error("上传结果不正确")
			}
			paths <- result.Path
		})
	}
	workers.Wait()
	close(paths)
	unique := map[string]bool{}
	for path := range paths {
		if unique[path] {
			t.Fatal("同名上传覆盖")
		}
		unique[path] = true
		file, err := f.Download(ctx, "first", path)
		if err != nil {
			t.Fatal(err)
		}
		stored, readErr := io.ReadAll(file.File)
		closeErr := file.File.Close()
		if err = errors.Join(readErr, closeErr); err != nil || !bytes.Equal(stored, content) || file.Size != int64(len(content)) || file.Name != "报告.bin" {
			t.Fatal("下载与上传不符", err)
		}
		stat, err := os.Stat(filepath.Join(root, "bots/first", filepath.FromSlash(path)))
		if err != nil || stat.Mode().Perm() != 0600 {
			t.Fatal("上传权限不正确", err)
		}
	}
	if len(unique) != 4 {
		t.Fatal("并发上传缺失")
	}
	result, err := f.Upload(ctx, "first", "empty.txt", strings.NewReader(""), audited)
	if err != nil || result.Bytes != 0 {
		t.Fatal("空文件上传失败", err)
	}
}

func TestUploadBoundariesAndFailedPublication(t *testing.T) {
	for _, name := range []string{"../escape", "a/b.txt", "a\\b", ".", "..", "bad\nname", ".tom-write-hidden", strings.Repeat("a", 241), string([]byte{255})} {
		f, _ := fixture(t)
		_, err := f.Upload(context.Background(), "first", name, strings.NewReader("content"), audited)
		requireCode(t, err, "INVALID_FILENAME")
	}
	for _, mode := range []string{"limit", "too_large", "reader", "audit", "cancel", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			f, root := fixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var source io.Reader = strings.NewReader("report")
			audit := BeforeWrite(audited)
			switch mode {
			case "limit":
				source = io.LimitReader(zeroReader{}, MaxUploadBytes)
			case "too_large":
				source = io.LimitReader(zeroReader{}, MaxUploadBytes+1)
			case "reader":
				source = io.MultiReader(strings.NewReader("partial"), brokenReader{})
			case "audit":
				audit = func(context.Context, string, string) error { return errors.New("审计不可用") }
			case "cancel":
				cancel()
			case "symlink":
				if err := os.Symlink("out", filepath.Join(root, "bots/first/inbox")); err != nil {
					t.Fatal(err)
				}
			}
			result, err := f.Upload(ctx, "first", "report.bin", source, audit)
			if mode == "limit" {
				if err != nil || result.Bytes != MaxUploadBytes {
					t.Fatal("边界容量失败", err)
				}
				return
			}
			if err == nil {
				t.Fatal("失败上传被发布")
			}
			if mode == "too_large" {
				requireCode(t, err, "UPLOAD_TOO_LARGE")
			}
			if mode == "symlink" {
				requireCode(t, err, "PATH_OUT_OF_WORKSPACE")
				return
			}
			entries, readErr := os.ReadDir(filepath.Join(root, "bots/first/inbox"))
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatal("失败上传残留可见文件或临时目录")
			}
		})
	}
}

func TestDownloadRejectsEscapesAndSpecialFiles(t *testing.T) {
	f, root := fixture(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "bots/first/escape")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "bots/first/pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../second/private", "escape/private", "pipe", "out"} {
		result, err := f.Download(context.Background(), "first", path)
		if err == nil || result.File != nil {
			t.Fatal("特殊或越界文件可下载", path)
		}
	}
}

func TestRawTextAndOptimisticEdits(t *testing.T) {
	f, _ := fixture(t)
	ctx := context.Background()
	content := "第一行\n\tsecond\n"
	original, err := f.Write(ctx, "first", "out/report.md", content, false, audited)
	if err != nil {
		t.Fatal(err)
	}
	text, err := f.Text(ctx, "first", "out/report.md")
	if err != nil || text.Content != content || text.Bytes != len(content) || text.SHA256 != original.SHA256 {
		t.Fatal("原文预览被改写", err)
	}
	updated, err := f.UpdateText(ctx, "first", "out/report.md", "新内容", text.SHA256, audited)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.UpdateText(ctx, "first", "out/report.md", "过期编辑", text.SHA256, audited)
	requireCode(t, err, "FILE_CHANGED")
	current, err := f.Text(ctx, "first", "out/report.md")
	if err != nil || current.SHA256 != updated.SHA256 {
		t.Fatal("冲突仍覆盖原文件", err)
	}
	_, err = f.UpdateText(ctx, "first", "out/report.md", "content", "invalid", audited)
	requireCode(t, err, "INVALID_INPUT")
}
