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
	if err := run(ctx, os.Args[1:], runtime.GOOS, os.Geteuid(), os.Getenv("TOM_SERVER_CONFIG"), os.Getenv("TOM_ADMIN_BOOTSTRAP_REQUEST"), os.Stdout); err != nil {
		logger.Error("首位管理员部署操作未完成", "原因", err.Error())
		os.Exit(1)
	}
}

// run 拒绝参数、root 与非 Linux；固定使用 API 服务身份和相同配置，不接收明文凭据。
func run(ctx context.Context, arguments []string, platform string, uid int, configurationPath, requestPath string, output io.Writer) error {
	if len(arguments) != 0 || platform != "linux" || uid <= 0 || output == nil {
		return errors.New("管理员引导须在 Linux 使用 API 非 root 身份运行，不接受参数；通过 TOM_SERVER_CONFIG 和 TOM_ADMIN_BOOTSTRAP_REQUEST 指定部署文件")
	}
	request, err := serverapp.LoadBootstrapRequest(requestPath)
	if err != nil {
		return err
	}
	configuration, err := serverapp.LoadConfig(configurationPath)
	if err != nil {
		return err
	}
	result, failure := serverapp.RunBootstrap(ctx, configuration, request)
	if result.Operation != nil || result.ActivationSaved {
		if err := json.NewEncoder(output).Encode(result); err != nil {
			return errors.Join(failure, errors.New("部署回执输出失败，请查询原操作；激活材料只保存在指定私有文件"))
		}
	}
	return failure
}
