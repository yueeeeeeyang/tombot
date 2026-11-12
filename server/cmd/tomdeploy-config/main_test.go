package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"io.yueyang/tom/internal/deployment"
)

type failedOutput struct{}

func (failedOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestConfigurationCommand(t *testing.T) {
	for _, kind := range []string{"success", "arguments", "source", "destination", "prepare", "output"} {
		t.Run(kind, func(t *testing.T) {
			var output bytes.Buffer
			var destination io.Writer = &output
			if kind == "output" {
				destination = failedOutput{}
			}
			args := []string{"tomdeploy-config"}
			if kind == "arguments" {
				args = append(args, "unexpected")
			}
			getenv := func(key string) string {
				name, ok := map[string]string{"TOM_DEPLOYMENT_REQUEST": "source", "TOM_DEPLOYMENT_OUTPUT": "destination"}[key]
				if !ok {
					t.Fatal("读取了无关环境变量")
				}
				if name == kind {
					return ""
				}
				return "/etc/tombot-setup/" + name
			}
			calls := 0
			prepare := func(ctx context.Context, source, destination string) (deployment.Report, error) {
				calls++
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 25*time.Second || source != "/etc/tombot-setup/source" || destination != "/etc/tombot-setup/destination" {
					t.Fatal("参数或预算丢失")
				}
				if kind == "prepare" {
					return deployment.Report{}, errors.New("测试配置生成失败")
				}
				return deployment.Report{Scope: "deployment_configuration_files"}, nil
			}
			code := run(t.Context(), args, getenv, destination, slog.New(slog.NewTextHandler(io.Discard, nil)), prepare)
			if kind == "success" {
				if code != 0 || output.Len() == 0 || calls != 1 {
					t.Fatal("正常结果缺失", code)
				}
			} else if code == 0 || output.Len() != 0 {
				t.Fatal("错误命令产生成功回执", code)
			}
			if (kind == "arguments" || kind == "source" || kind == "destination") && calls != 0 {
				t.Fatal("无效输入调用生成器")
			}
		})
	}
}
