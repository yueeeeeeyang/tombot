package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"io.yueyang/tom/internal/dockerbootstrap"
)

// main 仅暴露固定的受限启动引导，不接受远程命令、软件源或任意安装参数。
func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if len(os.Args) != 1 || os.Getenv("TOM_DOCKER_INSTALL_MANIFEST") == "" {
		logger.Error("Docker 引导不接受命令参数，必须指定受保护的安装清单")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	report, err := dockerbootstrap.Install(ctx, os.Getenv("TOM_DOCKER_INSTALL_MANIFEST"), logger)
	if err != nil {
		logger.Error("Docker 自动引导失败", "reason", err)
		os.Exit(1)
	}
	if _, err = fmt.Fprintf(os.Stdout, "Docker 服务与宿主磁盘工具已验证，引擎版本 %s，cgroup %s，工具包 %d 项\n", report.Version, report.CgroupVersion, len(report.HostPackages)); err != nil {
		os.Exit(1)
	}
}
