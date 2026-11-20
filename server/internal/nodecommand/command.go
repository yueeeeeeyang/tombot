// Package nodecommand 提供统一的节点命令入口，复用各领域的配置校验和生命周期实现。
package nodecommand

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"io.yueyang/tom/deploy"
	"io.yueyang/tom/internal/deployment"
	"io.yueyang/tom/internal/dockerbootstrap"
	"io.yueyang/tom/internal/dockernode"
)

type dependencies struct {
	configure       prepareConfiguration
	checkDeployment deploymentCheck
	checkNode       deploymentCheck
	bootstrap       installDocker
	image           prepareImage
	node            startNode
	install         installNode
	repair          repairNode
}

// Run 处理固定子命令及受保护配置路径，返回进程退出码；未知参数不会执行部署动作。
func Run(ctx context.Context, args []string, getenv func(string) string, output io.Writer, logger *slog.Logger) int {
	return execute(ctx, args, withDefaults(getenv), output, logger, dependencies{
		configure: deployment.Prepare, checkDeployment: deployment.Check,
		checkNode: deployment.CheckNode, bootstrap: dockerbootstrap.Install,
		image: dockernode.PrepareImage, node: start,
		repair: func(ctx context.Context, options deployment.NodeRepairOptions) (deployment.NodeInstallationReport, error) {
			executable, err := os.Executable()
			if err != nil {
				return deployment.NodeInstallationReport{}, err
			}
			caller := uint64(os.Getuid())
			if value := getenv("SUDO_UID"); value != "" {
				caller, err = strconv.ParseUint(value, 10, 32)
				if err != nil {
					return deployment.NodeInstallationReport{}, errors.New("sudo 调用者身份无效")
				}
			}
			return deployment.RepairNode(ctx, executable, uint32(caller), options, logger)
		},
		install: func(ctx context.Context, path string) (deployment.NodeInstallationReport, error) {
			executable, err := os.Executable()
			if err != nil {
				return deployment.NodeInstallationReport{}, err
			}
			caller := uint64(os.Getuid())
			if value := getenv("SUDO_UID"); value != "" {
				caller, err = strconv.ParseUint(value, 10, 32)
				if err != nil {
					return deployment.NodeInstallationReport{}, errors.New("sudo 调用者身份无效")
				}
			}
			return deployment.InstallNode(ctx, path, executable, uint32(caller), logger, readEnrollmentToken, readInstallationAddress)
		},
	})
}

func withDefaults(getenv func(string) string) func(string) string {
	return func(key string) string {
		if value := getenv(key); value != "" {
			return value
		}
		switch key {
		case "TOM_SERVER_CONFIG":
			return "/etc/tombot/server.json"
		case "TOM_NODE_CONFIG":
			return filepath.Join(deploy.NodeDirectory, "node.json")
		case "TOM_DOCKER_INSTALL_MANIFEST":
			return filepath.Join(deploy.NodeDirectory, "docker-install.json")
		case "TOM_IMAGE_PREPARATION_MANIFEST":
			return filepath.Join(deploy.NodeDirectory, "computer-image.json")
		default:
			return ""
		}
	}
}

func execute(ctx context.Context, args []string, getenv func(string) string, output io.Writer, logger *slog.Logger, actions dependencies) int {
	if len(args) == 1 || len(args) == 2 && (args[1] == "help" || args[1] == "--help") {
		return writeText(output, usage, logger)
	}
	if len(args) >= 2 && args[1] == "install" {
		return runInstall(ctx, args[2:], output, logger, actions.install)
	}
	if len(args) >= 2 && args[1] == "repair" {
		return runRepair(ctx, args[2:], output, logger, actions.repair)
	}
	if len(args) == 3 && args[1] == "upgrade" && args[2] == "--recover-computers" {
		return runUpgrade(ctx, output, logger, actions.repair, deployment.NodeRepairOptions{Upgrade: true, RecoverComputers: true})
	}
	if len(args) == 4 && args[1] == "upgrade" && args[2] == "--image-package" && filepath.IsAbs(args[3]) {
		return runUpgrade(ctx, output, logger, actions.repair, deployment.NodeRepairOptions{Upgrade: true, ImagePackage: args[3]})
	}
	if len(args) != 2 {
		logger.Error("节点命令只接受一个子命令，请运行 tomnode help")
		return 2
	}
	if ctx.Err() != nil {
		logger.Error("节点命令已取消")
		return 1
	}
	switch args[1] {
	case "sync-upgrade":
		if err := deployment.SyncNodeUpgrade(ctx, logger); err != nil {
			logger.Error("节点远程升级未完成", "reason", err)
			return 1
		}
		return 0
	case "upgrade":
		return runUpgrade(ctx, output, logger, actions.repair, deployment.NodeRepairOptions{Upgrade: true})
	case "template":
		return writeText(output, deploy.NodeInstallTemplate(), logger)
	case "start":
		return runStart(ctx, getenv, logger, actions)
	case "serve":
		return runServe(ctx, getenv, logger, actions.node)
	case "configure":
		return runConfigure(ctx, getenv, output, logger, actions.configure)
	case "check":
		return runNodeCheck(ctx, getenv, output, logger, actions.checkNode)
	case "check-deployment":
		return runDeploymentCheck(ctx, getenv, output, logger, actions.checkDeployment)
	case "bootstrap":
		return runBootstrap(ctx, getenv, output, logger, actions.bootstrap)
	case "prepare-image":
		return runPrepareImage(ctx, getenv, output, logger, actions.image)
	case "systemd":
		return writeText(output, deploy.NodeService(), logger)
	default:
		logger.Error("未知节点子命令，请运行 tomnode help")
		return 2
	}
}

func writeText(output io.Writer, content string, logger *slog.Logger) int {
	if _, err := io.WriteString(output, content); err != nil {
		logger.Error("节点命令输出失败")
		return 1
	}
	return 0
}

const usage = `用法：tomnode <子命令>

  template          输出可选的节点高级参数模板
  install [文件]    一次完成配置、证书、程序和系统服务安装并启动
  upgrade [--image-package 绝对目录]  从程序包升级节点；单独更新镜像时用已安装的 tomnode 指定镜像包目录
  upgrade --recover-computers   使用已激活的程序版本续接隔离电脑的原计划
  repair [选项]     修复已停止的节点，保留节点身份；支持 --listen、--url、--image-package
  start             检查节点配置、准备 Docker 和镜像，然后运行节点
  configure         从部署参数生成配置，不覆盖已有输出目录
  check             只检查节点配置与文件权限，不需要 API 或数据库文件
  systemd           输出内置节点服务单元
  check-deployment  核对 API 与节点的成套部署配置
  bootstrap         单独安装或核验 Docker 与宿主磁盘工具
  prepare-image     单独准备固定摘要的电脑镜像
  serve             运行已完成环境准备的节点，不安装宿主软件
  help              显示帮助

install 从程序所在目录读取离线发布清单与镜像；默认无需参数文件。
首次安装按提示粘贴接入码，输入不回显；需要覆盖自动值时显式传入高级参数文件。
可重试相同参数，复用私有凭据和节点身份，不覆盖已有部署。
运行时默认读取 /etc/tombot/node/ 下的 node.json、docker-install.json、computer-image.json。
可用 TOM_NODE_CONFIG、TOM_DOCKER_INSTALL_MANIFEST、TOM_IMAGE_PREPARATION_MANIFEST 指定受保护文件。
configure 必须提供 TOM_DEPLOYMENT_REQUEST 和 TOM_DEPLOYMENT_OUTPUT。
check-deployment 另读取 TOM_SERVER_CONFIG，默认 /etc/tombot/server.json。
启动成功只表示控制入口可用，完整电脑能力仍须独立验收。
`
