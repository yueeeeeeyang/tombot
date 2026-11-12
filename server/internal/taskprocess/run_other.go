//go:build !linux

package taskprocess

import (
	"errors"
	"os"
)

// Run 在非 Linux 平台拒绝用户程序执行，不退回宿主 Shell。
func Run() error { return errors.New("任务引导仅支持受管 Linux 容器") }

func CheckController() error { return errors.New("电脑控制进程仅支持受管 Linux 容器") }

// Seal 在非 Linux 平台拒绝用普通文件代替内核封存内容。
func Seal(Request) (*os.File, error) { return nil, errors.New("任务封存仅支持 Linux") }
