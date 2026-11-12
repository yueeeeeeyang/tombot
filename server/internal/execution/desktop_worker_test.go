package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computertools"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

// workerDesktop 是可核对持久记账顺序的节点替身；真实 X11 在 Linux 集成测试覆盖。
type workerDesktop struct {
	t                *testing.T
	fixture          fixture
	pixels           []byte
	captures, inputs int
	previous         int64
	uncertain        bool
}

func (runtime *workerDesktop) StartDesktop(_ context.Context, session computer.ExecutionSession) (computer.DesktopStatus, error) {
	return computer.DesktopStatus{Session: session, State: "ready", Width: 1280, Height: 800}, nil
}
func (runtime *workerDesktop) InspectDesktop(ctx context.Context, session computer.ExecutionSession) (computer.DesktopStatus, error) {
	return runtime.StartDesktop(ctx, session)
}
func (runtime *workerDesktop) check(session computer.ExecutionSession, sequence int64) {
	runtime.t.Helper()
	var count int
	err := runtime.fixture.pool.QueryRow(`SELECT count(*) FROM tombot.run_operations WHERE user_id=$1 AND run_id=$2 AND sequence=$3 AND computer_session_id=$4 AND kind='computer' AND status='started'`, session.UserID, session.RunID, sequence, session.SessionID).Scan(&count)
	if err != nil || count != 1 || sequence <= runtime.previous {
		runtime.t.Fatal("桌面动作先于持久记账或序号失序", err)
	}
	runtime.previous = sequence
}
func (runtime *workerDesktop) metadata(request computer.DesktopCapture) computer.DesktopImage {
	digest := sha256.Sum256(runtime.pixels)
	return computer.DesktopImage{DesktopCapture: request, Width: 1280, Height: 800, MediaType: "image/png", Size: len(runtime.pixels), SHA256: hex.EncodeToString(digest[:])}
}
func (runtime *workerDesktop) CaptureDesktop(_ context.Context, request computer.DesktopCapture) (computer.DesktopImage, error) {
	runtime.check(request.Session, request.Sequence)
	runtime.captures++
	return runtime.metadata(request), nil
}
func (runtime *workerDesktop) ReadDesktopImage(_ context.Context, request computer.DesktopImageRead) (computer.DesktopImageChunk, error) {
	return computer.DesktopImageChunk{Image: runtime.metadata(request.DesktopCapture), Offset: request.Offset, Bytes: bytes.Clone(runtime.pixels[request.Offset:min(len(runtime.pixels), request.Offset+request.MaximumBytes)])}, nil
}
func (runtime *workerDesktop) InputDesktop(_ context.Context, request computer.DesktopInput) (computer.DesktopInputReceipt, error) {
	runtime.check(request.Session, request.Sequence)
	runtime.inputs++
	state := "sent"
	if runtime.uncertain {
		state = "uncertain"
	}
	return computer.DesktopInputReceipt{Session: request.Session, Sequence: request.Sequence, State: state}, nil
}

func (fixture fixture) visionModel(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var providerID string
	if err := fixture.pool.QueryRow(`SELECT provider_id FROM tombot.models WHERE id=$1`, fixture.modelID).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	model, err := fixture.registry.CreateModel(ctx, fixture.actor, ids.New(), modelregistry.ModelRequest{ProviderID: providerID, ModelKey: "execution-vision", DisplayName: "视觉工具模型", ContextLimit: 4096, OutputLimit: 1024, SupportsTools: true, SupportsVision: true})
	if err != nil {
		t.Fatal(err)
	}
	version, err := fixture.registry.CreateDraft(ctx, fixture.actor, ids.New(), modelregistry.DraftRequest{PolicyID: fixture.policyID, ModelID: model.ID, OutputLimit: 256})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.registry.Publish(ctx, fixture.actor, ids.New(), fixture.policyID, version.Number, fixture.revision(t)); err != nil {
		t.Fatal(err)
	}
}

func emitDesktopTool(emit func(provider.Event) error, step int, name, arguments string) (modelgateway.Result, error) {
	id := fmt.Sprintf("desktop-%d", step)
	for _, event := range []provider.Event{{Kind: "tool_use_start", Index: 0, ToolCallID: id, ToolName: name}, {Kind: "tool_use_delta", Index: 0, ToolCallID: id, InputDelta: arguments}, {Kind: "tool_use_end", Index: 0, ToolCallID: id, ToolName: name, InputDelta: arguments}, {Kind: "stop", StopReason: "tool_calls"}} {
		if err := emit(event); err != nil {
			return modelgateway.Result{}, err
		}
	}
	return modelgateway.Result{IsComplete: true, Usage: reported(10, 2)}, nil
}

func TestDesktopWorkerVisionLoop(t *testing.T) {
	cluster := pgtest.Start(t)
	for _, mode := range []string{"正常循环", "容量耗尽", "不支持视觉", "输入结果未知"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newFixture(t, cluster)
			fixture.service.options.MaxOperations = 16
			if mode == "不支持视觉" {
				fixture.toolsModel(t)
			} else {
				fixture.visionModel(t)
			}
			if mode == "容量耗尽" {
				fixture.service.options.Images.MaxUserImages = 1
			}
			computers := fixture.computers(t, &sessionNode{}, computerOptions())
			fixture.send(t, fixture.a, fixture.desktopBot(t, fixture.first))
			lease := fixture.claim(t)
			runtime := &workerDesktop{t: t, fixture: fixture, pixels: screenshotPixels(t), uncertain: mode == "输入结果未知"}
			tools, err := NewComputerDesktop(runtime, computertools.DesktopOptions{CallTimeout: time.Second, PollInterval: 10 * time.Millisecond, StartTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			worker := fixture.workerWithComputers(t, callerFunc(func(_ context.Context, configuration modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
				calls++
				if !configuration.SupportsVision {
					t.Fatal("非视觉模型被调用")
				}
				images := 0
				for _, message := range input.Messages {
					for _, block := range message.Blocks {
						if block.ToolResult != nil && block.ToolResult.Artifact != nil {
							images++
							if !bytes.Equal(block.ToolResult.Artifact.Content, runtime.pixels) {
								t.Fatal("模型没有收到真实像素")
							}
						}
					}
				}
				wantImages := 1
				if calls == 1 {
					wantImages = 0
				}
				if calls == 5 && mode != "容量耗尽" {
					wantImages = 2
				}
				if images != wantImages {
					t.Fatalf("视觉上下文数量错误: %d", images)
				}
				switch calls {
				case 1, 4:
					return emitDesktopTool(emit, calls, "computer_screenshot", "{}")
				case 2:
					return emitDesktopTool(emit, calls, "computer_input", `{"action":"click","x":1280,"y":0}`)
				case 3:
					return emitDesktopTool(emit, calls, "computer_input", `{"action":"type","text":"中文"}`)
				default:
					if mode == "容量耗尽" && !input.Messages[len(input.Messages)-1].Blocks[0].ToolResult.IsError {
						t.Fatal("截图配额拒绝伪装成功")
					}
					return emitText(emit, "已完成桌面检查。")
				}
			}), computers, tools...)
			worker.options.MaxModelSteps = 6
			run, err := worker.Process(context.Background(), lease)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "不支持视觉":
				if run.Status == "succeeded" || calls != 0 || runtime.captures != 0 || runtime.inputs != 0 {
					t.Fatal("视觉能力缺失仍执行桌面任务")
				}
			case "输入结果未知":
				if run.Status == "succeeded" || calls != 3 || runtime.inputs != 1 || runtime.captures != 1 {
					t.Fatal("未知输入后继续或重放了操作")
				}
			default:
				if run.Status != "succeeded" || calls != 5 || runtime.inputs != 1 || runtime.captures != 2 {
					t.Fatal("桌面工具循环未完成", run.Status, calls, runtime.inputs, runtime.captures)
				}
			}
			if fixture.sessionState(t, lease) != "released" {
				t.Fatal("桌面未回收")
			}
			var inline bool
			if err := fixture.pool.QueryRow(`SELECT EXISTS(SELECT 1 FROM tombot.run_operations WHERE run_id=$1 AND result::text LIKE '%iVBOR%')`, lease.RunID).Scan(&inline); err != nil || inline {
				t.Fatal("操作日志包含 PNG 像素", err)
			}
			var screenshotCount int
			if err := fixture.pool.QueryRow(`SELECT count(*) FROM tombot.run_screenshots WHERE run_id=$1`, lease.RunID).Scan(&screenshotCount); err != nil {
				t.Fatal(err)
			}
			want := 1
			if mode == "正常循环" {
				want = 2
			}
			if mode == "不支持视觉" {
				want = 0
			}
			if screenshotCount != want {
				t.Fatal("截图持久数量错误", screenshotCount)
			}
			var encoded json.RawMessage
			if mode == "容量耗尽" {
				if err := fixture.pool.QueryRow(`SELECT result FROM tombot.run_operations WHERE run_id=$1 AND error_code='SCREENSHOT_CAPACITY'`, lease.RunID).Scan(&encoded); err != nil || bytes.Contains(encoded, []byte(`"artifact"`)) {
					t.Fatal("容量拒绝留下无效图片引用", err)
				}
			}
		})
	}
}
