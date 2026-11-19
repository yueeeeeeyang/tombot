package execution

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computertools"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

// browserWorkerExecutor 仅替代容器浏览器，授权、协议、记账和视觉上下文仍走真实链路。
type browserWorkerExecutor struct {
	execute func(context.Context, botagent.ToolGrant, botagent.ToolCall) (botagent.ToolResult, error)
}

func (browserWorkerExecutor) Supports(name string) bool    { return name == "computer_browser" }
func (browserWorkerExecutor) Finish(context.Context) error { return nil }
func (executor browserWorkerExecutor) Execute(ctx context.Context, grant botagent.ToolGrant, call botagent.ToolCall) (botagent.ToolResult, error) {
	return executor.execute(ctx, grant, call)
}

func TestDefaultComputerToolsAgentLoop(t *testing.T) {
	cluster := pgtest.Start(t)
	for _, mode := range []string{"正常", "不支持视觉", "浏览器结果未知", "浏览器桌面准备失败"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newFixture(t, cluster)
			fixture.service.options.MaxOperations = 16
			if mode == "不支持视觉" {
				fixture.toolsModel(t)
			} else {
				fixture.visionModel(t)
			}
			spec := fixture.first.Spec
			spec.ToolPermissions = []string{"computer_shell", "computer_browser", "computer_screenshot", "computer_input"}
			bot, err := fixture.catalog.Update(t.Context(), fixture.a, fixture.first.ID, bots.UpdateRequest{ExpectedRevision: fixture.first.Revision, Spec: spec})
			if err != nil {
				t.Fatal(err)
			}
			fixture.send(t, fixture.a, bot)
			lease := fixture.claim(t)
			node := &shellNode{sessionNode: &sessionNode{}, start: func(_ context.Context, request computer.TerminalStart) (computer.TerminalOutput, error) {
				if request.Session.RunID != lease.RunID || request.Session.BotID != bot.ID {
					t.Error("Shell 使用了错误桌面身份")
				}
				return computer.TerminalOutput{Text: "/workspace", Cursor: 10, Exited: true}, nil
			}}
			shell, err := NewComputerShell(node, computertools.ShellOptions{CallTimeout: time.Second, PollInterval: 10 * time.Millisecond, MaximumResultBytes: 8 << 10})
			if err != nil {
				t.Fatal(err)
			}
			desktop := &workerDesktop{t: t, fixture: fixture, pixels: screenshotPixels(t)}
			tools, err := NewComputerDesktop(desktop, computertools.DesktopOptions{CallTimeout: time.Second, PollInterval: 10 * time.Millisecond, StartTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			tools = append(tools, shell, ComputerBrowser{})
			models, browsers := 0, 0
			worker := fixture.workerWithComputers(t, callerFunc(func(_ context.Context, configuration modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
				models++
				if !configuration.SupportsVision || !configuration.SupportsTools {
					t.Error("完整电脑工具被分配到不支持工具或视觉的模型")
				}
				available := map[string]bool{}
				for _, tool := range input.Tools {
					available[tool.Name] = true
				}
				for _, name := range spec.ToolPermissions {
					if !available[name] {
						t.Error("模型缺少电脑工具", name)
					}
				}
				if models > 1 {
					result := input.Messages[len(input.Messages)-1].Blocks[0].ToolResult
					if models == 3 && mode == "浏览器桌面准备失败" {
						if result == nil || !result.IsError || !strings.Contains(result.Content, "Your desktop could not be prepared") {
							t.Fatal("模型未收到可恢复的浏览器故障")
						}
						return emitText(emit, "终端可以使用。浏览器未能恢复，请在设置的电脑页检查。")
					}
					if result == nil || result.IsError {
						return modelgateway.Result{}, errors.New("工具回执未进入模型")
					}
					if models == 3 && result.Content != "Browser window opened." {
						t.Error("浏览器回执未进入模型")
					}
					if models == 4 && (result.Artifact == nil || !bytes.Equal(result.Artifact.Content, desktop.pixels)) {
						t.Error("桌面截图像素未进入模型")
					}
				}
				switch models {
				case 1:
					return emitDesktopTool(emit, models, "computer_shell", `{"command":"pwd"}`)
				case 2:
					return emitDesktopTool(emit, models, "computer_browser", `{"action":"open"}`)
				case 3:
					return emitDesktopTool(emit, models, "computer_screenshot", `{}`)
				case 4:
					return emitDesktopTool(emit, models, "computer_input", `{"action":"type","text":"中文"}`)
				default:
					return emitText(emit, "已完成电脑操作。")
				}
			}), fixture.computers(t, node, computerOptions()), tools...)
			worker.options.MaxModelSteps = 5
			if mode == "浏览器桌面准备失败" {
				worker.agents.(*workerAgents).browserError = errors.New("浏览器未能恢复")
			}
			worker.agents.(*workerAgents).local = browserWorkerExecutor{execute: func(_ context.Context, grant botagent.ToolGrant, call botagent.ToolCall) (botagent.ToolResult, error) {
				browsers++
				if grant.Validate() != nil || grant.Session.RunID != lease.RunID || grant.Session.BotID != bot.ID || call.Name != "computer_browser" {
					t.Error("浏览器未收到当前运行授权")
				}
				var operations int
				if err := fixture.pool.QueryRow(`SELECT count(*) FROM tombot.run_operations WHERE user_id=$1 AND run_id=$2 AND id=$3 AND computer_session_id=$4 AND kind='computer' AND status='started'`, lease.UserID, lease.RunID, grant.OperationID, grant.Session.SessionID).Scan(&operations); err != nil || operations != 1 {
					t.Error("浏览器动作未先持久记账", err)
				}
				if mode == "浏览器结果未知" {
					return botagent.ToolResult{}, errors.New("浏览器回包中断")
				}
				return botagent.ToolResult{Content: "Browser window opened."}, nil
			}}
			run, err := worker.Process(t.Context(), lease)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "不支持视觉":
				if run.Status == "succeeded" || models != 0 || node.starts != 0 || browsers != 0 || desktop.captures != 0 || desktop.inputs != 0 {
					t.Fatal("不支持视觉时仍执行电脑操作", run.Status)
				}
			case "浏览器桌面准备失败":
				if run.Status != "succeeded" || models != 3 || node.starts != 1 || browsers != 0 {
					t.Fatal("局部故障阻断聊天或派发了网页动作", run.Status, models, browsers)
				}
			case "浏览器结果未知":
				if run.Status == "succeeded" || models != 2 || node.starts != 1 || browsers != 1 || desktop.captures != 0 || desktop.inputs != 0 {
					t.Fatal("浏览器结果未知后重放或继续执行", run.Status)
				}
			default:
				if run.Status != "succeeded" || models != 5 || node.starts != 1 || browsers != 1 || desktop.captures != 1 || desktop.inputs != 1 {
					t.Fatal("完整电脑工具循环未完成", run.Status, models, node.starts, browsers, desktop.captures, desktop.inputs)
				}
			}
			if fixture.sessionState(t, lease) != "released" {
				t.Fatal("电脑操作结束后未释放桌面")
			}
		})
	}
}
