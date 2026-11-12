//go:build linux

package computertools

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/sharedbrowser"
)

// readBrowserFiles 用 Agent 当前 UID 读取普通文件；共享文件允许正常链接，打开后使用同一描述符。
// 非阻塞打开避免 FIFO 卡住工具；拒绝设备、超限及读取期间可观测的文件变化。
func readBrowserFiles(ctx context.Context, paths []string) ([]sharedbrowser.UploadFile, error) {
	if len(paths) < 1 || len(paths) > sharedbrowser.MaximumUploadFiles {
		return nil, sharedbrowser.ErrInvalid
	}
	files := make([]sharedbrowser.UploadFile, 0, len(paths))
	remaining := int64(sharedbrowser.MaximumUploadBytes)
	for _, path := range paths {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if path == "" || len(path) > 4096 || strings.ContainsAny(path, "\x00\r\n") {
			return nil, sharedbrowser.ErrInvalid
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join("/workspace", path)
		}
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, sharedbrowser.ErrInvalid
		}
		file := os.NewFile(uintptr(fd), "browser-upload-source")
		content, readError := readBrowserFile(ctx, file, remaining)
		closeError := file.Close()
		if closeError != nil {
			return nil, errors.Join(sharedbrowser.ErrUnavailable, closeError)
		}
		if readError != nil {
			return nil, readError
		}
		remaining -= int64(len(content))
		files = append(files, sharedbrowser.UploadFile{Name: filepath.Base(path), Content: content})
	}
	if err := (sharedbrowser.UploadRequest{Sequence: 1, Selector: "input", Files: files}).Validate(); err != nil {
		return nil, err
	}
	return files, nil
}

func readBrowserFile(ctx context.Context, file *os.File, maximum int64) ([]byte, error) {
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return nil, sharedbrowser.ErrInvalid
	}
	if before.Size() < 0 || before.Size() > maximum {
		return nil, sharedbrowser.ErrCapacity
	}
	content := make([]byte, 0, int(before.Size()))
	buffer := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count, err := file.Read(buffer)
		if int64(len(content)+count) > maximum {
			return nil, sharedbrowser.ErrCapacity
		}
		content = append(content, buffer[:count]...)
		if errors.Is(err, io.EOF) {
			break
		}
		if count == 0 && err == nil {
			return nil, sharedbrowser.ErrInvalid
		}
		if err != nil {
			return nil, sharedbrowser.ErrInvalid
		}
	}
	after, err := file.Stat()
	if err != nil || before.Size() != after.Size() || before.ModTime() != after.ModTime() || int64(len(content)) != before.Size() {
		return nil, sharedbrowser.ErrConflict
	}
	return content, nil
}
