// Package processgroup 在受信 Linux 节点中管理用户任务的 cgroup v2 子树。
package processgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"io.yueyang/tom/internal/computer"
)

// Reference 由已核实容器的主进程定位，并持久绑定父资源组的设备号和 inode，不能由用户提供。
type Reference struct {
	Session      computer.ExecutionSession `json:"session"`
	ParentPath   string                    `json:"parent_path"`
	ParentDevice uint64                    `json:"parent_device"`
	ParentInode  uint64                    `json:"parent_inode"`
}

// ContainerReference 将用户电脑基础设施绑定到实际容器父组，不借用或伪造 Bot 执行会话。
type ContainerReference struct {
	ContainerID string `json:"container_id"`
	Path        string `json:"path"`
	Device      uint64 `json:"device"`
	Inode       uint64 `json:"inode"`
}

func (reference ContainerReference) Validate() error {
	if len(reference.ContainerID) != 64 || strings.Trim(reference.ContainerID, "0123456789abcdef") != "" || !validParent(reference.Path, reference.ContainerID) || reference.Device == 0 || reference.Inode == 0 {
		return errors.New("容器父资源组身份无效")
	}
	return nil
}

// Validate 检查完整会话及不可替换的父资源组身份；错误路径不能进入宿主文件系统操作。
func (reference Reference) Validate() error {
	if err := reference.Session.Validate(); err != nil {
		return err
	}
	if !validParent(reference.ParentPath, reference.Session.ContainerID) || reference.ParentDevice == 0 || reference.ParentInode == 0 {
		return errors.New("电脑进程资源组身份无效")
	}
	return nil
}

// Name 为每次执行生成独立路径，避免迟到清理复用另一会话的进程资源组。
func (reference Reference) Name() string {
	return fmt.Sprintf("tombot-session-%s-f%d", strings.ToLower(reference.Session.SessionID), reference.Session.FencingToken)
}

func (reference Reference) container() ContainerReference {
	return ContainerReference{ContainerID: reference.Session.ContainerID, Path: reference.ParentPath, Device: reference.ParentDevice, Inode: reference.ParentInode}
}

// Groups 所有方法都针对已定位的固定实例；Create 只建立分组，不执行任何用户代码。
// Membership 返回受控叶组的写入描述符，可信引导进程写入自身后必须关闭它，再降权执行用户代码。
type Groups interface {
	Locate(context.Context, computer.ExecutionSession, int) (Reference, error)
	Create(context.Context, Reference) error
	Drain(context.Context, Reference) error
	Membership(context.Context, Reference, string) (*os.File, error)
	// Controls 返回 Shell 入组、浏览器入组、整组终止和存活事件四个受限描述符。
	// 仅交给同一实例的受信代理；调用方负责关闭全部返回描述符。
	Controls(context.Context, Reference) ([]*os.File, error)
}

type Options struct {
	GracePeriod      time.Duration
	PollInterval     time.Duration
	MaximumProcesses int
}

// Validate 供部署入口在建立实际内核后端前检查显式配置。
func (options Options) Validate() error {
	if options.GracePeriod < 10*time.Millisecond || options.GracePeriod > 10*time.Second || options.PollInterval < time.Millisecond || options.PollInterval > 100*time.Millisecond || options.MaximumProcesses < 16 || options.MaximumProcesses > 4096 {
		return errors.New("进程资源组配置无效")
	}
	return nil
}
func validParent(value, containerID string) bool {
	if value == "" || strings.HasPrefix(value, "/") || path.Clean(value) != value || strings.ContainsAny(value, "\\\x00\n\r") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "." || part == ".." || part == "" {
			return false
		}
	}
	base := path.Base(value)
	return base == containerID || base == "docker-"+containerID+".scope"
}

func unifiedPath(encoded []byte) (string, error) {
	if len(encoded) > 16<<10 {
		return "", errors.New("进程资源组信息过大")
	}
	result := ""
	for _, line := range strings.Split(strings.TrimSpace(string(encoded)), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			return "", errors.New("进程资源组信息无效")
		}
		if parts[0] != "0" {
			continue
		}
		if result != "" || parts[1] != "" || !strings.HasPrefix(parts[2], "/") || path.Clean(parts[2]) != parts[2] {
			return "", errors.New("统一资源组路径无效")
		}
		result = strings.TrimPrefix(parts[2], "/")
	}
	if result == "" {
		return "", errors.New("进程不属于可管理的统一资源组")
	}
	return result, nil
}
func populated(encoded []byte) (bool, error) {
	if len(encoded) > 8192 {
		return false, errors.New("进程资源组事件过大")
	}
	found, value := false, false
	for _, line := range strings.Split(strings.TrimSpace(string(encoded)), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 2 {
			return false, errors.New("进程资源组事件无效")
		}
		if parts[0] != "populated" {
			continue
		}
		if found || parts[1] != "0" && parts[1] != "1" {
			return false, errors.New("进程资源组存活状态无效")
		}
		found, value = true, parts[1] == "1"
	}
	if !found {
		return false, errors.New("缺少进程资源组存活状态")
	}
	return value, nil
}
func processIDs(encoded []byte, limit int) ([]int, error) {
	if len(encoded) > 64<<10 {
		return nil, errors.New("进程列表过大")
	}
	fields := strings.Fields(string(encoded))
	if len(fields) > limit {
		return nil, errors.New("进程数量超过受管上限")
	}
	result := make([]int, 0, len(fields))
	for _, field := range fields {
		value, err := strconv.ParseInt(field, 10, 32)
		if err != nil || value <= 1 {
			return nil, errors.New("进程标识无效")
		}
		result = append(result, int(value))
	}
	return result, nil
}
