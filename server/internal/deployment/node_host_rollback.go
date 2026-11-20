package deployment

import (
	"context"
	"errors"
	"os"

	"io.yueyang/tom/internal/nodeapp"
	"io.yueyang/tom/internal/noderelease"
)

func (installer nodeInstaller) saveRepairMarker(name, operation string) error {
	if err := writeConfigurationFile(installer.fs.resolve(nodeRepairDirectory+"/"+name), []byte(operation), 0600); err != nil {
		return err
	}
	return syncDirectory(installer.fs.resolve(nodeRepairDirectory))
}

// rollbackHost 只恢复已授权的宿主程序和安装快照。原服务启动与控制端回执分别落盘，断电后可继续。
func (installer nodeInstaller) rollbackHost(ctx context.Context, journal nodeRepair, previous nodeInstallation, request nodeInstallRequest, checkStopped func(context.Context) error) (result error) {
	phase := "rollback"
	defer func() {
		if result != nil {
			result = upgradeFailure{phase, result}
		}
	}()
	if !journal.Options.hostProgramUpgrade() || installer.progress == nil {
		return errors.New("回滚缺少宿主维护授权")
	}
	marker := func(name string) error {
		body, err := installer.fs.readConfig(fileRule{path: nodeRepairDirectory + "/" + name, maximum: 64, private: true}, account{})
		if err != nil || string(body) != journal.OperationID {
			return errors.New("宿主回滚记录身份不符")
		}
		return nil
	}
	if err := marker("rollback"); err != nil {
		return err
	}
	installer.logger.WarnContext(ctx, "正在恢复上一宿主程序", "node_id", request.NodeID, "operation_id", journal.OperationID, "stage", "rollback")
	if _, err := os.Lstat(installer.fs.resolve(nodeRepairDirectory + "/rollback-started")); errors.Is(err, os.ErrNotExist) {
		// 回执暂时失联时原维护窗口仍有效，先把可启动的原宿主恢复，再重新交还窗口。
		if err := installer.progress(ctx, request, journal.OperationID, "rollback", journal.Options.hostRelease()); err != nil {
			installer.logger.WarnContext(ctx, "回滚阶段暂未送达", "node_id", request.NodeID, "operation_id", journal.OperationID, "reason", err)
		}
		if err := installer.systemctl(ctx, "stop", nodeServiceName); err != nil {
			return err
		}
		if err := checkStopped(ctx); err != nil {
			return err
		}
		for index, target := range nodeRepairTargets() {
			if target.name != "program" && target.name != "installation.json" {
				continue
			}
			body, err := installer.fs.readConfig(fileRule{path: nodeRepairDirectory + "/before/" + target.name, maximum: maximumNodeBinaryBytes, private: true}, account{})
			if err != nil || contentDigest(body) != journal.Files[index].Before {
				return errors.New("原程序或安装快照摘要不符，无法回滚")
			}
			if err := (serverInstaller{fs: installer.fs}).replaceServerFile(target.path, body, target.mode); err != nil {
				return err
			}
		}
		if _, err := installer.verifyInstallationConfig(ctx, previous, nodeapp.ParseConfig, false); err != nil {
			return err
		}
		old, digest, err := parseNodeInstallRequest(previous.Request)
		if err != nil || digest != previous.RequestSHA256 {
			return errors.New("回滚前的节点安装快照不符")
		}
		if err := installer.fs.activateBot(old.Programs); err != nil {
			return err
		}
		if err := installer.startNodeServices(ctx, true, previous.UpdateUnits); err != nil {
			return err
		}
		if err := installer.saveRepairMarker("rollback-started", journal.OperationID); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		if err := marker("rollback-started"); err != nil {
			return err
		}
		if err := installer.systemctl(ctx, "is-active", "--quiet", nodeServiceName); err != nil {
			return err
		}
	}
	// 旧服务已启动的标记只能证明当时的状态。补发回执前再核对当前实际二进制、快照和入口。
	for index, target := range nodeRepairTargets() {
		if target.name != "program" && target.name != "installation.json" {
			continue
		}
		body, err := installer.fs.readConfig(fileRule{path: target.path, maximum: maximumNodeBinaryBytes, private: target.mode == 0600}, account{})
		if err != nil || contentDigest(body) != journal.Files[index].Before {
			return errors.New("回滚后的宿主程序或安装快照与原版本不符")
		}
	}
	old, _, err := parseNodeInstallRequest(previous.Request)
	if err != nil {
		return err
	}
	link, err := os.Readlink(installer.fs.resolve(noderelease.ProgramRoot + "/bot"))
	if err != nil || link != old.Programs.Bot {
		return errors.New("回滚后 Bot 入口与原版本不符")
	}
	if err := installer.progress(ctx, request, journal.OperationID, "rolled_back", journal.Options.hostRelease()); err != nil {
		return err
	}
	if err := publishPath(installer.fs.resolve(nodeRepairDirectory), installer.fs.resolve(nodeRepairDirectory+"-rolled-back-"+journal.OperationID)); err != nil {
		return err
	}
	if err := syncDirectory(installer.fs.resolve("/etc/tombot")); err != nil {
		return err
	}
	phase = "rolled_back"
	return errors.New("新宿主启动失败，已恢复并启动原版本")
}
