package dockernode

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strings"

	computerimage "io.yueyang/tom/deploy/computer"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/noderelease"
)

type snapshot struct {
	volume                  volume
	container               container
	workspace               Workspace
	hasVolume, hasContainer bool
}

func (snapshot snapshot) absent() bool {
	return !snapshot.hasVolume && !snapshot.hasContainer && !snapshot.workspace.Exists
}

func (node *Runtime) load(ctx context.Context, resource Resource, state *record) (snapshot, error) {
	resource = node.withStorage(resource, state)
	var current snapshot
	var err error
	current.volume, err = node.engine.inspectVolume(ctx, resource.Name)
	if err != nil && !errors.Is(err, errMissing) {
		return current, err
	}
	current.hasVolume = err == nil
	current.container, err = node.engine.inspectContainer(ctx, resource.Name)
	if err != nil && !errors.Is(err, errMissing) {
		return current, err
	}
	current.hasContainer = err == nil
	current.workspace, err = node.storage.Inspect(ctx, node.diskResource(resource, state))
	if err != nil {
		return current, err
	}
	if err = node.validateSnapshot(resource, state, current); err != nil {
		return current, err
	}
	if current.hasContainer {
		if err = node.configureAgent(resource, false); err != nil {
			return current, err
		}
	}
	return current, nil
}

func (node *Runtime) validateSnapshot(resource Resource, state *record, current snapshot) error {
	if current.workspace.Exists && (current.workspace.Path != resource.WorkspacePath || current.workspace.CapacityBytes != resource.Specification.Resources.DiskBytes) {
		return failure("workspace_identity_mismatch")
	}
	if current.hasVolume {
		volume := current.volume
		expected := map[string]string{"type": "none", "device": resource.WorkspacePath, "o": "bind"}
		if volume.Name != resource.Name || volume.Driver != "local" || !sameLabels(volume.Labels, resource.Labels) || !reflect.DeepEqual(volume.Options, expected) {
			return failure("volume_identity_mismatch")
		}
	}
	if current.hasContainer {
		container := current.container
		if !observed(resource, "present", container.ID, computer.Capabilities{}).Matches(resource.Specification) || container.ID == "" || container.Name != "/"+resource.Name || !matchingComputerLabels(container.Config.Labels, node.mountContract(resource, container)) || state.ContainerID != "" && state.ContainerID != container.ID {
			return failure("container_identity_mismatch")
		}
		if err := node.validateContainer(resource, container); err != nil {
			return err
		}
		if !current.hasVolume || !current.workspace.Exists || current.container.State.Running && !current.workspace.Ready {
			return failure("container_dependencies_missing")
		}

	}
	return nil
}

// containerLabels 包含镜像继承的布局声明；卷只使用资源身份标签，不能混入容器镜像元数据。
func containerLabels(resource Resource) map[string]string {
	labels := make(map[string]string, len(resource.Labels)+1)
	for key, value := range resource.Labels {
		labels[key] = value
	}
	labels[computer.StorageLayoutLabel] = computer.StorageLayoutVersion
	labels[computer.DesktopBrowserLabel] = "1"
	if resource.ExternalPrograms {
		labels[noderelease.ProgramImageLabel] = "1"
	}
	return labels
}

func matchingComputerLabels(actual map[string]string, resource Resource) bool {
	expected := containerLabels(resource)
	if _, exists := actual[computer.DesktopBrowserLabel]; !exists {
		delete(expected, computer.DesktopBrowserLabel)
	}
	return sameLabels(actual, expected)
}

func (node *Runtime) configuration(resource Resource) createContainer {
	init := true
	resources := resource.Specification.Resources
	configuration := createContainer{Image: resource.Specification.ImageReference, User: "0:0", Hostname: strings.ToLower(resource.Specification.ComputerID), WorkingDir: "/workspace",
		Entrypoint: []string{"/usr/lib/tombot/computer-agent"}, Cmd: []string{}, Env: []string{"LANG=C.UTF-8", "HOME=/run/tombot", "TMPDIR=/tmp"}, Labels: containerLabels(resource), StopSignal: "SIGTERM"}
	configuration.HostConfig = hostConfig{NetworkMode: "none", ReadonlyRootfs: true, CapDrop: []string{"ALL"}, CapAdd: computer.AgentCapabilities(),
		SecurityOpt: computerimage.SecurityOptions(), NanoCpus: int64(resources.CPUMillis) * 1000000, Memory: resources.MemoryBytes, MemorySwap: resources.MemoryBytes, ShmSize: node.options.SharedMemoryBytes, PidsLimit: int64(resources.ProcessLimit), Init: &init, IpcMode: "private", CgroupnsMode: "private",
		Mounts: []mount{
			{Type: "volume", Source: resource.Name, Target: "/workspace", VolumeOptions: &volumeMountOptions{NoCopy: true, Subpath: computer.WorkspaceSubdirectory}},
			{Type: "volume", Source: resource.Name, Target: computer.BrowserDirectory, VolumeOptions: &volumeMountOptions{NoCopy: true, Subpath: computer.DesktopProfilesSubdirectory}},
			{Type: "bind", Source: resource.ControlPath, Target: computeragent.ControlDirectory}},
		Tmpfs: map[string]string{"/tmp": fmt.Sprintf("rw,nosuid,nodev,noexec,size=%d,mode=1777", node.options.TemporaryBytes), "/run": fmt.Sprintf("rw,nosuid,nodev,noexec,size=%d,mode=0755", node.options.RuntimeBytes)}}
	if resource.ExternalPrograms {
		configuration.HostConfig.Mounts = append(configuration.HostConfig.Mounts, mount{Type: "bind", Source: noderelease.ProgramRoot, Target: noderelease.ProgramRoot, ReadOnly: true})
	}
	configuration.HostConfig.RestartPolicy.Name = "no"
	configuration.HostConfig.LogConfig.Type = "local"
	configuration.HostConfig.LogConfig.Config = map[string]string{"max-size": "10m", "max-file": "2"}
	return configuration
}

func capabilityNames(values []string) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = strings.TrimPrefix(value, "CAP_")
	}
	slices.Sort(result)
	return result
}

func (node *Runtime) validateContainer(resource Resource, container container) error {
	resource = node.mountContract(resource, container)
	expected := node.configuration(resource)
	if err := validateNetwork(container); err != nil {
		return err
	}
	actual := container.HostConfig
	if container.Config.Image != expected.Image || container.Config.User != expected.User || !slices.Equal(container.Config.Entrypoint, expected.Entrypoint) || len(container.Config.Cmd) != 0 {
		return failure("container_program_mismatch")
	}
	if actual.NetworkMode != expected.HostConfig.NetworkMode || !actual.ReadonlyRootfs || actual.Privileged || actual.AutoRemove || actual.PublishAllPorts || len(actual.PortBindings) > 0 || actual.PidMode != "" || actual.IpcMode != "private" || actual.CgroupnsMode != "private" {
		return failure("container_isolation_mismatch")
	}
	if actual.NanoCpus != expected.HostConfig.NanoCpus || actual.Memory != expected.HostConfig.Memory || actual.MemorySwap != expected.HostConfig.MemorySwap || actual.ShmSize != expected.HostConfig.ShmSize || actual.PidsLimit != expected.HostConfig.PidsLimit || actual.Init == nil || !*actual.Init {
		return failure("container_limits_mismatch")
	}
	if !slices.Equal(capabilityNames(actual.CapDrop), []string{"ALL"}) || !slices.Equal(capabilityNames(actual.CapAdd), capabilityNames(expected.HostConfig.CapAdd)) || !slices.Equal(actual.SecurityOpt, expected.HostConfig.SecurityOpt) {
		return failure("container_privileges_mismatch")
	}
	if !reflect.DeepEqual(actual.Tmpfs, expected.HostConfig.Tmpfs) || actual.RestartPolicy.Name != "no" || actual.LogConfig.Type != "local" || !reflect.DeepEqual(actual.LogConfig.Config, expected.HostConfig.LogConfig.Config) {
		return failure("container_runtime_mismatch")
	}
	// 旧镜像只能沿其原始的严格挂载契约完成排空与换代。新镜像必须带独立目录标签。
	if container.Config.Labels[computer.DesktopBrowserLabel] != "1" {
		expected.HostConfig.Mounts[1].VolumeOptions.Subpath = computer.BrowserSubdirectory
	}
	if len(container.Mounts) != len(expected.HostConfig.Mounts) || !sameMountConfiguration(actual.Mounts, expected.HostConfig.Mounts) {
		return failure("container_mount_mismatch")
	}
	workspace, browser, control, programs := false, false, false, false
	for _, mounted := range container.Mounts {
		switch mounted.Destination {
		case "/workspace":
			if workspace || mounted.Type != "volume" || mounted.Name != resource.Name || !mounted.RW {
				return failure("container_mount_mismatch")
			}
			workspace = true
		case computer.BrowserDirectory:
			if browser || mounted.Type != "volume" || mounted.Name != resource.Name || !mounted.RW {
				return failure("container_mount_mismatch")
			}
			browser = true
		case noderelease.ProgramRoot:
			if !resource.ExternalPrograms || programs || mounted.Type != "bind" || mounted.Source != noderelease.ProgramRoot || mounted.RW {
				return failure("container_mount_mismatch")
			}
			programs = true
		case computeragent.ControlDirectory:
			if control || mounted.Type != "bind" || mounted.Source != resource.ControlPath || !mounted.RW {
				return failure("container_mount_mismatch")
			}
			control = true
		default:
			return failure("container_mount_mismatch")
		}
	}
	if !workspace || !browser || !control || programs != resource.ExternalPrograms {
		return failure("container_mount_mismatch")
	}
	return nil
}

func sameMountConfiguration(actual, expected []mount) bool {
	if len(actual) != len(expected) {
		return false
	}
	remaining := make(map[string]mount, len(expected))
	for _, value := range expected {
		remaining[value.Target] = value
	}
	for _, value := range actual {
		wanted, exists := remaining[value.Target]
		if !exists || !reflect.DeepEqual(value, wanted) {
			return false
		}
		delete(remaining, value.Target)
	}
	return len(remaining) == 0
}

func (node *Runtime) inspect(ctx context.Context, resource Resource, state *record) (computer.Observation, error) {
	current, err := node.load(ctx, resource, state)
	if err != nil {
		return computer.Observation{}, err
	}
	if current.absent() {
		if err := node.browsers.retire(ctx, resource); err != nil {
			return computer.Observation{}, err
		}
		if err := node.probes.retire(ctx, resource); err != nil {
			return computer.Observation{}, err
		}
		if err := node.egress.stop(ctx, resource); err != nil {
			return computer.Observation{}, err
		}
		if state.Phase == "removing" {
			if err = node.storage.Remove(ctx, resource); err != nil {
				return computer.Observation{}, err
			}
			state.Phase = "absent"
			state.ContainerID = ""
			if err = node.journal.save(state); err != nil {
				return computer.Observation{}, err
			}
		}
		return observed(resource, "absent", "", computer.Capabilities{}), nil
	}
	if state.Phase == "absent" {
		return observed(resource, "quarantined", current.container.ID, computer.Capabilities{}), nil
	}
	if current.hasContainer && current.container.Config.Labels[computer.DesktopBrowserLabel] != "1" && state.Phase != "removing" {
		return observed(resource, "present", current.container.ID, computer.Capabilities{}), nil
	}
	if current.hasContainer && current.container.State.Running && state.Phase != "removing" {
		if resource.ExternalPrograms {
			core, err := node.selectedComputerPrograms(resource)
			if err != nil {
				return computer.Observation{}, err
			}
			if core != node.options.ProgramCore {
				return computer.Observation{}, failure("computer_program_upgrade_pending")
			}
		}
		if err := node.egress.ensure(ctx, resource, current.container); err != nil {
			return computer.Observation{}, err
		}
		if err := node.agent.wait(ctx, resource, current.container); err != nil {
			return computer.Observation{}, err
		}
		evidence, err := node.probes.check(ctx, resource, current.container)
		if err != nil {
			return computer.Observation{}, err
		}
		capabilities, err := node.probe.Check(ctx, resource, current.container.ID, evidence)
		if err != nil {
			return computer.Observation{}, err
		}
		if capabilities.Ready() {
			recovering, err := node.journal.hasComputerRecovery(state)
			if err != nil {
				return computer.Observation{}, err
			}
			state.Phase = "ready"
			state.ContainerID = current.container.ID
			if err = node.journal.save(state); err != nil {
				return computer.Observation{}, err
			}
			if recovering {
				if err = node.journal.root.Remove(state.recoveryFile()); err != nil {
					return computer.Observation{}, failure("computer_recovery_intent_remove_failed")
				}
			}
			return observed(resource, "ready", current.container.ID, capabilities), nil
		}
	} else {
		if err := node.browsers.retire(ctx, resource); err != nil {
			return computer.Observation{}, err
		}
		if err := node.egress.stop(ctx, resource); err != nil {
			return computer.Observation{}, err
		}
	}
	return observed(resource, "present", current.container.ID, computer.Capabilities{}), nil
}

func (node *Runtime) ensure(ctx context.Context, resource Resource, state *record) (computer.Observation, error) {
	if state.Phase == "removing" || state.Phase == "absent" {
		return computer.Observation{}, failure("generation_closed")
	}
	image, err := node.engine.inspectImage(ctx, resource.Specification.ImageReference)
	if err != nil {
		return computer.Observation{}, err
	}
	if err := image.validate(resource.Specification.ImageReference, "linux/"+runtime.GOARCH); err != nil {
		return computer.Observation{}, err
	}
	if err := node.verifyProgramImage(image, resource); err != nil {
		return computer.Observation{}, err
	}
	current, err := node.load(ctx, resource, state)
	if err != nil {
		return computer.Observation{}, err
	}
	state.Phase = "creating"
	if state.StorageGeneration != 0 && state.ContainerID == "" && current.hasContainer {
		// 已识别为本代的 Docker 创建回包即使丢失，也必须先固定容器 ID 再启动。
		state.ContainerID = current.container.ID
	}
	if err = node.journal.save(state); err != nil {
		return computer.Observation{}, err
	}
	if !current.workspace.Ready {
		if state.StorageGeneration == 0 {
			current.workspace, err = node.storage.Ensure(ctx, resource)
		} else {
			current.workspace, err = node.storage.Restore(ctx, node.diskResource(resource, state))
		}
		if err != nil {
			return computer.Observation{}, err
		}
		if !current.workspace.Exists || !current.workspace.Ready || current.workspace.Path != resource.WorkspacePath || current.workspace.CapacityBytes != resource.Specification.Resources.DiskBytes {
			return computer.Observation{}, failure("workspace_not_ready")
		}
	}
	if !current.hasVolume {
		current.volume, err = node.engine.createVolume(ctx, resource.Name, resource.WorkspacePath, resource.Labels)
		if err != nil {
			return computer.Observation{}, err
		}
		current.hasVolume = true
		if err = node.validateSnapshot(resource, state, current); err != nil {
			return computer.Observation{}, err
		}
	}
	if !current.hasContainer {
		if err = node.configureAgent(resource, true); err != nil {
			return computer.Observation{}, err
		}
		id, err := node.engine.createContainer(ctx, resource.Name, node.configuration(resource))
		if err != nil {
			return computer.Observation{}, err
		}
		if !observed(resource, "ready", id, computer.Capabilities{}).Matches(resource.Specification) {
			return computer.Observation{}, failure("container_identity_mismatch")
		}
		state.ContainerID = id
		if err = node.journal.save(state); err != nil {
			return computer.Observation{}, err
		}
	}
	current, err = node.load(ctx, resource, state)
	if err != nil {
		return computer.Observation{}, err
	}
	if !current.hasContainer {
		return computer.Observation{}, failure("container_missing_after_creation")
	}
	if !current.container.State.Running {
		if err = node.selectComputerPrograms(resource); err != nil {
			return computer.Observation{}, err
		}
		if err = node.engine.startContainer(ctx, current.container.ID); err != nil {
			return computer.Observation{}, err
		}
	}
	observation, err := node.inspect(ctx, resource, state)
	if err != nil {
		return observation, err
	}
	if observation.State != "ready" {
		return observation, failure("computer_probe_failed")
	}
	return observation, nil
}

func (node *Runtime) remove(ctx context.Context, resource Resource, state *record) (computer.Observation, error) {
	if err := node.journal.requireClosedSessions(ctx, resource.Specification.UserID); err != nil {
		return computer.Observation{}, err
	}
	current, err := node.load(ctx, resource, state)
	if err != nil {
		return computer.Observation{}, err
	}
	if state.Phase == "absent" && !current.absent() {
		return computer.Observation{}, failure("closed_generation_has_resources")
	}
	state.Phase = "removing"
	if err = node.journal.save(state); err != nil {
		return computer.Observation{}, err
	}
	if err = node.retireRuntime(ctx, resource, current, ""); err != nil {
		return computer.Observation{}, err
	}
	// 未创建过物理磁盘的代次也必须持久关闭，防止后续代次越过或复活旧请求。
	if err = node.storage.Remove(ctx, resource); err != nil {
		return computer.Observation{}, err
	}
	remaining, err := node.storage.Inspect(ctx, resource)
	if err != nil {
		return computer.Observation{}, err
	}
	if remaining.Exists {
		return computer.Observation{}, failure("workspace_removal_unconfirmed")
	}
	state.Phase = "absent"
	state.ContainerID = ""
	if err = node.journal.save(state); err != nil {
		return computer.Observation{}, err
	}
	return observed(resource, "absent", "", computer.Capabilities{}), nil
}

// retireRuntime 只关闭本代运行资源，避免电脑换镜像触及用户的持久磁盘。
func (node *Runtime) retireRuntime(ctx context.Context, resource Resource, current snapshot, knownContainerID string) error {
	if !current.hasContainer && knownContainerID != "" {
		if _, err := node.engine.inspectContainer(ctx, knownContainerID); !errors.Is(err, errMissing) {
			if err != nil {
				return err
			}
			return failure("container_identity_mismatch")
		}
	}
	if err := node.browsers.retire(ctx, resource); err != nil {
		return err
	}
	if err := node.egress.stop(ctx, resource); err != nil {
		return err
	}
	if err := node.probes.retire(ctx, resource); err != nil {
		return err
	}
	if current.hasContainer {
		if err := node.engine.stopContainer(ctx, current.container.ID); err != nil && !errors.Is(err, errMissing) {
			return err
		}
		stopped, err := node.engine.inspectContainer(ctx, current.container.ID)
		if err != nil && !errors.Is(err, errMissing) {
			return err
		}
		if err == nil {
			if stopped.ID != current.container.ID || !sameLabels(stopped.Config.Labels, containerLabels(resource)) || stopped.State.Running || knownContainerID != "" && stopped.State.Pid != 0 {
				return failure("container_stop_unconfirmed")
			}
			if err = node.engine.removeContainer(ctx, current.container.ID); err != nil && !errors.Is(err, errMissing) {
				return err
			}
		}
		if _, err = node.engine.inspectContainer(ctx, current.container.ID); !errors.Is(err, errMissing) {
			if err != nil {
				return err
			}
			return failure("container_removal_unconfirmed")
		}
	}
	// 不强制删除卷，Docker 必须拒绝仍被容器引用的卷。
	if current.hasVolume {
		if err := node.engine.removeVolume(ctx, resource.Name); err != nil && !errors.Is(err, errMissing) {
			return err
		}
	}
	if _, err := node.engine.inspectVolume(ctx, resource.Name); !errors.Is(err, errMissing) {
		if err != nil {
			return err
		}
		return failure("volume_removal_unconfirmed")
	}
	return nil
}

// validateNetwork 要求没有可路由接口；只通过控制目录中的宿主代理提供出网。
func validateNetwork(container container) error {
	endpoint, exists := container.NetworkSettings.Networks["none"]
	if !exists || len(container.NetworkSettings.Networks) != 1 || endpoint.IPAddress != "" || endpoint.GlobalIPv6Address != "" || endpoint.Gateway != "" || endpoint.IPv6Gateway != "" {
		return failure("container_network_mismatch")
	}
	return nil
}
