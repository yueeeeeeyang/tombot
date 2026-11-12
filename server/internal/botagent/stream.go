package botagent

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"

	"io.yueyang/tom/internal/modelgateway"
)

// MaximumFrameBytes 包含最大上下文及协议封装；读取前校验长度，禁止无界分配。
const MaximumFrameBytes = 2 << 20

// ProtocolVersion 将本地执行授权、结果确认与图片分块纳入完整轨迹，不兼容旧 Agent。
const ProtocolVersion = 3

// Frame 用于已绑定单个运行的私有标准流，不允许帧内切换用户、Bot 或模型策略。
// 一个序号只对应一次请求及回包，连接断开后不得用新连接自动补发。
type Frame struct {
	Version  int                 `json:"version"`
	Sequence int                 `json:"sequence"`
	Kind     string              `json:"kind"`
	Key      string              `json:"key,omitempty"`
	Start    *Start              `json:"start,omitempty"`
	Input    *modelgateway.Input `json:"input,omitempty"`
	Call     *ToolCall           `json:"call,omitempty"`
	Model    *ModelOutput        `json:"model,omitempty"`
	Tool     *ToolResult         `json:"tool,omitempty"`
	Grant    *ToolGrant          `json:"grant,omitempty"`
	Report   *ToolReport         `json:"report,omitempty"`
	Image    *ToolImageChunk     `json:"image,omitempty"`
	Answer   string              `json:"answer,omitempty"`
	Failure  string              `json:"failure,omitempty"`
}

func (frame Frame) validate() error {
	if frame.Version != ProtocolVersion || frame.Sequence < 0 || frame.Sequence > maximumProtocolFrames {
		return ErrProtocol
	}
	count := 0
	for _, set := range []bool{frame.Start != nil, frame.Input != nil, frame.Call != nil, frame.Model != nil, frame.Tool != nil, frame.Grant != nil, frame.Report != nil, frame.Image != nil} {
		if set {
			count++
		}
	}
	switch frame.Kind {
	case "start":
		if frame.Sequence != 0 || count != 1 || frame.Start == nil || frame.Key != "" || frame.Answer != "" || frame.Failure != "" {
			return ErrProtocol
		}
	case "model":
		if frame.Sequence == 0 || count != 1 || frame.Input == nil || frame.Key == "" || len(frame.Key) > 32 || frame.Answer != "" || frame.Failure != "" {
			return ErrProtocol
		}
	case "tool":
		if frame.Sequence == 0 || count != 1 || frame.Call == nil || frame.Key == "" || len(frame.Key) > 32 || frame.Answer != "" || frame.Failure != "" {
			return ErrProtocol
		}
	case "result":
		if frame.Sequence == 0 || count != 1 || (frame.Model == nil && frame.Tool == nil && frame.Grant == nil) || frame.Key != "" || frame.Answer != "" || frame.Failure != "" {
			return ErrProtocol
		}
		if frame.Grant != nil && frame.Grant.Validate() != nil {
			return ErrProtocol
		}
	case "tool_complete":
		if frame.Sequence == 0 || count != 1 || frame.Report == nil || frame.Key == "" || len(frame.Key) > 32 || frame.Answer != "" || frame.Failure != "" {
			return ErrProtocol
		}
	case "tool_image":
		if frame.Sequence == 0 || count != 1 || frame.Image == nil || frame.Key != "" || frame.Answer != "" || frame.Failure != "" || len(frame.Image.Bytes) < 1 || len(frame.Image.Bytes) > ToolImageChunkBytes {
			return ErrProtocol
		}
	case "done":
		if frame.Sequence == 0 || count != 0 || frame.Key != "" || !textValid(frame.Answer, 24<<10) {
			return ErrProtocol
		}
		switch frame.Failure {
		case "", "AGENT_INPUT_INVALID", "MODEL_OUTPUT_INVALID", "CONTEXT_LIMIT", "OPERATION_LIMIT", "AGENT_PROTOCOL_FAILED", "AGENT_CANCELED":
		default:
			return ErrProtocol
		}
	default:
		return ErrProtocol
	}
	return nil
}

// WriteFrame 写入一个完整帧；部分写入或编码失败均明确报错，不发送下一帧补偿。
func WriteFrame(writer io.Writer, frame Frame) error {
	if err := frame.validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(frame)
	if err != nil || len(encoded) > MaximumFrameBytes {
		return ErrProtocol
	}
	payload := make([]byte, 4, len(encoded)+4)
	binary.BigEndian.PutUint32(payload, uint32(len(encoded)))
	payload = append(payload, encoded...)
	count, err := writer.Write(payload)
	if err != nil || count != len(payload) {
		return ErrProtocol
	}
	return nil
}

// ReadFrame 拒绝未知字段、重复键、大小写别名、尾随内容和超限帧。
func ReadFrame(reader io.Reader) (Frame, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return Frame{}, ErrProtocol
	}
	size := binary.BigEndian.Uint32(prefix[:])
	if size == 0 || size > MaximumFrameBytes {
		return Frame{}, ErrProtocol
	}
	encoded := make([]byte, int(size))
	if _, err := io.ReadFull(reader, encoded); err != nil || !utf8.Valid(encoded) {
		return Frame{}, ErrProtocol
	}
	var frame Frame
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if decoder.Decode(&frame) != nil {
		return Frame{}, ErrProtocol
	}
	if frame.Input != nil {
		// provider 的工具参数字段为 any；保留原始 JSON，避免默认 map 解码排序或大整数精度变化。
		var raw struct {
			Input struct {
				Messages []struct {
					Blocks []struct {
						ToolUse *struct {
							Input json.RawMessage `json:"input"`
						} `json:"tool_use"`
					}
				}
			} `json:"input"`
		}
		if json.Unmarshal(encoded, &raw) != nil {
			return Frame{}, ErrProtocol
		}
		for messageIndex, message := range raw.Input.Messages {
			for blockIndex, block := range message.Blocks {
				if block.ToolUse != nil {
					frame.Input.Messages[messageIndex].Blocks[blockIndex].ToolUse.Input = block.ToolUse.Input
				}
			}
		}
	}
	canonical, err := json.Marshal(frame)
	if err != nil || !bytes.Equal(canonical, encoded) || frame.validate() != nil {
		return Frame{}, ErrProtocol
	}
	return frame, nil
}

type streamRelay struct {
	stream     io.ReadWriter
	sequence   int
	local      LocalExecutor
	localTools map[string]bool
}

func (relay *streamRelay) exchange(ctx context.Context, frame Frame) (Frame, error) {
	if err := context.Cause(ctx); err != nil {
		return Frame{}, err
	}
	relay.sequence++
	frame.Version, frame.Sequence = ProtocolVersion, relay.sequence
	if err := WriteFrame(relay.stream, frame); err != nil {
		return Frame{}, err
	}
	result, err := ReadFrame(relay.stream)
	if err != nil || result.Kind != "result" || result.Sequence != frame.Sequence {
		return Frame{}, ErrProtocol
	}
	if err := context.Cause(ctx); err != nil {
		return Frame{}, err
	}
	return result, nil
}

func (relay *streamRelay) Model(ctx context.Context, key string, input modelgateway.Input) (ModelOutput, error) {
	frame, err := relay.exchange(ctx, Frame{Kind: "model", Key: key, Input: &input})
	if err != nil {
		return ModelOutput{}, err
	}
	if frame.Model == nil {
		return ModelOutput{}, ErrProtocol
	}
	return *frame.Model, nil
}

func (relay *streamRelay) Tool(ctx context.Context, key string, call ToolCall) (ToolResult, error) {
	frame, err := relay.exchange(ctx, Frame{Kind: "tool", Key: key, Call: &call})
	if err != nil {
		return ToolResult{}, err
	}
	if frame.Grant != nil {
		if !relay.localTools[call.Name] || relay.local == nil || !relay.local.Supports(call.Name) {
			return ToolResult{}, ErrProtocol
		}
		return relay.executeLocal(ctx, key, call, *frame.Grant)
	}
	if frame.Tool == nil {
		return ToolResult{}, ErrProtocol
	}
	return *frame.Tool, nil
}

func failureCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrInvalidInput):
		return "AGENT_INPUT_INVALID"
	case errors.Is(err, ErrModelOutput):
		return "MODEL_OUTPUT_INVALID"
	case errors.Is(err, ErrContextLimit):
		return "CONTEXT_LIMIT"
	case errors.Is(err, ErrOperationLimit):
		return "OPERATION_LIMIT"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "AGENT_CANCELED"
	default:
		return "AGENT_PROTOCOL_FAILED"
	}
}

// Serve 接管一个私有双向流，读取一次启动内容后执行并退出；取消会关闭流以唤醒阻塞读写。
// EOF、协议错误或序号不符不发送成功结果，也不在此建立新连接。
func Serve(ctx context.Context, stream io.ReadWriteCloser, local LocalExecutor) (failure error) {
	if stream == nil {
		return ErrInvalidInput
	}
	closed := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() { closed <- stream.Close() })
	defer func() {
		var err error
		if stop() {
			err = stream.Close()
		} else {
			err = <-closed
		}
		if err != nil {
			failure = errors.Join(failure, ErrProtocol)
		}
	}()
	first, err := ReadFrame(stream)
	if err != nil || first.Kind != "start" {
		if context.Cause(ctx) != nil {
			return context.Cause(ctx)
		}
		return ErrProtocol
	}
	if first.Start.Validate() != nil {
		return ErrInvalidInput
	}
	localTools := map[string]bool{}
	for _, name := range first.Start.LocalTools {
		if local == nil || !local.Supports(name) {
			return ErrProtocol
		}
		localTools[name] = true
	}
	relay := &streamRelay{stream: stream, local: local, localTools: localTools}
	answer, runErr := Run(ctx, *first.Start, relay)
	if runErr == nil && len(localTools) > 0 {
		if err := local.Finish(ctx); err != nil {
			return errors.Join(ErrProtocol, err)
		}
	}
	if context.Cause(ctx) != nil {
		return context.Cause(ctx)
	}
	if errors.Is(runErr, ErrProtocol) {
		return runErr
	}
	// 无效模型内容不能原样回传为可展示答复；错误只使用固定代码。
	if !textValid(answer, first.Start.Limits.OutputBytes) {
		answer = ""
	}
	err = WriteFrame(stream, Frame{Version: ProtocolVersion, Sequence: relay.sequence + 1, Kind: "done", Answer: answer, Failure: failureCode(runErr)})
	return errors.Join(runErr, err)
}
