// computer-probe 只作为受信容器代理的固定子进程使用，不提供用户命令入口。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"io.yueyang/tom/internal/computerprobe"
	"io.yueyang/tom/internal/taskprocess"
)

func main() {
	ctx, stop := context.Background(), func() {}
	if len(os.Args) == 3 && os.Args[1] == "--worker" {
		ctx, stop = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	}
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "电脑能力探针未完成：", err)
		os.Exit(125)
	}
}

func run(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		if err := taskprocess.RunProbe(); err != nil {
			return err
		}
		return errors.New("能力探针引导没有替换进程")
	}
	if len(arguments) != 2 || arguments[0] != "--worker" || output == nil {
		return errors.New("能力探针不接受外部执行配置")
	}
	request, err := taskprocess.DecodeProbe([]byte(arguments[1]))
	if err != nil {
		return err
	}
	evidence, err := computerprobe.Run(ctx, request)
	if err != nil {
		return err
	}
	if err = json.NewEncoder(output).Encode(evidence); err != nil {
		return errors.New("能力探针证据输出未确认")
	}
	return nil
}
