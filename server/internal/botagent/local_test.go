package botagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/png"
	"net"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
)

type localExecutorStub struct {
	execute func(context.Context, ToolGrant, ToolCall) (ToolResult, error)
	finish  func(context.Context) error
}

func (localExecutorStub) Supports(name string) bool { return name == "file_read" }
func (executor localExecutorStub) Execute(ctx context.Context, grant ToolGrant, call ToolCall) (ToolResult, error) {
	return executor.execute(ctx, grant, call)
}
func (executor localExecutorStub) Finish(ctx context.Context) error {
	if executor.finish != nil {
		return executor.finish(ctx)
	}
	return nil
}

type localRelayStub struct {
	testRelay
	prepare  func(context.Context, string, ToolCall) (ToolPreparation, error)
	complete func(context.Context, ToolGrant, ToolReport) (ToolResult, error)
}

func (relay localRelayStub) PrepareLocal(ctx context.Context, key string, call ToolCall) (ToolPreparation, error) {
	return relay.prepare(ctx, key, call)
}
func (relay localRelayStub) CompleteLocal(ctx context.Context, grant ToolGrant, report ToolReport) (ToolResult, error) {
	return relay.complete(ctx, grant, report)
}

func localGrant() ToolGrant {
	session := computer.ExecutionSession{DesktopIdentity: computer.DesktopIdentity{UserID: ids.New(), ComputerID: ids.New(), BotID: ids.New(), DesktopID: ids.New(), UID: 100001, GID: 20000, DisplayNumber: 1}, NodeID: ids.New(), ContainerID: strings.Repeat("a", 64), Generation: 1, SessionID: ids.New(), RunID: ids.New(), FencingToken: 1}
	return ToolGrant{OperationID: ids.New(), Sequence: 2, Session: session, ApprovalMode: "standard", TimeoutMS: 2000, OutputBytes: 4096}
}

func TestLocalToolExecutesOnlyInAgentAndUsesConfirmedResult(t *testing.T) {
	for _, mode := range []string{"正常", "缓存回执", "图片", "分块图片", "执行未知", "错误图片归属", "持久确认失败", "未完成浏览器动作"} {
		t.Run(mode, func(t *testing.T) {
			grant := localGrant()
			result := ToolResult{Content: "Local result."}
			if mode == "图片" || mode == "分块图片" || mode == "错误图片归属" {
				result = screenshotResult(t)
				result.Artifact.ID = grant.OperationID
				result.Artifact.BotID = grant.Session.BotID
			}
			if mode == "分块图片" {
				var pixels bytes.Buffer
				encoder := png.Encoder{CompressionLevel: png.NoCompression}
				if err := encoder.Encode(&pixels, image.NewNRGBA(image.Rect(0, 0, 320, 240))); err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(pixels.Bytes())
				ref := result.Artifact
				ref.Content, ref.Bytes, ref.Width, ref.Height, ref.SHA256 = pixels.Bytes(), pixels.Len(), 320, 240, hex.EncodeToString(digest[:])
				if ref.Bytes <= ToolImageChunkBytes {
					t.Fatal("夹具没有跨越图片分块")
				}
			}
			if mode == "错误图片归属" {
				result.Artifact.BotID = ids.New()
			}
			parent, child := net.Pipe()
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			done := make(chan error, 1)
			executions, preparations, completions, models := 0, 0, 0, 0
			local := localExecutorStub{execute: func(context.Context, ToolGrant, ToolCall) (ToolResult, error) {
				executions++
				if mode == "执行未知" {
					return ToolResult{}, errors.New("执行未确认")
				}
				return result, nil
			}, finish: func(context.Context) error {
				if mode == "未完成浏览器动作" {
					return errors.New("页面仍待确认")
				}
				return nil
			}}
			go func() { done <- Serve(ctx, child, local) }()
			start := fixtureStart()
			start.LocalTools = []string{"file_read"}
			relay := localRelayStub{testRelay: testRelay{model: func(_ context.Context, key string, input modelgateway.Input) (ModelOutput, error) {
				models++
				if key == "model.1" {
					return toolOutput(), nil
				}
				if input.Messages[2].Blocks[0].ToolResult.Content != "Confirmed result." {
					t.Error("未确认的本地结果进入模型")
				}
				return ModelOutput{Text: "完成", StopReason: "stop"}, nil
			}, tool: func(context.Context, string, ToolCall) (ToolResult, error) {
				t.Error("本地工具回退到服务端执行")
				return ToolResult{}, ErrProtocol
			}},
				prepare: func(context.Context, string, ToolCall) (ToolPreparation, error) {
					preparations++
					if mode == "缓存回执" {
						return ToolPreparation{Result: &ToolResult{Content: "Confirmed result."}}, nil
					}
					return ToolPreparation{Grant: &grant}, nil
				},
				complete: func(_ context.Context, actual ToolGrant, report ToolReport) (ToolResult, error) {
					completions++
					if actual != grant || report.Validate(grant) != nil {
						t.Error("授权或回执丢失")
					}
					if mode == "执行未知" || mode == "错误图片归属" {
						if report.Failure == "" {
							t.Error("未知结果未保留")
						}
						return report.Result, ErrProtocol
					}
					if mode == "持久确认失败" {
						return ToolResult{}, ErrProtocol
					}
					if (mode == "图片" || mode == "分块图片") && (report.Result.Artifact == nil || !bytes.Equal(report.Result.Artifact.Content, result.Artifact.Content)) {
						t.Error("图片未完整回传")
					}
					confirmed := report.Result
					confirmed.Content = "Confirmed result."
					if confirmed.Artifact != nil {
						ref := *confirmed.Artifact
						ref.Content = nil
						confirmed.Artifact = &ref
					}
					return confirmed, nil
				}}
			answer, err := DispatchStream(ctx, parent, start, relay)
			agentError := <-done
			failed := mode == "执行未知" || mode == "错误图片归属" || mode == "持久确认失败" || mode == "未完成浏览器动作"
			if failed {
				if err == nil || agentError == nil {
					t.Fatal("失败被报告为成功", answer, err, agentError)
				}
			} else if err != nil || agentError != nil || answer != "完成" {
				t.Fatal(answer, err, agentError)
			}
			want := 1
			if mode == "缓存回执" {
				want = 0
			}
			if executions != want || completions != want || preparations != 1 {
				t.Fatal(executions, completions, preparations)
			}
			if failed && mode != "未完成浏览器动作" && models != 1 {
				t.Fatal("未确认结果触发后续模型", models)
			}
		})
	}
}

func TestLocalReportRejectsWrongIdentitySequenceAndPixels(t *testing.T) {
	for _, mode := range []string{"正确", "错误操作", "错误步骤", "缺失块", "错序", "错误偏移", "篡改像素"} {
		t.Run(mode, func(t *testing.T) {
			grant := localGrant()
			result := screenshotResult(t)
			result.Artifact.ID = grant.OperationID
			result.Artifact.BotID = grant.Session.BotID
			report := ToolReport{OperationID: grant.OperationID, Result: result}
			frame := Frame{Version: ProtocolVersion, Sequence: 3, Kind: "tool_complete", Key: "tool.1.1", Report: &report}
			if mode == "错误操作" {
				report.OperationID = ids.New()
			}
			if mode == "错误步骤" {
				frame.Key = "tool.1.2"
			}
			var wire bytes.Buffer
			mustWrite(t, &wire, frame)
			if mode != "缺失块" {
				chunk := ToolImageChunk{OperationID: grant.OperationID, Bytes: bytes.Clone(result.Artifact.Content)}
				if mode == "错误偏移" {
					chunk.Offset = 1
				}
				if mode == "篡改像素" {
					chunk.Bytes[20] ^= 1
				}
				sequence := 4
				if mode == "错序" {
					sequence++
				}
				mustWrite(t, &wire, Frame{Version: ProtocolVersion, Sequence: sequence, Kind: "tool_image", Image: &chunk})
			}
			sequence := 2
			_, err := readLocalReport(&wire, &sequence, "tool.1.1", grant)
			if (err == nil) != (mode == "正确") {
				t.Fatal(mode, err)
			}
		})
	}
}

func TestLocalDisconnectRecordsUnknownWithoutReplay(t *testing.T) {
	parent, child := net.Pipe()
	defer parent.Close()
	grant := localGrant()
	grant.TimeoutMS = 1000
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := ReadFrame(child); done <- errors.Join(err, child.Close()) }()
	commits := 0
	relay := localRelayStub{prepare: func(context.Context, string, ToolCall) (ToolPreparation, error) {
		return ToolPreparation{Grant: &grant}, nil
	}, complete: func(ctx context.Context, _ ToolGrant, report ToolReport) (ToolResult, error) {
		commits++
		if ctx.Err() != nil || report.Failure != "TOOL_RESULT_UNKNOWN" {
			t.Error("断连未留下未知回执")
		}
		return report.Result, ErrProtocol
	}}
	sequence := 2
	if _, err := dispatchLocal(ctx, parent, parent.Close, &sequence, "tool.1.1", toolOutput().Tools[0], relay, 4096); err == nil || commits != 1 {
		t.Fatal(commits, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
