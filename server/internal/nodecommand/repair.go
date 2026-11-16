package nodecommand

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"path/filepath"

	"io.yueyang/tom/internal/deployment"
)

type repairNode func(context.Context, deployment.NodeRepairOptions) (deployment.NodeInstallationReport, error)

func runRepair(ctx context.Context, args []string, output io.Writer, logger *slog.Logger, repair repairNode) int {
	var options deployment.NodeRepairOptions
	flags := flag.NewFlagSet("repair", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.Listen, "listen", "", "节点本机监听地址")
	flags.StringVar(&options.URL, "url", "", "服务端可访问的节点 HTTPS 地址")
	flags.StringVar(&options.ImagePackage, "image-package", "", "完整离线安装包所在目录")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		logger.Error("用法：tomnode repair [--listen 本机IP:端口] [--url 节点HTTPS地址] [--image-package 离线安装包目录]")
		return 2
	}
	if ctx.Err() != nil {
		logger.Error("节点修复已取消")
		return 1
	}
	if options.ImagePackage != "" {
		directory, err := filepath.Abs(options.ImagePackage)
		if err != nil {
			logger.Error("离线安装包路径无效")
			return 2
		}
		options.ImagePackage = directory
	}
	report, err := repair(ctx, options)
	if err != nil {
		logger.Error("节点修复或启动未完成，保留身份和修复备份", "reason", err)
		return 1
	}
	if json.NewEncoder(output).Encode(report) != nil {
		logger.Error("节点修复回执输出失败", "node_id", report.NodeID)
		return 1
	}
	return 0
}
