package main

import (
	"io"
	"strings"
	"testing"
)

func TestBootstrapCommandBoundary(t *testing.T) {
	for _, fixture := range []struct {
		name, platform string
		uid            int
		arguments      []string
		output         io.Writer
	}{
		{"root", "linux", 0, nil, io.Discard},
		{"非 Linux", "darwin", 501, nil, io.Discard},
		{"参数", "linux", 20001, []string{"create"}, io.Discard},
		{"无输出", "linux", 20001, nil, nil},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			err := run(t.Context(), fixture.arguments, fixture.platform, fixture.uid, "", "", fixture.output)
			if err == nil || !strings.Contains(err.Error(), "不接受参数") {
				t.Fatal("入口边界未在读取文件前拒绝", err)
			}
		})
	}
	if err := run(t.Context(), nil, "linux", 20001, "", "", io.Discard); err == nil || strings.Contains(err.Error(), "不接受参数") {
		t.Fatal("有效身份未进入部署文件检查", err)
	}
}
