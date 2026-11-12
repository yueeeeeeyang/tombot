package nodeapi

import (
	"context"
	"errors"

	"io.yueyang/tom/internal/computer"
)

// validateResult 两端共同核对类型化结果；不能把结构完整但归属错误的回包当作成功。
func validateResult(action string, input, result any) error {
	valid := false
	switch action {
	case "heartbeat":
		value, ok := result.(computer.NodeHeartbeat)
		valid = ok && value.Validate(input.(computer.NodeQuery)) == nil
	case "inspect", "ensure", "remove":
		value, ok := result.(computer.Observation)
		valid = ok && value.Matches(input.(computer.Specification)) && (value.State != "ready" || value.Capabilities.Ready())
	case "open", "renew", "close":
		value, ok := result.(computer.SessionObservation)
		state := "active"
		if action == "close" {
			state = "drained"
		}
		valid = ok && value.Matches(input.(computer.SessionCommand), state)
	case "start", "read":
		value, ok := result.(computer.TerminalOutput)
		var cursor int64
		maximum := computer.MaximumTerminalReadBytes
		if action == "read" {
			read := input.(computer.TerminalRead)
			cursor, maximum = read.Cursor, read.MaximumBytes
		}
		valid = ok && value.Validate(cursor, maximum) == nil
	case "write":
		value, ok := result.(computer.TerminalReceipt)
		valid = ok && value.Validate(input.(computer.TerminalWrite)) == nil
	case "resize", "agent_start":
		value, ok := result.(confirmation)
		valid = ok && value.Done
	case "desktop_start", "desktop_status":
		value, ok := result.(computer.DesktopStatus)
		valid = ok && value.Session == input.(computer.ExecutionSession) && value.Validate() == nil
	case "desktop_capture":
		value, ok := result.(computer.DesktopImage)
		valid = ok && value.Validate(input.(computer.DesktopCapture)) == nil
	case "desktop_image_read":
		value, ok := result.(computer.DesktopImageChunk)
		valid = ok && value.Validate(input.(computer.DesktopImageRead)) == nil
	case "desktop_input":
		value, ok := result.(computer.DesktopInputReceipt)
		valid = ok && value.Validate(input.(computer.DesktopInput)) == nil
	case "agent_write":
		value, ok := result.(agentReceipt)
		write := input.(computer.AgentWrite)
		valid = ok && value.Offset == write.Offset+int64(len(write.Bytes))
	case "agent_read":
		value, ok := result.(computer.AgentOutput)
		valid = ok && value.Validate(input.(computer.AgentRead).Offset) == nil
	}
	if !valid {
		return errors.New("节点响应的资源身份、状态或范围不符")
	}
	return nil
}

func call[T any](ctx context.Context, client *Client, action string, input any) (T, error) {
	var output T
	if err := client.exchange(ctx, action, input, &output); err != nil {
		return output, err
	}
	if err := validateResult(action, input, output); err != nil {
		var empty T
		return empty, err
	}
	return output, nil
}

func (client *Client) Inspect(ctx context.Context, input computer.Specification) (computer.Observation, error) {
	return call[computer.Observation](ctx, client, "inspect", input)
}

func (client *Client) Heartbeat(ctx context.Context, input computer.NodeQuery) (computer.NodeHeartbeat, error) {
	return call[computer.NodeHeartbeat](ctx, client, "heartbeat", input)
}
func (client *Client) Ensure(ctx context.Context, input computer.Specification) (computer.Observation, error) {
	return call[computer.Observation](ctx, client, "ensure", input)
}
func (client *Client) Remove(ctx context.Context, input computer.Specification) (computer.Observation, error) {
	return call[computer.Observation](ctx, client, "remove", input)
}
func (client *Client) Open(ctx context.Context, input computer.SessionCommand) (computer.SessionObservation, error) {
	return call[computer.SessionObservation](ctx, client, "open", input)
}
func (client *Client) Renew(ctx context.Context, input computer.SessionCommand) (computer.SessionObservation, error) {
	return call[computer.SessionObservation](ctx, client, "renew", input)
}
func (client *Client) Close(ctx context.Context, input computer.SessionCommand) (computer.SessionObservation, error) {
	return call[computer.SessionObservation](ctx, client, "close", input)
}
func (client *Client) StartTerminal(ctx context.Context, input computer.TerminalStart) (computer.TerminalOutput, error) {
	return call[computer.TerminalOutput](ctx, client, "start", input)
}
func (client *Client) ReadTerminal(ctx context.Context, input computer.TerminalRead) (computer.TerminalOutput, error) {
	return call[computer.TerminalOutput](ctx, client, "read", input)
}
func (client *Client) WriteTerminal(ctx context.Context, input computer.TerminalWrite) (computer.TerminalReceipt, error) {
	return call[computer.TerminalReceipt](ctx, client, "write", input)
}
func (client *Client) ResizeTerminal(ctx context.Context, input computer.TerminalResize) error {
	_, err := call[confirmation](ctx, client, "resize", input)
	return err
}
func (client *Client) StartDesktop(ctx context.Context, input computer.ExecutionSession) (computer.DesktopStatus, error) {
	return call[computer.DesktopStatus](ctx, client, "desktop_start", input)
}
func (client *Client) InspectDesktop(ctx context.Context, input computer.ExecutionSession) (computer.DesktopStatus, error) {
	return call[computer.DesktopStatus](ctx, client, "desktop_status", input)
}
func (client *Client) CaptureDesktop(ctx context.Context, input computer.DesktopCapture) (computer.DesktopImage, error) {
	return call[computer.DesktopImage](ctx, client, "desktop_capture", input)
}
func (client *Client) ReadDesktopImage(ctx context.Context, input computer.DesktopImageRead) (computer.DesktopImageChunk, error) {
	return call[computer.DesktopImageChunk](ctx, client, "desktop_image_read", input)
}
func (client *Client) InputDesktop(ctx context.Context, input computer.DesktopInput) (computer.DesktopInputReceipt, error) {
	return call[computer.DesktopInputReceipt](ctx, client, "desktop_input", input)
}
func (client *Client) StartAgent(ctx context.Context, input computer.ExecutionSession) error {
	_, err := call[confirmation](ctx, client, "agent_start", input)
	return err
}
func (client *Client) WriteAgent(ctx context.Context, input computer.AgentWrite) (int64, error) {
	output, err := call[agentReceipt](ctx, client, "agent_write", input)
	return output.Offset, err
}
func (client *Client) ReadAgent(ctx context.Context, input computer.AgentRead) (computer.AgentOutput, error) {
	return call[computer.AgentOutput](ctx, client, "agent_read", input)
}

var _ computer.Runtime = (*Client)(nil)
var _ computer.NodeRuntime = (*Client)(nil)
var _ computer.SessionRuntime = (*Client)(nil)
var _ computer.TerminalRuntime = (*Client)(nil)
var _ computer.DesktopRuntime = (*Client)(nil)
var _ computer.AgentRuntime = (*Client)(nil)
