package dockernode

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computeragent"
)

func (node *Runtime) agentConfiguration(resource Resource) computeragent.Configuration {
	specification := resource.Specification
	return computeragent.Configuration{Version: computeragent.ConfigurationVersion, Identity: computeragent.Identity{UserID: specification.UserID, ComputerID: specification.ComputerID, NodeID: specification.NodeID, Generation: specification.Generation}, Options: node.options.Agent}
}

// agentDirectory 只处理本代次独立控制目录，绝不把宿主控制目录树或用户卷作为代理控制挂载。
func (node *Runtime) agentDirectory(resource Resource, create bool) (*journal, error) {
	name := "agent-" + resourceName(resource.Specification)
	if resource.ControlPath != filepath.Join(node.options.StateDirectory, name) {
		return nil, failure("agent_directory_identity")
	}
	if create {
		if err := node.journal.root.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, failure("agent_directory_create")
		}
		parent, err := node.journal.root.Open(".")
		if err != nil {
			return nil, failure("agent_directory_parent")
		}
		if err = errors.Join(parent.Sync(), parent.Close()); err != nil {
			return nil, failure("agent_directory_persistence")
		}
	}
	stat, err := node.journal.root.Lstat(name)
	if err != nil {
		return nil, failure("agent_directory_missing")
	}
	identity, ok := stat.Sys().(*syscall.Stat_t)
	if !ok || identity.Uid != node.journal.owner || !stat.IsDir() || stat.Mode().Perm() != 0700 {
		return nil, failure("agent_directory_unsafe")
	}
	root, err := node.journal.root.OpenRoot(name)
	if err != nil {
		return nil, failure("agent_directory_open")
	}
	return &journal{root: root, owner: node.journal.owner}, nil
}

// configureAgent 在创建容器前刷盘固定配置；已有实例配置不被重写，也不删除其关闭屏障。
func (node *Runtime) configureAgent(resource Resource, create bool) (failureResult error) {
	expected := node.agentConfiguration(resource)
	if err := expected.Validate(); err != nil {
		return err
	}
	directory, err := node.agentDirectory(resource, create)
	if err != nil {
		return err
	}
	defer func() { failureResult = errors.Join(failureResult, directory.root.Close()) }()
	encoded, err := directory.readFile(computeragent.ConfigName)
	if err != nil {
		return err
	}
	if encoded == nil {
		if !create {
			return failure("agent_configuration_missing")
		}
		encoded, err = agentchannel.Encode(expected)
		if err != nil {
			return err
		}
		return directory.writeFile(computeragent.ConfigName, encoded)
	}
	var existing computeragent.Configuration
	if agentchannel.Decode(encoded, &existing) != nil || existing != expected {
		return failure("agent_configuration_mismatch")
	}
	return nil
}
