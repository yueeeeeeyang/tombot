package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockDirectory 保证一个数据目录仅由一个服务进程写入，保护认证文件与引导状态。
func LockDirectory(directory string) (*os.File, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(directory, ".tom.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	stat, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if !stat.Mode().IsRegular() {
		return nil, errors.Join(fmt.Errorf("数据目录锁必须为普通文件"), file.Close())
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(fmt.Errorf("数据目录已由其他 tomd 进程使用: %w", err), file.Close())
	}
	return file, nil
}
