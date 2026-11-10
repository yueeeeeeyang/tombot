package workspace

import (
	"context"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/memory"
)

type MemoryFile struct {
	TextFile
	ModifiedAt time.Time
}

func (f *Files) memoryRoot(botID string) (directory *os.Root, result error) {
	root, _, err := f.scope(botID, ".")
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			result = errors.Join(result, closeErr)
			if directory != nil {
				result = errors.Join(result, directory.Close())
				directory = nil
			}
		}
	}()
	for _, name := range []string{".tom", ".tom/memory"} {
		stat, err := root.Lstat(name)
		if err != nil {
			return nil, fileError(err)
		}
		if !stat.IsDir() || stat.Mode()&os.ModeSymlink != 0 {
			return nil, failure("MEMORY_PATH_INVALID", "The private memory directory must not be a symlink.")
		}
	}
	directory, err = root.OpenRoot(".tom/memory")
	return directory, fileError(err)
}

func readMemory(ctx context.Context, root *os.Root, name string) (result MemoryFile, err error) {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return result, fileError(err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	stat, err := file.Stat()
	if err != nil {
		return result, err
	}
	if !stat.Mode().IsRegular() {
		return result, failure("MEMORY_PATH_INVALID", "Memory entries must be ordinary files, not links or directories.")
	}
	if stat.Size() > domain.MaxMemoryFileBytes {
		return result, failure("MEMORY_CAPACITY", "A memory file exceeds one MiB.")
	}
	content, err := io.ReadAll(io.LimitReader(file, domain.MaxMemoryFileBytes+1))
	if err != nil {
		return result, err
	}
	if len(content) > domain.MaxMemoryFileBytes {
		return result, failure("MEMORY_CAPACITY", "A memory file exceeds one MiB.")
	}
	result = MemoryFile{TextFile: TextFile{Path: memory.Directory + name, Content: string(content), Bytes: len(content), SHA256: digest(content)}, ModifiedAt: stat.ModTime().UTC()}
	return result, ctx.Err()
}

// MemoryFiles 扫描平铺的私有 Markdown 目录，同时限制条目数和总容量，避免短词回退产生无界加载。
func (f *Files) MemoryFiles(ctx context.Context, botID string) (files []MemoryFile, result error) {
	root, err := f.memoryRoot(botID)
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, root.Close()) }()
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, dir.Close()) }()
	files = []MemoryFile{}
	entries, total := 0, 0
	for {
		batch, readErr := dir.ReadDir(64)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, readErr
		}
		for _, entry := range batch {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			entries++
			if entries > domain.MaxMemoryFiles*4 {
				return nil, failure("MEMORY_CAPACITY", "The memory directory contains too many entries.")
			}
			if !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			if !memory.ValidPath(memory.Directory + entry.Name()) {
				return nil, failure("MEMORY_PATH_INVALID", "A memory file has an unsupported name.")
			}
			if len(files) >= domain.MaxMemoryFiles {
				return nil, failure("MEMORY_CAPACITY", "A bot supports at most 512 memory files.")
			}
			file, err := readMemory(ctx, root, entry.Name())
			if err != nil {
				return nil, err
			}
			total += file.Bytes
			if total > domain.MaxMemoryTotalBytes {
				return nil, failure("MEMORY_CAPACITY", "The bot's memory exceeds eight MiB.")
			}
			files = append(files, file)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// ReadMemory 返回指定 Bot 私有普通文件的正文与摘要；链接、特殊文件和越界路径均返回错误。
func (f *Files) ReadMemory(ctx context.Context, botID, path string) (file MemoryFile, result error) {
	if !memory.ValidPath(path) {
		return file, failure("MEMORY_PATH_INVALID", "Use a Markdown file directly under .tom/memory/.")
	}
	root, err := f.memoryRoot(botID)
	if err != nil {
		return file, err
	}
	defer func() { result = errors.Join(result, root.Close()) }()
	return readMemory(ctx, root, strings.TrimPrefix(path, memory.Directory))
}

// WriteMemory 的 expected 为空表示只创建；否则以原文件摘要进行条件替换，审批后的覆盖必须重新校验。
func (f *Files) WriteMemory(ctx context.Context, botID, path, content, expected string, before BeforeWrite) (write WriteResult, result error) {
	if !memory.ValidPath(path) {
		return write, failure("MEMORY_PATH_INVALID", "Use a Markdown file directly under .tom/memory/.")
	}
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	root, err := f.memoryRoot(botID)
	if err != nil {
		return write, err
	}
	defer func() { result = errors.Join(result, root.Close()) }()
	check := func(ctx context.Context, path, hash string) error {
		if hash != expected {
			return domain.NewError(409, "MEMORY_CHANGED", "The memory changed or already exists. Read it before writing again.")
		}
		if before == nil {
			return domain.NewError(500, "AUDIT_REQUIRED", "A memory mutation requires an audit callback.")
		}
		return before(ctx, path, hash)
	}
	return f.replaceRoot(ctx, root, strings.TrimPrefix(path, memory.Directory), path, func([]byte) ([]byte, error) { return []byte(content), nil }, false, expected != "", domain.MaxMemoryFileBytes, true, check)
}

// DeleteMemory 先校验路径与当前摘要，再审计并删除；冲突或审计失败时保留原文件。
func (f *Files) DeleteMemory(ctx context.Context, botID, path, expected string, before BeforeWrite) (result error) {
	if !memory.ValidPath(path) || expected == "" {
		return failure("MEMORY_PATH_INVALID", "Deleting a memory requires its private path and current SHA-256.")
	}
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	root, err := f.memoryRoot(botID)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, root.Close()) }()
	name := strings.TrimPrefix(path, memory.Directory)
	file, err := readMemory(ctx, root, name)
	if err != nil {
		return err
	}
	if file.SHA256 != expected {
		return domain.NewError(409, "MEMORY_CHANGED", "The memory changed after it was opened.")
	}
	if before == nil {
		return domain.NewError(500, "AUDIT_REQUIRED", "A memory mutation requires an audit callback.")
	}
	if err = before(ctx, path, file.SHA256); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = root.Remove(name); err != nil {
		return fileError(err)
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
