package deployment

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"io.yueyang/tom/deploy"
	"io.yueyang/tom/internal/dockernode"
)

// upgradeWithRecovery 先用同一程序和已登记镜像接续旧计划，验收完成后再应用本包目标镜像。
// 两次安装共用外层安装锁，每次维护各自保存恢复日志。服务端用发布摘要限制重复领取额度。
func (installer nodeInstaller) upgradeWithRecovery(ctx context.Context, executable string, program []byte, options NodeRepairOptions, checkStopped func(context.Context) error) (NodeInstallationReport, error) {
	var report NodeInstallationReport
	repaired := false
	applied := false
	apply := func(opts NodeRepairOptions) (NodeInstallationReport, error) {
		return installer.applyNodeRepair(ctx, executable, program, opts, checkStopped)
	}
	// 已经开始过替换时，本机配置可能跨越两个版本，必须先恢复磁盘上那次操作。
	_, err := os.Lstat(installer.fs.resolve(nodeRepairDirectory))
	if err == nil {
		body, err := installer.fs.readConfig(fileRule{path: nodeRepairDirectory + "/plan.json", maximum: 64 << 10, private: true}, account{})
		var journal nodeRepair
		if err != nil || decodePreparation(body, &journal) != nil {
			return report, errors.New("无法读取待续的节点升级计划")
		}
		prior := options
		prior.AwaitComputers = journal.Options.AwaitComputers
		if journal.Options.Recovery.ComputerID != "" {
			prior.ImagePackage = ""
			prior.Recovery = journal.Options.Recovery
		}
		report, err = apply(prior)
		if err != nil {
			return report, err
		}
		applied = prior.Recovery.ComputerID == ""
		repaired = prior.AwaitComputers || !applied
		if applied && !repaired {
			return report, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return report, err
	}
	record, err := installer.installedNodeRecord()
	if err != nil {
		return report, err
	}
	current, digest, err := parseNodeInstallRequest(record.Request)
	if err != nil || digest != record.RequestSHA256 || current.Server == nil {
		return report, errors.New("无法核对已登记节点的当前镜像")
	}
	body, err := installer.fs.readConfig(fileRule{path: deploy.NodeDirectory + "/node.json", maximum: 64 << 10, private: true}, account{})
	if err != nil {
		return report, err
	}
	if contentDigest(body) != record.Files["node.json"] {
		return report, errors.New("已安装的 node.json 与 installation.json 记录的文件摘要不符")
	}
	node, err := parseRepairNodeConfig(body)
	if err != nil {
		return report, err
	}
	if node.NodeID != current.NodeID {
		return report, errors.New("已安装的 node.json 中 node_id 与安装请求快照不符")
	}
	wait, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	checkedArchive := false
	delay := time.Second
	for {
		if err := wait.Err(); err != nil {
			return report, fmt.Errorf("等待节点 %s 的原电脑升级验收：%w", current.NodeID, err)
		}
		inspection, err := installer.inspectRecovery(wait, current, options.PackageSHA256)
		if err != nil {
			return report, fmt.Errorf("查询原电脑升级计划失败，请核对配套 tomd 版本及连接：%w", err)
		}
		if inspection == nil {
			return report, errors.New("控制服务未返回电脑恢复结果")
		}
		switch inspection.State {
		case "clear":
			if !applied {
				options.AwaitComputers = repaired
				report, err = apply(options)
				if err != nil || !repaired {
					return report, err
				}
				applied = true
				if report.ImageID != current.ImageID {
					record, err = installer.installedNodeRecord()
					if err != nil {
						return report, err
					}
					current, digest, err = parseNodeInstallRequest(record.Request)
					if err != nil || digest != record.RequestSHA256 || current.ImageID != report.ImageID {
						return report, errors.New("升级后的节点安装回执与目标镜像不符")
					}
					continue
				}
			}
			report.State = "computer_ready"
			return report, nil
		case "waiting":
			repaired = true
			select {
			case <-wait.Done():
				return report, fmt.Errorf("等待电脑 %s 验收：%w", inspection.Target.ComputerID, wait.Err())
			case <-time.After(delay):
			}
			delay = min(5*time.Second, delay*2)
		case "blocked":
			return report, fmt.Errorf("电脑 %s 的升级计划已隔离，无法自动恢复，错误码 %s", inspection.Target.ComputerID, inspection.ErrorCode)
		case "retry":
			if inspection.Source == nil || inspection.Source.NodeID != current.NodeID || inspection.Target.Release != options.PackageSHA256 || inspection.Target.ComputerID != inspection.Source.ComputerID {
				return report, errors.New("控制服务返回的电脑恢复身份不符")
			}
			proof := dockernode.UpgradeRecoveryProof{Source: *inspection.Source, ContainerID: inspection.Target.ContainerID, TargetImage: current.Image.Reference, TargetFence: inspection.Target.Fence, ErrorCode: inspection.ErrorCode}
			if err := proof.CheckSource(wait, installer.fs.resolve(node.Runtime.StateDirectory), installer.fs.owner, installer.fs.resolve(node.Runtime.Socket), time.Duration(node.Runtime.CallTimeout)*time.Millisecond); err != nil {
				return report, fmt.Errorf("电脑 %s 的原容器或恢复标记未通过核验：%w", inspection.Target.ComputerID, err)
			}
			if !checkedArchive && options.ImagePackage != "" {
				// 先完整核验后续要安装的包；校验通过的归档可在切换时复用。
				candidate := current
				if options.Programs.valid() {
					candidate.Programs = options.Programs
				}
				if err := installer.prepareImageUpgrade(ctx, &candidate, &record, options.ImagePackage); err != nil {
					return report, err
				}
				checkedArchive = true
			}
			repair := options
			repair.ImagePackage = ""
			repair.Recovery = inspection.Target
			installer.logger.InfoContext(ctx, "开始恢复隔离电脑的原升级计划", "node_id", current.NodeID, "computer_id", inspection.Target.ComputerID)
			if _, err := apply(repair); err != nil {
				return report, err
			}
			repaired = true
			delay = time.Second
		default:
			return report, errors.New("控制服务返回了未知的电脑恢复状态")
		}
	}
}
