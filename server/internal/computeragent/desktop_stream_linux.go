//go:build linux

package computeragent

import (
	"context"
	"errors"
	"net"
	"os"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/desktopstream"
)

// OpenDesktopStreamFile 返回受代理持有的转发端，绝不把原始 VNC 描述符交出，租约撤销即断流。
func (scope *kernelScope) OpenDesktopStreamFile(ctx context.Context, request computer.DesktopStream) (result *os.File, failure error) {
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if request.Validate() != nil || scope.closed || scope.revoked || scope.drained || scope.directory == nil || scope.directory.session != request.Session || scope.desktop == nil || scope.desktop.snapshot().State != "ready" {
		return nil, errors.New("桌面观看没有有效会话")
	}
	for id, stream := range scope.streams {
		select {
		case <-stream.Done():
			delete(scope.streams, id)
		default:
		}
	}
	if len(scope.streams) >= 4 {
		return nil, errors.New("桌面观看连接已达上限")
	}
	if previous := scope.streams[request.ViewerID]; previous != nil {
		if err := previous.Close(); err != nil {
			return nil, err
		}
		delete(scope.streams, request.ViewerID)
	}
	upstream, err := openDesktopConnection(ctx, request.Session, scope.desktopGroup)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result == nil {
			failure = errors.Join(failure, upstream.Close())
		}
	}()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	localFile, remoteFile := os.NewFile(uintptr(pair[0]), "desktop-relay"), os.NewFile(uintptr(pair[1]), "desktop-viewer")
	defer func() {
		if result == nil {
			failure = errors.Join(failure, remoteFile.Close())
		}
	}()
	local, err := net.FileConn(localFile)
	closeError := localFile.Close()
	if err != nil || closeError != nil {
		if local != nil {
			closeError = errors.Join(closeError, local.Close())
		}
		return nil, errors.Join(err, closeError)
	}
	relay := desktopstream.New(local, upstream)
	if scope.isPaused && scope.consoleFailure == nil && scope.consoleViewer == request.ViewerID {
		if err := relay.SetControl(true); err != nil {
			return nil, errors.Join(err, relay.Close())
		}
	}
	if scope.streams == nil {
		scope.streams = map[string]*desktopstream.Relay{}
	}
	scope.streams[request.ViewerID] = relay
	go func() {
		if err := relay.Run(); err != nil {
			scope.logger.Warn("桌面观看连接已中断", "bot_id", request.Session.BotID, "console_id", request.ViewerID, "原因", err)
		}
	}()
	return remoteFile, nil
}
