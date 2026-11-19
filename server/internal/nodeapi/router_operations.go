package nodeapi

import (
	"context"

	"io.yueyang/tom/internal/computer"
)

func (router *Router) Inspect(ctx context.Context, input computer.Specification) (computer.Observation, error) {
	return routeCall[computer.Observation](ctx, router, input.NodeID, "inspect", input)
}

func (router *Router) Heartbeat(ctx context.Context, input computer.NodeQuery) (computer.NodeHeartbeat, error) {
	return routeCall[computer.NodeHeartbeat](ctx, router, input.NodeID, "heartbeat", input)
}
func (router *Router) Ensure(ctx context.Context, input computer.Specification) (computer.Observation, error) {
	return routeCall[computer.Observation](ctx, router, input.NodeID, "ensure", input)
}
func (router *Router) Remove(ctx context.Context, input computer.Specification) (computer.Observation, error) {
	return routeCall[computer.Observation](ctx, router, input.NodeID, "remove", input)
}

// UpgradeComputer 按已登记节点地址发送迁移请求，目标与原电脑必须属于同一节点。
func (router *Router) UpgradeComputer(ctx context.Context, input computer.ImageUpgrade) (computer.Observation, error) {
	return routeCall[computer.Observation](ctx, router, input.Target.NodeID, "computer_upgrade", input)
}
func (router *Router) Open(ctx context.Context, input computer.SessionCommand) (computer.SessionObservation, error) {
	return routeCall[computer.SessionObservation](ctx, router, input.Session.NodeID, "open", input)
}
func (router *Router) Renew(ctx context.Context, input computer.SessionCommand) (computer.SessionObservation, error) {
	return routeCall[computer.SessionObservation](ctx, router, input.Session.NodeID, "renew", input)
}
func (router *Router) Close(ctx context.Context, input computer.SessionCommand) (computer.SessionObservation, error) {
	return routeCall[computer.SessionObservation](ctx, router, input.Session.NodeID, "close", input)
}
func (router *Router) StartTerminal(ctx context.Context, input computer.TerminalStart) (computer.TerminalOutput, error) {
	return routeCall[computer.TerminalOutput](ctx, router, input.Session.NodeID, "start", input)
}
func (router *Router) ReadTerminal(ctx context.Context, input computer.TerminalRead) (computer.TerminalOutput, error) {
	return routeCall[computer.TerminalOutput](ctx, router, input.Session.NodeID, "read", input)
}
func (router *Router) WriteTerminal(ctx context.Context, input computer.TerminalWrite) (computer.TerminalReceipt, error) {
	return routeCall[computer.TerminalReceipt](ctx, router, input.Session.NodeID, "write", input)
}
func (router *Router) ResizeTerminal(ctx context.Context, input computer.TerminalResize) error {
	_, err := routeCall[confirmation](ctx, router, input.Session.NodeID, "resize", input)
	return err
}
func (router *Router) StartDesktop(ctx context.Context, input computer.ExecutionSession) (computer.DesktopStatus, error) {
	return routeCall[computer.DesktopStatus](ctx, router, input.NodeID, "desktop_start", input)
}
func (router *Router) InspectDesktop(ctx context.Context, input computer.ExecutionSession) (computer.DesktopStatus, error) {
	return routeCall[computer.DesktopStatus](ctx, router, input.NodeID, "desktop_status", input)
}
func (router *Router) CaptureDesktop(ctx context.Context, input computer.DesktopCapture) (computer.DesktopImage, error) {
	return routeCall[computer.DesktopImage](ctx, router, input.Session.NodeID, "desktop_capture", input)
}
func (router *Router) ReadDesktopImage(ctx context.Context, input computer.DesktopImageRead) (computer.DesktopImageChunk, error) {
	return routeCall[computer.DesktopImageChunk](ctx, router, input.Session.NodeID, "desktop_image_read", input)
}
func (router *Router) InputDesktop(ctx context.Context, input computer.DesktopInput) (computer.DesktopInputReceipt, error) {
	return routeCall[computer.DesktopInputReceipt](ctx, router, input.Session.NodeID, "desktop_input", input)
}
func (router *Router) StartAgent(ctx context.Context, input computer.ExecutionSession) error {
	_, err := routeCall[confirmation](ctx, router, input.NodeID, "agent_start", input)
	return err
}
func (router *Router) WriteAgent(ctx context.Context, input computer.AgentWrite) (int64, error) {
	output, err := routeCall[agentReceipt](ctx, router, input.Session.NodeID, "agent_write", input)
	return output.Offset, err
}
func (router *Router) ReadAgent(ctx context.Context, input computer.AgentRead) (computer.AgentOutput, error) {
	return routeCall[computer.AgentOutput](ctx, router, input.Session.NodeID, "agent_read", input)
}

var _ computer.Runtime = (*Router)(nil)
var _ computer.ImageUpgrader = (*Router)(nil)
var _ computer.NodeRuntime = (*Router)(nil)
var _ computer.SessionRuntime = (*Router)(nil)
var _ computer.TerminalRuntime = (*Router)(nil)
var _ computer.DesktopRuntime = (*Router)(nil)
var _ computer.AgentRuntime = (*Router)(nil)

func (router *Router) TransferFile(ctx context.Context, input computer.FileRequest) (computer.FileReceipt, error) {
	return routeCall[computer.FileReceipt](ctx, router, input.Session.NodeID, "file_transfer", input)
}

var _ computer.FileRuntime = (*Router)(nil)

func (router *Router) Console(ctx context.Context, input computer.ConsoleRequest) (computer.ConsoleReceipt, error) {
	return routeCall[computer.ConsoleReceipt](ctx, router, input.Session.NodeID, "console", input)
}

var _ computer.ConsoleRuntime = (*Router)(nil)

func (router *Router) ManageComputer(ctx context.Context, input computer.Maintenance) (computer.MaintenanceReceipt, error) {
	return routeCall[computer.MaintenanceReceipt](ctx, router, input.NodeID, "computer_manage", input)
}
func (router *Router) PrepareBrowser(ctx context.Context, input computer.ExecutionSession) error {
	_, err := routeCall[confirmation](ctx, router, input.NodeID, "browser_prepare", input)
	return err
}
