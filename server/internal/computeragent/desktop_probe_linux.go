//go:build linux

package computeragent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/computer"
)

type desktopReady struct {
	Display  int    `json:"display"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Protocol string `json:"protocol"`
}

// readyFile 仅把原子发布的就绪标记作为探测条件，不把用户可写文件当成连接授权。
func (directory *desktopDirectory) readyFile() (ready bool, failure error) {
	file, err := directory.root.OpenFile(directory.session.SessionID+"/ready.json", os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("读取桌面就绪标记失败")
	}
	defer func() {
		if file.Close() != nil {
			failure = errors.Join(failure, errors.New("关闭桌面就绪标记失败"))
		}
	}()
	var identity unix.Stat_t
	if unix.Fstat(int(file.Fd()), &identity) != nil || identity.Mode&unix.S_IFMT != unix.S_IFREG || identity.Mode&07777 != 0600 || identity.Uid != uint32(directory.session.UID) || identity.Gid != uint32(directory.session.GID) || identity.Size < 1 || identity.Size > 1024 {
		return false, errors.New("桌面就绪标记身份或类型无效")
	}
	encoded, err := io.ReadAll(io.LimitReader(file, 1025))
	if err != nil || len(encoded) > 1024 {
		return false, errors.New("桌面就绪标记读取失败或超过限制")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var record desktopReady
	if decoder.Decode(&record) != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) || record.Display != directory.session.DisplayNumber || record.Width != computer.DesktopWidth || record.Height != computer.DesktopHeight || record.Protocol != "x11" {
		return false, errors.New("桌面就绪标记与授权不符")
	}
	return true, nil
}

// probeDesktopSocket 核验私有文件权限、内核连接对端及进程资源组，然后检查实际 RFB 初始化结果。
func probeDesktopSocket(ctx context.Context, session computer.ExecutionSession, expectedGroup string) (failure error) {
	connection, err := openDesktopConnection(ctx, session, expectedGroup)
	if err != nil {
		return err
	}
	defer func() {
		if connection.Close() != nil {
			failure = errors.Join(failure, errors.New("关闭桌面探测连接失败"))
		}
	}()
	return checkDesktopHandshake(ctx, connection)
}

// openDesktopConnection 是健康检查和图形操作共用的身份核验入口；成功后由调用方关闭连接。
func openDesktopConnection(ctx context.Context, session computer.ExecutionSession, expectedGroup string) (result net.Conn, failure error) {
	path := session.RuntimeDirectory() + "/desktop.sock"
	var identity unix.Stat_t
	if unix.Lstat(path, &identity) != nil || identity.Mode&unix.S_IFMT != unix.S_IFSOCK || identity.Mode&07777 != 0600 || identity.Uid != uint32(session.UID) || identity.Gid != uint32(session.GID) {
		return nil, errors.New("桌面连接入口身份或权限不符")
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, errors.New("连接私有桌面失败")
	}
	defer func() {
		if result == nil && connection.Close() != nil {
			failure = errors.Join(failure, errors.New("关闭未获授权的桌面连接失败"))
		}
	}()
	local, ok := connection.(*net.UnixConn)
	if !ok {
		return nil, errors.New("桌面连接类型不符")
	}
	control, err := local.SyscallConn()
	if err != nil {
		return nil, errors.New("读取桌面连接控制入口失败")
	}
	var peer *unix.Ucred
	var peerError error
	if control.Control(func(fd uintptr) { peer, peerError = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }) != nil || peerError != nil || peer == nil || peer.Pid <= 1 || peer.Uid != uint32(session.UID) || peer.Gid != uint32(session.GID) {
		return nil, errors.New("桌面内核对端身份不符")
	}
	file, err := os.Open("/proc/" + strconv.Itoa(int(peer.Pid)) + "/cgroup")
	if err != nil {
		return nil, errors.New("检查桌面对端资源组失败")
	}
	encoded, readError := io.ReadAll(io.LimitReader(file, 16385))
	closeError := file.Close()
	if readError != nil || closeError != nil || len(encoded) > 16384 || strings.TrimSpace(string(encoded)) != "0::"+expectedGroup {
		return nil, errors.New("桌面对端不属于当前执行资源组")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return connection, nil
}
