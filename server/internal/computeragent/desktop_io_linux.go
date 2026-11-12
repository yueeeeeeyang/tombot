//go:build linux

package computeragent

import (
	"context"
	"errors"

	"io.yueyang/tom/internal/computer"
)

func (scope *kernelScope) desktopAccess(session computer.ExecutionSession) (*desktopAccess, error) {
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if session.Validate() != nil || scope.closed || scope.revoked || scope.drained || scope.directory == nil || scope.directory.session != session || scope.desktop == nil || scope.desktop.access == nil || scope.desktop.snapshot().State != "ready" {
		return nil, errors.New("桌面输入输出没有就绪的授权会话")
	}
	return scope.desktop.access, nil
}

func (scope *kernelScope) CaptureDesktop(ctx context.Context, request computer.DesktopCapture) (computer.DesktopImage, error) {
	access, err := scope.desktopAccess(request.Session)
	if err != nil {
		return computer.DesktopImage{}, err
	}
	return access.capture(ctx, request)
}

func (scope *kernelScope) ReadDesktopImage(ctx context.Context, request computer.DesktopImageRead) (computer.DesktopImageChunk, error) {
	access, err := scope.desktopAccess(request.Session)
	if err != nil {
		return computer.DesktopImageChunk{}, err
	}
	return access.read(ctx, request)
}

func (scope *kernelScope) InputDesktop(ctx context.Context, request computer.DesktopInput) (computer.DesktopInputReceipt, error) {
	access, err := scope.desktopAccess(request.Session)
	if err != nil {
		return computer.DesktopInputReceipt{}, err
	}
	return access.send(ctx, request)
}
