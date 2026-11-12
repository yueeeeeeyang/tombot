package dockernode

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

type fakeGraphicsRuntime struct{ *fakeTerminalRuntime }

func (runtime *fakeGraphicsRuntime) StartDesktop(ctx context.Context, session computer.ExecutionSession) (computer.DesktopStatus, error) {
	return computer.DesktopStatus{Session: session, State: "starting"}, runtime.call(ctx)
}
func (runtime *fakeGraphicsRuntime) InspectDesktop(ctx context.Context, session computer.ExecutionSession) (computer.DesktopStatus, error) {
	return computer.DesktopStatus{Session: session, State: "ready", Width: 1280, Height: 800}, runtime.call(ctx)
}
func (runtime *fakeGraphicsRuntime) CaptureDesktop(ctx context.Context, request computer.DesktopCapture) (computer.DesktopImage, error) {
	return computer.DesktopImage{DesktopCapture: request, Width: 1280, Height: 800, MediaType: "image/png", Size: 10, SHA256: strings.Repeat("a", 64)}, runtime.call(ctx)
}
func (runtime *fakeGraphicsRuntime) ReadDesktopImage(ctx context.Context, request computer.DesktopImageRead) (computer.DesktopImageChunk, error) {
	image := computer.DesktopImage{DesktopCapture: request.DesktopCapture, Width: 1280, Height: 800, MediaType: "image/png", Size: 10, SHA256: strings.Repeat("a", 64)}
	return computer.DesktopImageChunk{Image: image, Offset: request.Offset, Bytes: make([]byte, min(request.MaximumBytes, image.Size-request.Offset))}, runtime.call(ctx)
}

func TestNodeDesktopIOAuthorization(t *testing.T) {
	fixture := newFixture(t)
	session := fixture.prepareComputer(t, fixture.specification)
	controller := fixture.sessionController(t, &fakeGroups{path: t.TempDir()}, sessionOptions())
	authority := &fakeGraphicsAuthority{fakeAuthority: controller.agent.(*fakeAuthority), runtime: &fakeGraphicsRuntime{&fakeTerminalRuntime{}}}
	controller.agent = authority
	ctx := context.Background()
	calls := []func(computer.ExecutionSession) error{
		func(session computer.ExecutionSession) error {
			request := computer.DesktopCapture{Session: session, Sequence: 1}
			result, err := controller.CaptureDesktop(ctx, request)
			if err != nil {
				return err
			}
			return result.Validate(request)
		},
		func(session computer.ExecutionSession) error {
			request := computer.DesktopImageRead{DesktopCapture: computer.DesktopCapture{Session: session, Sequence: 1}, MaximumBytes: 10}
			result, err := controller.ReadDesktopImage(ctx, request)
			if err != nil {
				return err
			}
			return result.Validate(request)
		},
		func(session computer.ExecutionSession) error {
			request := computer.DesktopInput{Session: session, Sequence: 1, Events: []computer.DesktopEvent{{Kind: "pointer"}}}
			result, err := controller.InputDesktop(ctx, request)
			if err != nil {
				return err
			}
			return result.Validate(request)
		},
	}
	for _, call := range calls {
		if err := call(session); err == nil {
			t.Fatal("未授权桌面输入输出被接受")
		}
	}
	if _, err := controller.Open(ctx, commandFor(session)); err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		wrong := session
		wrong.FencingToken++
		if err := call(wrong); err == nil {
			t.Fatal("错误代次被接受")
		}
		if err := call(session); err != nil {
			t.Fatal(err)
		}
	}
	if authority.connections != 3 {
		t.Fatal("拒绝动作仍连接代理")
	}
	lease, _ := controller.lease(sessionKeyOf(session))
	if authority.runtime.deadline.After(lease.deadline) {
		t.Fatal("桌面输入输出延长授权")
	}
	authority.runtime.fail = true
	if err := calls[2](session); err == nil {
		t.Fatal("未确认输入被当作成功")
	}
	if authority.runtime.calls != 4 {
		t.Fatal("未知输入被自动重试")
	}
	lease.deadline = time.Now().Add(-time.Second)
	controller.remember(sessionKeyOf(session), lease)
	for _, call := range calls {
		if err := call(session); err == nil {
			t.Fatal("过期后仍操作桌面")
		}
	}
	if authority.connections != 4 {
		t.Fatal("过期动作仍连接代理")
	}
}
func (runtime *fakeGraphicsRuntime) InputDesktop(ctx context.Context, request computer.DesktopInput) (computer.DesktopInputReceipt, error) {
	return computer.DesktopInputReceipt{Session: request.Session, Sequence: request.Sequence, State: "sent"}, runtime.call(ctx)
}

type fakeGraphicsAuthority struct {
	*fakeAuthority
	runtime     *fakeGraphicsRuntime
	connections int
}

func (authority *fakeGraphicsAuthority) desktopClient(_ context.Context, command computer.SessionCommand, reference *processgroup.Reference) (computer.DesktopRuntime, error) {
	if reference == nil || reference.Session != command.Session {
		return nil, errors.New("测试图形资源组身份不符")
	}
	authority.connections++
	return authority.runtime, nil
}

func TestNodeGraphicalSessionAuthorization(t *testing.T) {
	fixture := newFixture(t)
	session := fixture.prepareComputer(t, fixture.specification)
	controller := fixture.sessionController(t, &fakeGroups{path: t.TempDir()}, sessionOptions())
	authority := &fakeGraphicsAuthority{fakeAuthority: controller.agent.(*fakeAuthority), runtime: &fakeGraphicsRuntime{&fakeTerminalRuntime{}}}
	controller.agent = authority
	ctx := context.Background()
	if _, err := controller.StartDesktop(ctx, session); err == nil {
		t.Fatal("未取得执行权即可启动桌面")
	}
	if _, err := controller.Open(ctx, commandFor(session)); err != nil {
		t.Fatal(err)
	}
	status, err := controller.StartDesktop(ctx, session)
	if err != nil || status.State != "starting" {
		t.Fatal(status, err)
	}
	status, err = controller.InspectDesktop(ctx, session)
	if err != nil || status.State != "ready" {
		t.Fatal(status, err)
	}
	lease, _ := controller.lease(sessionKeyOf(session))
	if authority.runtime.deadline.After(lease.deadline) {
		t.Fatal("图形调用延长了执行期限")
	}
	wrong := session
	wrong.FencingToken++
	if _, err := controller.InspectDesktop(ctx, wrong); err == nil {
		t.Fatal("旧控制代次读取了桌面")
	}
	if authority.connections != 2 {
		t.Fatal("无效身份仍连接了图形代理")
	}
	authority.runtime.fail = true
	if _, err := controller.StartDesktop(ctx, session); err == nil {
		t.Fatal("未知图形启动被报告为成功")
	}
	if authority.runtime.calls != 3 {
		t.Fatal("未知启动被自动重放")
	}
	lease.deadline = time.Now().Add(-time.Second)
	controller.remember(sessionKeyOf(session), lease)
	if _, err := controller.InspectDesktop(ctx, session); err == nil {
		t.Fatal("过期授权仍查询图形会话")
	}
	if authority.connections != 3 {
		t.Fatal("过期授权仍连接图形代理")
	}
}
