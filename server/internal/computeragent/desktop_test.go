package computeragent

import (
	"context"
	"errors"
	"os"
	"testing"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

type recordingDesktopScope struct {
	*fakeScope
	status    computer.DesktopStatus
	starts    int
	onFailure func(string)
}

func (scope *recordingDesktopScope) StartDesktop(_ context.Context, reference processgroup.Reference, session computer.ExecutionSession, membership, revocation *os.File, onFailure func(string)) (computer.DesktopStatus, error) {
	if reference.Session != session || membership == nil || revocation == nil || onFailure == nil {
		return computer.DesktopStatus{}, errors.New("测试图形会话授权缺失")
	}
	scope.starts++
	scope.onFailure = onFailure
	scope.status = computer.DesktopStatus{Session: session, State: "starting"}
	return scope.status, nil
}
func (scope *recordingDesktopScope) InspectDesktop(_ context.Context, session computer.ExecutionSession) (computer.DesktopStatus, error) {
	if scope.status.State == "" {
		return computer.DesktopStatus{Session: session, State: "absent"}, nil
	}
	if scope.status.Session != session {
		return computer.DesktopStatus{}, errors.New("测试图形会话身份不符")
	}
	return scope.status, nil
}

func TestDesktopFailureRevokesOnlyItsSession(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	if _, err := fixture.controller.Open(ctx, fixture.command, fixture.reference, fixture.files(t)); err != nil {
		t.Fatal(err)
	}
	scope := &recordingDesktopScope{fakeScope: fixture.factory.scopes[0]}
	fixture.guard.scope = scope
	status, err := fixture.controller.InspectDesktop(ctx, fixture.command.Session)
	if err != nil || status.State != "absent" {
		t.Fatal(status, err)
	}
	status, err = fixture.controller.StartDesktop(ctx, fixture.command.Session)
	if err != nil || status.State != "starting" || scope.starts != 1 {
		t.Fatal(status, err)
	}
	second, reference := desktopCommand(fixture.command, 2)
	if _, err := fixture.controller.Open(ctx, second, reference, fixture.files(t)); err != nil {
		t.Fatal(err)
	}
	wrong := fixture.command.Session
	wrong.FencingToken++
	if _, err := fixture.controller.StartDesktop(ctx, wrong); err == nil {
		t.Fatal("旧执行权启动了图形会话")
	}
	if scope.starts != 1 {
		t.Fatal("拒绝请求仍进入了启动器")
	}
	scope.onFailure("desktop_exited")
	if fixture.guard.permit.active(fixture.command.Revision) {
		t.Fatal("桌面失败没有撤销该桌面授权")
	}
	if _, err := fixture.controller.InspectDesktop(ctx, fixture.command.Session); err == nil {
		t.Fatal("撤销后仍返回图形会话")
	}
	second.Revision++
	if _, err := fixture.controller.Renew(ctx, second); err != nil {
		t.Fatal("其他桌面受到图形失败影响", err)
	}
	select {
	case <-fixture.controller.fatal:
		t.Fatal("单桌面失败升级成了整机故障")
	default:
	}
}

func TestDesktopStatusValidation(t *testing.T) {
	fixture := newFixture(t)
	valid := computer.DesktopStatus{Session: fixture.command.Session, State: "ready", Width: 1280, Height: 800}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*computer.DesktopStatus){func(s *computer.DesktopStatus) { s.Width = 1920 }, func(s *computer.DesktopStatus) { s.State = "starting" }, func(s *computer.DesktopStatus) { s.ErrorCode = "secret" }, func(s *computer.DesktopStatus) { s.Session.UID++ }, func(s *computer.DesktopStatus) { s.State = "unknown" }} {
		wrong := valid
		mutate(&wrong)
		if wrong.Validate() == nil {
			t.Fatal("接受了无效桌面状态")
		}
	}
}
