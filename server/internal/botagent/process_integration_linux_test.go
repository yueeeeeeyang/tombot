//go:build linux && botagentintegration && !cgo

package botagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type runningAgent struct {
	command *exec.Cmd
	input   io.WriteCloser
	output  io.ReadCloser
	waited  bool
}

// launchContainerAgent 只验证镜像中的降权独立进程及协议，不替代生产 cgroup 入组验收。
func launchContainerAgent(t *testing.T, uid uint32) *runningAgent {
	t.Helper()
	program := os.Getenv("TOM_BOT_AGENT_ENTRY_TEST")
	if os.Geteuid() != 0 || !filepath.IsAbs(program) {
		t.Fatal("此项验收需要专用受限容器和明确的 Bot Agent 路径")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	command := exec.CommandContext(ctx, program)
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", fmt.Sprintf("DISPLAY=:%d", uid-100000)}
	command.Dir = "/workspace"
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: 20000, NoSetGroups: true}, Pdeathsig: syscall.SIGKILL}
	input, err := command.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	agent := &runningAgent{command: command, input: input, output: output}
	t.Cleanup(func() {
		cancel()
		if !agent.waited {
			if err := command.Wait(); err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Error(err)
				}
			}
		}
	})
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", command.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(status), fmt.Sprintf("Uid:\t%d\t%d\t%d\t%d\n", uid, uid, uid, uid)) || !strings.Contains(string(status), "Gid:\t20000\t20000\t20000\t20000\n") {
		t.Fatal("Agent 没有使用所属桌面身份")
	}
	return agent
}

func finishProcess(t *testing.T, agent *runningAgent, expectedCode int) {
	t.Helper()
	err := agent.command.Wait()
	agent.waited = true
	if expectedCode == 0 && err != nil {
		t.Fatal(err)
	}
	if agent.command.ProcessState.ExitCode() != expectedCode {
		t.Fatal("Agent 退出状态不符", agent.command.ProcessState.ExitCode(), err)
	}
}

func TestContainerBotAgentProcesses(t *testing.T) {
	first, second := launchContainerAgent(t, 100031), launchContainerAgent(t, 100032)
	if first.command.Process.Pid == second.command.Process.Pid {
		t.Fatal("两个 Bot 共享 Agent 进程")
	}
	start := fixtureStart()
	for _, agent := range []*runningAgent{first, second} {
		mustWrite(t, agent.input, Frame{Version: ProtocolVersion, Kind: "start", Start: &start})
		mustRead(t, agent.output, "model", 1)
	}
	// 第一进程的中继中断不能让第二进程退出，也不能自动重发模型请求。
	if err := first.input.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFrame(first.output); !errors.Is(err, ErrProtocol) {
		t.Fatal("中继中断后仍产生新动作")
	}
	finishProcess(t, first, 1)
	output := toolOutput()
	mustWrite(t, second.input, Frame{Version: ProtocolVersion, Sequence: 1, Kind: "result", Model: &output})
	call := mustRead(t, second.output, "tool", 2)
	if *call.Call != output.Tools[0] {
		t.Fatal("独立进程丢失工具参数原文")
	}
	mustWrite(t, second.input, Frame{Version: ProtocolVersion, Sequence: 2, Kind: "result", Tool: &ToolResult{Content: "共享文件内容"}})
	model := mustRead(t, second.output, "model", 3)
	if model.Input.Messages[2].Blocks[0].ToolResult.Content != "共享文件内容" {
		t.Fatal("工具结果未参与下一轮推理")
	}
	mustWrite(t, second.input, Frame{Version: ProtocolVersion, Sequence: 3, Kind: "result", Model: &ModelOutput{Text: "已完成。", StopReason: "stop"}})
	completed := mustRead(t, second.output, "done", 4)
	if completed.Answer != "已完成。" || completed.Failure != "" {
		t.Fatal("第二进程受另一 Bot 中断影响", completed)
	}
	finishProcess(t, second, 0)
}

func TestContainerBotAgentRejectsRootAndArguments(t *testing.T) {
	program := os.Getenv("TOM_BOT_AGENT_ENTRY_TEST")
	if os.Geteuid() != 0 || !filepath.IsAbs(program) {
		t.Fatal("需要受限容器内的真实入口")
	}
	for _, arguments := range [][]string{nil, {"unexpected"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		command := exec.CommandContext(ctx, program, arguments...)
		command.Env = []string{"LANG=C.UTF-8"}
		if len(arguments) > 0 {
			command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 100031, Gid: 20000, NoSetGroups: true}}
		}
		output, err := command.CombinedOutput()
		cancel()
		if err == nil || command.ProcessState == nil || command.ProcessState.ExitCode() != 125 || !strings.Contains(string(output), "Bot Agent") {
			t.Fatal("错误入口没有明确拒绝", err)
		}
	}
}

func TestContainerBotAgentSignalInterruptsBlockedRead(t *testing.T) {
	agent := launchContainerAgent(t, 100033)
	start := fixtureStart()
	mustWrite(t, agent.input, Frame{Version: ProtocolVersion, Kind: "start", Start: &start})
	mustRead(t, agent.output, "model", 1)
	if err := agent.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- agent.command.Wait() }()
	select {
	case err := <-done:
		agent.waited = true
		if err == nil || agent.command.ProcessState.ExitCode() != 1 {
			t.Fatal("取消后退出状态不符", err)
		}
	case <-time.After(time.Second):
		// 只终止此测试启动的单个进程，不掩盖取消未能唤醒读取的问题。
		if err := agent.command.Process.Kill(); err != nil {
			t.Error(err)
		}
		<-done
		agent.waited = true
		t.Fatal("信号取消未能打断标准输入阻塞读取")
	}
}

func TestContainerBotAgentRejectsFileChannel(t *testing.T) {
	program := os.Getenv("TOM_BOT_AGENT_ENTRY_TEST")
	if os.Geteuid() != 0 || !filepath.IsAbs(program) {
		t.Fatal("需要受限容器内的真实入口")
	}
	for _, direction := range []string{"input", "output"} {
		t.Run(direction, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "invalid-channel-")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := file.Close(); err != nil {
					t.Error(err)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, program)
			command.Env = []string{"LANG=C.UTF-8"}
			command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 100031, Gid: 20000, NoSetGroups: true}}
			command.Stdin, command.Stdout = strings.NewReader(""), io.Discard
			if direction == "input" {
				command.Stdin = file
			} else {
				command.Stdout = file
			}
			var diagnostic strings.Builder
			command.Stderr = &diagnostic
			if err := command.Run(); err == nil || command.ProcessState == nil || command.ProcessState.ExitCode() != 125 || !strings.Contains(diagnostic.String(), "私有标准流") {
				t.Fatal("普通文件被接受为协议通道", err)
			}
		})
	}
}
