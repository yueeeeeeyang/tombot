package serverapp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/accounts"
)

type activationFile struct {
	directory *os.File
	file      *os.File
	name      string
	identity  os.FileInfo
	path      string
	saved     bool
}

// reserveActivationFile 先独占空目标再签发，避免文件冲突使已经交付的激活值失效。
// 父目录须由部署预建为当前身份的私有目录；相对目录描述符避免写入期间的路径替换。
func reserveActivationFile(path string) (output *activationFile, failure error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, errors.New("激活文件路径无效")
	}
	directory, err := os.OpenFile(filepath.Dir(path), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("激活文件的私有目录不可打开")
	}
	defer func() {
		if output == nil {
			failure = errors.Join(failure, safeFileClose(directory))
		}
	}()
	info, err := directory.Stat()
	if err != nil {
		return nil, errors.New("激活文件目录身份不可读取")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("激活文件目录必须由当前部署身份拥有且权限为 0700")
	}
	name := filepath.Base(path)
	descriptor, err := unix.Openat(int(directory.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, errors.New("激活文件必须为新文件，已有文件或链接不能覆盖")
	}
	file := os.NewFile(uintptr(descriptor), name)
	created, err := file.Stat()
	if err != nil {
		return nil, errors.Join(errors.New("新激活文件身份不可读取，请检查空白输出文件"), safeFileClose(file))
	}
	return &activationFile{directory: directory, file: file, name: name, identity: created, path: path}, nil
}

func (output *activationFile) save(activation accounts.Activation) error {
	if output.saved {
		return errors.New("激活文件不能重复写入")
	}
	if err := output.checkIdentity(); err != nil {
		return err
	}
	if err := json.NewEncoder(output.file).Encode(activation); err != nil {
		return errors.New("激活材料写入失败，需重新签发")
	}
	if output.file.Sync() != nil || output.directory.Sync() != nil {
		return errors.New("激活材料未确认持久化，需重新签发")
	}
	if err := output.checkIdentity(); err != nil {
		return err
	}
	output.saved = true
	return nil
}

func (output *activationFile) checkIdentity() error {
	current, err := os.Lstat(output.path)
	if err != nil || !os.SameFile(output.identity, current) {
		return errors.New("激活文件路径身份已变更，未确认交付")
	}
	owner, ok := current.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) || owner.Nlink != 1 || !current.Mode().IsRegular() || current.Mode().Perm() != 0600 {
		return errors.New("激活文件权限或链接已变更，未确认交付")
	}
	return nil
}

func (output *activationFile) close() error {
	var failure error
	// 未交付的文件只在仍为本次创建的 inode 时删除，不清理替换后的未知内容。
	if !output.saved {
		var current unix.Stat_t
		if err := unix.Fstatat(int(output.directory.Fd()), output.name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if !errors.Is(err, unix.ENOENT) {
				failure = errors.New("未交付激活文件身份不可确认，未清理")
			}
		} else {
			original := output.identity.Sys().(*syscall.Stat_t)
			if uint64(current.Dev) != uint64(original.Dev) || current.Ino != original.Ino {
				failure = errors.New("未交付激活文件已被替换，未清理")
			} else if unix.Unlinkat(int(output.directory.Fd()), output.name, 0) != nil || output.directory.Sync() != nil {
				failure = errors.New("清理未交付激活文件失败")
			}
		}
	}
	return errors.Join(failure, safeFileClose(output.file), safeFileClose(output.directory))
}

func safeFileClose(file *os.File) error {
	if file.Close() != nil {
		return errors.New("关闭部署输出文件失败")
	}
	return nil
}
