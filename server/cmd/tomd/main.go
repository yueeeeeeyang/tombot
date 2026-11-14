package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	safelog "io.yueyang/tom/internal/infra/log"
	"io.yueyang/tom/internal/serverapp"
	"io.yueyang/tom/internal/servercommand"
	"io.yueyang/tom/web/admin/bundle"
)

func main() {
	logger := slog.New(safelog.NewHandler(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, logger, os.Args[1:], os.Getenv("TOM_SERVER_CONFIG")); err != nil {
		logger.Error("多用户服务退出", "原因", err.Error())
		os.Exit(1)
	}
}

// run 分派部署子命令；无参数时以非 root 身份运行服务，自动迁移成功后才监听端口。
func run(ctx context.Context, logger *slog.Logger, arguments []string, configurationPath string) (result error) {
	if len(arguments) != 0 {
		return servercommand.Run(ctx, arguments, os.Stdout, logger)
	}
	if err := validateHost(os.Geteuid()); err != nil {
		return err
	}
	configuration, err := serverapp.LoadConfig(configurationPath)
	if err != nil {
		return err
	}
	page, err := bundle.New()
	if err != nil {
		return err
	}
	application, err := serverapp.Open(ctx, configuration, page, logger)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, application.Close()) }()
	listener, err := net.Listen("tcp", configuration.Address)
	if err != nil {
		return errors.New("多用户服务监听地址无法绑定")
	}
	defer func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			result = errors.Join(result, errors.New("关闭多用户服务监听器失败"))
		}
	}()
	return application.Serve(ctx, listener)
}

// validateHost 只要求专用非 root 身份。
// API 与节点可以分处不同主机，因此这里不再限制服务端平台；
// 节点仍是 Linux root 进程，跨主机访问由 TLS 证书指纹与网络边界共同约束。
func validateHost(uid int) error {
	if uid <= 0 {
		return errors.New("多用户 API 必须使用独立非 root 身份，节点管理由受保护通道调用")
	}
	return nil
}
