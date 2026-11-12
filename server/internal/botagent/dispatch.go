package botagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/provider"
)

// Dispatch 只向已经授权的电脑发起固定 Agent；不在调用者进程中执行推理循环。
// 每个分块调用有独立期限，未知启动或写入不会自动重试，资源回收由运行控制层负责。
func Dispatch(ctx context.Context, runtime computer.AgentRuntime, session computer.ExecutionSession, start Start, relay Relay, timeout time.Duration) (string, error) {
	if runtime == nil || relay == nil || session.Validate() != nil || timeout < 200*time.Millisecond || timeout > 10*time.Second {
		return "", ErrInvalidInput
	}
	if err := start.Validate(); err != nil {
		return "", err
	}
	if _, ok := relay.(LocalRelay); len(start.LocalTools) > 0 && !ok {
		return "", ErrInvalidInput
	}
	bounded, stop := context.WithTimeout(ctx, timeout)
	err := runtime.StartAgent(bounded, session)
	stop()
	if err != nil {
		return "", errors.Join(ErrProtocol, err)
	}
	streamContext, cancel := context.WithCancel(ctx)
	stream := &remoteStream{ctx: streamContext, cancel: cancel, runtime: runtime, session: session, timeout: timeout}
	return DispatchStream(ctx, stream, start, relay)
}

type remoteStream struct {
	ctx                     context.Context
	cancel                  context.CancelFunc
	runtime                 computer.AgentRuntime
	session                 computer.ExecutionSession
	timeout                 time.Duration
	readOffset, writeOffset int64
	pending                 []byte
	exited                  bool
	exitCode                int
}

func (stream *remoteStream) Close() error { stream.cancel(); return nil }

func (stream *remoteStream) Write(payload []byte) (int, error) {
	written := 0
	for written < len(payload) {
		if err := stream.ctx.Err(); err != nil {
			return written, err
		}
		chunk := payload[written:min(written+computer.AgentChunkBytes, len(payload))]
		ctx, cancel := context.WithTimeout(stream.ctx, stream.timeout)
		offset, err := stream.runtime.WriteAgent(ctx, computer.AgentWrite{Session: stream.session, Offset: stream.writeOffset, Bytes: chunk})
		cancel()
		if err != nil || offset != stream.writeOffset+int64(len(chunk)) {
			return written, ErrProtocol
		}
		stream.writeOffset = offset
		written += len(chunk)
	}
	return written, nil
}

func (stream *remoteStream) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	for len(stream.pending) == 0 {
		if err := stream.ctx.Err(); err != nil {
			return 0, err
		}
		if stream.exited {
			return 0, io.EOF
		}
		ctx, cancel := context.WithTimeout(stream.ctx, stream.timeout)
		output, err := stream.runtime.ReadAgent(ctx, computer.AgentRead{Session: stream.session, Offset: stream.readOffset})
		cancel()
		if err != nil || output.Validate(stream.readOffset) != nil {
			return 0, ErrProtocol
		}
		stream.pending, stream.readOffset, stream.exited, stream.exitCode = output.Bytes, output.Offset, output.Exited, output.ExitCode
		if len(stream.pending) == 0 && !stream.exited {
			timer := time.NewTimer(10 * time.Millisecond)
			select {
			case <-stream.ctx.Done():
				timer.Stop()
				return 0, stream.ctx.Err()
			case <-timer.C:
			}
		}
	}
	count := copy(buffer, stream.pending)
	stream.pending = stream.pending[count:]
	return count, nil
}

func sameInput(first, second any) bool {
	left, err := json.Marshal(first)
	if err != nil {
		return false
	}
	right, err := json.Marshal(second)
	return err == nil && bytes.Equal(left, right)
}

// DispatchStream 中继私有进程请求，并独立核对整个执行轨迹。下一步由 Agent 发起，
// 控制面只允许既有快照和已持久返回结果所确定的步骤，不接受任意模型上下文或工具动作。
func DispatchStream(ctx context.Context, stream io.ReadWriteCloser, start Start, relay Relay) (answer string, failure error) {
	if stream == nil {
		return "", ErrInvalidInput
	}
	closeStream := sync.OnceValue(stream.Close)
	closed := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() { closed <- closeStream() })
	defer func() {
		var err error
		if stop() {
			err = closeStream()
		} else {
			err = <-closed
		}
		if err != nil {
			failure = errors.Join(failure, ErrProtocol)
		}
		if context.Cause(ctx) != nil {
			failure = context.Cause(ctx)
		}
	}()
	if relay == nil {
		return "", ErrInvalidInput
	}
	if err := start.Validate(); err != nil {
		return "", err
	}
	localRelay, localSupported := relay.(LocalRelay)
	if len(start.LocalTools) > 0 && !localSupported {
		return "", ErrInvalidInput
	}
	localTools := map[string]bool{}
	for _, name := range start.LocalTools {
		localTools[name] = true
	}
	if err := WriteFrame(stream, Frame{Version: ProtocolVersion, Kind: "start", Start: &start}); err != nil {
		return "", err
	}
	expected := start.Input
	expected.Messages = append(expected.Messages[:0:0], expected.Messages...)
	allowed := map[string]bool{}
	for _, schema := range expected.Tools {
		allowed[schema.Name] = true
	}
	step, toolIndex := 1, 0
	var calls []ToolCall
	var results []domain.MessageBlock
	var lastText, finalText, finalFailure string
	wantDone := false
	for sequence := 1; sequence <= maximumProtocolFrames; sequence++ {
		if err := context.Cause(ctx); err != nil {
			return lastText, err
		}
		frame, err := ReadFrame(stream)
		if err != nil || frame.Sequence != sequence {
			return lastText, ErrProtocol
		}
		if wantDone {
			if frame.Kind != "done" || frame.Answer != finalText || frame.Failure != finalFailure {
				return lastText, ErrProtocol
			}
			// 完成帧后还须确认无额外输出并等待 Agent 退出；不能仅凭一条文本宣称执行结束。
			var extra [1]byte
			if count, err := stream.Read(extra[:]); count != 0 || !errors.Is(err, io.EOF) {
				return lastText, ErrProtocol
			}
			if remote, ok := stream.(*remoteStream); ok && (finalFailure == "" && remote.exitCode != 0 || finalFailure != "" && remote.exitCode == 0) {
				return lastText, ErrProtocol
			}
			switch finalFailure {
			case "":
				return finalText, nil
			case "OPERATION_LIMIT":
				return "", ErrOperationLimit
			case "CONTEXT_LIMIT":
				return "", ErrContextLimit
			default:
				return lastText, ErrProtocol
			}
		}
		if len(calls) == 0 {
			if frame.Kind != "model" || frame.Key != fmt.Sprintf("model.%d", step) || !sameInput(frame.Input, expected) {
				return lastText, ErrProtocol
			}
			output, err := relay.Model(ctx, frame.Key, *frame.Input)
			lastText = output.Text
			if err != nil {
				return lastText, err
			}
			if err = validateOutput(output, start, allowed); err != nil {
				return lastText, err
			}
			if err = WriteFrame(stream, Frame{Version: ProtocolVersion, Sequence: sequence, Kind: "result", Model: &output}); err != nil {
				return lastText, err
			}
			if len(output.Tools) == 0 {
				wantDone, finalText = true, output.Text
				continue
			}
			expected.Messages = append(expected.Messages, assistantMessage(output))
			calls, toolIndex, results = output.Tools, 0, nil
		} else {
			if frame.Kind != "tool" || frame.Key != fmt.Sprintf("tool.%d.%d", step, toolIndex+1) || frame.Call == nil || *frame.Call != calls[toolIndex] {
				return lastText, ErrProtocol
			}
			var result ToolResult
			var err error
			if localTools[frame.Call.Name] {
				result, err = dispatchLocal(ctx, stream, closeStream, &sequence, frame.Key, *frame.Call, localRelay, start.Limits.OutputBytes)
			} else {
				result, err = relay.Tool(ctx, frame.Key, *frame.Call)
			}
			if err != nil {
				return lastText, err
			}
			if result.Validate(start.Limits.OutputBytes) != nil {
				return lastText, ErrProtocol
			}
			if err = WriteFrame(stream, Frame{Version: ProtocolVersion, Sequence: sequence, Kind: "result", Tool: &result}); err != nil {
				return lastText, err
			}
			results = append(results, toolBlock(*frame.Call, result))
			toolIndex++
			if toolIndex == len(calls) {
				expected.Messages = append(expected.Messages, provider.Message{Role: "user", Blocks: results})
				calls = nil
				step++
				if step > start.Limits.ModelSteps {
					wantDone, finalFailure = true, "OPERATION_LIMIT"
				} else if err := checkContext(expected, start.Limits.ContextBytes); err != nil {
					wantDone, finalFailure = true, "CONTEXT_LIMIT"
				}
			}
		}
	}
	return lastText, ErrProtocol
}
