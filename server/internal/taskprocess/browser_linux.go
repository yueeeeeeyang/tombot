//go:build linux

package taskprocess

import (
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/computer"
)

func SealBrowser(request BrowserRequest) (*os.File, error) {
	encoded, err := encodeBrowser(request)
	if err != nil {
		return nil, err
	}
	return sealBytes(encoded)
}

// RunBrowser 仅在纯 Go 受信引导子进程调用，成功后原位执行固定 Chrome，不返回。
// 3/4/5 是封存授权、服务入组和撤销读端，6/7 是私有 CDP 管道；不继承其他控制入口。
func RunBrowser() (failure error) {
	if err := CheckController(); err != nil {
		return err
	}
	requestFile := os.NewFile(3, "browser-request")
	groupFile := os.NewFile(4, "browser-group")
	revocation := os.NewFile(5, "browser-revocation")
	commands, responses := os.NewFile(6, "browser-commands"), os.NewFile(7, "browser-responses")
	var chromeCommands, chromeResponses *os.File
	defer func() {
		for _, file := range []*os.File{requestFile, groupFile, revocation, commands, responses, chromeCommands, chromeResponses} {
			if file != nil {
				failure = errors.Join(failure, file.Close())
			}
		}
	}()
	var owner unix.Stat_t
	if unix.Fstat(3, &owner) != nil || owner.Uid != 0 || owner.Gid != 0 {
		return errors.New("共享浏览器授权不属于受信代理")
	}
	encoded, err := readSealedBytes(requestFile)
	if err != nil {
		return err
	}
	request, err := DecodeBrowser(encoded)
	if err != nil {
		return err
	}
	if _, err = request.Deadline.Local(BrowserMaximumStartTime); err != nil {
		return err
	}
	if err = verifyBrowserDirectories(request); err != nil {
		return err
	}
	if err = checkBrowserPipes(commands, responses); err != nil {
		return err
	}
	if err = requestFile.Close(); err != nil {
		requestFile = nil
		return errors.New("关闭共享浏览器启动授权失败")
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
		return errors.New("关闭共享浏览器资源组入口失败")
	}
	groupFile = nil
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err = prepareProcess(computer.BrowserUID, computer.DesktopSharedGID); err != nil {
		return err
	}
	// 浏览器配置和临时文件不属于共享业务产出，不继承业务任务的组可读默认权限。
	unix.Umask(0077)
	if err = os.Chdir(computer.BrowserDirectory); err != nil {
		return errors.New("进入共享浏览器持久目录失败")
	}
	if _, err = request.Deadline.Local(BrowserMaximumStartTime); err != nil {
		return err
	}
	if err = checkRevocation(revocation); err != nil {
		return err
	}
	if err = requireGroupPath("/" + request.Group.Name()); err != nil {
		return err
	}
	// 全线程降权已把所有附加入口设为 CLOEXEC；只开放 Chrome 固定的两个协议描述符。
	if err = unix.Dup3(6, 3, 0); err != nil {
		return errors.New("映射共享浏览器命令管道失败")
	}
	chromeCommands = os.NewFile(3, "chrome-commands")
	if err = unix.Dup3(7, 4, 0); err != nil {
		return errors.New("映射共享浏览器响应管道失败")
	}
	chromeResponses = os.NewFile(4, "chrome-responses")
	arguments, environment, err := browserCommand(request)
	if err != nil {
		return err
	}
	if err = syscall.Exec(BrowserProgram, arguments, environment); err != nil {
		return errors.New("启动共享 Chrome 失败")
	}
	return errors.New("共享 Chrome 没有原位执行")
}

func checkBrowserPipes(commands, responses *os.File) error {
	var identities [2]unix.Stat_t
	for index, file := range []*os.File{commands, responses} {
		if file == nil {
			return errors.New("共享浏览器协议管道缺失")
		}
		flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
		mode := unix.O_RDONLY
		if index == 1 {
			mode = unix.O_WRONLY
		}
		stat := &identities[index]
		if err != nil || flags&unix.O_ACCMODE != mode || unix.Fstat(int(file.Fd()), stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFIFO || stat.Uid != 0 || stat.Gid != 0 {
			return errors.New("共享浏览器协议入口不是受信管道")
		}
		name, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(int(file.Fd())))
		if err != nil || !strings.HasPrefix(name, "pipe:[") || !strings.HasSuffix(name, "]") {
			return errors.New("共享浏览器协议入口不是匿名管道")
		}
	}
	if identities[0].Dev == identities[1].Dev && identities[0].Ino == identities[1].Ino {
		return errors.New("共享浏览器不能复用同一条双向管道")
	}
	return nil
}

func verifyBrowserDirectories(request BrowserRequest) (failure error) {
	if err := request.Validate(); err != nil {
		return err
	}
	for _, expected := range []struct {
		path     string
		uid, gid uint32
		mode     uint32
	}{
		{computer.BrowserDirectory, computer.BrowserUID, computer.DesktopSharedGID, 0700},
		{computer.BrowserRuntimeRoot, 0, 0, 0711},
		{request.RuntimeDirectory(), computer.BrowserUID, computer.DesktopSharedGID, 0700},
	} {
		fd, err := unix.Openat2(unix.AT_FDCWD, expected.path, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
		if err != nil {
			return errors.New("共享浏览器固定目录缺失或包含链接")
		}
		var stat unix.Stat_t
		valid := unix.Fstat(fd, &stat) == nil && stat.Uid == expected.uid && stat.Gid == expected.gid && stat.Mode&07777 == expected.mode
		if err = unix.Close(fd); err != nil {
			return errors.New("关闭共享浏览器目录入口失败")
		}
		if !valid {
			return errors.New("共享浏览器固定目录所属身份或权限无效")
		}
	}
	return nil
}
