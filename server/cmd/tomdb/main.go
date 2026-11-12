package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"io.yueyang/tom/internal/store/postgres"
)

// main 仅负责显式数据库初始化；日常 API 进程不使用迁移账户。
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if len(os.Args) != 1 {
		if _, err := fmt.Fprintln(os.Stderr, "数据库初始化命令不接受参数，请通过 TOM_DATABASE_MIGRATION_URL 提供连接配置"); err != nil {
			os.Exit(1)
		}
		os.Exit(2)
	}
	if err := postgres.Initialize(ctx, os.Getenv("TOM_DATABASE_MIGRATION_URL")); err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, err); writeErr != nil {
			os.Exit(2)
		}
		os.Exit(1)
	}
	if _, err := fmt.Fprintln(os.Stdout, "PostgreSQL 数据库版本已就绪"); err != nil {
		os.Exit(1)
	}
}
