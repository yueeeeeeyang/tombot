package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	safelog "io.yueyang/tom/internal/infra/log"
	"io.yueyang/tom/internal/serverapp"
)

func main() {
	logger := slog.New(safelog.NewHandler(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], runtime.GOOS, os.Geteuid(), os.Getenv("TOM_SERVER_CONFIG"), os.Getenv("TOM_NODE_REGISTRATION_REQUEST"), os.Stdout, logger); err != nil {
		logger.Error("节点目录部署操作未完成", "原因", err.Error())
		os.Exit(1)
	}
}

// run 使用 API 的非 root 身份和受保护文件，不接受命令参数、外部节点地址或就绪覆盖。
func run(ctx context.Context, arguments []string, platform string, uid int, configurationPath, requestPath string, output io.Writer, logger *slog.Logger) error {
	if len(arguments) != 0 || platform != "linux" || uid <= 0 || output == nil || logger == nil {
		return errors.New("节点登记须在 Linux 使用 API 非 root 身份运行，不接受参数；通过 TOM_SERVER_CONFIG 和 TOM_NODE_REGISTRATION_REQUEST 指定部署文件")
	}
	request, err := serverapp.LoadCatalogRequest(requestPath)
	if err != nil {
		return err
	}
	configuration, err := serverapp.LoadConfig(configurationPath)
	if err != nil {
		return err
	}
	result, err := serverapp.RunCatalog(ctx, configuration, request)
	if err != nil {
		logger.ErrorContext(ctx, "节点目录操作失败或结果未确认", "node_id", configuration.Node.ID, "image_id", request.ImageID, "request_id", result.RequestID)
		return err
	}
	if err = json.NewEncoder(output).Encode(result); err != nil {
		logger.ErrorContext(ctx, "节点目录回执输出失败", "node_id", configuration.Node.ID, "image_id", request.ImageID, "request_id", result.RequestID)
		return errors.New("部署回执输出失败，请查询原节点和镜像登记")
	}
	logger.InfoContext(ctx, "节点目录部署操作完成", "node_id", result.Catalog.Node.ID, "image_id", result.Catalog.Image.ID, "request_id", result.RequestID, "node_status", result.Catalog.Node.Status)
	return nil
}
