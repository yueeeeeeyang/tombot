// Package computer 定义用户电脑及受信节点协议，业务 API 不接触通用 Docker 权限。
package computer

import (
	"context"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// Resources 为一台用户电脑的硬限制；磁盘限额必须由节点实际存储后端执行。
type Resources struct {
	CPUMillis    int   `json:"cpu_millis"`
	MemoryBytes  int64 `json:"memory_bytes"`
	DiskBytes    int64 `json:"disk_bytes"`
	ProcessLimit int   `json:"process_limit"`
}

// Validate 拒绝无界分配，部署还须依据节点实际容量执行准入。
func (resources Resources) Validate() error {
	const minMemory = int64(128 << 20)
	const maxMemory = int64(1 << 40)
	const minDisk = int64(512 << 20)
	const maxDisk = int64(100 << 40)
	const minCPU, maxCPU, minProcesses, maxProcesses = 100, 256000, 16, 4096
	if resources.CPUMillis < minCPU || resources.CPUMillis > maxCPU || resources.MemoryBytes < minMemory || resources.MemoryBytes > maxMemory || resources.DiskBytes < minDisk || resources.DiskBytes > maxDisk || resources.ProcessLimit < minProcesses || resources.ProcessLimit > maxProcesses {
		return errors.New("电脑资源限额无效")
	}
	return nil
}

// Specification 是受信控制服务发出的不可变资源身份与当前租约；节点必须持久校验代次和 fencing token。
type Specification struct {
	OperationID     string    `json:"operation_id"`
	UserID          string    `json:"user_id"`
	ComputerID      string    `json:"computer_id"`
	NodeID          string    `json:"node_id"`
	ImageReference  string    `json:"image_reference"`
	VolumeReference string    `json:"volume_reference"`
	Generation      int64     `json:"generation"`
	FencingToken    int64     `json:"fencing_token"`
	Attempt         int       `json:"attempt"`
	LeaseExpiresAt  time.Time `json:"lease_expires_at"`
	Resources       Resources `json:"resources"`
}

// Validate 在调用节点前和节点接收后共同验证资源身份；外部输入不能指定宿主路径或浮动镜像。
func (specification Specification) Validate() error {
	for _, id := range []string{specification.OperationID, specification.UserID, specification.ComputerID, specification.NodeID} {
		if _, err := ulid.ParseStrict(id); err != nil {
			return errors.New("电脑资源标识无效")
		}
	}
	if specification.Generation < 1 || specification.FencingToken < 1 || specification.Attempt < 1 || specification.LeaseExpiresAt.IsZero() {
		return errors.New("电脑操作租约无效")
	}
	if err := ValidateImageReference(specification.ImageReference); err != nil {
		return err
	}
	if specification.VolumeReference != "tombot-user-"+specification.UserID {
		return errors.New("电脑持久卷名称与用户不符")
	}
	return specification.Resources.Validate()
}

var imageReferencePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9./:_-]{0,220}@sha256:[0-9a-f]{64}$`)

// ValidateImageReference 供部署登记和实际资源规格共用；固定摘要不代表镜像已下载或能力已验收。
func ValidateImageReference(reference string) error {
	if !imageReferencePattern.MatchString(reference) {
		return errors.New("电脑镜像必须使用固定摘要")
	}
	return nil
}

// Capabilities 只能由节点实际探针生成；缺少任何必需能力均不能将电脑标为就绪。
type Capabilities struct {
	Terminal         bool `json:"terminal"`
	Browser          bool `json:"browser"`
	Files            bool `json:"files"`
	ProcessReaping   bool `json:"process_reaping"`
	DiskQuota        bool `json:"disk_quota"`
	NetworkIsolation bool `json:"network_isolation"`
}

// Ready 要求所有基础能力同时通过，不能用进程存在或 HTTP 可达替代健康探针。
func (capabilities Capabilities) Ready() bool {
	return capabilities.Terminal && capabilities.Browser && capabilities.Files && capabilities.ProcessReaping && capabilities.DiskQuota && capabilities.NetworkIsolation
}

// Observation 返回节点刚刚核实的状态；absent 表示本次开户的容器、卷和网络全部不存在。
type Observation struct {
	OperationID     string       `json:"operation_id"`
	UserID          string       `json:"user_id"`
	ComputerID      string       `json:"computer_id"`
	Generation      int64        `json:"generation"`
	FencingToken    int64        `json:"fencing_token"`
	State           string       `json:"state"`
	ContainerID     string       `json:"container_id,omitempty"`
	VolumeReference string       `json:"volume_reference"`
	Capabilities    Capabilities `json:"capabilities"`
}

// Matches 拒绝错误用户、旧实例和旧控制者的回包；缺失状态不能被解释为资源不存在。
func (observation Observation) Matches(specification Specification) bool {
	if observation.ContainerID != "" {
		if len(observation.ContainerID) != 64 || strings.ToLower(observation.ContainerID) != observation.ContainerID {
			return false
		}
		if _, err := hex.DecodeString(observation.ContainerID); err != nil {
			return false
		}
	}
	return observation.OperationID == specification.OperationID && observation.UserID == specification.UserID && observation.ComputerID == specification.ComputerID && observation.Generation == specification.Generation && observation.FencingToken == specification.FencingToken && observation.VolumeReference == specification.VolumeReference &&
		(observation.State == "absent" && observation.ContainerID == "" || observation.State == "present" || observation.State == "ready" && observation.ContainerID != "" || observation.State == "quarantined")
}

// Runtime 仅提供受限生命周期动作，不能执行任意宿主命令或任意 Docker 参数。
// Ensure 必须按资源身份幂等；Remove 先确认停止，再仅删除尚未启用且属于该操作的资源。
type Runtime interface {
	Inspect(context.Context, Specification) (Observation, error)
	Ensure(context.Context, Specification) (Observation, error)
	Remove(context.Context, Specification) (Observation, error)
}

// Failure 为节点确认的失败分类，不包含命令、凭据或原始远端错误响应。
type Failure struct {
	Code        string `json:"code"`
	IsPermanent bool   `json:"is_permanent"`
}

func (failure *Failure) Error() string { return "电脑节点操作失败" }
