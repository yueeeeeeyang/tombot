package dockernode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/computerprobe"
	"io.yueyang/tom/internal/egressproxy"
	"io.yueyang/tom/internal/processgroup"
)

// Resource 的宿主存储目录和 Docker 名称均由节点派生，调用者不能传入任意名称或路径。
type Resource struct {
	Specification computer.Specification
	Name          string
	WorkspacePath string
	ControlPath   string
	Labels        map[string]string
}

// Workspace 区分资源存在和可用挂载；只有 Ready 时 CapacityBytes 才表示已执行的硬容量。
type Workspace struct {
	Exists        bool
	Ready         bool
	Path          string
	CapacityBytes int64
}

// Storage 实现持久文件系统及硬配额；Remove 仅允许清理本次未投入使用的开户代次。
type Storage interface {
	Inspect(context.Context, Resource) (Workspace, error)
	Ensure(context.Context, Resource) (Workspace, error)
	Remove(context.Context, Resource) error
	VerifyFilesystem(context.Context, Resource, int64, uint64) error
}

// Probe 在指定实例执行实际能力检查，不允许根据镜像标签或配置推断全部能力成功。
type Probe interface {
	Check(context.Context, Resource, string, computerprobe.Evidence) (computer.Capabilities, error)
}

// Options 全部来自受保护节点配置，不通过管理 HTTP 接受 Docker 地址、入口命令或宿主目录。
type Options struct {
	NodeID                 string
	Socket                 string
	StateDirectory         string
	StorageDirectory       string
	CallTimeout            time.Duration
	MaximumLease           time.Duration
	TemporaryBytes         int64
	RuntimeBytes           int64
	SharedMemoryBytes      int64
	Agent                  computeragent.RuntimeOptions
	AgentStartup           AgentStartupOptions
	Probes                 ProbeOptions
	Browser                BrowserOptions
	Egress                 egressproxy.Options
	MaximumEgressComputers int
}

// Runtime 以每用户持久锁串行处理生命周期，并将代次与清理意图先刷盘再调用 Docker。
type Runtime struct {
	options     Options
	engine      engine
	storage     Storage
	probe       Probe
	probes      computerProbes
	browsers    computerBrowsers
	agent       agentAvailability
	journal     *journal
	closeEngine func()
	egress      computerEgress
	releaseLock func() error
	heartbeat   *runtimeHeartbeat
	mounts      func() ([]filesystemMount, error)
}

// New 仅在 Linux 受信 root 节点中启用，拒绝不受保护的目录及缺失的真实存储和能力探针。
func New(ctx context.Context, options Options, storage Storage, logger *slog.Logger) (*Runtime, error) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return nil, errors.New("电脑节点要求 Linux 受信系统服务")
	}
	if err := validateOptions(options); err != nil {
		return nil, err
	}
	for _, path := range []string{options.StateDirectory, options.StorageDirectory, filepath.Dir(options.Socket)} {
		if err := checkProtectedParents(path); err != nil {
			return nil, err
		}
	}
	if storage == nil || logger == nil {
		return nil, errors.New("电脑存储或日志未配置")
	}
	if err := checkRootDockerSocket(options.Socket); err != nil {
		return nil, err
	}
	journal, err := openJournal(options.StateDirectory, 0)
	if err != nil {
		return nil, err
	}
	engine, err := newEngine(options.Socket, options.CallTimeout)
	if err != nil {
		return nil, errors.Join(err, journal.root.Close())
	}
	startup, cancel := context.WithTimeout(ctx, options.CallTimeout)
	defer cancel()
	release, err := journal.lock(startup, "lifecycle-service")
	if err != nil {
		engine.close()
		return nil, errors.Join(err, journal.root.Close())
	}
	options.Egress.Ports = slices.Clone(options.Egress.Ports)
	options.Egress.DeniedNetworks = slices.Clone(options.Egress.DeniedNetworks)
	node := &Runtime{options: options, engine: engine, storage: storage, journal: journal, closeEngine: engine.close, releaseLock: release, mounts: readMounts}
	node.probe = &capabilityProbe{node: node, locate: processgroup.LocateContainer}
	node.agent = newAgentStartup(node)
	node.egress = newEgress(node, logger)
	node.heartbeat, err = newHeartbeat(journal, options.NodeID)
	if err != nil {
		return nil, errors.Join(err, node.Close())
	}
	probes, err := newProbes(node, logger)
	if err == nil {
		node.probes = probes
		err = probes.recover(ctx)
	}
	if err != nil {
		return nil, errors.Join(err, node.Close())
	}
	browsers, err := newBrowsers(node, logger)
	if err == nil {
		node.browsers = browsers
		err = browsers.recover(ctx)
	}
	if err != nil {
		return nil, errors.Join(err, node.Close())
	}
	return node, nil
}

// checkRootDockerSocket 拒绝普通文件、替身及对所有用户开放的 Docker 管理入口。
func checkRootDockerSocket(path string) error {
	socket, err := os.Lstat(path)
	if err != nil {
		return failure("docker_socket_unavailable")
	}
	owner, ok := socket.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 || socket.Mode()&os.ModeSocket == 0 || socket.Mode().Perm() != 0600 && socket.Mode().Perm() != 0660 {
		return failure("docker_socket_unsafe")
	}
	return nil
}

// Validate 在打开宿主资源前检查显式节点参数，运行时仍须复验实际目录和 Docker 身份。
func (options Options) Validate() error { return validateOptions(options) }

func validateOptions(options Options) error {
	if (computer.NodeQuery{NodeID: options.NodeID}).Validate() != nil {
		return errors.New("电脑节点标识无效")
	}
	if err := options.Agent.Validate(); err != nil {
		return err
	}
	if err := options.AgentStartup.Validate(); err != nil {
		return err
	}
	if err := options.Probes.Validate(); err != nil {
		return err
	}
	if err := options.Browser.Validate(); err != nil {
		return err
	}
	if options.Browser.Limits.CallTimeoutMS > options.Agent.CallTimeoutMS || options.Browser.Limits.ShutdownTimeoutMS > options.Agent.CleanupTimeoutMS || options.Browser.CleanupTimeout <= options.Browser.CallTimeout+time.Duration(options.Agent.CleanupTimeoutMS)*time.Millisecond+options.Browser.Groups.GracePeriod+options.Browser.Groups.PollInterval {
		return errors.New("共享浏览器与代理回收预算不匹配")
	}
	if err := options.Egress.Validate(); err != nil {
		return err
	}
	if options.MaximumEgressComputers < 1 || options.MaximumEgressComputers > 10000 {
		return errors.New("节点出网电脑容量无效")
	}
	if options.CallTimeout < time.Second || options.CallTimeout > 5*time.Minute || options.MaximumLease < options.CallTimeout || options.MaximumLease > 30*time.Minute || options.TemporaryBytes < 16<<20 || options.TemporaryBytes > 4<<30 {
		return errors.New("电脑节点时限或临时空间配置无效")
	}
	if options.RuntimeBytes < 16<<20 || options.RuntimeBytes > 4<<30 || options.SharedMemoryBytes < 64<<20 || options.SharedMemoryBytes > 4<<30 {
		return errors.New("电脑图形运行空间或共享内存配置无效")
	}
	for _, path := range []string{options.StateDirectory, options.StorageDirectory, options.Socket} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return errors.New("电脑节点目录必须是规范绝对路径")
		}
	}
	if options.StateDirectory == options.StorageDirectory || strings.HasPrefix(options.StateDirectory, options.StorageDirectory+string(filepath.Separator)) {
		return errors.New("节点控制记录不得保存在用户存储中")
	}
	return nil
}

// Close 在服务已停止接受请求且在途操作全部结束后释放节点资源。
func (node *Runtime) Close() error {
	if node.browsers != nil {
		ctx, cancel := context.WithTimeout(context.Background(), node.options.Browser.ShutdownTimeout)
		err := node.browsers.shutdown(ctx)
		cancel()
		if err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), node.options.Egress.ShutdownTimeout)
	defer cancel()
	if err := node.egress.shutdown(ctx); err != nil {
		return err
	}
	node.closeEngine()
	var err error
	if node.releaseLock != nil {
		err = node.releaseLock()
	}
	return errors.Join(err, node.journal.root.Close())
}

// Serve 监督用户出网服务；任一实例意外结束时封闭全部节点出网并报告失败，供节点主程序终止接单。
func (node *Runtime) Serve(ctx context.Context) error { return node.egress.wait(ctx) }

func failure(code string) error { return &computer.Failure{Code: code, IsPermanent: true} }

func (node *Runtime) resource(specification computer.Specification) Resource {
	name := resourceName(specification)
	identity := specification
	identity.FencingToken = 0
	identity.LeaseExpiresAt = time.Time{}
	encoded, err := json.Marshal(identity)
	if err != nil {
		panic("电脑资源身份无法编码")
	}
	digest := sha256.Sum256(encoded)
	return Resource{Specification: specification, Name: name, WorkspacePath: filepath.Join(node.options.StorageDirectory, name, "workspace"), ControlPath: filepath.Join(node.options.StateDirectory, "agent-"+name), Labels: map[string]string{
		"io.tombot.managed": "computer", "io.tombot.node": specification.NodeID, "io.tombot.user": specification.UserID,
		"io.tombot.computer": specification.ComputerID, "io.tombot.operation": specification.OperationID, "io.tombot.generation": fmt.Sprint(specification.Generation), "io.tombot.identity": hex.EncodeToString(digest[:])}}
}

func resourceName(specification computer.Specification) string {
	return fmt.Sprintf("tombot-%s-g%d", strings.ToLower(specification.ComputerID), specification.Generation)
}

func immutableEqual(left, right computer.Specification) bool {
	left.FencingToken = 0
	left.LeaseExpiresAt = time.Time{}
	right.FencingToken = 0
	right.LeaseExpiresAt = time.Time{}
	return left == right
}

func sameComputerIdentity(left, right computer.Specification) bool {
	return left.UserID == right.UserID && left.ComputerID == right.ComputerID && left.NodeID == right.NodeID && left.OperationID == right.OperationID && left.VolumeReference == right.VolumeReference
}

func (node *Runtime) guarded(ctx context.Context, specification computer.Specification, action func(context.Context, Resource, *record) (computer.Observation, error)) (observation computer.Observation, result error) {
	if specification.Validate() != nil || specification.NodeID != node.options.NodeID {
		return observation, failure("specification_invalid")
	}
	remaining := time.Until(specification.LeaseExpiresAt)
	if remaining <= 0 || remaining > node.options.MaximumLease {
		return observation, failure("lease_invalid")
	}
	// 将收到时的剩余期限转换为本地单调时钟截止点；等待资源锁也消耗同一租约。
	ctx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	release, err := node.journal.lock(ctx, specification.UserID)
	if err != nil {
		return observation, err
	}
	defer func() { result = errors.Join(result, release()) }()
	if err = ctx.Err(); err != nil {
		return observation, err
	}
	state, err := node.journal.read(specification.UserID)
	if err != nil {
		return observation, err
	}
	if state == nil {
		state = &record{Version: lifecycleVersion, Specification: specification, Phase: "reserved"}
	} else {
		previous := state.Specification
		if !sameComputerIdentity(previous, specification) {
			return observation, failure("resource_identity_mismatch")
		}
		if specification.Generation == previous.Generation {
			if state.Retirement != nil {
				return observation, failure("generation_retirement_started")
			}
			if !immutableEqual(previous, specification) || specification.FencingToken < previous.FencingToken || specification.FencingToken == previous.FencingToken && !specification.LeaseExpiresAt.Equal(previous.LeaseExpiresAt) {
				return observation, failure("stale_controller")
			}
		} else {
			if state.Phase != "absent" || specification.Generation != previous.Generation+1 || specification.Attempt != previous.Attempt+1 || specification.FencingToken <= previous.FencingToken {
				return observation, failure("generation_not_released")
			}
			if state.Retirement != nil {
				pending := state.Retirement.Next
				if !immutableEqual(pending, specification) || specification.FencingToken < pending.FencingToken || specification.FencingToken == pending.FencingToken && !specification.LeaseExpiresAt.Equal(pending.LeaseExpiresAt) {
					return observation, failure("stale_controller")
				}
				state.Retirement.Next = specification
				if err = node.journal.save(state); err != nil {
					return observation, err
				}
			}
			if err = node.retireAgent(ctx, state, specification); err != nil {
				return observation, err
			}
			state.Phase = "reserved"
			state.ContainerID = ""
			state.Retirement = nil
		}
		state.Specification = specification
	}
	if err = node.journal.save(state); err != nil {
		return observation, err
	}
	return action(ctx, node.resource(specification), state)
}

func observed(resource Resource, state, containerID string, capabilities computer.Capabilities) computer.Observation {
	specification := resource.Specification
	return computer.Observation{OperationID: specification.OperationID, UserID: specification.UserID, ComputerID: specification.ComputerID, Generation: specification.Generation, FencingToken: specification.FencingToken, VolumeReference: specification.VolumeReference, State: state, ContainerID: containerID, Capabilities: capabilities}
}

// Inspect 只返回实际资源状态；缺失、部分存在和完整能力就绪分别处理，未知结果不解释为不存在。
func (node *Runtime) Inspect(ctx context.Context, specification computer.Specification) (computer.Observation, error) {
	return node.guarded(ctx, specification, node.inspect)
}

// Ensure 按同一代次的固定名称创建资源；调用失败后由 Inspect 对账，不换名称重试。
func (node *Runtime) Ensure(ctx context.Context, specification computer.Specification) (computer.Observation, error) {
	return node.guarded(ctx, specification, node.ensure)
}

// Remove 持久封闭本代次创建及出网，再停止并核实容器已删除，最后删除本代次卷与存储。
func (node *Runtime) Remove(ctx context.Context, specification computer.Specification) (computer.Observation, error) {
	return node.guarded(ctx, specification, node.remove)
}
