//go:build linux

package taskprocess

import (
	"errors"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/computer"
)

// Run 只用于独立、纯 Go 构建的容器引导进程，成功时由用户程序原位替换，不返回。
// 固定描述符 3 为 Seal 生成的内容，4 为当前会话的 cgroup.procs，5 为执行授权撤销管道的读端。
// 代理必须先关闭撤销管道写端再回收资源组；任何失败都要求调用者退出。
// 本函数改变进程目录、资源限制及所有线程身份，禁止在服务进程或宿主机直接调用。
func Run() (failure error) {
	if err := CheckController(); err != nil {
		return err
	}
	requestFile := os.NewFile(3, "task-request")
	groupFile := os.NewFile(4, "task-group")
	revocation := os.NewFile(5, "task-revocation")
	defer func() {
		if requestFile != nil {
			failure = errors.Join(failure, requestFile.Close())
		}
		if groupFile != nil {
			failure = errors.Join(failure, groupFile.Close())
		}
		failure = errors.Join(failure, revocation.Close())
	}()
	var stat unix.Stat_t
	if unix.Fstat(3, &stat) != nil || stat.Uid != 0 || stat.Gid != 0 {
		return errors.New("任务引导内容不属于受信代理")
	}
	request, err := readSealed(requestFile)
	if err != nil {
		return err
	}
	if err = requestFile.Close(); err != nil {
		requestFile = nil
		return errors.New("关闭任务引导内容失败")
	}
	requestFile = nil
	if err = checkRevocation(revocation); err != nil {
		return err
	}
	if err = verifyDesktopDirectory(request); err != nil {
		return err
	}
	if err = joinGroup(request, groupFile); err != nil {
		return err
	}
	if err = groupFile.Close(); err != nil {
		groupFile = nil
		return errors.New("关闭任务资源组描述符失败")
	}
	groupFile = nil
	return execute(request, revocation)
}

// CheckController 复验受信容器进程的构建、权限及私有资源组；不改变进程身份或执行任何用户代码。
func CheckController() error {
	if hasNativeThreads {
		return errors.New("电脑控制进程必须禁用 CGO 构建")
	}
	if err := requireController(); err != nil {
		return err
	}
	return requireGroupPath("/")
}

// requireController 与节点的最小权限容器配置一致，拒绝从普通宿主进程或扩权容器引导。
func requireController() error {
	status, err := currentStatus()
	if err != nil {
		return err
	}
	capabilityBits := map[string]uint{"CHOWN": unix.CAP_CHOWN, "DAC_OVERRIDE": unix.CAP_DAC_OVERRIDE, "FOWNER": unix.CAP_FOWNER, "SETUID": unix.CAP_SETUID, "SETGID": unix.CAP_SETGID, "KILL": unix.CAP_KILL}
	var capabilities uint64
	for _, name := range computer.AgentCapabilities() {
		bit, known := capabilityBits[name]
		if !known {
			return errors.New("任务引导无法识别节点权限清单")
		}
		capabilities |= 1 << bit
	}
	if !status.isolated() || status.uid != [4]uint32{} || status.gid != [4]uint32{} || status.capabilities["CapInh"] != 0 || status.capabilities["CapAmb"] != 0 || status.capabilities["CapPrm"] != capabilities || status.capabilities["CapEff"] != capabilities {
		return errors.New("任务引导进程缺少规定的隔离或身份边界")
	}
	secureBits, err := unix.PrctlRetInt(unix.PR_GET_SECUREBITS, 0, 0, 0, 0)
	if err != nil || secureBits != 0 {
		return errors.New("任务引导进程权限继承状态无效")
	}
	return nil
}

func currentStatus() (processStatus, error) {
	encoded, err := readKernelFile("/proc/thread-self/status", 64<<10)
	if err != nil {
		return processStatus{}, err
	}
	return parseStatus(encoded)
}

func readKernelFile(name string, maximum int64) (encoded []byte, failure error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, errors.New("打开任务内核状态失败")
	}
	defer func() {
		if file.Close() != nil {
			failure = errors.Join(failure, errors.New("关闭任务内核状态失败"))
		}
	}()
	encoded, err = io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(encoded)) > maximum {
		return nil, errors.New("读取任务内核状态失败或超过限制")
	}
	return encoded, nil
}

func joinGroup(request Request, file *os.File) error {
	expected := "/" + request.Group.ParentPath + "/" + request.Group.Name() + "/" + request.Role + "/cgroup.procs"
	return joinGroupFile(file, expected, "/"+request.Group.Name()+"/"+request.Role)
}

func joinGroupFile(file *os.File, expected, privatePath string) error {
	var stat unix.Stat_t
	var filesystem unix.Statfs_t
	fd := int(file.Fd())
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
	if err != nil || flags&unix.O_ACCMODE != unix.O_WRONLY || unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != 0 || stat.Gid != 0 || stat.Mode&0022 != 0 || unix.Fstatfs(fd, &filesystem) != nil || filesystem.Type != unix.CGROUP2_SUPER_MAGIC {
		return errors.New("任务入组描述符不是受管内核入口")
	}
	target, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil || !strings.HasSuffix(target, expected) {
		return errors.New("任务入组描述符与固定会话不符")
	}
	// 私有 cgroup 命名空间中，容器主进程位于根。拒绝另一会话或宿主路径下的引导。
	if err = requireGroupPath("/"); err != nil {
		return err
	}
	if count, err := file.Write([]byte("0")); err != nil || count != 1 {
		return errors.New("任务进程入组失败")
	}
	return requireGroupPath(privatePath)
}

func requireGroupPath(expected string) error {
	encoded, err := readKernelFile("/proc/self/cgroup", 16<<10)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(encoded)) != "0::"+expected {
		return errors.New("任务进程未处于预期的私有资源组")
	}
	return nil
}

// execute 在入组及控制描述符关闭后执行；失败返回时身份可能已改变，调用方必须立即退出。
func execute(request Request, revocation *os.File) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	identity := request.Group.Session.DesktopIdentity
	if err := prepareProcess(identity.UID, identity.GID); err != nil {
		return err
	}
	if err := checkRevocation(revocation); err != nil {
		return err
	}
	// 不能借 root 打开目录后把特权目录句柄交给任务；普通链接和其他 Bot 的业务文件按内核权限访问。
	if err := os.Chdir(request.WorkingDirectory); err != nil {
		return errors.New("进入任务工作目录失败")
	}
	environment, err := request.environment()
	if err != nil {
		return err
	}
	if err = checkRevocation(revocation); err != nil {
		return err
	}
	// 此只读描述符保留到最后一次授权检查，并通过 CLOEXEC 在原位执行时关闭。
	// 不能在此提前关闭后复用同一对象，否则失败路径无法可靠释放描述符。
	arguments := append([]string{request.Program}, request.Arguments...)
	if err = syscall.Exec(request.Program, arguments, environment); err != nil {
		return errors.New("启动任务用户程序失败")
	}
	return errors.New("任务用户程序未替换引导进程")
}

// prepareProcess 是任务与固定探针共用的全线程降权入口，只能在独立引导进程中调用。
func prepareProcess(uid, gid int) error {
	if hasNativeThreads {
		return errors.New("任务身份降权不能包含非 Go 线程")
	}
	// 调用方在原位执行完成前保持线程锁，不在长期运行的代理中修改线程权限。
	if err := unix.CloseRange(3, uint(^uint32(0)), unix.CLOSE_RANGE_CLOEXEC); err != nil {
		return errors.New("封闭任务继承描述符失败")
	}
	if _, _, err := syscall.AllThreadsSyscall6(syscall.SYS_PRCTL, unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0, 0); err != 0 {
		return errors.New("设置任务不可提权状态失败")
	}
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{}); err != nil {
		return errors.New("关闭任务内存转储失败")
	}
	// 同一用户的 Bot 通过公共组共享新业务文件；桌面控制文件由专用入口显式设为私有。
	unix.Umask(0007)
	if err := syscall.Setgroups(nil); err != nil {
		return errors.New("清除任务附加组失败")
	}
	if err := syscall.Setresgid(gid, gid, gid); err != nil {
		return errors.New("切换任务组身份失败")
	}
	if err := syscall.Setresuid(uid, uid, uid); err != nil {
		return errors.New("切换任务用户身份失败")
	}
	status, err := currentStatus()
	if err != nil {
		return err
	}
	if !status.unprivileged(uint32(uid), uint32(gid)) {
		return errors.New("任务权限清理后复验失败")
	}
	return nil
}
