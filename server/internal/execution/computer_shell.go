package execution

import (
	"context"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computertools"
	"io.yueyang/tom/internal/provider"
)

type ComputerShell struct {
	shell *computertools.Shell
}

// NewComputerShell 将容器 Shell 适配为持久执行工具，必须使用当前电脑节点提供的终端运行器。
func NewComputerShell(runtime computer.TerminalRuntime, options computertools.ShellOptions) (*ComputerShell, error) {
	shell, err := computertools.NewShell(runtime, options)
	if err != nil {
		return nil, err
	}
	return &ComputerShell{shell: shell}, nil
}

func (tool *ComputerShell) Schema() provider.ToolSchema { return tool.shell.Schema() }
func (tool *ComputerShell) UsesComputer() bool          { return true }
func (tool *ComputerShell) RequiresVision() bool        { return false }

// Execute 复核工具调用与运行的归属；租约和工具权限已由 Worker 在操作记账事务内验证。
func (tool *ComputerShell) Execute(ctx context.Context, invocation Invocation) (botagent.ToolResult, error) {
	if invocation.Computer == nil || invocation.Call.Name != "computer_shell" || invocation.Computer.UserID != invocation.Lease.UserID || invocation.Computer.BotID != invocation.Lease.BotID || invocation.Computer.RunID != invocation.Lease.RunID {
		return botagent.ToolResult{}, ErrComputerLost
	}
	return tool.shell.Execute(ctx, computer.TerminalAddress{Session: *invocation.Computer, OperationID: invocation.OperationID}, invocation.Call.Arguments, invocation.ApprovalMode)
}

var _ Tool = (*ComputerShell)(nil)
