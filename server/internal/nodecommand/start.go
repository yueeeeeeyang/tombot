package nodecommand

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"time"

	"io.yueyang/tom/internal/deployment"
	"io.yueyang/tom/internal/dockerbootstrap"
)

type installDocker func(context.Context, string, *slog.Logger) (dockerbootstrap.Report, error)

// runStart 每次启动重新核验，任一步失败或取消都不执行后续动作，不重试未知安装结果。
func runStart(ctx context.Context, getenv func(string) string, logger *slog.Logger, actions dependencies) int {
	steps := []struct {
		name string
		run  func() int
	}{
		{"检查节点部署配置", func() int { return runNodeCheck(ctx, getenv, io.Discard, logger, actions.checkNode) }},
		{"准备 Docker 与宿主磁盘工具", func() int { return runBootstrap(ctx, getenv, io.Discard, logger, actions.bootstrap) }},
		{"准备固定电脑镜像", func() int {
			return runPrepareImage(ctx, getenv, io.Discard, logger, actions.image)
		}},
		{"恢复并启动节点", func() int { return runServe(ctx, getenv, logger, actions.node) }},
	}
	for _, step := range steps {
		if ctx.Err() != nil {
			logger.Error("节点启动已取消，未执行后续阶段", "stage", step.name)
			return 1
		}
		logger.Info("节点启动阶段开始", "stage", step.name)
		if code := step.run(); code != 0 {
			return code
		}
	}
	return 0
}

func runNodeCheck(ctx context.Context, getenv func(string) string, output io.Writer, logger *slog.Logger, check deploymentCheck) int {
	paths := deployment.Paths{Node: getenv("TOM_NODE_CONFIG"), Docker: getenv("TOM_DOCKER_INSTALL_MANIFEST"), Image: getenv("TOM_IMAGE_PREPARATION_MANIFEST")}
	if paths.Node == "" || paths.Docker == "" || paths.Image == "" {
		logger.Error("节点检查必须指定节点、Docker 和镜像三份受保护配置")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	report, err := check(ctx, paths)
	if err != nil {
		logger.Error("节点部署配置检查未通过", "reason", err)
		return 1
	}
	if json.NewEncoder(output).Encode(report) != nil {
		logger.Error("节点部署检查回执输出失败", "node_id", report.NodeID)
		return 1
	}
	return 0
}

func runBootstrap(ctx context.Context, getenv func(string) string, output io.Writer, logger *slog.Logger, install installDocker) int {
	path := getenv("TOM_DOCKER_INSTALL_MANIFEST")
	if path == "" {
		logger.Error("Docker 引导必须指定受保护的安装清单")
		return 2
	}
	report, err := install(ctx, path, logger)
	if err != nil {
		logger.Error("Docker 自动引导失败", "reason", err)
		return 1
	}
	if json.NewEncoder(output).Encode(report) != nil {
		logger.Error("Docker 引导回执输出失败，请保留安装状态重新核验")
		return 1
	}
	return 0
}
