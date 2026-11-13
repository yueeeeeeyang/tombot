package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestRegistrationCommandBoundary(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, fixture := range []struct {
		name      string
		uid       int
		arguments []string
		output    io.Writer
		logger    *slog.Logger
	}{
		{"root", 0, nil, io.Discard, logger},
		{"参数", 20001, []string{"register"}, io.Discard, logger},
		{"无输出", 20001, nil, nil, logger},
		{"无日志", 20001, nil, io.Discard, nil},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			err := run(t.Context(), fixture.arguments, fixture.uid, "", "", fixture.output, fixture.logger)
			if err == nil || !strings.Contains(err.Error(), "不接受参数") {
				t.Fatal("部署边界未在读取文件前拒绝", err)
			}
		})
	}
	// 不再限制操作系统：非 root 身份直接进入部署文件检查。
	if err := run(t.Context(), nil, 501, "", "", io.Discard, logger); err == nil || strings.Contains(err.Error(), "不接受参数") {
		t.Fatal("正确身份未进入部署文件检查", err)
	}
}
