package execution

import (
	"context"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computertools"
	"io.yueyang/tom/internal/provider"
)

type ComputerDesktop struct {
	desktop *computertools.Desktop
	name    string
}

// NewComputerDesktop 返回共享无状态执行器的截图与输入工具；每次调用均由持久操作和完整会话定位。
func NewComputerDesktop(runtime computer.DesktopRuntime, options computertools.DesktopOptions) ([]Tool, error) {
	desktop, err := computertools.NewDesktop(runtime, options)
	if err != nil {
		return nil, err
	}
	return []Tool{&ComputerDesktop{desktop: desktop, name: "computer_screenshot"}, &ComputerDesktop{desktop: desktop, name: "computer_input"}}, nil
}
func (tool *ComputerDesktop) Schema() provider.ToolSchema { return tool.desktop.Schema(tool.name) }
func (tool *ComputerDesktop) UsesComputer() bool          { return true }
func (tool *ComputerDesktop) RequiresVision() bool        { return true }

// Execute 不接受模型指定电脑、显示号或操作序号，只使用已持久记账的 Invocation。
func (tool *ComputerDesktop) Execute(ctx context.Context, invocation Invocation) (botagent.ToolResult, error) {
	if invocation.Computer == nil || invocation.Call.Name != tool.name || invocation.Computer.UserID != invocation.Lease.UserID || invocation.Computer.BotID != invocation.Lease.BotID || invocation.Computer.RunID != invocation.Lease.RunID {
		return botagent.ToolResult{}, ErrComputerLost
	}
	return tool.desktop.Execute(ctx, *invocation.Computer, invocation.OperationID, invocation.Sequence, tool.name, invocation.Call.Arguments, invocation.ApprovalMode)
}

var _ Tool = (*ComputerDesktop)(nil)
