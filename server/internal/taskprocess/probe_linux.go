//go:build linux

package taskprocess

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/computer"
)

// SealProbe 使用与任务相同的不可改写匿名内容，调用方持有返回的入口并负责关闭。
func SealProbe(request ProbeRequest) (*os.File, error) {
	encoded, err := encodeProbe(request)
	if err != nil {
		return nil, err
	}
	return sealBytes(encoded)
}

// RunProbe 只在独立纯 Go 容器引导进程运行，成功时以固定非 root 身份原位执行探针。
// 描述符 3、4、5 分别为封存授权、服务入组与撤销读端；失败必须立即退出进程。
func RunProbe() (failure error) {
	if err := CheckController(); err != nil {
		return err
	}
	requestFile := os.NewFile(3, "probe-request")
	groupFile := os.NewFile(4, "probe-group")
	revocation := os.NewFile(5, "probe-revocation")
	defer func() {
		for _, file := range []*os.File{requestFile, groupFile, revocation} {
			if file != nil {
				failure = errors.Join(failure, file.Close())
			}
		}
	}()
	var owner unix.Stat_t
	if unix.Fstat(3, &owner) != nil || owner.Uid != 0 || owner.Gid != 0 {
		return errors.New("能力探针授权不属于受信代理")
	}
	encoded, err := readSealedBytes(requestFile)
	if err != nil {
		return err
	}
	request, err := DecodeProbe(encoded)
	if err != nil {
		return err
	}
	if _, err = request.Deadline.Local(ProbeMaximumLifetime); err != nil {
		return err
	}
	if err = requestFile.Close(); err != nil {
		requestFile = nil
		return errors.New("关闭探针授权入口失败")
	}
	requestFile = nil
	if err = checkRevocation(revocation); err != nil {
		return err
	}
	expected := "/" + request.Group.Container.Path + "/" + request.Group.Name() + "/cgroup.procs"
	if err = joinGroupFile(groupFile, expected, "/"+request.Group.Name()); err != nil {
		return err
	}
	if err = groupFile.Close(); err != nil {
		groupFile = nil
		return errors.New("关闭探针资源组入口失败")
	}
	groupFile = nil
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err = prepareProcess(computer.ProbeUID, computer.DesktopSharedGID); err != nil {
		return err
	}
	if err = os.Chdir("/"); err != nil {
		return errors.New("进入探针固定目录失败")
	}
	if err = checkRevocation(revocation); err != nil {
		return err
	}
	if _, err = CheckProbe(request); err != nil {
		return err
	}
	// 探针仅继承固定系统工具环境；授权中没有用户内容、凭据、命令或可执行文件路径。
	if err = syscall.Exec(ProbeProgram, []string{ProbeProgram, "--worker", string(encoded)}, []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "HOME=/", "TMPDIR=/tmp"}); err != nil {
		return errors.New("启动固定能力探针失败")
	}
	return errors.New("能力探针没有原位执行")
}

// CheckProbe 只读取实际身份、内核归属及原授权期限；既有授权不能通过重复检查延长。
func CheckProbe(request ProbeRequest) (time.Time, error) {
	if err := request.Validate(); err != nil {
		return time.Time{}, err
	}
	status, err := currentStatus()
	if err != nil {
		return time.Time{}, err
	}
	if !status.unprivileged(computer.ProbeUID, computer.DesktopSharedGID) {
		return time.Time{}, errors.New("能力探针缺少固定非 root 身份或隔离")
	}
	if err = requireGroupPath("/" + request.Group.Name()); err != nil {
		return time.Time{}, err
	}
	return request.Deadline.Local(ProbeMaximumLifetime)
}
