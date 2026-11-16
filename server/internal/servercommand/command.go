// Package servercommand 处理服务端部署命令，服务运行仍由 serverapp 负责。
package servercommand

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"io.yueyang/tom/internal/deployment"
)

const usage = `用法：
  tomd                          运行服务，读取 TOM_SERVER_CONFIG
  sudo ./tomd install           自动安装，确认 HTTPS 访问地址
  sudo ./tomd install --url https://主机:9443
  sudo ./新版tomd upgrade       备份并升级到当前执行的二进制
  sudo ./新版tomd upgrade --renew-https  升级并重新签发自签名 HTTPS 证书
  sudo tomd status              检查系统服务、证书、配置和 API/数据库健康
  tomd help                     显示帮助

首次安装可选参数：
  --certificate 文件 --private-key 文件  使用已有 HTTPS 证书
  --config 文件                        使用完整现有服务配置，不安装数据库或生成密钥

默认自动安装支持 Debian/Ubuntu、RHEL/Rocky/AlmaLinux/CentOS Stream/Fedora（x64/arm64）。
不锁定系统版本号，需要 systemd 和可用的软件源网络访问。
访问地址必须可由浏览器和电脑节点访问；自签名证书需在访问设备上信任。
重复 install 复用首次配置；更换版本使用新版程序执行 upgrade。
升级失败保留备份和阶段记录，不自动降级数据库。程序不会下载未经指定的新版本。
重新签发会改变 HTTPS 证书指纹；浏览器与固定证书指纹的节点需要信任新证书。
`

type manager func(context.Context, string, deployment.ServerInstallOptions) (deployment.ServerReport, error)

// Run 解析固定子命令；先拒绝无效输入，再取得安装权限，输出中不包含凭据。
func Run(ctx context.Context, args []string, output io.Writer, logger *slog.Logger) error {
	return execute(ctx, args, output, func(ctx context.Context, action string, options deployment.ServerInstallOptions) (deployment.ServerReport, error) {
		executable, err := os.Executable()
		if err != nil {
			return deployment.ServerReport{}, errors.New("无法定位当前服务端程序")
		}
		caller := uint64(os.Getuid())
		if value := os.Getenv("SUDO_UID"); value != "" {
			caller, err = strconv.ParseUint(value, 10, 32)
			if err != nil {
				return deployment.ServerReport{}, errors.New("sudo 调用者身份无效")
			}
		}
		return deployment.ManageServer(ctx, action, options, executable, uint32(caller), logger, confirmAddress)
	})
}

func execute(ctx context.Context, args []string, output io.Writer, manage manager) error {
	if len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		_, err := io.WriteString(output, usage)
		return err
	}
	if len(args) == 0 || args[0] != "install" && args[0] != "upgrade" && args[0] != "status" {
		return errors.New("未知子命令，请运行 tomd help")
	}
	var options deployment.ServerInstallOptions
	flags := flag.NewFlagSet("tomd "+args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	if args[0] == "install" {
		flags.StringVar(&options.URL, "url", "", "")
		flags.StringVar(&options.Config, "config", "", "")
		flags.StringVar(&options.Certificate, "certificate", "", "")
		flags.StringVar(&options.PrivateKey, "private-key", "", "")
	}
	if args[0] == "upgrade" {
		flags.BoolVar(&options.RenewHTTPS, "renew-https", false, "")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err := io.WriteString(output, usage)
			return err
		}
		return errors.New("部署命令参数无效，请运行 tomd help")
	}
	if flags.NArg() != 0 {
		return errors.New("部署命令不接受位置参数，请运行 tomd help")
	}
	if options.Config != "" && (options.URL != "" || options.Certificate != "" || options.PrivateKey != "") || (options.Certificate == "") != (options.PrivateKey == "") {
		return errors.New("高级配置与自动参数不能混用，证书和私钥须成对提供")
	}
	if options.URL != "" {
		value, err := deployment.NormalizeServerURL(options.URL)
		if err != nil {
			return err
		}
		options.URL = value
	}
	for _, path := range []*string{&options.Config, &options.Certificate, &options.PrivateKey} {
		if *path != "" {
			absolute, err := filepath.Abs(*path)
			if err != nil {
				return errors.New("部署输入路径无效")
			}
			*path = absolute
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	report, err := manage(ctx, args[0], options)
	if report.Service != "" || report.URL != "" || report.Backup != "" {
		text := fmt.Sprintf("服务状态：%s\n开机启动：%t\nAPI 与数据库就绪：%t\n", report.Service, report.Enabled, report.Healthy)
		if report.URL != "" {
			text += "管理平台：" + report.URL + "\n"
		}
		if report.CertificateExpires != "" {
			text += "HTTPS 证书到期：" + report.CertificateExpires + "\n"
		}
		if report.SelfSigned {
			text += "当前使用自签名证书，请在访问设备上信任服务证书。\n"
		}
		if report.CertificateFile != "" {
			text += "HTTPS 公共证书：" + report.CertificateFile + "\n证书 SHA-256 指纹：" + report.CertificateSHA256 + "\n"
		}
		if report.Backup != "" {
			text += "升级备份：" + report.Backup + "\n"
		}
		if err == nil && args[0] == "install" {
			text += "安装完成。首次访问管理平台时创建管理员。\n"
		}
		if _, writeErr := io.WriteString(output, text); writeErr != nil {
			return errors.Join(err, errors.New("输出部署结果失败"))
		}
	}
	return err
}
