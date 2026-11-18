package computer

import (
	"context"
	"errors"
)

// ImageUpgrade 只允许把原电脑的持久磁盘交给下一代容器，原容器 ID 用于校验旧实例。
type ImageUpgrade struct {
	Current            Specification `json:"current"`
	Target             Specification `json:"target"`
	CurrentContainerID string        `json:"current_container_id"`
}

// Validate 验证前向代次和不可变业务身份，实际租约与原磁盘由节点核实。
func (upgrade ImageUpgrade) Validate() error {
	current, target := upgrade.Current, upgrade.Target
	if current.Validate() != nil || target.Validate() != nil || !containerPattern.MatchString(upgrade.CurrentContainerID) {
		return errors.New("电脑镜像升级身份无效")
	}
	if current.UserID != target.UserID || current.ComputerID != target.ComputerID || current.NodeID != target.NodeID ||
		current.OperationID != target.OperationID || current.VolumeReference != target.VolumeReference || current.Resources != target.Resources ||
		current.ImageReference == target.ImageReference || target.Generation-current.Generation != 1 ||
		target.Attempt-current.Attempt != 1 || target.FencingToken <= current.FencingToken {
		return errors.New("电脑镜像升级必须保留原身份和磁盘并推进代次")
	}
	return nil
}

// ImageUpgrader 必须保留已投用电脑的原盘，并在目标实例通过全项探针后返回就绪回执。
type ImageUpgrader interface {
	UpgradeComputer(context.Context, ImageUpgrade) (Observation, error)
}
