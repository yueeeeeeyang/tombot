package dockernode

import (
	"errors"

	"io.yueyang/tom/internal/noderelease"
)

// mountContract 允许旧镜像按原有严格挂载约束排空；当前目标始终使用节点登记的约束。
func (node *Runtime) mountContract(resource Resource, c container) Resource {
	if resource.Specification.ImageReference != node.options.ProgramImage && node.options.ProgramCore != "" {
		resource.ExternalPrograms = c.Config.Labels[noderelease.ProgramImageLabel] == "1"
	}
	return resource
}

// selectComputerPrograms 仅在指定容器尚未启动或已确认退出时调用。选择记录随控制目录原子落盘。
func (node *Runtime) selectComputerPrograms(resource Resource) (result error) {
	if !resource.ExternalPrograms {
		return nil
	}
	if !noderelease.ValidDigest(node.options.ProgramCore) {
		return failure("program_revision_invalid")
	}
	dir, err := node.agentDirectory(resource, false)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, dir.root.Close()) }()
	return dir.writeFile("program-core", []byte(node.options.ProgramCore+"\n"))
}

func (node *Runtime) selectedComputerPrograms(resource Resource) (core string, result error) {
	dir, err := node.agentDirectory(resource, false)
	if err != nil {
		return "", err
	}
	defer func() { result = errors.Join(result, dir.root.Close()) }()
	raw, err := dir.readFile("program-core")
	if err != nil {
		return "", err
	}
	if len(raw) != 65 || raw[64] != '\n' || !noderelease.ValidDigest(string(raw[:64])) {
		return "", failure("program_revision_unconfirmed")
	}
	return string(raw[:64]), nil
}

// verifyProgramImage 在删除旧容器或分配磁盘前核对镜像和宿主的程序交付约定。
func (node *Runtime) verifyProgramImage(image imageDescription, resource Resource) error {
	if resource.ExternalPrograms != (image.Config.Labels[noderelease.ProgramImageLabel] == "1") {
		return failure("image_program_contract_mismatch")
	}
	return nil
}
