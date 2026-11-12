package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
)

const MaxTextBytes = 1 << 20
const MaxOutputBytes = 16 << 20

// Files 通过目录句柄约束每次打开，避免先校验路径再访问的符号链接竞争。
type Files struct {
	root            *os.Root
	path            string
	writeMu         sync.Mutex
	attachmentSlots chan struct{}
}

func Open(path string) (*Files, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, err
	}
	return &Files{root: root, path: absolute, attachmentSlots: make(chan struct{}, 2)}, nil
}
func (f *Files) Close() error            { return f.root.Close() }
func failure(code, message string) error { return domain.NewError(400, code, message) }

// Describe 返回统一策略目标；out/ 的任何已存在链接都会提升为普通写风险。
func (f *Files) Describe(botID, path string) (target string, isOutput bool, err error) {
	root, name, err := f.scope(botID, path)
	if err != nil {
		return "", false, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	target = f.displayPath(path, name)
	isOutput = strings.HasPrefix(target, "out/")
	current := "."
	for _, part := range strings.Split(name, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		stat, statErr := root.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			break
		}
		if statErr != nil {
			return "", false, fileError(statErr)
		}
		if stat.Mode()&os.ModeSymlink != 0 {
			isOutput = false
		}
	}
	return target, isOutput, nil
}

// SaveOutput 为工具完整结果保留专用容量，不改变普通文件写入接口的一 MiB 限制。
func (f *Files) SaveOutput(ctx context.Context, botID, callID string, content []byte, before BeforeWrite) (WriteResult, error) {
	if callID == "" || strings.ContainsAny(callID, "/\\\x00") || callID == "." || callID == ".." {
		return WriteResult{}, failure("INVALID_INPUT", "A valid tool call identifier is required.")
	}
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	return f.replace(ctx, botID, ".tom/tmp/tool-"+callID+".json", func([]byte) ([]byte, error) { return content, nil }, true, false, MaxOutputBytes, true, before)
}

// scope 同时接受 Bot 相对路径、shared/ 路径以及这两个根内的绝对路径。
func (f *Files) scope(botID, path string) (*os.Root, string, error) {
	if botID == "" || strings.ContainsAny(botID, "/\\\x00") || botID == "." || botID == ".." {
		return nil, "", failure("INVALID_BOT", "A valid bot identifier is required.")
	}
	if strings.ContainsAny(path, "\\\x00") || len(path) > 4096 {
		return nil, "", failure("PATH_OUT_OF_WORKSPACE", "The path is outside the permitted workspace.")
	}
	base := filepath.Join("bots", botID)
	if filepath.IsAbs(path) {
		shared := filepath.Join(f.path, "shared")
		own := filepath.Join(f.path, base)
		if rel, err := filepath.Rel(shared, path); err == nil && filepath.IsLocal(rel) {
			base = "shared"
			path = rel
		} else if rel, err = filepath.Rel(own, path); err == nil && filepath.IsLocal(rel) {
			path = rel
		} else {
			return nil, "", failure("PATH_OUT_OF_WORKSPACE", "The path is outside the permitted workspace.")
		}
	} else if path == "shared" || strings.HasPrefix(path, "shared/") {
		base = "shared"
		path = strings.TrimPrefix(strings.TrimPrefix(path, "shared"), "/")
	}
	for _, segment := range strings.Split(filepath.ToSlash(path), "/") {
		if segment == ".." {
			return nil, "", failure("PATH_OUT_OF_WORKSPACE", "Parent traversal is not allowed.")
		}
	}
	if path == "" {
		path = "."
	}
	path = filepath.Clean(path)
	if !filepath.IsLocal(path) {
		return nil, "", failure("PATH_OUT_OF_WORKSPACE", "The path is outside the permitted workspace.")
	}
	root, err := f.root.OpenRoot(base)
	if err != nil {
		return nil, "", fileError(err)
	}
	return root, path, nil
}

func fileError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return domain.NewError(404, "FILE_NOT_FOUND", "The file or directory does not exist.")
	}
	if errors.Is(err, os.ErrPermission) {
		return failure("FILE_PERMISSION_DENIED", "The file cannot be accessed with current permissions.")
	}
	return failure("FILE_ACCESS_FAILED", "The workspace operation failed. Check the path and file permissions.")
}

func readText(ctx context.Context, root *os.Root, path string) (content []byte, result error) {
	return readTextLimit(ctx, root, path, MaxTextBytes)
}

func readTextLimit(ctx context.Context, root *os.Root, path string, maximum int) ([]byte, error) {
	content, err := readRegular(ctx, root, path, maximum)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(content) || strings.IndexByte(string(content), 0) >= 0 {
		return nil, failure("NOT_TEXT_FILE", "The file is not UTF-8 text.")
	}
	return content, nil
}

func readRegular(ctx context.Context, root *os.Root, path string, maximum int) (content []byte, result error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 非阻塞打开后再检查类型，FIFO 或设备即使在检查间被替换也不会挂住执行器。
	file, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fileError(err)
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	stat, err := file.Stat()
	if err != nil {
		return nil, fileError(err)
	}
	if !stat.Mode().IsRegular() {
		return nil, failure("NOT_TEXT_FILE", "Only regular UTF-8 text files are supported.")
	}
	if stat.Size() > int64(maximum) {
		return nil, failure("FILE_TOO_LARGE", "The text file exceeds the permitted size limit.")
	}
	content, err = io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil {
		return nil, fileError(err)
	}
	if len(content) > maximum {
		return nil, failure("FILE_TOO_LARGE", "The text file exceeds the permitted size limit.")
	}

	return content, ctx.Err()
}

type ReadResult struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
	TotalLines int    `json:"total_lines"`
	Truncated  bool   `json:"truncated"`
	SHA256     string `json:"sha256"`
}

func digest(content []byte) string { sum := sha256.Sum256(content); return hex.EncodeToString(sum[:]) }

// Read 返回带行号的有界文本窗口，调用方可用返回的总行数继续分页。
func (f *Files) Read(ctx context.Context, botID, path string, start, limit int) (result ReadResult, err error) {
	if start == 0 {
		start = 1
	}
	if limit == 0 {
		limit = 2000
	}
	if start < 1 || start > MaxOutputBytes+1 || limit < 1 || limit > 2000 {
		return result, failure("INVALID_INPUT", "Line bounds must be positive and the limit must not exceed 2000.")
	}
	root, name, err := f.scope(botID, path)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	maximum := MaxTextBytes
	// 完整工具结果有独立容量，仍使用同一目录边界和文本校验；普通文件不扩大限制。
	if (strings.HasPrefix(filepath.ToSlash(name), ".tom/tmp/tool-") && strings.HasSuffix(name, ".json")) || (strings.HasPrefix(filepath.ToSlash(name), ".tom/tmp/out-") && strings.HasSuffix(name, ".log")) {
		maximum = MaxOutputBytes
	}
	content, err := readTextLimit(ctx, root, name, maximum)
	if err != nil {
		return result, err
	}
	lines := strings.Split(string(content), "\n")
	if len(content) == 0 {
		lines = nil
	} else if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	end := min(len(lines), start-1+limit)
	var numbered strings.Builder
	for index := min(start-1, len(lines)); index < end; index++ {
		fmt.Fprintf(&numbered, "%d\t%s\n", index+1, lines[index])
	}
	return ReadResult{Path: path, Content: numbered.String(), StartLine: start, EndLine: end, TotalLines: len(lines), Truncated: end < len(lines), SHA256: digest(content)}, nil
}

type WriteResult struct {
	Path      string `json:"path"`
	Bytes     int    `json:"bytes"`
	OldSHA256 string `json:"old_sha256"`
	SHA256    string `json:"sha256"`
}
type BeforeWrite func(context.Context, string, string) error

// Write 在同目录暂存后原子替换；审计回调先记录旧摘要，失败时不会修改目标。
func (f *Files) Write(ctx context.Context, botID, path, content string, createDirs bool, before BeforeWrite) (result WriteResult, err error) {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	return f.replace(ctx, botID, path, func([]byte) ([]byte, error) { return []byte(content), nil }, createDirs, false, MaxTextBytes, true, before)
}

// Edit 在写锁内读取并校验匹配唯一性，避免两个编辑同时基于旧内容覆盖对方。
func (f *Files) Edit(ctx context.Context, botID, path, oldText, newText string, before BeforeWrite) (result WriteResult, err error) {
	if oldText == "" {
		return result, failure("INVALID_INPUT", "The exact text to replace must not be empty.")
	}
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	return f.replace(ctx, botID, path, func(content []byte) ([]byte, error) {
		count := strings.Count(string(content), oldText)
		if count == 0 {
			return nil, failure("EDIT_NOT_FOUND", "The exact text was not found.")
		}
		if count > 1 {
			return nil, failure("EDIT_NOT_UNIQUE", "The exact text occurs more than once.")
		}
		return []byte(strings.Replace(string(content), oldText, newText, 1)), nil
	}, false, true, MaxTextBytes, true, before)
}

func (f *Files) replace(ctx context.Context, botID, path string, transform func([]byte) ([]byte, error), createDirs, mustExist bool, maximum int, textOnly bool, before BeforeWrite) (result WriteResult, err error) {
	root, name, err := f.scope(botID, path)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	return f.replaceRoot(ctx, root, name, path, transform, createDirs, mustExist, maximum, textOnly, before)
}

func (f *Files) replaceRoot(ctx context.Context, root *os.Root, name, path string, transform func([]byte) ([]byte, error), createDirs, mustExist bool, maximum int, textOnly bool, before BeforeWrite) (result WriteResult, err error) {
	if name == "." {
		return result, failure("INVALID_INPUT", "A file path is required.")
	}
	var old []byte
	stat, statErr := root.Lstat(name)
	if statErr == nil {
		if !stat.Mode().IsRegular() {
			return result, failure("NOT_TEXT_FILE", "Writes require a regular file, not a symlink or directory.")
		}
		old, err = readRegular(ctx, root, name, maximum)
		if err != nil {
			return result, err
		}
		if textOnly && (!utf8.Valid(old) || strings.IndexByte(string(old), 0) >= 0) {
			return result, failure("NOT_TEXT_FILE", "The file is not UTF-8 text.")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) || mustExist {
		return result, fileError(statErr)
	}
	content, err := transform(old)
	if err != nil {
		return result, err
	}
	if len(content) > maximum {
		return result, failure("FILE_TOO_LARGE", "The text content exceeds the permitted size limit.")
	}
	if textOnly && (!utf8.Valid(content) || strings.IndexByte(string(content), 0) >= 0) {
		return result, failure("NOT_TEXT_FILE", "Only UTF-8 text without NUL bytes can be written.")
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	parent := filepath.Dir(name)
	if !createDirs {
		if _, err = root.Stat(parent); err != nil {
			return result, failure("PARENT_MISSING", "The parent directory does not exist; set create_dirs to create it.")
		}
	}
	oldHash := ""
	if statErr == nil {
		oldHash = digest(old)
	}
	if before == nil {
		return result, domain.NewError(500, "AUDIT_REQUIRED", "A write audit callback is required.")
	}
	if err = before(ctx, path, oldHash); err != nil {
		return result, err
	}
	if createDirs {
		if err = root.MkdirAll(parent, 0700); err != nil {
			return result, fileError(err)
		}
	}
	staging := filepath.Join(parent, ".tom-write-"+ids.New())
	file, err := root.OpenFile(staging, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, fileError(err)
	}
	defer func() {
		removeErr := root.Remove(staging)
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	_, writeErr := file.Write(content)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err = errors.Join(writeErr, syncErr, closeErr, ctx.Err()); err != nil {
		return result, err
	}
	if err = root.Rename(staging, name); err != nil {
		return result, fileError(err)
	}
	directory, err := root.Open(parent)
	if err != nil {
		return result, fileError(err)
	}
	err = errors.Join(directory.Sync(), directory.Close())
	if err != nil {
		return result, err
	}
	return WriteResult{Path: path, Bytes: len(content), OldSHA256: oldHash, SHA256: digest(content)}, nil
}

func (f *Files) displayPath(requested, path string) string {
	isShared := requested == "shared" || strings.HasPrefix(requested, "shared/")
	if filepath.IsAbs(requested) {
		relative, err := filepath.Rel(filepath.Join(f.path, "shared"), requested)
		isShared = err == nil && filepath.IsLocal(relative)
	}
	if isShared {
		return filepath.ToSlash(filepath.Join("shared", path))
	}
	return filepath.ToSlash(path)
}

// Directory 通过目录句柄验证工作目录，再返回已解析路径供受审批约束的子进程使用。
func (f *Files) Directory(botID, path string) (absolute string, err error) {
	root, name, err := f.scope(botID, path)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	directory, err := root.OpenRoot(name)
	if err != nil {
		return "", fileError(err)
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	absolute, err = filepath.EvalSymlinks(directory.Name())
	if err != nil {
		return "", fileError(err)
	}
	// macOS 的 /var、/tmp 本身是系统链接；保留配置根的拼写，仅归一化根内目标。
	physicalRoot, err := filepath.EvalSymlinks(f.path)
	if err != nil {
		return "", fileError(err)
	}
	relative, err := filepath.Rel(physicalRoot, absolute)
	if err != nil || !filepath.IsLocal(relative) {
		return "", failure("PATH_OUT_OF_WORKSPACE", "The directory is outside the permitted workspace.")
	}
	return filepath.Join(f.path, relative), nil
}

// CommandHome 为受审批保护的命令准备私有临时目录，避免继承宿主进程的主目录。
func (f *Files) CommandHome(botID string) (home string, err error) {
	return f.commandHome(botID, ".")
}

// SharedCommandHome 将用户终端的主目录和临时目录限定在共享工作区。
func (f *Files) SharedCommandHome() (string, error) {
	return f.commandHome("user", "shared")
}

func (f *Files) commandHome(botID, path string) (home string, err error) {
	root, _, err := f.scope(botID, path)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	if err = root.MkdirAll(".tom/tmp", 0700); err != nil {
		return "", fileError(err)
	}
	return f.Directory(botID, path)
}

// SaveShellOutput 保留脱敏后的完整命令输出，容量与工具结果一致。
func (f *Files) SaveShellOutput(ctx context.Context, botID, outputID, content string, before BeforeWrite) (WriteResult, error) {
	if outputID == "" || strings.ContainsAny(outputID, "/\\\x00") || outputID == "." || outputID == ".." {
		return WriteResult{}, failure("INVALID_INPUT", "A valid output identifier is required.")
	}
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	return f.replace(ctx, botID, ".tom/tmp/out-"+outputID+".log", func([]byte) ([]byte, error) { return []byte(content), nil }, true, false, MaxOutputBytes, false, before)
}
