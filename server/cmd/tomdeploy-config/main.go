package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"io.yueyang/tom/internal/deployment"
)

type prepareConfiguration func(context.Context, string, string) (deployment.Report, error)

// run 只接受受保护参数文件和全新输出目录，失败不输出成功回执或配置内容。
func run(ctx context.Context, args []string, getenv func(string) string, output io.Writer, logger *slog.Logger, prepare prepareConfiguration) int {
	source, destination := getenv("TOM_DEPLOYMENT_REQUEST"), getenv("TOM_DEPLOYMENT_OUTPUT")
	if len(args) != 1 || source == "" || destination == "" {
		logger.Error("配置生成不接受命令参数，必须指定部署参数文件和全新输出目录")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	report, err := prepare(ctx, source, destination)
	if err != nil {
		logger.Error("部署配置生成失败", "reason", err.Error())
		return 1
	}
	if json.NewEncoder(output).Encode(report) != nil {
		logger.Error("部署配置回执输出失败，请保留原输出目录核验", "node_id", report.NodeID)
		return 1
	}
	return 0
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args, os.Getenv, os.Stdout, slog.New(slog.NewTextHandler(os.Stderr, nil)), deployment.Prepare))
}
