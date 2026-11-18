package nodeapi

import (
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

func TestComputerImageUpgradeProtocol(t *testing.T) {
	current := computer.Specification{OperationID: ids.New(), UserID: ids.New(), ComputerID: ids.New(), NodeID: ids.New(),
		Generation: 1, Attempt: 1, FencingToken: 1, LeaseExpiresAt: time.Now().UTC(),
		ImageReference: "computer@sha256:" + strings.Repeat("a", 64),
		Resources:      computer.Resources{CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 1 << 30, ProcessLimit: 256}}
	current.VolumeReference = "tombot-user-" + current.UserID
	target := current
	target.Generation, target.Attempt, target.FencingToken = 2, 2, 2
	target.ImageReference = "computer@sha256:" + strings.Repeat("b", 64)
	upgrade := computer.ImageUpgrade{Current: current, Target: target, CurrentContainerID: strings.Repeat("c", 64)}
	encoded, err := agentchannel.Encode(upgrade)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = decodeInput(current.NodeID, "computer_upgrade", encoded); err != nil {
		t.Fatal("有效目标被拒绝", err)
	}
	if _, _, err = decodeInput(ids.New(), "computer_upgrade", encoded); err == nil {
		t.Fatal("其他节点接受了升级请求")
	}
	wrongDisk := upgrade
	wrongDisk.Target.Resources.DiskBytes *= 2
	if wrongDisk.Validate() == nil {
		t.Fatal("升级允许变更原磁盘容量")
	}
	observation := computer.Observation{OperationID: target.OperationID, UserID: target.UserID, ComputerID: target.ComputerID,
		Generation: target.Generation, FencingToken: target.FencingToken, VolumeReference: target.VolumeReference, State: "ready",
		ContainerID: strings.Repeat("d", 64), Capabilities: computer.Capabilities{Terminal: true, Browser: true, Files: true, ProcessReaping: true, DiskQuota: true, NetworkIsolation: true}}
	if err = validateResult("computer_upgrade", upgrade, observation); err != nil {
		t.Fatal("完整的目标验收被拒绝", err)
	}
	observation.ContainerID = upgrade.CurrentContainerID
	if validateResult("computer_upgrade", upgrade, observation) == nil {
		t.Fatal("重复使用旧容器 ID 被当作已升级")
	}
}
