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
		name, platform string
		uid            int
		arguments      []string
		output         io.Writer
		logger         *slog.Logger
	}{
		{"root", "linux", 0, nil, io.Discard, logger},
		{"非 Linux", "darwin", 501, nil, io.Discard, logger},
		{"参数", "linux", 20001, []string{"register"}, io.Discard, logger},
		{"无输出", "linux", 20001, nil, nil, logger},
		{"无日志", "linux", 20001, nil, io.Discard, nil},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			err := run(t.Context(), fixture.arguments, fixture.platform, fixture.uid, "", "", fixture.output, fixture.logger)
			if err == nil || !strings.Contains(err.Error(), "不接受参数") {
				t.Fatal("部署边界未在读取文件前拒绝", err)
			}
		})
	}
	if err := run(t.Context(), nil, "linux", 20001, "", "", io.Discard, logger); err == nil || strings.Contains(err.Error(), "不接受参数") {
		t.Fatal("正确身份未进入部署文件检查", err)
	}
}
