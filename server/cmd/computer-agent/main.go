// computer-agent 是用户电脑容器入口，退出即要求 Docker 停止当前实例。
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"io.yueyang/tom/internal/computeragent"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if len(os.Args) != 1 {
		logger.Error("电脑代理不接收命令行覆盖")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	if err := computeragent.Run(ctx, logger); err != nil {
		logger.Error("电脑代理退出，执行环境需要节点对账", "reason", err.Error())
		os.Exit(1)
	}
	stop()
}
