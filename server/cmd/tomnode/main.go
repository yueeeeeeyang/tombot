package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"io.yueyang/tom/internal/nodecommand"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(nodecommand.Run(ctx, os.Args, os.Getenv, os.Stdout, slog.New(slog.NewTextHandler(os.Stderr, nil))))
}
