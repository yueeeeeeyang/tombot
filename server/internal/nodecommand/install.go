package nodecommand

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"

	"io.yueyang/tom/internal/deployment"
)

type installNode func(context.Context, string) (deployment.NodeInstallationReport, error)

func runInstall(ctx context.Context, args []string, output io.Writer, logger *slog.Logger, install installNode) int {
	if len(args) > 1 || len(args) == 1 && (args[0] == "" || strings.HasPrefix(args[0], "-")) {
		logger.Error("用法：tomnode install [节点安装参数文件]")
		return 2
	}
	if ctx.Err() != nil {
		logger.Error("节点安装已取消")
		return 1
	}
	path := ""
	if len(args) == 1 {
		path = args[0]
	}
	var err error
	if path != "" {
		path, err = filepath.Abs(path)
	}
	if err != nil {
		logger.Error("节点安装参数路径无效")
		return 2
	}
	report, err := install(ctx, path)
	if err != nil {
		logger.Error("节点安装未完成，保留已发布的身份和配置", "reason", err)
		return 1
	}
	if json.NewEncoder(output).Encode(report) != nil {
		logger.Error("节点安装回执输出失败，请用原参数重新核对", "node_id", report.NodeID)
		return 1
	}
	return 0
}
