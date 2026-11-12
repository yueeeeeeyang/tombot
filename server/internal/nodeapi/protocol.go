// Package nodeapi 提供非特权控制服务到受信宿主节点的本机有界协议。
package nodeapi

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
)

// Operations 由节点生命周期与执行控制器组合；协议不开放任意宿主命令、Docker 参数或路径。
type Operations struct {
	Node      computer.NodeRuntime
	Computers computer.Runtime
	Sessions  computer.SessionRuntime
	Terminals computer.TerminalRuntime
	Desktops  computer.DesktopRuntime
	Agents    computer.AgentRuntime
}

type request struct {
	NodeID        string                 `json:"node_id"`
	Action        string                 `json:"action"`
	Deadline      agentchannel.Deadline  `json:"deadline"`
	LeaseDeadline *agentchannel.Deadline `json:"lease_deadline,omitempty"`
	Input         json.RawMessage        `json:"input"`
}
type response struct {
	NodeID  string            `json:"node_id"`
	Action  string            `json:"action"`
	Result  json.RawMessage   `json:"result,omitempty"`
	Failure *computer.Failure `json:"failure,omitempty"`
}
type confirmation struct {
	Done bool `json:"done"`
}
type agentReceipt struct {
	Offset int64 `json:"offset"`
}

var codePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func validNode(id string) bool { return (computer.NodeQuery{NodeID: id}).Validate() == nil }
func validTimeout(timeout time.Duration) bool {
	return timeout >= time.Second && timeout <= time.Minute
}

// decodeInput 在客户端和节点使用同一份类型和归属校验，原始解码错误不离开协议层。
func decodeInput(nodeID, action string, encoded []byte) (any, time.Duration, error) {
	var input any
	var node string
	var lifetime time.Duration
	var err error
	switch action {
	case "heartbeat":
		var value computer.NodeQuery
		err = agentchannel.Decode(encoded, &value)
		if err == nil {
			err = value.Validate()
		}
		input, node = value, value.NodeID
	case "inspect", "ensure", "remove":
		var value computer.Specification
		err = agentchannel.Decode(encoded, &value)
		if err == nil {
			err = value.Validate()
		}
		input, node = value, value.NodeID
	case "open", "renew", "close":
		var value computer.SessionCommand
		err = agentchannel.Decode(encoded, &value)
		if err == nil {
			err = value.Validate()
		}
		input, node = value, value.Session.NodeID
		if action != "close" {
			lifetime = value.Lifetime
		}
	case "start":
		var value computer.TerminalStart
		err = agentchannel.Decode(encoded, &value)
		if err == nil {
			err = value.Validate()
		}
		input, node = value, value.Session.NodeID
	case "read":
		var value computer.TerminalRead
		err = agentchannel.Decode(encoded, &value)
		if err == nil {
			err = value.Validate()
		}
		input, node = value, value.Session.NodeID
	case "write":
		var value computer.TerminalWrite
		err = agentchannel.Decode(encoded, &value)
		if err == nil {
			err = value.Validate()
		}
		input, node = value, value.Session.NodeID
	case "resize":
		var value computer.TerminalResize
		err = agentchannel.Decode(encoded, &value)
		if err == nil {
			err = value.Validate()
		}
		input, node = value, value.Session.NodeID
	case "desktop_start", "desktop_status", "agent_start":
		var value computer.ExecutionSession
		err = agentchannel.Decode(encoded, &value)
		if err == nil {
			err = value.Validate()
		}
		input, node = value, value.NodeID
	case "desktop_capture":
		var value computer.DesktopCapture
		err = agentchannel.Decode(encoded, &value)
		if err == nil {
			err = value.Validate()
		}
		input, node = value, value.Session.NodeID
	case "desktop_image_read":
		var value computer.DesktopImageRead
		err = agentchannel.Decode(encoded, &value)
		if err == nil {
			err = value.Validate()
		}
		input, node = value, value.Session.NodeID
	case "desktop_input":
		var value computer.DesktopInput
		err = agentchannel.Decode(encoded, &value)
		if err == nil {
			err = value.Validate()
		}
		input, node = value, value.Session.NodeID
	case "agent_write":
		var value computer.AgentWrite
		err = agentchannel.Decode(encoded, &value)
		if err == nil {
			err = value.Validate()
		}
		input, node = value, value.Session.NodeID
	case "agent_read":
		var value computer.AgentRead
		err = agentchannel.Decode(encoded, &value)
		if err == nil {
			err = value.Validate()
		}
		input, node = value, value.Session.NodeID
	default:
		return nil, 0, errors.New("节点动作不受支持")
	}
	if err != nil || node != nodeID || !validNode(nodeID) {
		return nil, 0, errors.New("节点请求内容或归属无效")
	}
	return input, lifetime, nil
}

func (operations Operations) invoke(ctx context.Context, action string, input any) (any, error) {
	switch action {
	case "heartbeat":
		return operations.Node.Heartbeat(ctx, input.(computer.NodeQuery))
	case "inspect":
		return operations.Computers.Inspect(ctx, input.(computer.Specification))
	case "ensure":
		return operations.Computers.Ensure(ctx, input.(computer.Specification))
	case "remove":
		return operations.Computers.Remove(ctx, input.(computer.Specification))
	case "open":
		return operations.Sessions.Open(ctx, input.(computer.SessionCommand))
	case "renew":
		return operations.Sessions.Renew(ctx, input.(computer.SessionCommand))
	case "close":
		return operations.Sessions.Close(ctx, input.(computer.SessionCommand))
	case "start":
		return operations.Terminals.StartTerminal(ctx, input.(computer.TerminalStart))
	case "read":
		return operations.Terminals.ReadTerminal(ctx, input.(computer.TerminalRead))
	case "write":
		return operations.Terminals.WriteTerminal(ctx, input.(computer.TerminalWrite))
	case "resize":
		err := operations.Terminals.ResizeTerminal(ctx, input.(computer.TerminalResize))
		return confirmation{Done: err == nil}, err
	case "desktop_start":
		return operations.Desktops.StartDesktop(ctx, input.(computer.ExecutionSession))
	case "desktop_status":
		return operations.Desktops.InspectDesktop(ctx, input.(computer.ExecutionSession))
	case "desktop_capture":
		return operations.Desktops.CaptureDesktop(ctx, input.(computer.DesktopCapture))
	case "desktop_image_read":
		return operations.Desktops.ReadDesktopImage(ctx, input.(computer.DesktopImageRead))
	case "desktop_input":
		return operations.Desktops.InputDesktop(ctx, input.(computer.DesktopInput))
	case "agent_start":
		err := operations.Agents.StartAgent(ctx, input.(computer.ExecutionSession))
		return confirmation{Done: err == nil}, err
	case "agent_write":
		offset, err := operations.Agents.WriteAgent(ctx, input.(computer.AgentWrite))
		return agentReceipt{offset}, err
	case "agent_read":
		return operations.Agents.ReadAgent(ctx, input.(computer.AgentRead))
	default:
		return nil, errors.New("节点动作不受支持")
	}
}
