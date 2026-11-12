package dockernode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

// lifecycleVersion 禁止忽略用户浏览器消费记录的旧节点接管新状态。
const lifecycleVersion = 4

type record struct {
	Version       int                    `json:"version"`
	Specification computer.Specification `json:"specification"`
	Phase         string                 `json:"phase"`
	ContainerID   string                 `json:"container_id,omitempty"`
	Retirement    *controlRetirement     `json:"control_retirement,omitempty"`
}

type journal struct {
	root  *os.Root
	owner uint32
}

func checkProtectedDirectory(path string, owner uint32) error {
	stat, err := os.Lstat(path)
	if err != nil {
		return errors.New("节点状态目录不可用")
	}
	identity, ok := stat.Sys().(*syscall.Stat_t)
	if !ok || identity.Uid != owner || !stat.IsDir() || stat.Mode().Perm()&0022 != 0 {
		return errors.New("节点状态目录权限或所有者无效")
	}
	return nil
}

func checkProtectedParents(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("节点目录必须使用规范绝对路径")
	}
	current := string(filepath.Separator)
	if err := checkProtectedDirectory(current, 0); err != nil {
		return err
	}
	for _, component := range strings.Split(strings.TrimPrefix(path, current), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		if err := checkProtectedDirectory(current, 0); err != nil {
			return err
		}
	}
	return nil
}

func openJournal(path string, owner uint32) (*journal, error) {
	if err := checkProtectedDirectory(path, owner); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, errors.New("打开节点状态目录失败")
	}
	return &journal{root, owner}, nil
}

func (journal *journal) checkFile(file *os.File) error {
	stat, err := file.Stat()
	if err != nil {
		return errors.New("读取节点状态属性失败")
	}
	identity, ok := stat.Sys().(*syscall.Stat_t)
	if !ok || identity.Uid != journal.owner || identity.Nlink != 1 || !stat.Mode().IsRegular() || stat.Mode().Perm()&0077 != 0 {
		return errors.New("节点状态文件权限、类型或链接数量无效")
	}
	return nil
}

// openFile 对平面状态文件直接使用受控目录描述符与 O_NOFOLLOW；os.Root 默认会解析根内符号链接。
func (journal *journal) openFile(name string, flags int, mode os.FileMode) (*os.File, error) {
	directory, err := journal.root.Open(".")
	if err != nil {
		return nil, err
	}
	descriptor, openErr := unix.Openat(int(directory.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, uint32(mode.Perm()))
	closeErr := directory.Close()
	if openErr != nil {
		return nil, errors.Join(openErr, closeErr)
	}
	if closeErr != nil {
		return nil, errors.Join(closeErr, unix.Close(descriptor))
	}
	return os.NewFile(uintptr(descriptor), name), nil
}

func (journal *journal) lock(ctx context.Context, userID string) (release func() error, result error) {
	return journal.lockMode(ctx, userID, syscall.LOCK_EX)
}

// lockMode 让桌面操作共享生命周期读锁，容器变更仍须取得同一文件的独占锁。
func (journal *journal) lockMode(ctx context.Context, name string, mode int) (release func() error, result error) {
	// 独占创建后再打开既有文件，避免多个首次请求同时参与文件创建；锁文件永不删除或替换。
	file, err := journal.openFile(name+".lock", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if errors.Is(err, os.ErrExist) {
		file, err = journal.openFile(name+".lock", os.O_RDWR, 0)
	}
	if err != nil {
		var errno syscall.Errno
		if errors.As(err, &errno) {
			return nil, fmt.Errorf("打开节点资源锁失败（系统错误 %d）", errno)
		}
		return nil, errors.New("打开节点资源锁失败")
	}
	if err = journal.checkFile(file); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err = ctx.Err(); err != nil {
			return nil, errors.Join(err, file.Close())
		}
		err = syscall.Flock(int(file.Fd()), mode|syscall.LOCK_NB)
		if err == nil {
			return func() error { return errors.Join(syscall.Flock(int(file.Fd()), syscall.LOCK_UN), file.Close()) }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(errors.New("取得节点资源锁失败"), file.Close())
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), file.Close())
		case <-ticker.C:
		}
	}
}

func (journal *journal) readFile(name string) (encoded []byte, result error) {
	file, err := journal.openFile(name, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("打开节点资源记录失败")
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	if err = journal.checkFile(file); err != nil {
		return nil, err
	}
	const maximum = 16 << 10
	encoded, err = io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || len(encoded) > maximum {
		return nil, errors.New("节点资源记录损坏或过大")
	}
	return encoded, nil
}

func (journal *journal) read(userID string) (*record, error) {
	encoded, err := journal.readFile(userID + ".json")
	if err != nil || encoded == nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var state record
	if err = decoder.Decode(&state); err != nil {
		return nil, errors.New("节点资源记录格式无效")
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("节点资源记录包含多份内容")
	}
	if state.Version != lifecycleVersion || state.Specification.UserID != userID || state.Specification.Validate() != nil {
		return nil, errors.New("节点资源记录身份无效")
	}
	switch state.Phase {
	case "reserved", "creating", "ready", "removing", "absent":
	default:
		return nil, errors.New("节点资源记录状态无效")
	}
	if state.Retirement != nil {
		next, previous := state.Retirement.Next, state.Specification
		if state.Phase != "absent" || state.ContainerID != "" || state.Retirement.Inode == 0 || next.Validate() != nil || !sameComputerIdentity(next, previous) || next.Generation <= previous.Generation || next.Generation-previous.Generation != 1 || next.Attempt <= previous.Attempt || next.Attempt-previous.Attempt != 1 || next.FencingToken <= previous.FencingToken {
			return nil, errors.New("节点控制目录回收记录无效")
		}
	}
	return &state, nil
}

// save 先写入并刷盘，再原子替换和同步目录；完成之前绝不能调用外部资源变更。
func (journal *journal) save(state *record) (result error) {
	encoded, err := json.Marshal(state)
	if err != nil {
		return errors.New("编码节点资源记录失败")
	}
	return journal.writeFile(state.Specification.UserID+".json", encoded)
}

func (journal *journal) writeFile(target string, encoded []byte) (result error) {
	name := ".write-" + ids.New()
	file, err := journal.openFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("创建节点资源记录失败")
	}
	defer func() {
		if err := journal.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, errors.New("清理节点状态临时文件失败"))
		}
	}()
	_, writeErr := file.Write(encoded)
	if err = errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return errors.New("保存节点资源记录失败")
	}
	if err = journal.root.Rename(name, target); err != nil {
		return errors.New("替换节点资源记录失败")
	}
	directory, err := journal.root.Open(".")
	if err != nil {
		return errors.New("打开节点状态目录失败")
	}
	if err = errors.Join(directory.Sync(), directory.Close()); err != nil {
		return errors.New("同步节点状态目录失败")
	}
	return nil
}
