package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
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
	if err := run(ctx, logger, os.Args[1:], os.Getenv("TOM_SERVER_CONFIG")); err != nil {
		logger.Error("多用户服务退出", "原因", err.Error())
		os.Exit(1)
	}
}

// run 只启动新 PostgreSQL 服务；部署文件必须显式指定，数据库初始化由 tomdb 执行。
func run(ctx context.Context, logger *slog.Logger, arguments []string, configurationPath string) (result error) {
	if len(arguments) != 0 {
		return errors.New("服务入口不接受子命令，请使用 TOM_SERVER_CONFIG 指定多用户配置；数据库初始化使用 tomdb")
	}
	if err := validateHost(runtime.GOOS, os.Geteuid()); err != nil {
		return err
	}
	configuration, err := serverapp.LoadConfig(configurationPath)
	if err != nil {
		return err
	}
	application, err := serverapp.Open(ctx, configuration, logger)
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

func validateHost(platform string, uid int) error {
	if platform != "linux" {
		return errors.New("多用户服务需要 Linux 电脑节点，请在 Linux 部署服务端")
	}
	if uid <= 0 {
		return errors.New("多用户 API 必须使用独立非 root 身份，节点管理由受保护通道调用")
	}
	return nil
}
