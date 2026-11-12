//go:build linux

package computerprobe

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"math"
	"os"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/infra/ids"
)

// checkFiles 仅新建本次探针的随机私有目录，不遍历、覆盖或递归删除用户内容。
func checkFiles(ctx context.Context, workspace string) (filesystem int64, size uint64, failure error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return 0, 0, errors.New("能力探针无法打开共享文件系统")
	}
	defer func() { failure = errors.Join(failure, root.Close()) }()
	parent, err := root.Open(".")
	if err != nil {
		return 0, 0, errors.New("能力探针无法固定共享目录")
	}
	defer func() { failure = errors.Join(failure, parent.Close()) }()
	name := ".tombot-probe-" + ids.New()
	if err = root.Mkdir(name, 0700); err != nil {
		return 0, 0, errors.New("能力探针无法在共享文件系统建立目录")
	}
	directory, err := root.OpenRoot(name)
	if err != nil {
		return 0, 0, errors.New("能力探针目录打开失败，保留未确认资源")
	}
	directoryFile, err := directory.Open(".")
	if err != nil {
		return 0, 0, errors.Join(errors.New("能力探针目录固定失败"), directory.Close())
	}
	identity, err := directoryFile.Stat()
	if err != nil {
		return 0, 0, errors.Join(errors.New("能力探针目录身份未知"), directoryFile.Close(), directory.Close())
	}
	defer func() {
		failure = errors.Join(failure, directoryFile.Close(), directory.Close())
		current, err := root.Lstat(name)
		if err != nil || !os.SameFile(identity, current) || !current.IsDir() {
			failure = errors.Join(failure, errors.New("能力探针目录已变化，保留未知路径"))
			return
		}
		// 只移除已经为空且身份未变化的本次目录；出现额外文件时保留并报告失败。
		if err = unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR); err != nil {
			failure = errors.Join(failure, errors.New("能力探针目录未完全回收"))
		}
	}()
	file, err := directory.OpenFile("check", os.O_RDWR|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return 0, 0, errors.New("能力探针无法创建读写验证文件")
	}
	fileIdentity, err := file.Stat()
	if err != nil {
		return 0, 0, errors.Join(errors.New("能力探针文件身份未知"), file.Close())
	}
	defer func() {
		failure = errors.Join(failure, file.Close())
		current, err := directory.Lstat("check")
		if err != nil || !os.SameFile(fileIdentity, current) || !current.Mode().IsRegular() {
			failure = errors.Join(failure, errors.New("能力探针文件已变化，保留未知内容"))
			return
		}
		if err = directory.Remove("check"); err != nil {
			failure = errors.Join(failure, errors.New("能力探针文件未完全回收"))
		}
	}()
	var storage unix.Statfs_t
	if unix.Fstatfs(int(file.Fd()), &storage) != nil || storage.Bsize <= 0 || storage.Blocks == 0 || storage.Blocks > math.MaxUint64/uint64(storage.Bsize) {
		return 0, 0, errors.New("能力探针无法读取实际文件系统容量")
	}
	content := make([]byte, 4096)
	if _, err = rand.Read(content); err != nil {
		return 0, 0, errors.New("能力探针无法生成验证内容")
	}
	if count, err := file.Write(content); err != nil || count != len(content) {
		return 0, 0, errors.New("能力探针共享文件写入失败")
	}
	if err = errors.Join(file.Sync(), directoryFile.Sync()); err != nil {
		return 0, 0, errors.New("能力探针共享文件刷盘失败")
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return 0, 0, errors.New("能力探针共享文件定位失败")
	}
	actual, err := io.ReadAll(io.LimitReader(file, int64(len(content)+1)))
	if err != nil || !bytes.Equal(actual, content) {
		return 0, 0, errors.New("能力探针共享文件读取或内容核验失败")
	}
	if err = ctx.Err(); err != nil {
		return 0, 0, err
	}
	return storage.Type, storage.Blocks * uint64(storage.Bsize), nil
}
