package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"io.yueyang/tom/internal/backup"
	"io.yueyang/tom/internal/config"
)

// archiveCommand 只从进程环境接收主密钥，命令行和结果输出只包含操作说明。
func archiveCommand(ctx context.Context, cfg config.Config, args []string, environment string, output io.Writer) error {
	usage := "用法：tomd backup [-o 归档路径] 或 tomd restore --input 归档路径 --target 新目录"
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	switch args[0] {
	case "backup":
		dest := flags.String("o", "tom-backup-"+time.Now().UTC().Format("20060102T150405Z")+".tar.gz", "归档路径")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *dest == "" {
			return fmt.Errorf("%s", usage)
		}
		if err := backup.Create(ctx, cfg.DataDir, cfg.WorkspaceDir, *dest, environment); err != nil {
			return err
		}
		_, err := fmt.Fprintln(output, "备份已写入", *dest, backup.KeyNotice(environment))
		return err
	case "restore":
		input := flags.String("input", "", "归档路径")
		target := flags.String("target", "", "新目录")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *input == "" || *target == "" {
			return fmt.Errorf("%s", usage)
		}
		if err := backup.CheckDestination(*target, cfg.DataDir, cfg.WorkspaceDir); err != nil {
			return err
		}
		if err := backup.Restore(ctx, *input, *target, environment); err != nil {
			return err
		}
		_, err := fmt.Fprintln(output, "还原已完成，请将服务的数据和工作区目录指向", *target, "下的 data 和 workspace")
		return err
	default:
		return fmt.Errorf("支持的命令为 doctor、backup 和 restore")
	}
}
