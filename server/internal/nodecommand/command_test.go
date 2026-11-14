package nodecommand

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/deploy"
	"io.yueyang/tom/internal/deployment"
	"io.yueyang/tom/internal/dockerbootstrap"
	"io.yueyang/tom/internal/dockernode"
)

func TestUnifiedCommandRouting(t *testing.T) {
	for _, name := range []string{"configure", "check", "check-deployment", "bootstrap", "prepare-image", "serve"} {
		t.Run(name, func(t *testing.T) {
			var calls []string
			actions := commandFixture(t, &calls, "")
			getenv := withDefaults(func(key string) string {
				if key == "TOM_DEPLOYMENT_REQUEST" || key == "TOM_DEPLOYMENT_OUTPUT" {
					return "/etc/tombot/" + key
				}
				return ""
			})
			if code := execute(t.Context(), []string{"tomnode", name}, getenv, io.Discard, slog.Default(), actions); code != 0 || !reflect.DeepEqual(calls, []string{name}) {
				t.Fatal("命令分派或默认配置丢失", code, calls)
			}
		})
	}
}

func TestStartStopsAfterFailureOrCancellation(t *testing.T) {
	want := []string{"check", "bootstrap", "prepare-image", "serve"}
	for _, failure := range []string{"", "check", "bootstrap", "prepare-image", "serve", "cancel-before", "cancel-after-check"} {
		t.Run(failure, func(t *testing.T) {
			var calls []string
			actions := commandFixture(t, &calls, failure)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if failure == "cancel-before" {
				cancel()
			}
			if failure == "cancel-after-check" {
				check := actions.checkNode
				actions.checkNode = func(ctx context.Context, paths deployment.Paths) (deployment.Report, error) {
					report, err := check(ctx, paths)
					cancel()
					return report, err
				}
			}
			code := execute(ctx, []string{"tomnode", "start"}, withDefaults(func(string) string { return "" }), io.Discard, slog.Default(), actions)
			expected := want
			for index, name := range want {
				if failure == name {
					expected = want[:index+1]
				}
			}
			if failure == "cancel-before" {
				expected = nil
			}
			if failure == "cancel-after-check" {
				expected = want[:1]
			}
			if !reflect.DeepEqual(calls, expected) || (code == 0) != (failure == "") {
				t.Fatal("启动顺序错误、失败仍继续或取消后执行", code, calls, expected)
			}
		})
	}
}

func commandFixture(t *testing.T, calls *[]string, failure string) dependencies {
	t.Helper()
	called := func(name string) error {
		*calls = append(*calls, name)
		if name == failure {
			return errors.New("测试阶段失败")
		}
		return nil
	}
	return dependencies{
		configure: func(context.Context, string, string) (deployment.Report, error) {
			return deployment.Report{}, called("configure")
		},
		checkDeployment: func(_ context.Context, paths deployment.Paths) (deployment.Report, error) {
			if paths.Server != "/etc/tombot/server.json" {
				t.Fatal("成套检查缺少服务配置")
			}
			return deployment.Report{}, called("check-deployment")
		},
		checkNode: func(ctx context.Context, paths deployment.Paths) (deployment.Report, error) {
			deadline, ok := ctx.Deadline()
			if paths.Server != "" || paths.Node != "/etc/tombot/node/node.json" || paths.Docker != "/etc/tombot/node/docker-install.json" || paths.Image != "/etc/tombot/node/computer-image.json" || !ok || time.Until(deadline) > 25*time.Second {
				t.Fatal("节点检查读取了 API 或丢失路径与预算")
			}
			return deployment.Report{}, called("check")
		},
		bootstrap: func(_ context.Context, path string, _ *slog.Logger) (dockerbootstrap.Report, error) {
			if path != "/etc/tombot/node/docker-install.json" {
				t.Fatal("宿主清单被替换")
			}
			return dockerbootstrap.Report{}, called("bootstrap")
		},
		image: func(_ context.Context, path string, _ *slog.Logger) (dockernode.ImagePreparation, error) {
			if path != "/etc/tombot/node/computer-image.json" {
				t.Fatal("镜像清单被替换")
			}
			return dockernode.ImagePreparation{}, called("prepare-image")
		},
		node: func(_ context.Context, path string, _ *slog.Logger) (nodeService, error) {
			if path != "/etc/tombot/node/node.json" {
				t.Fatal("节点配置被替换")
			}
			return &testService{}, called("serve")
		},
	}
}

func TestHelpSystemdAndInvalidArguments(t *testing.T) {
	for _, args := range [][]string{{"tomnode"}, {"tomnode", "help"}, {"tomnode", "--help"}, {"tomnode", "systemd"}, {"tomnode", "template"}, {"tomnode", "unknown"}, {"tomnode", "start", "unexpected"}, {"tomnode", "template", "unexpected"}, {}} {
		var output bytes.Buffer
		getenv := func(string) string { t.Fatal("静态输出或错误参数读取了配置"); return "" }
		code := execute(t.Context(), args, getenv, &output, slog.Default(), dependencies{})
		valid := len(args) == 1 || len(args) == 2 && args[1] != "unknown"
		if (code == 0) != valid || (output.Len() > 0) != valid {
			t.Fatal("命令参数处理错误", args, code)
		}
		if len(args) == 2 && args[1] == "systemd" && output.String() != deploy.NodeService() {
			t.Fatal("服务输出与内置单元不一致")
		}
		if len(args) == 2 && args[1] == "template" && output.String() != deploy.NodeInstallTemplate() {
			t.Fatal("安装模板与随包文件不一致")
		}
	}
	for _, name := range []string{"help", "systemd", "template"} {
		if code := execute(t.Context(), []string{"tomnode", name}, func(string) string { return "" }, failedOutput{}, slog.Default(), dependencies{}); code == 0 {
			t.Fatal("输出失败仍成功")
		}
	}
	getenv := withDefaults(func(key string) string {
		if key == "TOM_NODE_CONFIG" {
			return "/custom/node.json"
		}
		return ""
	})
	if getenv("TOM_NODE_CONFIG") != "/custom/node.json" || getenv("NOTIFY_SOCKET") != "" {
		t.Fatal("覆盖配置或可选环境变量被改写")
	}
}

func TestNodeCheckAndBootstrapFailures(t *testing.T) {
	for _, command := range []string{"check", "bootstrap"} {
		for _, kind := range []string{"missing", "operation", "output"} {
			t.Run(command+"/"+kind, func(t *testing.T) {
				var calls []string
				failure := ""
				if kind == "operation" {
					failure = command
				}
				actions := commandFixture(t, &calls, failure)
				getenv := withDefaults(func(string) string { return "" })
				if kind == "missing" {
					getenv = func(string) string { return "" }
				}
				var output bytes.Buffer
				var writer io.Writer = &output
				if kind == "output" {
					writer = failedOutput{}
				}
				if code := execute(t.Context(), []string{"tomnode", command}, getenv, writer, slog.Default(), actions); code == 0 || strings.TrimSpace(output.String()) != "" {
					t.Fatal("失败产生成功回执", code)
				}
				if kind == "missing" && len(calls) != 0 {
					t.Fatal("缺少清单仍执行安装或检查")
				}
			})
		}
	}
}
