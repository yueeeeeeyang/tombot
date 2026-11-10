package backup

import "golang.org/x/sys/unix"

// publish 在同一文件系统原子发布，仅允许使用尚不存在的目标名。
func publish(from, to string) error {
	return unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
}
