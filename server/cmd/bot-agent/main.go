// bot-agent 由电脑代理在 Bot 所属 UID 和资源组内启动，标准流只连接该运行的受限中继。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/computertools"
)

func main() {
	if len(os.Args) != 1 || os.Geteuid() == 0 {
		fmt.Fprintln(os.Stderr, "Bot Agent 必须由受信引导降权启动，且不接收命令行配置")
		os.Exit(125)
	}
	stream, err := standardStream()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Bot Agent 缺少可取消的私有标准流")
		os.Exit(125)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	err = botagent.Serve(ctx, stream, computertools.NewBrowser())
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Bot Agent 已停止，运行结果须由控制面确认")
		os.Exit(1)
	}
}
