package nodecommand

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"io.yueyang/tom/internal/deployment"
)

func TestRepairCommandOptionsAndFailures(t *testing.T) {
	for _, kind := range []string{"正常", "未知参数", "位置参数", "取消", "修复失败", "输出失败"} {
		t.Run(kind, func(t *testing.T) {
			args := []string{"tomnode", "repair", "--listen", "10.0.0.5:9444", "--url", "https://node.example.test:9444"}
			if kind == "未知参数" {
				args = append(args, "--unsafe")
			}
			if kind == "位置参数" {
				args = append(args, "extra")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if kind == "取消" {
				cancel()
			}
			var output bytes.Buffer
			var writer io.Writer = &output
			if kind == "输出失败" {
				writer = failedOutput{}
			}
			called := false
			actions := dependencies{repair: func(_ context.Context, options deployment.NodeRepairOptions) (deployment.NodeInstallationReport, error) {
				called = true
				if options.Listen != "10.0.0.5:9444" || options.URL != "https://node.example.test:9444" {
					t.Fatal("修复参数丢失")
				}
				if kind == "修复失败" {
					return deployment.NodeInstallationReport{}, errors.New("测试失败")
				}
				return deployment.NodeInstallationReport{State: "control_started"}, nil
			}}
			code := execute(ctx, args, func(string) string { return "" }, writer, slog.New(slog.NewTextHandler(io.Discard, nil)), actions)
			if (code == 0) != (kind == "正常") {
				t.Fatal("修复状态错误", code)
			}
			if called != (kind == "正常" || kind == "修复失败" || kind == "输出失败") {
				t.Fatal("错误参数执行了修复")
			}
			if code != 0 && output.Len() != 0 {
				t.Fatal("失败仍有成功回执")
			}
		})
	}
}

func TestUpgradeAcceptsOnlyAnExplicitAbsoluteImageDirectory(t *testing.T) {
	for _, args := range [][]string{{"tomnode", "upgrade"}, {"tomnode", "upgrade", "--image-package", "/image"}, {"tomnode", "upgrade", "--image-package", "relative"}, {"tomnode", "upgrade", "/other"}, {"tomnode", "upgrade", "--listen", "10.0.0.1:9444"}} {
		calls := 0
		image := ""
		if len(args) == 4 && args[2] == "--image-package" && args[3] == "/image" {
			image = args[3]
		}
		valid := len(args) == 2 || image != ""
		code := execute(t.Context(), args, func(string) string { return "" }, io.Discard, slog.Default(), dependencies{repair: func(_ context.Context, o deployment.NodeRepairOptions) (deployment.NodeInstallationReport, error) {
			calls++
			if !o.Upgrade || o.Listen != "" || o.ImagePackage != image {
				t.Fatal(o)
			}
			return deployment.NodeInstallationReport{State: "control_started"}, nil
		}})
		if valid && (code != 0 || calls != 1) || !valid && (code != 2 || calls != 0) {
			t.Fatal(args, code, calls)
		}
	}
}

func TestRecoveryFlagSelectsInstalledPrograms(t *testing.T) {
	called := false
	code := execute(t.Context(), []string{"tomnode", "upgrade", "--recover-computers"}, func(string) string { return "" }, io.Discard, slog.Default(), dependencies{repair: func(_ context.Context, o deployment.NodeRepairOptions) (deployment.NodeInstallationReport, error) {
		called = o.RecoverComputers && o.Upgrade && o.ImagePackage == ""
		return deployment.NodeInstallationReport{}, nil
	}})
	if !called || code != 0 {
		t.Fatal(called, code)
	}
}
