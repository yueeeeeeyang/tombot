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

func TestDeploymentCommand(t *testing.T) {
	for _, kind := range []string{"success", "arguments", "server", "node", "docker", "image", "check", "output"} {
		t.Run(kind, func(t *testing.T) {
			var output bytes.Buffer
			var destination io.Writer = &output
			if kind == "output" {
				destination = failedOutput{}
			}
			args := []string{"tomdeploy-check"}
			if kind == "arguments" {
				args = append(args, "unexpected")
			}
			getenv := func(key string) string {
				value, ok := map[string]string{"TOM_SERVER_CONFIG": "server", "TOM_NODE_CONFIG": "node", "TOM_DOCKER_INSTALL_MANIFEST": "docker", "TOM_IMAGE_PREPARATION_MANIFEST": "image"}[key]
				if !ok {
					t.Fatal("读取非部署环境")
				}
				if kind == value {
					return ""
				}
				return "/etc/tombot/" + value + ".json"
			}
			calls := 0
			check := func(ctx context.Context, paths deployment.Paths) (deployment.Report, error) {
				calls++
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 25*time.Second || paths.Server != "/etc/tombot/server.json" {
					t.Fatal("检查参数或预算丢失")
				}
				if kind == "check" {
					return deployment.Report{}, errors.New("测试检查失败")
				}
				return deployment.Report{Scope: "deployment_configuration"}, nil
			}
			code := run(t.Context(), args, getenv, destination, slog.New(slog.NewTextHandler(io.Discard, nil)), check)
			if kind == "success" {
				if code != 0 || output.Len() == 0 || calls != 1 {
					t.Fatal("成功回执缺失", code)
				}
			} else if code == 0 || output.Len() != 0 {
				t.Fatal("失败产生成功回执", code)
			}
			if kind != "success" && kind != "check" && kind != "output" && calls != 0 {
				t.Fatal("无效入口触发检查")
			}
		})
	}
}
