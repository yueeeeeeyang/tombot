package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"io.yueyang/tom/internal/dockernode"
)

type failedOutput struct{}

func (failedOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestImagePreparationCommand(t *testing.T) {
	for _, kind := range []string{"success", "arguments", "missing", "prepare", "output"} {
		t.Run(kind, func(t *testing.T) {
			var output bytes.Buffer
			var destination io.Writer = &output
			if kind == "output" {
				destination = failedOutput{}
			}
			args := []string{"tomimage-prepare"}
			if kind == "arguments" {
				args = append(args, "unexpected")
			}
			getenv := func(key string) string {
				if key != "TOM_IMAGE_PREPARATION_MANIFEST" {
					t.Fatal("读取非部署环境")
				}
				if kind == "missing" {
					return ""
				}
				return "/etc/tombot/computer-image.json"
			}
			calls := 0
			prepare := func(ctx context.Context, path string, logger *slog.Logger) (dockernode.ImagePreparation, error) {
				calls++
				if path != "/etc/tombot/computer-image.json" {
					t.Fatal("配置路径不符")
				}
				if kind == "prepare" {
					return dockernode.ImagePreparation{}, errors.New("测试准备失败")
				}
				return dockernode.ImagePreparation{Scope: "image", State: "available"}, nil
			}
			code := run(t.Context(), args, getenv, destination, slog.New(slog.NewTextHandler(io.Discard, nil)), prepare)
			if kind == "success" {
				if code != 0 || output.Len() == 0 || calls != 1 {
					t.Fatal("成功回执缺失", code)
				}
				return
			}
			if code == 0 || output.Len() != 0 {
				t.Fatal("失败产生成功回执", code)
			}
			if (kind == "arguments" || kind == "missing") && calls != 0 {
				t.Fatal("错误命令调用准备入口")
			}
		})
	}
}
