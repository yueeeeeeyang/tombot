//go:build linux

package processgroup

import (
	"context"
	"errors"
	"os"
	"path"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Controls 只从已固定的父组打开当前会话所需入口，不授予目录写权限或其他会话权限。
func (groups *controller) Controls(ctx context.Context, reference Reference) (files []*os.File, failure error) {
	if err := reference.Validate(); err != nil {
		return nil, err
	}
	return openControls(ctx, reference.container(), groupNames(reference.Name(), []string{"shell", "browser"}), controlPaths(reference))
}

func openControls(ctx context.Context, parent ContainerReference, names, paths []string) (files []*os.File, failure error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := openContainerParent(parent)
	if err != nil {
		return nil, err
	}
	defer func() {
		failure = errors.Join(failure, root.Close())
		if failure != nil {
			for _, file := range files {
				failure = errors.Join(failure, file.Close())
			}
			files = nil
		}
	}()
	for _, name := range names {
		if err = verifyGroup(root, name); err != nil {
			return files, err
		}
	}
	for index, name := range paths {
		flags := os.O_WRONLY
		if index == len(paths)-1 {
			flags = os.O_RDONLY
		}
		file, err := root.OpenFile(name, flags|unix.O_NOFOLLOW, 0)
		if err != nil {
			return files, err
		}
		files = append(files, file)
	}
	if err := ctx.Err(); err != nil {
		return files, err
	}
	return files, nil
}

func controlPaths(reference Reference) []string {
	return []string{path.Join(reference.Name(), "shell", "cgroup.procs"), path.Join(reference.Name(), "browser", "cgroup.procs"), path.Join(reference.Name(), "cgroup.kill"), path.Join(reference.Name(), "cgroup.events")}
}

// ValidateControls 在容器中复验收到的四个入口；描述符顺序、权限、内核类型和完整实例路径必须一致。
// 不检查当前租约，调用者必须先执行协议身份和控制代次校验。
func ValidateControls(reference Reference, files []*os.File) error {
	if reference.Validate() != nil || len(files) != 4 {
		return errors.New("代理资源组控制身份或数量无效")
	}
	return validateControlFiles(reference.ParentPath, controlPaths(reference), files)
}

func validateControlFiles(parent string, paths []string, files []*os.File) error {
	if len(files) != len(paths) {
		return errors.New("代理资源组控制描述符数量无效")
	}
	for index, name := range paths {
		file := files[index]
		if file == nil {
			return errors.New("代理资源组控制描述符缺失")
		}
		fd := int(file.Fd())
		var stat unix.Stat_t
		var filesystem unix.Statfs_t
		mode := unix.O_WRONLY
		if index == len(paths)-1 {
			mode = unix.O_RDONLY
		}
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
		if err != nil || flags&unix.O_ACCMODE != mode || unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != 0 || stat.Gid != 0 || stat.Mode&0022 != 0 || unix.Fstatfs(fd, &filesystem) != nil || filesystem.Type != unix.CGROUP2_SUPER_MAGIC {
			return errors.New("代理资源组描述符类型或权限无效")
		}
		target, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
		if err != nil || !strings.HasSuffix(target, "/"+parent+"/"+name) {
			return errors.New("代理资源组描述符与固定实例不符")
		}
	}
	return nil
}

// Populated 校验有界的内核存活事件，供持有只读事件描述符的容器看门狗使用。
func Populated(encoded []byte) (bool, error) { return populated(encoded) }
