package botagent

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/policy"
)

const ToolImageChunkBytes = 32 << 10
const maximumToolImageChunks = domain.MaxVisionImageBytes / ToolImageChunkBytes
const maximumProtocolFrames = 64*(1+16*(2+maximumToolImageChunks)) + 1

// ToolGrant 仅描述已持久登记的一次本地操作，不携带平台凭据或任意连接地址。
type ToolGrant struct {
	OperationID  string                    `json:"operation_id"`
	Sequence     int64                     `json:"sequence"`
	Session      computer.ExecutionSession `json:"session"`
	ApprovalMode string                    `json:"approval_mode"`
	TimeoutMS    int64                     `json:"timeout_ms"`
	OutputBytes  int                       `json:"output_bytes"`
}

func (grant ToolGrant) Validate() error {
	if _, err := ulid.ParseStrict(grant.OperationID); err != nil {
		return ErrProtocol
	}
	if grant.Sequence < 1 || grant.Session.Validate() != nil || grant.TimeoutMS < 1000 || grant.TimeoutMS > 600000 || grant.OutputBytes < 1024 || grant.OutputBytes > 24<<10 {
		return ErrProtocol
	}
	if _, err := policy.NeedsApproval(grant.ApprovalMode, "medium"); err != nil {
		return ErrProtocol
	}
	return nil
}

type ToolPreparation struct {
	Grant  *ToolGrant
	Result *ToolResult
}

// LocalRelay 只登记和确认操作，不执行本地工具。读回包失败也须用未知结果结束已登记的操作。
type LocalRelay interface {
	PrepareLocal(context.Context, string, ToolCall) (ToolPreparation, error)
	CompleteLocal(context.Context, ToolGrant, ToolReport) (ToolResult, error)
}

// LocalExecutor 由固定 Agent 程序注册；模型和服务端授权不能指定要启动的执行器。
type LocalExecutor interface {
	Supports(string) bool
	Execute(context.Context, ToolGrant, ToolCall) (ToolResult, error)
	Finish(context.Context) error
}

type ToolReport struct {
	OperationID string     `json:"operation_id"`
	Result      ToolResult `json:"result"`
	Failure     string     `json:"failure,omitempty"`
}

func (report ToolReport) Validate(grant ToolGrant) error {
	if grant.Validate() != nil || report.OperationID != grant.OperationID || report.Result.Validate(grant.OutputBytes) != nil {
		return ErrProtocol
	}
	switch report.Failure {
	case "", "TOOL_RESULT_UNKNOWN", "TOOL_RESULT_INVALID":
	default:
		return ErrProtocol
	}
	if report.Failure != "" && (!report.Result.IsError || report.Result.Artifact != nil) {
		return ErrProtocol
	}
	if ref := report.Result.Artifact; ref != nil && (ref.ID != grant.OperationID || ref.BotID != grant.Session.BotID) {
		return ErrProtocol
	}
	return nil
}

type ToolImageChunk struct {
	OperationID string `json:"operation_id"`
	Offset      int    `json:"offset"`
	Bytes       []byte `json:"bytes"`
}

func unknownToolReport(grant ToolGrant, code string) ToolReport {
	return ToolReport{OperationID: grant.OperationID, Failure: code, Result: ToolResult{Content: "The local tool result is unknown. Do not repeat the action.", IsError: true}}
}

// executeLocal 只在收到授权后执行一次，结果必须得到服务端持久确认后才进入模型上下文。
func (relay *streamRelay) executeLocal(ctx context.Context, key string, call ToolCall, grant ToolGrant) (ToolResult, error) {
	if grant.Validate() != nil {
		return ToolResult{}, ErrProtocol
	}
	bounded, cancel := context.WithTimeout(ctx, time.Duration(grant.TimeoutMS)*time.Millisecond)
	result, err := relay.local.Execute(bounded, grant, call)
	callError := bounded.Err()
	cancel()
	report := ToolReport{OperationID: grant.OperationID, Result: result}
	if err != nil || callError != nil {
		report = unknownToolReport(grant, "TOOL_RESULT_UNKNOWN")
	}
	if report.Validate(grant) != nil || report.Result.Artifact != nil && ValidateScreenshotPixels(report.Result.Artifact) != nil {
		report = unknownToolReport(grant, "TOOL_RESULT_INVALID")
	}
	relay.sequence++
	if err := WriteFrame(relay.stream, Frame{Version: ProtocolVersion, Sequence: relay.sequence, Kind: "tool_complete", Key: key, Report: &report}); err != nil {
		return ToolResult{}, err
	}
	if ref := report.Result.Artifact; ref != nil {
		if ValidateScreenshotPixels(ref) != nil {
			return ToolResult{}, ErrProtocol
		}
		for offset := 0; offset < len(ref.Content); offset += ToolImageChunkBytes {
			relay.sequence++
			chunk := ToolImageChunk{OperationID: grant.OperationID, Offset: offset, Bytes: ref.Content[offset:min(offset+ToolImageChunkBytes, len(ref.Content))]}
			if err := WriteFrame(relay.stream, Frame{Version: ProtocolVersion, Sequence: relay.sequence, Kind: "tool_image", Image: &chunk}); err != nil {
				return ToolResult{}, err
			}
		}
	}
	confirmed, err := ReadFrame(relay.stream)
	if err != nil || confirmed.Kind != "result" || confirmed.Sequence != relay.sequence || confirmed.Tool == nil || confirmed.Tool.Validate(grant.OutputBytes) != nil || ctx.Err() != nil {
		return ToolResult{}, ErrProtocol
	}
	if (ToolReport{OperationID: grant.OperationID, Result: *confirmed.Tool}).Validate(grant) != nil {
		return ToolResult{}, ErrProtocol
	}
	return *confirmed.Tool, nil
}

// readLocalReport 拒绝图片替换、错序、缺块与超限；一次只装配当前操作的有界图片。
func readLocalReport(stream io.Reader, sequence *int, key string, grant ToolGrant) (ToolReport, error) {
	frame, err := ReadFrame(stream)
	if err != nil || frame.Sequence != *sequence+1 || frame.Kind != "tool_complete" || frame.Key != key || frame.Report == nil || frame.Report.Validate(grant) != nil {
		return ToolReport{}, ErrProtocol
	}
	*sequence = frame.Sequence
	report := *frame.Report
	if ref := report.Result.Artifact; ref != nil {
		ref.Content = make([]byte, 0, ref.Bytes)
		for len(ref.Content) < ref.Bytes {
			frame, err := ReadFrame(stream)
			if err != nil || frame.Sequence != *sequence+1 || frame.Kind != "tool_image" || frame.Image == nil {
				return ToolReport{}, ErrProtocol
			}
			chunk := frame.Image
			if chunk.OperationID != grant.OperationID || chunk.Offset != len(ref.Content) || len(chunk.Bytes) != min(ToolImageChunkBytes, ref.Bytes-len(ref.Content)) {
				return ToolReport{}, ErrProtocol
			}
			ref.Content = append(ref.Content, chunk.Bytes...)
			*sequence = frame.Sequence
		}
		if ValidateScreenshotPixels(ref) != nil {
			return ToolReport{}, ErrProtocol
		}
	}
	return report, nil
}

// dispatchLocal 将有界本地执行结果提交给持久层；超时关闭原流，不创建补发连接。
func dispatchLocal(ctx context.Context, stream io.ReadWriter, closeStream func() error, sequence *int, key string, call ToolCall, relay LocalRelay, maximum int) (ToolResult, error) {
	prepared, err := relay.PrepareLocal(ctx, key, call)
	if err != nil {
		return ToolResult{}, err
	}
	if (prepared.Grant == nil) == (prepared.Result == nil) {
		return ToolResult{}, ErrProtocol
	}
	if prepared.Result != nil {
		if prepared.Result.Validate(maximum) != nil {
			return ToolResult{}, ErrProtocol
		}
		return *prepared.Result, nil
	}
	grant := *prepared.Grant
	if grant.Validate() != nil || grant.OutputBytes != maximum {
		return ToolResult{}, ErrProtocol
	}
	bounded, cancel := context.WithTimeout(ctx, time.Duration(grant.TimeoutMS)*time.Millisecond)
	defer cancel()
	closed := make(chan error, 1)
	stop := context.AfterFunc(bounded, func() { closed <- closeStream() })
	writeError := WriteFrame(stream, Frame{Version: ProtocolVersion, Sequence: *sequence, Kind: "result", Grant: &grant})
	var report ToolReport
	if writeError == nil {
		report, err = readLocalReport(stream, sequence, key, grant)
	} else {
		err = writeError
	}
	if !stop() {
		err = errors.Join(err, <-closed, bounded.Err())
	}
	err = errors.Join(err, bounded.Err())
	if err != nil {
		report = unknownToolReport(grant, "TOOL_RESULT_UNKNOWN")
	}
	result, recordError := relay.CompleteLocal(context.WithoutCancel(ctx), grant, report)
	return result, errors.Join(err, recordError)
}
