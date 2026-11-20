package deployment

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"io.yueyang/tom/deploy"
	"io.yueyang/tom/internal/dockernode"
	"io.yueyang/tom/internal/noderelease"
)

// upgradeWithRecovery 先续接磁盘事务，再按组件决定局部激活或等待原电脑迁移。
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
		prior.ProgramScope = journal.Options.ProgramScope
		if prior.localProgramUpgrade() {
			prior.ImagePackage = journal.Options.ImagePackage
		}
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
	// 显式带了同一镜像时仍按组件升级。核实已安装归档后复用，避免重复暂存大文件。
	if options.ImagePackage != "" && current.Programs.valid() && options.Programs.valid() {
		body, err := installer.fs.readInstallationSource(filepath.Join(options.ImagePackage, "node-release.json"), 64<<10, installer.caller)
		if err != nil {
			return report, err
		}
		var image nodeRelease
		if decodePreparation(body, &image) != nil || image.Version != 2 || image.Platform != installer.platform {
			return report, errors.New("独立镜像的格式或平台不符")
		}
		if image.ImageReference == current.Image.Reference && image.ArchiveSHA256 == current.Image.ArchiveSHA256 {
			oldRuntime, err := noderelease.RuntimeDigest(current.Docker)
			if err != nil {
				return report, err
			}
			newRuntime, err := noderelease.RuntimeDigest(image.Docker)
			if err != nil {
				return report, err
			}
			if oldRuntime == newRuntime {
				if err := installer.fs.verifyInstallationArchive(ctx, current.Image.ArchiveFile, image.ArchiveSHA256); err != nil {
					return report, err
				}
				options.ImagePackage = ""
			}
		}
	}
	options.ProgramScope = programUpgradeScope(record, current, contentDigest(program), options)
	installer.logger.InfoContext(ctx, "已按组件摘要确定升级范围", "node_id", current.NodeID, "scope", options.ProgramScope)
	if options.RecoverComputers {
		if options.ProgramScope != "unchanged" {
			return report, errors.New("恢复原电脑计划须从本机当前版本的独立程序包运行")
		}
		repaired = true
	}
	if options.localProgramUpgrade() && !repaired {
		return apply(options)
	}
	wait, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	deadline, _ := wait.Deadline()
	var lastNotice time.Time
	var waitingComputer, waitingStage string
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
			if lastNotice.IsZero() || waitingComputer != inspection.Target.ComputerID || waitingStage != inspection.ErrorCode || time.Since(lastNotice) >= time.Minute {
				installer.logger.InfoContext(ctx, "正在等待电脑升级验收，可在管理平台节点详情查看验收状态和错误码", "node_id", current.NodeID, "computer_id", inspection.Target.ComputerID, "stage", inspection.ErrorCode, "deadline", deadline)
				waitingComputer, waitingStage, lastNotice = inspection.Target.ComputerID, inspection.ErrorCode, time.Now()
			}
			select {
			case <-wait.Done():
				return report, upgradeFailure{inspection.ErrorCode, fmt.Errorf("等待电脑 %s 验收：%w", inspection.Target.ComputerID, wait.Err())}
			case <-time.After(delay):
			}
			delay = min(5*time.Second, delay*2)
		case "blocked":
			stage := inspection.ErrorCode
			if !strings.Contains(stage, "/") {
				stage = "computer_quarantined/" + stage
			}
			return report, upgradeFailure{stage, fmt.Errorf("电脑 %s 的升级已隔离，当前阻塞步骤 %s", inspection.Target.ComputerID, stage)}
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
			repair.ProgramScope = ""
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
