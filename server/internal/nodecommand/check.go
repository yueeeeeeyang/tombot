package nodecommand

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"time"

	"io.yueyang/tom/internal/deployment"
)

type deploymentCheck func(context.Context, deployment.Paths) (deployment.Report, error)

// runDeploymentCheck 不接受命令行配置覆盖；失败仅输出中文原因，没有成功回执。
func runDeploymentCheck(ctx context.Context, getenv func(string) string, output io.Writer, logger *slog.Logger, check deploymentCheck) int {
	paths := deployment.Paths{Server: getenv("TOM_SERVER_CONFIG"), Node: getenv("TOM_NODE_CONFIG"), Docker: getenv("TOM_DOCKER_INSTALL_MANIFEST"), Image: getenv("TOM_IMAGE_PREPARATION_MANIFEST")}
	if paths.Server == "" || paths.Node == "" || paths.Docker == "" || paths.Image == "" {
		logger.Error("部署检查必须指定四份受保护配置")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	report, err := check(ctx, paths)
	if err != nil {
		logger.Error("部署配置检查未通过", "reason", err.Error())
		return 1
	}
	if err := json.NewEncoder(output).Encode(report); err != nil {
		logger.Error("部署配置检查回执输出失败", "node_id", report.NodeID)
		return 1
	}
	return 0
}
