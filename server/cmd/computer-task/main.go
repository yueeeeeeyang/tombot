// computer-task 仅作为容器内受信子进程使用，控制内容由固定描述符传入。
package main

import (
	"errors"
	"fmt"
	"os"

	"io.yueyang/tom/internal/taskprocess"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(125)
	}
	// 成功必须由 exec 原位替换；异常返回不能被外层误认为用户程序正常退出。
	fmt.Fprintln(os.Stderr, "任务引导未执行用户程序")
	os.Exit(125)
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return taskprocess.Run()
	}
	if len(arguments) == 1 && arguments[0] == "--browser" {
		return taskprocess.RunBrowser()
	}
	return errors.New("任务引导不接收外部执行配置")
}
