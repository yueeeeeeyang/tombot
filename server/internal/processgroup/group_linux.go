//go:build linux

package processgroup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/computer"
)

type controller struct {
	options Options
	logger  *slog.Logger
}

// New 验证宿主统一层级。用户电脑仍由 Docker 管理 CPU、内存及进程上限，本模块不重配控制器。
func New(options Options, logger *slog.Logger) (Groups, error) {
	return newController(options, logger)
}

func newController(options Options, logger *slog.Logger) (*controller, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		return nil, errors.New("进程资源组日志未配置")
	}
	if os.Geteuid() != 0 {
		return nil, errors.New("进程资源组要求受信 root 节点")
	}
	root, err := openCgroupRoot()
	if err != nil {
		return nil, err
	}
	if err = root.Close(); err != nil {
		return nil, errors.New("关闭进程资源组根目录失败")
	}
	return &controller{options, logger}, nil
}

func openCgroupRoot() (*os.Root, error) {
	root, err := os.OpenRoot("/sys/fs/cgroup")
	if err != nil {
		return nil, errors.New("打开统一资源组失败")
	}
	file, err := root.Open(".")
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	_, _, checkErr := directoryIdentity(file)
	if err = errors.Join(checkErr, file.Close()); err != nil {
		return nil, errors.Join(err, root.Close())
	}
	return root, nil
}
func directoryIdentity(file *os.File) (uint64, uint64, error) {
	var fs unix.Statfs_t
	var stat unix.Stat_t
	if unix.Fstatfs(int(file.Fd()), &fs) != nil || fs.Type != unix.CGROUP2_SUPER_MAGIC || unix.Fstat(int(file.Fd()), &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != 0 || stat.Mode&0022 != 0 {
		return 0, 0, errors.New("进程资源组文件系统或所有者无效")
	}
	return uint64(stat.Dev), stat.Ino, nil
}
func readFile(root *os.Root, name string, maximum int) (encoded []byte, result error) {
	file, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	encoded, err = io.ReadAll(io.LimitReader(file, int64(maximum+1)))
	if err != nil || len(encoded) > maximum {
		return nil, errors.New("读取进程资源组文件失败或内容过大")
	}
	return encoded, nil
}
func writeFile(root *os.Root, name, value string) (result error) {
	file, err := root.OpenFile(name, os.O_WRONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	count, err := file.WriteString(value)
	if err != nil {
		return err
	}
	if count != len(value) {
		return io.ErrShortWrite
	}
	return nil
}
func processPath(pid int) (string, error) {
	if pid <= 1 {
		return "", errors.New("容器主进程标识无效")
	}
	root, err := os.OpenRoot("/proc")
	if err != nil {
		return "", errors.New("打开宿主进程目录失败")
	}
	encoded, readErr := readFile(root, strconv.Itoa(pid)+"/cgroup", 16<<10)
	if err = errors.Join(readErr, root.Close()); err != nil {
		return "", err
	}
	return unifiedPath(encoded)
}

func (groups *controller) Locate(ctx context.Context, session computer.ExecutionSession, pid int) (reference Reference, result error) {
	if err := ctx.Err(); err != nil {
		return reference, err
	}
	if err := session.Validate(); err != nil {
		return reference, err
	}
	parent, err := LocateContainer(ctx, session.ContainerID, pid)
	if err != nil {
		return reference, err
	}
	reference = Reference{Session: session, ParentPath: parent.Path, ParentDevice: parent.Device, ParentInode: parent.Inode}
	return reference, reference.Validate()
}

// LocateContainer 读取内核进程归属和 cgroup v2 的设备/inode；只接受指定容器的父组，不接受任务子组。
func LocateContainer(ctx context.Context, containerID string, pid int) (reference ContainerReference, result error) {
	if err := ctx.Err(); err != nil {
		return reference, err
	}
	if len(containerID) != 64 || strings.Trim(containerID, "0123456789abcdef") != "" {
		return reference, errors.New("容器标识无效")
	}
	parent, err := processPath(pid)
	if err != nil {
		return reference, errors.New("读取容器进程归属失败")
	}
	if !validParent(parent, containerID) {
		return reference, errors.New("主进程不属于指定 Docker 容器")
	}
	root, err := openCgroupRoot()
	if err != nil {
		return reference, err
	}
	defer func() { result = errors.Join(result, root.Close()) }()
	file, err := root.OpenFile(parent, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return reference, err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	device, inode, err := directoryIdentity(file)
	if err != nil {
		return reference, err
	}
	current, err := processPath(pid)
	if err != nil || current != parent || ctx.Err() != nil {
		return reference, errors.New("核验期间容器进程归属改变或调用已取消")
	}
	reference = ContainerReference{ContainerID: containerID, Path: parent, Device: device, Inode: inode}
	return reference, reference.Validate()
}

func openParent(reference Reference) (*os.Root, error) {
	if err := reference.Validate(); err != nil {
		return nil, err
	}
	return openContainerParent(reference.container())
}

func openContainerParent(reference ContainerReference) (*os.Root, error) {
	if err := reference.Validate(); err != nil {
		return nil, err
	}
	root, err := openCgroupRoot()
	if err != nil {
		return nil, err
	}
	parent, openErr := root.OpenRoot(reference.Path)
	if err = errors.Join(openErr, root.Close()); err != nil {
		if parent != nil {
			err = errors.Join(err, parent.Close())
		}
		return nil, err
	}
	file, err := parent.Open(".")
	if err != nil {
		return nil, errors.Join(err, parent.Close())
	}
	device, inode, checkErr := directoryIdentity(file)
	if checkErr == nil && (device != reference.Device || inode != reference.Inode) {
		checkErr = errors.New("容器进程资源组已更换")
	}
	if err = errors.Join(checkErr, file.Close()); err != nil {
		return nil, errors.Join(err, parent.Close())
	}
	return parent, nil
}

func verifyGroup(root *os.Root, name string) error {
	file, err := root.OpenFile(name, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	_, _, checkErr := directoryIdentity(file)
	if err = errors.Join(checkErr, file.Close()); err != nil {
		return err
	}
	encoded, err := readFile(root, path.Join(name, "cgroup.type"), 128)
	if err != nil || strings.TrimSpace(string(encoded)) != "domain" {
		return errors.New("进程资源组不支持完整进程回收")
	}
	return nil
}
func (groups *controller) Create(ctx context.Context, reference Reference) error {
	if err := reference.Validate(); err != nil {
		return err
	}
	return createTree(ctx, reference.container(), reference.Name(), []string{"shell", "browser"})
}

func createTree(ctx context.Context, parent ContainerReference, name string, children []string) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := openContainerParent(parent)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, root.Close()) }()
	// 名称绑定不可复用的执行或服务实例；既有目录只允许在同一持久开启记录下核验。
	for _, current := range groupNames(name, children) {
		if err = root.Mkdir(current, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if err = verifyGroup(root, current); err != nil {
			return err
		}
	}
	if busy, err := isPopulated(root, name); err != nil || busy {
		if err != nil {
			return err
		}
		return errors.New("新资源组已有未知进程")
	}
	// 能力必须在开启时实际可用，不能直到取消才发现缺少强制回收接口。
	file, err := root.OpenFile(path.Join(name, "cgroup.kill"), os.O_WRONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return errors.New("内核缺少受管进程树终止能力")
	}
	return file.Close()
}

func groupNames(name string, children []string) []string {
	names := []string{name}
	for _, child := range children {
		names = append(names, path.Join(name, child))
	}
	return names
}
func isPopulated(root *os.Root, name string) (bool, error) {
	encoded, err := readFile(root, path.Join(name, "cgroup.events"), 8192)
	if err != nil {
		return false, err
	}
	return populated(encoded)
}

// Membership 只返回两个受管叶组之一的描述符；关闭会话必须先阻止新的授权，再移除全部叶组以封闭迟到写入。
func (groups *controller) Membership(ctx context.Context, reference Reference, role string) (file *os.File, result error) {
	if role != "shell" && role != "browser" {
		return nil, errors.New("进程资源组用途无效")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := openParent(reference)
	if err != nil {
		return nil, err
	}
	name := path.Join(reference.Name(), role)
	if err = verifyGroup(root, name); err == nil {
		file, err = root.OpenFile(path.Join(name, "cgroup.procs"), os.O_WRONLY|unix.O_NOFOLLOW, 0)
	}
	if result = errors.Join(err, root.Close()); result != nil && file != nil {
		result = errors.Join(result, file.Close())
		file = nil
	}
	return file, result
}

func (groups *controller) signal(ctx context.Context, root *os.Root, parent string, names []string) error {
	remaining := groups.options.MaximumProcesses
	for _, name := range names {
		encoded, err := readFile(root, path.Join(name, "cgroup.procs"), 64<<10)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		pids, err := processIDs(encoded, remaining)
		if err != nil {
			return err
		}
		remaining -= len(pids)
		for _, pid := range pids {
			if err = ctx.Err(); err != nil {
				return err
			}
			fd, err := unix.PidfdOpen(pid, 0)
			if errors.Is(err, unix.ESRCH) {
				continue
			}
			if err != nil {
				return err
			}
			// pidfd 保持目标身份，重新核实归属后才发送信号，避免枚举后的 PID 复用误伤。
			current, readErr := processPath(pid)
			if readErr == nil && current == path.Join(parent, name) {
				err = unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0)
			} else if errors.Is(readErr, os.ErrNotExist) {
				err = nil
			} else if readErr != nil {
				err = readErr
			} else {
				err = errors.New("退出信号目标已离开受管资源组")
			}
			if errors.Is(err, unix.ESRCH) {
				err = nil
			}
			if err = errors.Join(err, unix.Close(fd)); err != nil {
				return err
			}
		}
	}
	return nil
}
func (groups *controller) waitEmpty(ctx context.Context, root *os.Root, name string) error {
	ticker := time.NewTicker(groups.options.PollInterval)
	defer ticker.Stop()
	for {
		busy, err := isPopulated(root, name)
		if err != nil {
			return err
		}
		if !busy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Drain 仅对固定会话子树终止进程。移除叶组及父组成功后才返回，迟到的入组描述符不能复活已移除分组。
func (groups *controller) Drain(ctx context.Context, reference Reference) error {
	if err := reference.Validate(); err != nil {
		return err
	}
	return groups.drainTree(ctx, reference.container(), reference.Name(), []string{"shell", "browser"}, []any{"user_id", reference.Session.UserID, "run_id", reference.Session.RunID, "session_id", reference.Session.SessionID})
}

func (groups *controller) drainTree(ctx context.Context, parent ContainerReference, name string, children []string, attributes []any) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := openContainerParent(parent)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, root.Close()) }()
	if err = verifyGroup(root, name); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err = groups.signal(ctx, root, parent.Path, groupNames(name, children)); err != nil {
		groups.logger.WarnContext(ctx, "正常退出信号未完成，将强制回收受管进程", attributes...)
	}
	grace, stop := context.WithTimeout(ctx, groups.options.GracePeriod)
	err = groups.waitEmpty(grace, root, name)
	stop()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err = writeFile(root, path.Join(name, "cgroup.kill"), "1"); err != nil {
			return errors.New("强制终止电脑进程树失败")
		}
		if err = groups.waitEmpty(ctx, root, name); err != nil {
			return err
		}
	}
	// 不递归删除未知子组；任何漂移或迟到入组导致的忙状态都保留为清理未确认。
	for _, child := range children {
		if err = root.Remove(path.Join(name, child)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("电脑进程叶组尚未释放")
		}
	}
	if err = root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("电脑进程树尚未释放")
	}
	groups.logger.InfoContext(ctx, "电脑受管进程树已回收", attributes...)
	return nil
}

var _ Groups = (*controller)(nil)
