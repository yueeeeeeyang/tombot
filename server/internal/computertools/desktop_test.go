package computertools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/png"
	"slices"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

type desktopStub struct {
	starts, reads, inputs int
	pixels                []byte
	state                 string
	corrupt, uncertain    bool
	last                  computer.DesktopInput
}

func (runtime *desktopStub) StartDesktop(_ context.Context, session computer.ExecutionSession) (computer.DesktopStatus, error) {
	runtime.starts++
	return runtime.InspectDesktop(context.Background(), session)
}
func (runtime *desktopStub) InspectDesktop(_ context.Context, session computer.ExecutionSession) (computer.DesktopStatus, error) {
	status := computer.DesktopStatus{Session: session, State: runtime.state}
	if status.State == "" {
		status.State = "ready"
	}
	if status.State == "ready" {
		status.Width, status.Height = 1280, 800
	}
	return status, nil
}
func (runtime *desktopStub) CaptureDesktop(_ context.Context, request computer.DesktopCapture) (computer.DesktopImage, error) {
	digest := sha256.Sum256(runtime.pixels)
	return computer.DesktopImage{DesktopCapture: request, Width: 1280, Height: 800, MediaType: "image/png", Size: len(runtime.pixels), SHA256: hex.EncodeToString(digest[:])}, nil
}
func (runtime *desktopStub) ReadDesktopImage(ctx context.Context, request computer.DesktopImageRead) (computer.DesktopImageChunk, error) {
	runtime.reads++
	metadata, err := runtime.CaptureDesktop(ctx, request.DesktopCapture)
	if err != nil {
		return computer.DesktopImageChunk{}, err
	}
	pixels := slices.Clone(runtime.pixels[request.Offset:min(request.Offset+request.MaximumBytes, len(runtime.pixels))])
	if runtime.corrupt && len(pixels) > 0 {
		pixels[0] ^= 1
	}
	return computer.DesktopImageChunk{Image: metadata, Offset: request.Offset, Bytes: pixels}, nil
}
func (runtime *desktopStub) InputDesktop(_ context.Context, request computer.DesktopInput) (computer.DesktopInputReceipt, error) {
	runtime.inputs++
	runtime.last = request
	state := "sent"
	if runtime.uncertain {
		state = "uncertain"
	}
	return computer.DesktopInputReceipt{Session: request.Session, Sequence: request.Sequence, State: state}, nil
}

func testDesktopPixels(t *testing.T) []byte {
	t.Helper()
	frame := image.NewRGBA(image.Rect(0, 0, 1280, 800))
	var noise uint32 = 123
	for index := 0; index < len(frame.Pix); index += 4 {
		noise = noise*1664525 + 1013904223
		frame.Pix[index] = byte(noise)
		frame.Pix[index+1] = byte(noise >> 8)
		frame.Pix[index+2] = byte(noise >> 16)
		frame.Pix[index+3] = 255
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, frame); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}
func testDesktopTool(t *testing.T, runtime computer.DesktopRuntime) *Desktop {
	t.Helper()
	tool, err := NewDesktop(runtime, DesktopOptions{CallTimeout: time.Second, PollInterval: 10 * time.Millisecond, StartTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func TestDesktopScreenshotReassemblesOwnedPixels(t *testing.T) {
	session := shellAddress().Session
	runtime := &desktopStub{pixels: testDesktopPixels(t)}
	tool := testDesktopTool(t, runtime)
	operation := ids.New()
	result, err := tool.Execute(context.Background(), session, operation, 7, "computer_screenshot", "{}", "standard")
	if err != nil || result.IsError || result.Artifact == nil || result.Artifact.ID != operation || result.Artifact.BotID != session.BotID || !bytes.Equal(result.Artifact.Content, runtime.pixels) || runtime.reads < 2 {
		t.Fatal("截图未通过多分块返回实际像素", err)
	}
	runtime.corrupt = true
	if _, err := tool.Execute(context.Background(), session, ids.New(), 9, "computer_screenshot", "{}", "standard"); err == nil {
		t.Fatal("接受摘要不符的截图")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.Execute(ctx, session, ids.New(), 10, "computer_screenshot", "{}", "standard"); err == nil {
		t.Fatal("取消后截图仍成功")
	}
}

func TestDesktopActionsAndStrictArguments(t *testing.T) {
	session := shellAddress().Session
	for _, arguments := range []string{`{"action":"click","x":0,"y":799}`, `{"action":"double_click","x":1279,"y":0,"button":"right"}`, `{"action":"drag","x":10,"y":20,"to_x":30,"to_y":40}`, `{"action":"scroll","x":5,"y":6,"direction":"down","amount":20}`, `{"action":"move","x":20,"y":30}`, `{"action":"type","text":"中文\n"}`, `{"action":"key","keys":["CTRL","L"]}`} {
		runtime := &desktopStub{}
		result, err := testDesktopTool(t, runtime).Execute(context.Background(), session, ids.New(), 12, "computer_input", arguments, "standard")
		if err != nil || result.IsError || runtime.inputs != 1 || runtime.last.Validate() != nil {
			t.Fatal(arguments, result, err)
		}
	}
	for _, arguments := range []string{`null`, `{} {}`, `{"action":"move","x":0,"x":1,"y":0}`, `{"Action":"move","x":0,"y":0}`, `{"action":"click","x":1280,"y":0}`, `{"action":"click","x":null,"y":0}`, `{"action":"click","x":0,"y":0,"text":"unexpected"}`, `{"action":"scroll","x":0,"y":0,"amount":21,"direction":"down"}`, `{"action":"key","keys":["CTRL","CTRL"]}`, `{"action":"key","keys":["F13"]}`, `{"action":"type","text":"\u0000"}`, `{"action":"type","text":"` + strings.Repeat("中", 257) + `"}`} {
		runtime := &desktopStub{}
		result, err := testDesktopTool(t, runtime).Execute(context.Background(), session, ids.New(), 13, "computer_input", arguments, "standard")
		if err != nil || !result.IsError || runtime.inputs != 0 || runtime.starts != 0 {
			t.Fatal("无效参数进入桌面", arguments, result, err)
		}
	}
	runtime := &desktopStub{uncertain: true}
	if _, err := testDesktopTool(t, runtime).Execute(context.Background(), session, ids.New(), 14, "computer_input", `{"action":"type","text":"abc"}`, "standard"); err == nil || runtime.inputs != 1 {
		t.Fatal("未知输入被重试或报告成功")
	}
	runtime = &desktopStub{state: "starting"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := testDesktopTool(t, runtime).Execute(ctx, session, ids.New(), 15, "computer_screenshot", "{}", "standard"); !errors.Is(err, context.DeadlineExceeded) || runtime.reads != 0 {
		t.Fatal("未就绪桌面被截图", err)
	}
}

type cancelledDesktop struct {
	desktopStub
	stage  string
	cancel context.CancelFunc
}

func (runtime *cancelledDesktop) StartDesktop(ctx context.Context, session computer.ExecutionSession) (computer.DesktopStatus, error) {
	if runtime.stage == "start" {
		runtime.cancel()
		<-ctx.Done()
	}
	return runtime.desktopStub.StartDesktop(ctx, session)
}
func (runtime *cancelledDesktop) InspectDesktop(ctx context.Context, session computer.ExecutionSession) (computer.DesktopStatus, error) {
	if runtime.stage == "status" {
		runtime.cancel()
		<-ctx.Done()
	}
	return runtime.desktopStub.InspectDesktop(ctx, session)
}

func TestDesktopReadinessPreservesCancellation(t *testing.T) {
	for _, stage := range []string{"start", "status"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			runtime := &cancelledDesktop{desktopStub: desktopStub{state: "starting"}, stage: stage, cancel: cancel}
			_, err := testDesktopTool(t, runtime).Execute(ctx, shellAddress().Session, ids.New(), 1, "computer_screenshot", "{}", "standard")
			if !errors.Is(err, context.Canceled) || runtime.reads != 0 {
				t.Fatal("状态调用期间的取消原因丢失或仍读取画面", err)
			}
		})
	}
}
