//go:build linux

package taskprocess

import (
	"errors"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// checkRevocation 只接受代理持有写端的匿名只读管道；写端关闭后不能再次激活该授权。
// 检查必须发生在入组之后、exec 之前，配合先撤销再终止资源组封闭迟到引导的竞态。
func checkRevocation(file *os.File) error {
	if file == nil {
		return errors.New("任务撤销描述符缺失")
	}
	fd := int(file.Fd())
	var stat unix.Stat_t
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil || flags&unix.O_ACCMODE != unix.O_RDONLY || unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFIFO || stat.Uid != 0 || stat.Gid != 0 {
		return errors.New("任务撤销描述符无效")
	}
	name, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil || !strings.HasPrefix(name, "pipe:[") || !strings.HasSuffix(name, "]") {
		return errors.New("任务撤销描述符不是匿名管道")
	}
	if err = unix.SetNonblock(fd, true); err != nil {
		return errors.New("设置任务撤销检查失败")
	}
	var one [1]byte
	count, err := unix.Read(fd, one[:])
	if count <= 0 && errors.Is(err, unix.EAGAIN) {
		return nil
	}
	return errors.New("任务执行授权已撤销或撤销通道异常")
}
