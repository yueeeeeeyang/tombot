package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
)

const MaxUploadBytes = 32 << 20

type TextFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Bytes   int    `json:"bytes"`
	SHA256  string `json:"sha256"`
}

// Text 提供完整原始文本供用户预览与编辑；模型的带行号窗口继续由 Read 提供。
func (f *Files) Text(ctx context.Context, botID, path string) (result TextFile, err error) {
	root, name, err := f.scope(botID, path)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	content, err := readText(ctx, root, name)
	if err != nil {
		if isCode(err, "FILE_TOO_LARGE") {
			return result, domain.NewError(413, "FILE_TOO_LARGE", "Text previews are limited to one MiB. Download the full file instead.")
		}
		return result, err
	}
	return TextFile{Path: f.displayPath(path, name), Content: string(content), Bytes: len(content), SHA256: digest(content)}, nil
}

// UpdateText 以读取时的摘要作并发条件，避免用户编辑覆盖 Bot 在预览后写入的内容。
func (f *Files) UpdateText(ctx context.Context, botID, path, content, expected string, before BeforeWrite) (WriteResult, error) {
	decoded, err := hex.DecodeString(expected)
	if err != nil || len(decoded) != sha256.Size {
		return WriteResult{}, failure("INVALID_INPUT", "Provide the SHA-256 of the text being edited.")
	}
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	return f.replace(ctx, botID, path, func(old []byte) ([]byte, error) {
		if digest(old) != strings.ToLower(expected) {
			return nil, domain.NewError(409, "FILE_CHANGED", "The file changed after it was opened. Reload before saving.")
		}
		return []byte(content), nil
	}, false, true, MaxTextBytes, true, before)
}

type Download struct {
	File       *os.File
	Path       string
	Name       string
	Size       int64
	ModifiedAt time.Time
}

// Download 固定已打开的普通文件句柄；调用方必须关闭，不能重新按路径打开。
func (f *Files) Download(ctx context.Context, botID, path string) (result Download, err error) {
	if err = ctx.Err(); err != nil {
		return result, err
	}
	root, name, err := f.scope(botID, path)
	if err != nil {
		return result, err
	}
	defer func() {
		err = errors.Join(err, root.Close())
		if err != nil && result.File != nil {
			err = errors.Join(err, result.File.Close())
			result.File = nil
		}
	}()
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return result, fileError(err)
	}
	result.File = file
	stat, err := file.Stat()
	if err != nil {
		return result, fileError(err)
	}
	if !stat.Mode().IsRegular() {
		return result, failure("NOT_REGULAR_FILE", "Only regular files can be downloaded.")
	}
	result.Path = f.displayPath(path, name)
	result.Name = filepath.Base(name)
	result.Size = stat.Size()
	result.ModifiedAt = stat.ModTime().UTC()
	return result, ctx.Err()
}

func validUploadName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, ".tom-write-") || len(name) > 240 || !utf8.ValidString(name) || strings.ContainsAny(name, "/\\") {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

type contextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(buffer)
}

// Upload 流式暂存完整上传，在最终 EOF 校验和审计通过后才发布；唯一子目录避免同名覆盖。
func (f *Files) Upload(ctx context.Context, botID, filename string, source io.Reader, before BeforeWrite) (result WriteResult, err error) {
	return f.upload(ctx, botID, ".", filename, source, before)
}

// UploadShared 将独立用户浏览器的文件保存到共享收件目录，沿用同一发布与路径校验规则。
func (f *Files) UploadShared(ctx context.Context, filename string, source io.Reader, before BeforeWrite) (WriteResult, error) {
	return f.upload(ctx, "user", "shared/", filename, source, before)
}

func (f *Files) upload(ctx context.Context, botID, scope, filename string, source io.Reader, before BeforeWrite) (result WriteResult, err error) {
	if !validUploadName(filename) {
		return result, failure("INVALID_FILENAME", "Provide a plain filename of at most 240 UTF-8 bytes without path separators or control characters.")
	}
	if source == nil || before == nil {
		return result, domain.NewError(500, "UPLOAD_DEPENDENCY_MISSING", "Upload source and audit callback are required.")
	}
	root, _, err := f.scope(botID, scope)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	uploadID := ids.New()
	target := filepath.ToSlash(filepath.Join("inbox", uploadID, filename))
	if scope == "shared/" {
		target = "shared/" + target
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if err = before(ctx, target, ""); err != nil {
		return result, err
	}
	if err = root.Mkdir("inbox", 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return result, fileError(err)
	}
	stat, err := root.Lstat("inbox")
	if err != nil {
		return result, fileError(err)
	}
	if !stat.IsDir() {
		return result, failure("PATH_OUT_OF_WORKSPACE", "The inbox must be a real directory.")
	}
	inbox, err := root.OpenRoot("inbox")
	if err != nil {
		return result, fileError(err)
	}
	defer func() { err = errors.Join(err, inbox.Close()) }()
	opened, err := inbox.Stat(".")
	if err != nil {
		return result, fileError(err)
	}
	if !os.SameFile(stat, opened) {
		return result, failure("PATH_OUT_OF_WORKSPACE", "The inbox changed while it was being opened.")
	}
	if err = inbox.Mkdir(uploadID, 0700); err != nil {
		return result, fileError(err)
	}
	published := false
	defer func() {
		if !published {
			err = errors.Join(err, inbox.Remove(uploadID))
		}
	}()
	destination, err := inbox.OpenRoot(uploadID)
	if err != nil {
		return result, fileError(err)
	}
	defer func() { err = errors.Join(err, destination.Close()) }()
	staging := ".tom-write-" + ids.New()
	file, err := destination.OpenFile(staging, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, fileError(err)
	}
	defer func() {
		removeErr := destination.Remove(staging)
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	hash := sha256.New()
	written, copyErr := io.CopyBuffer(io.MultiWriter(file, hash), io.LimitReader(contextReader{ctx, source}, MaxUploadBytes+1), make([]byte, 32<<10))
	if written > MaxUploadBytes {
		copyErr = domain.NewError(413, "UPLOAD_TOO_LARGE", "Uploads are limited to 32 MiB.")
	}
	var syncErr error
	if copyErr == nil && ctx.Err() == nil {
		syncErr = file.Sync()
	}
	if err = errors.Join(copyErr, syncErr, file.Close(), ctx.Err()); err != nil {
		return result, err
	}
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	if err = ctx.Err(); err != nil {
		return result, err
	}
	// Link 的目标必须不存在，阻止外部进程在发布前创建同名文件造成覆盖。
	if err = destination.Link(staging, filename); err != nil {
		return result, fileError(err)
	}
	published = true
	result = WriteResult{Path: target, Bytes: int(written), SHA256: hex.EncodeToString(hash.Sum(nil))}
	if err = destination.Remove(staging); err != nil {
		return result, fileError(err)
	}
	for _, directoryRoot := range []*os.Root{destination, inbox, root} {
		directory, openErr := directoryRoot.Open(".")
		if openErr != nil {
			return result, fileError(openErr)
		}
		if err = errors.Join(directory.Sync(), directory.Close()); err != nil {
			return result, err
		}
	}
	return result, err
}
