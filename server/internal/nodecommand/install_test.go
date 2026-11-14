package nodecommand

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"io.yueyang/tom/internal/deployment"
)

func TestInstallCommandPathsAndFailures(t *testing.T) {
	for _, kind := range []string{"default", "relative", "absolute", "extra", "option", "empty", "cancel", "operation", "output"} {
		t.Run(kind, func(t *testing.T) {
			args := []string{"tomnode", "install"}
			path := ""
			switch kind {
			case "relative":
				path = "config/install.json"
				args = append(args, path)
			case "absolute":
				path = "/opt/tombot/install.json"
				args = append(args, path)
			case "extra":
				args = append(args, "first", "second")
			case "option":
				args = append(args, "--unsafe")
			case "empty":
				args = append(args, "")
			}
			wantPath := path
			var err error
			if path != "" {
				wantPath, err = filepath.Abs(path)
			}
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if kind == "cancel" {
				cancel()
			}
			called := false
			report := deployment.NodeInstallationReport{Scope: "node_installation", State: "control_started"}
			actions := dependencies{install: func(_ context.Context, actual string) (deployment.NodeInstallationReport, error) {
				called = true
				if actual != wantPath {
					t.Fatal("安装路径未正确解析", actual)
				}
				if kind == "operation" {
					return deployment.NodeInstallationReport{}, errors.New("测试安装失败")
				}
				return report, nil
			}}
			var output bytes.Buffer
			var writer io.Writer = &output
			if kind == "output" {
				writer = failedOutput{}
			}
			code := execute(ctx, args, func(string) string { t.Fatal("安装命令不应读取运行时配置覆盖"); return "" }, writer, slog.New(slog.NewTextHandler(io.Discard, nil)), actions)
			success := kind == "default" || kind == "relative" || kind == "absolute"
			wantCall := success || kind == "operation" || kind == "output"
			if (code == 0) != success || called != wantCall {
				t.Fatal("安装命令未传播失败或参数错误仍安装", code, called)
			}
			if success {
				var actual deployment.NodeInstallationReport
				if json.Unmarshal(output.Bytes(), &actual) != nil || actual != report {
					t.Fatal("安装回执不完整")
				}
			} else if output.Len() != 0 {
				t.Fatal("安装失败仍输出成功回执")
			}
		})
	}
}
