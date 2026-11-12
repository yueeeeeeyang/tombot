//go:build darwin

package deployment

import "golang.org/x/sys/unix"

// publishDirectory 保留开发机测试的原子不覆盖语义；正式入口仍只支持 Linux。
func publishDirectory(source, destination string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_EXCL)
}
