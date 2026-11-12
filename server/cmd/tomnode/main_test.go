package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

type testService struct {
	failure error
	called  bool
}

func (service *testService) Serve(ctx context.Context, ready func(context.Context) error) error {
	service.called = true
	return errors.Join(ready(ctx), service.failure)
}

func TestNodeCommand(t *testing.T) {
	for _, scenario := range []string{"success", "missing", "arguments", "construction", "serve", "notification"} {
		t.Run(scenario, func(t *testing.T) {
			service := &testService{}
			args := []string{"tomnode"}
			if scenario == "arguments" {
				args = append(args, "unexpected")
			}
			builds := 0
			getenv := func(name string) string {
				if name == "TOM_NODE_CONFIG" && scenario != "missing" {
					return "/etc/tombot/node.json"
				}
				if name == "NOTIFY_SOCKET" && scenario == "notification" {
					return "invalid"
				}
				return ""
			}
			build := func(ctx context.Context, path string, logger *slog.Logger) (nodeService, error) {
				builds++
				if path != "/etc/tombot/node.json" {
					t.Fatal("配置路径被替换")
				}
				if scenario == "construction" {
					return nil, errors.New("测试初始化故障")
				}
				return service, nil
			}
			if scenario == "serve" {
				service.failure = errors.New("测试监督故障")
			}
			code := run(t.Context(), args, getenv, slog.New(slog.NewTextHandler(io.Discard, nil)), build)
			expected := 1
			if scenario == "success" {
				expected = 0
			}
			if scenario == "missing" || scenario == "arguments" {
				expected = 2
			}
			if code != expected {
				t.Fatal(code, expected)
			}
			if expected == 2 && (builds != 0 || service.called) {
				t.Fatal("非法调用仍初始化节点")
			}
			if scenario == "construction" && service.called {
				t.Fatal("初始化失败仍启动服务")
			}
		})
	}
}
