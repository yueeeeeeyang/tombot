//go:build linux

package processgroup

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path"
)

type serviceController struct{ groups *controller }

// NewServices 与任务资源组共用真实内核检查；仅供受信节点管理容器基础服务。
func NewServices(options Options, logger *slog.Logger) (ServiceGroups, error) {
	groups, err := newController(options, logger)
	if err != nil {
		return nil, err
	}
	return &serviceController{groups: groups}, nil
}

// Create 只建立当前服务的空组；已有未知进程、错误实例或缺少整树回收能力时失败。
func (services *serviceController) Create(ctx context.Context, reference ServiceReference) error {
	if err := reference.Validate(); err != nil {
		return err
	}
	return createTree(ctx, reference.Container, reference.Name(), nil)
}

// Controls 返回服务入组、终止和只读事件入口；不包含任何 Bot 会话入口。
func (services *serviceController) Controls(ctx context.Context, reference ServiceReference) ([]*os.File, error) {
	if err := reference.Validate(); err != nil {
		return nil, err
	}
	return openControls(ctx, reference.Container, []string{reference.Name()}, serviceControlPaths(reference))
}

// Contains 验证实际 PID 处于当前服务的精确资源组；不存在返回 false，未知内核状态返回错误。
// 调用方还须校验对端 UID/GID、服务授权和生命周期，不能单凭 PID 放行连接。
func (services *serviceController) Contains(ctx context.Context, reference ServiceReference, pid int) (member bool, failure error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if reference.Validate() != nil || pid <= 1 {
		return false, errors.New("服务进程身份无效")
	}
	root, err := openContainerParent(reference.Container)
	if err != nil {
		return false, err
	}
	defer func() { failure = errors.Join(failure, root.Close()) }()
	if err = verifyGroup(root, reference.Name()); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	current, err := processPath(pid)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = ctx.Err(); err != nil {
		return false, err
	}
	return current == path.Join(reference.Container.Path, reference.Name()), nil
}

// Drain 先尝试正常退出，再强制终止并确认空组、移除目录；不触碰其他服务或 Bot 任务。
func (services *serviceController) Drain(ctx context.Context, reference ServiceReference) error {
	if err := reference.Validate(); err != nil {
		return err
	}
	// Docker 已删除整个父组时，内核目录不存在才能证明旧入口已失效；不将其他读取错误当作不存在。
	absent, err := containerGroupAbsent(ctx, reference.Container)
	if err != nil || absent {
		return err
	}
	return services.groups.drainTree(ctx, reference.Container, reference.Name(), nil, []any{"container_id", reference.Container.ContainerID, "service_id", reference.ID, "service_kind", reference.Kind})
}

func containerGroupAbsent(ctx context.Context, reference ContainerReference) (absent bool, failure error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	root, err := openCgroupRoot()
	if err != nil {
		return false, err
	}
	defer func() {
		failure = errors.Join(failure, root.Close())
		if failure != nil {
			absent = false
		}
	}()
	_, err = root.Lstat(reference.Path)
	if errors.Is(err, os.ErrNotExist) {
		return true, ctx.Err()
	}
	return false, errors.Join(err, ctx.Err())
}

func serviceControlPaths(reference ServiceReference) []string {
	return []string{path.Join(reference.Name(), "cgroup.procs"), path.Join(reference.Name(), "cgroup.kill"), path.Join(reference.Name(), "cgroup.events")}
}

// ValidateServiceControls 在容器代理中复验三个服务入口，拒绝 Bot 入口、其他服务、文件替身和可写事件。
func ValidateServiceControls(reference ServiceReference, files []*os.File) error {
	if reference.Validate() != nil || len(files) != 3 {
		return errors.New("基础服务控制身份或数量无效")
	}
	return validateControlFiles(reference.Container.Path, serviceControlPaths(reference), files)
}

var _ ServiceGroups = (*serviceController)(nil)
