//go:build linux

package sharedbrowser

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/computer"
)

// NewNativeDesktop 只供容器代理在固定浏览器服务运行目录中构造，不接受来自模型或用户的控制路径。
func NewNativeDesktop(runtime string) NativeDesktop {
	return nativeBrowser{socket: filepath.Join(runtime, "native.sock")}
}

type nativeBrowser struct{ socket string }

type nativeBrowserRequest struct {
	Action        string   `json:"action"`
	Owner         string   `json:"owner"`
	Window        int64    `json:"window,omitempty"`
	Display       int      `json:"display,omitempty"`
	Authorization []byte   `json:"authorization,omitempty"`
	Bounds        [4]int   `json:"bounds,omitempty"`
	Command       *Command `json:"command,omitempty"`
	Paused        bool     `json:"paused,omitempty"`
}

func (native nativeBrowser) HumanControl(ctx context.Context, session computer.ExecutionSession, paused bool) error {
	receipt, err := native.call(ctx, nativeBrowserRequest{Action: "control", Owner: session.SessionID, Paused: paused})
	if err == nil && receipt.State != "ready" {
		err = ErrUnknown
	}
	return err
}

type nativeBrowserReceipt struct {
	State string `json:"state"`
	PNG   []byte `json:"png,omitempty"`
	X     int    `json:"x,omitempty"`
	Y     int    `json:"y,omitempty"`
}

// call 只连接固定服务的私有套接字，复验对端 UID；非幂等输入失败后不重放。
func (native nativeBrowser) call(ctx context.Context, request nativeBrowserRequest) (receipt nativeBrowserReceipt, failure error) {
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(bounded, "unix", native.socket)
	if err != nil {
		return receipt, errors.New("原生浏览器控制入口不可用")
	}
	defer func() { failure = errors.Join(failure, connection.Close()) }()
	peer, err := connection.(*net.UnixConn).SyscallConn()
	var identity *unix.Ucred
	if err == nil {
		err = peer.Control(func(fd uintptr) { identity, failure = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	}
	if err != nil || failure != nil || identity == nil || identity.Uid != computer.BrowserUID || identity.Gid != computer.DesktopSharedGID {
		return receipt, errors.New("原生浏览器对端身份无效")
	}
	deadline, _ := bounded.Deadline()
	if err = connection.SetDeadline(deadline); err != nil {
		return receipt, err
	}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > 32767 {
		return receipt, errors.New("原生浏览器请求超过限制")
	}
	if _, err = io.Copy(connection, bytes.NewReader(append(encoded, '\n'))); err != nil {
		return receipt, errors.New("原生浏览器请求结果未确认")
	}
	response, err := bufio.NewReader(io.LimitReader(connection, 6<<20+1)).ReadBytes('\n')
	if err != nil || len(response) > 6<<20 || json.Unmarshal(response, &receipt) != nil || receipt.State != "ready" && receipt.State != "starting" && receipt.State != "closed" {
		return receipt, errors.New("原生浏览器操作失败或回执无效")
	}
	return receipt, bounded.Err()
}

func desktopAuthorization(session computer.ExecutionSession) (_ []byte, failure error) {
	if session.Validate() != nil {
		return nil, errors.New("桌面授权无效")
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, filepath.Join(session.RuntimeDirectory(), "Xauthority"), &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK, Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
	if err != nil {
		return nil, errors.New("桌面显示认证不可用")
	}
	file := os.NewFile(uintptr(fd), "desktop-authorization")
	defer func() { failure = errors.Join(failure, file.Close()) }()
	var identity unix.Stat_t
	if unix.Fstat(fd, &identity) != nil || identity.Uid != uint32(session.UID) || identity.Gid != uint32(session.GID) || identity.Mode&unix.S_IFMT != unix.S_IFREG || identity.Mode&0777 != 0600 || identity.Size < 1 || identity.Size > 4096 || identity.Nlink != 1 {
		return nil, errors.New("桌面显示认证身份或权限无效")
	}
	return io.ReadAll(io.LimitReader(file, 4097))
}

func (native nativeBrowser) Bind(ctx context.Context, session computer.ExecutionSession, window int64, bounds [4]int) error {
	authorization, err := desktopAuthorization(session)
	if err != nil {
		return err
	}
	defer clear(authorization)
	request := nativeBrowserRequest{Action: "bind", Owner: session.SessionID, Window: window, Bounds: bounds, Display: session.DisplayNumber, Authorization: authorization}
	timer := time.NewTicker(50 * time.Millisecond)
	defer timer.Stop()
	for {
		receipt, err := native.call(ctx, request)
		if err != nil {
			return err
		}
		if receipt.State == "ready" {
			return nil
		}
		if receipt.State != "starting" {
			return errors.New("原生窗口绑定回执无效")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func (native nativeBrowser) Execute(ctx context.Context, session computer.ExecutionSession, window int64, command Command) error {
	if err := command.Validate(computer.DesktopWidth, computer.DesktopHeight); err != nil {
		return err
	}
	receipt, err := native.call(ctx, nativeBrowserRequest{Action: "input", Owner: session.SessionID, Window: window, Command: &command})
	if err == nil && receipt.State != "ready" {
		err = errors.New("原生输入结果未确认")
	}
	return err
}
func (native nativeBrowser) Screenshot(ctx context.Context, session computer.ExecutionSession, window int64) ([]byte, error) {
	receipt, err := native.call(ctx, nativeBrowserRequest{Action: "capture", Owner: session.SessionID, Window: window})
	if err == nil && (receipt.State != "ready" || len(receipt.PNG) == 0) {
		err = errors.New("原生桌面截图未确认")
	}
	return receipt.PNG, err
}
func (native nativeBrowser) Release(ctx context.Context, session computer.ExecutionSession) error {
	receipt, err := native.call(ctx, nativeBrowserRequest{Action: "release", Owner: session.SessionID})
	if err == nil && receipt.State != "closed" {
		err = errors.New("原生桌面窗口未确认回收")
	}
	return err
}
