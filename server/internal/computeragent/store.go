package computeragent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"github.com/oklog/ulid/v2"
	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

// FileStore 在受保护目录中保存会话关闭屏障，并用持续持有的文件锁禁止双代理接管同一卷。
type FileStore struct {
	mu         sync.RWMutex
	root       *os.Root
	lock       *os.File
	owner      uint32
	closed     bool
	closeError error
}

// NewFileStore 只接受已建立的 root 私有目录。目录必须位于用户可写区域之外。
func NewFileStore(directory string) (*FileStore, error) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, errors.New("电脑代理控制记录要求 Linux root 私有目录")
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(directory, current), string(filepath.Separator)) {
		current = filepath.Join(current, part)
		stat, err := os.Lstat(current)
		if err != nil || !stat.IsDir() || stat.Mode().Perm()&0022 != 0 {
			return nil, errors.New("电脑代理控制目录父级不受保护")
		}
		owner, ok := stat.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != 0 {
			return nil, errors.New("电脑代理控制目录父级不属于 root")
		}
	}
	return openFileStore(directory, 0)
}

func openFileStore(directory string, owner uint32) (result *FileStore, failure error) {
	stat, err := os.Lstat(directory)
	if err != nil || !stat.IsDir() || stat.Mode().Perm() != 0700 {
		return nil, errors.New("电脑代理控制目录权限无效")
	}
	identity, ok := stat.Sys().(*syscall.Stat_t)
	if !ok || identity.Uid != owner {
		return nil, errors.New("电脑代理控制目录所有者无效")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errors.New("打开电脑代理控制目录失败")
	}
	store := &FileStore{root: root, owner: owner}
	defer func() {
		if result == nil {
			failure = errors.Join(failure, root.Close())
		}
	}()
	file, err := store.open("agent.lock", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if errors.Is(err, os.ErrExist) {
		file, err = store.open("agent.lock", os.O_RDWR, 0)
	}
	if err != nil {
		return nil, errors.New("打开电脑代理独占锁失败")
	}
	if err = store.check(file); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(errors.New("电脑代理控制目录已被占用"), file.Close())
	}
	store.lock = file
	return store, nil
}

func (store *FileStore) check(file *os.File) error {
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Mode().Perm() != 0600 {
		return errors.New("电脑代理控制文件类型或权限无效")
	}
	identity, ok := stat.Sys().(*syscall.Stat_t)
	if !ok || identity.Uid != store.owner || identity.Nlink != 1 {
		return errors.New("电脑代理控制文件所有者或链接数无效")
	}
	return nil
}

func (store *FileStore) open(name string, flags int, mode os.FileMode) (*os.File, error) {
	directory, err := store.root.Open(".")
	if err != nil {
		return nil, err
	}
	fd, openErr := unix.Openat(int(directory.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, uint32(mode.Perm()))
	closeErr := directory.Close()
	if openErr != nil {
		return nil, errors.Join(openErr, closeErr)
	}
	if closeErr != nil {
		return nil, errors.Join(closeErr, unix.Close(fd))
	}
	return os.NewFile(uintptr(fd), name), nil
}

// Desktop 返回固定桌面的记录入口；同桌面的读写由桌面守卫串行，不同桌面不共享刷盘互斥锁。
func (store *FileStore) Desktop(desktopID string) (Store, error) {
	if _, err := ulid.ParseStrict(desktopID); err != nil {
		return nil, errors.New("电脑代理桌面记录标识无效")
	}
	return desktopFileStore{store, desktopID}, nil
}

type desktopFileStore struct {
	store     *FileStore
	desktopID string
}

func (record desktopFileStore) Load(ctx context.Context) (*Snapshot, error) {
	return record.store.load(ctx, record.desktopID)
}

func (record desktopFileStore) Save(ctx context.Context, snapshot Snapshot) error {
	if snapshot.Command.Session.DesktopID != record.desktopID {
		return errors.New("电脑代理不能覆盖其他桌面记录")
	}
	return record.store.save(ctx, snapshot)
}

func (store *FileStore) readRecord(ctx context.Context, name string) (encoded []byte, failure error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return nil, errors.New("电脑代理控制记录已关闭")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	file, err := store.open(name, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("读取电脑代理控制记录失败")
	}
	defer func() {
		if file.Close() != nil {
			failure = errors.Join(failure, errors.New("关闭电脑代理控制记录失败"))
			encoded = nil
		}
	}()
	if err = store.check(file); err != nil {
		return nil, err
	}
	encoded, err = io.ReadAll(io.LimitReader(file, 16385))
	if err != nil || len(encoded) > 16384 {
		return nil, errors.New("电脑代理控制记录读取失败或超过限制")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return encoded, nil
}

func (store *FileStore) load(ctx context.Context, desktopID string) (*Snapshot, error) {
	encoded, err := store.readRecord(ctx, desktopID+".session.json")
	if err != nil || encoded == nil {
		return nil, err
	}
	var decoded Snapshot
	if err = agentchannel.Decode(encoded, &decoded); err != nil {
		return nil, err
	}
	if err = decoded.validate(); err != nil {
		return nil, err
	}
	if decoded.Command.Session.DesktopID != desktopID {
		return nil, errors.New("电脑代理记录文件与桌面归属不符")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return &decoded, nil
}

func (store *FileStore) save(ctx context.Context, snapshot Snapshot) (failure error) {
	if err := snapshot.validate(); err != nil {
		return err
	}
	encoded, err := agentchannel.Encode(snapshot)
	if err != nil || len(encoded) > 16384 {
		return errors.New("电脑代理控制记录内容无效或超过限制")
	}
	return store.writeRecord(ctx, snapshot.Command.Session.DesktopID+".session.json", encoded)
}

// writeRecord 共用已固定私有目录中的原子替换及文件、目录刷盘；目标名称只由内部记录类型生成。
func (store *FileStore) writeRecord(ctx context.Context, target string, encoded []byte) (failure error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return errors.New("电脑代理控制记录已关闭")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	previous, err := store.open(target, os.O_RDONLY, 0)
	if err == nil {
		if err = errors.Join(store.check(previous), previous.Close()); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("电脑代理控制记录已被替换")
	}
	name := ".session-" + ids.New() + ".tmp"
	file, err := store.open(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("创建电脑代理控制记录失败")
	}
	renamed := false
	defer func() {
		if !renamed && store.root.Remove(name) != nil {
			failure = errors.Join(failure, errors.New("清理电脑代理未提交记录失败"))
		}
	}()
	count, writeErr := file.Write(encoded)
	if writeErr == nil && count != len(encoded) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err = errors.Join(writeErr, file.Close()); err != nil {
		return errors.New("保存电脑代理控制记录失败")
	}
	if err = store.root.Rename(name, target); err != nil {
		return errors.New("提交电脑代理控制记录失败")
	}
	renamed = true
	directory, err := store.root.Open(".")
	if err != nil {
		return errors.New("打开电脑代理控制目录刷盘入口失败")
	}
	if err = errors.Join(directory.Sync(), directory.Close()); err != nil {
		return errors.New("电脑代理控制记录持久结果需要对账")
	}
	return ctx.Err()
}

// Visit 仅供接单前的恢复使用，逐批核验全部桌面记录，拒绝旧单会话记录及超出稳定桌面数量上限的目录。
func (store *FileStore) Visit(ctx context.Context, visit func(Snapshot) error) (failure error) {
	store.mu.RLock()
	if store.closed || visit == nil {
		store.mu.RUnlock()
		return errors.New("电脑代理记录遍历入口不可用")
	}
	directory, err := store.root.Open(".")
	store.mu.RUnlock()
	if err != nil {
		return err
	}
	defer func() { failure = errors.Join(failure, directory.Close()) }()
	count := 0
	for {
		entries, readErr := directory.ReadDir(100)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, entry := range entries {
			if err = ctx.Err(); err != nil {
				return err
			}
			name := entry.Name()
			if name == "session.json" {
				return errors.New("电脑代理不支持旧用户级会话记录")
			}
			if !strings.HasSuffix(name, ".session.json") {
				continue
			}
			count++
			if count > computer.MaxDesktops {
				return errors.New("电脑代理桌面记录超过数量上限")
			}
			record, err := store.Desktop(strings.TrimSuffix(name, ".session.json"))
			if err != nil {
				return err
			}
			snapshot, err := record.Load(ctx)
			if err != nil {
				return err
			}
			if snapshot == nil {
				return errors.New("电脑代理恢复期间桌面记录丢失")
			}
			if err = visit(*snapshot); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return ctx.Err()
		}
	}
}

// Close 只能在全部桌面收尾后调用；不删除持久锁或会话关闭屏障。
func (store *FileStore) Close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return store.closeError
	}
	store.closed = true
	if err := errors.Join(unix.Flock(int(store.lock.Fd()), unix.LOCK_UN), store.lock.Close(), store.root.Close()); err != nil {
		store.closeError = errors.New("关闭电脑代理控制记录失败")
	}
	return store.closeError
}
