package deployment

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/deploy"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/nodeapp"
	"io.yueyang/tom/internal/nodeenrollment"
)

func TestUpgradeRepairsQuarantinedComputerWithOneCommand(t *testing.T) {
	i, binary, _ := legacyRepairFixture(t)
	body, err := os.ReadFile(i.fs.resolve(deploy.NodeDirectory + "/node.json"))
	if err != nil {
		t.Fatal(err)
	}
	node, err := nodeapp.ParseConfig(body)
	if err != nil {
		t.Fatal(err)
	}
	target := nodeenrollment.RecoveryTarget{ComputerID: ids.New(), ContainerID: strings.Repeat("e", 64), Fence: 10}
	source := computer.Specification{NodeID: node.NodeID, UserID: ids.New(), ComputerID: target.ComputerID, OperationID: ids.New(),
		ImageReference: "example.test/computer@sha256:" + strings.Repeat("c", 64),
		Generation:     2, Attempt: 2, FencingToken: 4, LeaseExpiresAt: time.Now().UTC(), Resources: computer.Resources{CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 1 << 30, ProcessLimit: 64}}
	source.VolumeReference = "tombot-user-" + source.UserID
	writeInstallFixture(t, i.fs, filepath.Join(node.Runtime.StateDirectory, source.UserID+".json"),
		encodeFixture(t, map[string]any{"version": 4, "phase": "creating", "specification": source, "container_id": target.ContainerID}), 0600)
	image := []byte("标准镜像归档")
	reference := "registry.example.test/new@sha256:" + strings.Repeat("b", 64)
	writeFullUpgradeFixture(t, i, binary, image, reference)
	state := "retry"
	var steps []string
	i.systemctl = func(_ context.Context, args ...string) error { steps = append(steps, args[0]); return nil }
	i.inspectRecovery = func(_ context.Context, _ nodeInstallRequest, release string) (*nodeenrollment.RecoveryInspection, error) {
		target.Release = release
		return &nodeenrollment.RecoveryInspection{State: state, Source: &source, Target: target, ErrorCode: ""}, nil
	}
	holdAllowed, finishAllowed, imageFinishAllowed := false, false, false
	i.holdRecovery = func(_ context.Context, r nodeInstallRequest, _ string, proof nodeenrollment.RecoveryTarget) error {
		steps = append(steps, "hold")
		if proof != target || r.ImageID != node.Enrollment.Registration.ImageID {
			t.Fatal("恢复窗口的源镜像或原实例证据不符")
		}
		if !holdAllowed {
			return nodeenrollment.ErrMaintenanceConflict
		}
		state = "waiting"
		return nil
	}
	i.maintain = func(_ context.Context, r nodeInstallRequest, _ string, finish bool) error {
		if r.ImageID == node.Enrollment.Registration.ImageID {
			if !finish {
				t.Fatal("旧计划未取得专用的同镜像恢复窗口")
			}
			steps = append(steps, "release")
			if !finishAllowed {
				return errors.New("交还回执中断")
			}
			state = "clear"
		} else {
			if state != "clear" || r.Image.Reference != reference {
				t.Fatal("旧计划完成前切换了下一镜像")
			}
			if finish {
				steps = append(steps, "finish")
				if !imageFinishAllowed {
					return errors.New("新镜像维护回执中断")
				}
			} else {
				steps = append(steps, "begin")
			}
		}
		return nil
	}
	run := func() (NodeInstallationReport, error) {
		return i.repair(t.Context(), "/source/tomnode", NodeRepairOptions{Upgrade: true, ImagePackage: "/source"}, stoppedNode)
	}
	writeInstallFixture(t, i.fs, filepath.Join(node.Runtime.StateDirectory, ".recover-"+source.UserID), []byte(target.ContainerID), 0600)
	writeInstallFixture(t, i.fs, "/source/computer-image.tar", []byte("损坏"), 0644)
	if _, err := run(); err == nil || len(steps) != 0 {
		t.Fatal("坏包引起停机", steps, err)
	}
	writeInstallFixture(t, i.fs, "/source/computer-image.tar", image, 0644)
	writeInstallFixture(t, i.fs, filepath.Join(node.Runtime.StateDirectory, ".recover-"+source.UserID), []byte(strings.Repeat("a", 64)), 0600)
	if _, err := run(); err == nil || len(steps) != 0 {
		t.Fatal("原容器标记不符时准备停机", steps, err)
	}
	writeInstallFixture(t, i.fs, filepath.Join(node.Runtime.StateDirectory, ".recover-"+source.UserID), []byte(target.ContainerID), 0600)
	if _, err := run(); err == nil || !slices.Equal(steps, []string{"hold"}) {
		t.Fatal("未排空时停止了节点", steps, err)
	}
	steps, holdAllowed = nil, true
	if _, err := run(); err == nil || !slices.Equal(steps, []string{"hold", "stop", "daemon-reload", "enable", "is-active", "release"}) {
		t.Fatal("回执中断后提前开始下一镜像升级", steps, err)
	}
	steps, finishAllowed = nil, true
	report, err := run()
	if err == nil || !slices.Equal(steps, []string{"daemon-reload", "enable", "is-active", "release", "begin", "stop", "daemon-reload", "enable", "is-active", "finish"}) {
		t.Fatal("标准升级未从原窗口续接并安装发布镜像", steps, err)
	}
	steps, imageFinishAllowed = nil, true
	report, err = run()
	if err != nil || report.ImageID == node.Enrollment.Registration.ImageID || report.State != "computer_ready" || !slices.Equal(steps, []string{"daemon-reload", "enable", "is-active", "finish"}) {
		t.Fatal("新镜像启动后的重试遗漏最终验收或重复停机", steps, err)
	}
	state, steps = "blocked", nil
	if _, err := run(); err == nil || len(steps) != 0 {
		t.Fatal("再次隔离后重复重置额度", steps, err)
	}
}
