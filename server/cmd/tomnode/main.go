package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"io.yueyang/tom/internal/nodeapp"
)

type nodeService interface {
	Serve(context.Context, func(context.Context) error) error
}
type startNode func(context.Context, string, *slog.Logger) (nodeService, error)

func start(ctx context.Context, path string, logger *slog.Logger) (nodeService, error) {
	configuration, err := nodeapp.LoadConfig(path)
	if err != nil {
		return nil, err
	}
	return nodeapp.New(ctx, configuration, logger)
}

// run 只读取节点配置与 systemd 通知地址，错误退出保留状态供后续重启对账。
func run(ctx context.Context, args []string, getenv func(string) string, logger *slog.Logger, build startNode) int {
	path := getenv("TOM_NODE_CONFIG")
	if len(args) != 1 || path == "" {
		logger.Error("节点服务不接受命令参数，必须指定受保护配置")
		return 2
	}
	application, err := build(ctx, path, logger)
	if err != nil {
		logger.Error("节点初始化未完成", "reason", err)
		return 1
	}
	if err := application.Serve(ctx, func(ctx context.Context) error { return nodeapp.NotifyReady(ctx, getenv("NOTIFY_SOCKET")) }); err != nil {
		logger.Error("节点服务退出未完成", "reason", err)
		return 1
	}
	return 0
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args, os.Getenv, slog.New(slog.NewTextHandler(os.Stderr, nil)), start))
}
