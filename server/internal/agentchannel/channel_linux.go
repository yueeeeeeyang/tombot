//go:build linux

package agentchannel

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Peer 来自内核 SO_PEERCRED，不从控制帧中的自报字段建立信任。
type Peer struct {
	PID      int32
	UID, GID uint32
}

// Channel 仅支持 Linux Unix 顺序包套接字。超时或取消使连接失效，不能在同一通道重发未知动作。
type Channel struct {
	connection          *net.UnixConn
	timeout             time.Duration
	readGate, writeGate chan struct{}
	closeOnce           sync.Once
	closeError          error
}

// New 接管成功验证的连接。调用方还须用 Peer 校验节点、容器或宿主命名空间归属。
func New(connection *net.UnixConn, timeout time.Duration) (*Channel, Peer, error) {
	if connection == nil || timeout < 10*time.Millisecond || timeout > time.Minute {
		return nil, Peer{}, errors.New("电脑代理控制连接配置无效")
	}
	raw, err := connection.SyscallConn()
	if err != nil {
		return nil, Peer{}, errors.New("读取电脑代理连接失败")
	}
	var credentials *unix.Ucred
	var inspectError error
	if err = raw.Control(func(fd uintptr) {
		kind, failure := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_TYPE)
		if failure != nil || kind != unix.SOCK_SEQPACKET {
			inspectError = errors.New("电脑代理连接必须保留消息边界")
			return
		}
		credentials, inspectError = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || inspectError != nil || credentials == nil {
		return nil, Peer{}, errors.New("电脑代理连接类型或对端身份无效")
	}
	return &Channel{connection: connection, timeout: timeout, readGate: make(chan struct{}, 1), writeGate: make(chan struct{}, 1)}, Peer{credentials.Pid, credentials.Uid, credentials.Gid}, nil
}

// Close 只关闭本次连接，不终止租约；容器看门狗独立于连接存活状态。
func (channel *Channel) Close() error {
	channel.closeOnce.Do(func() {
		if channel.connection.Close() != nil {
			channel.closeError = errors.New("关闭电脑代理连接失败")
		}
	})
	return channel.closeError
}

func (channel *Channel) begin(ctx context.Context, gate chan struct{}, read bool) (finish func() error, failure error) {
	ctx, cancel := context.WithTimeout(ctx, channel.timeout)
	if ctx.Err() != nil {
		cancel()
		return nil, ctx.Err()
	}
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
	if ctx.Err() != nil {
		<-gate
		cancel()
		return nil, ctx.Err()
	}
	deadline, _ := ctx.Deadline()
	if read {
		failure = channel.connection.SetReadDeadline(deadline)
	} else {
		failure = channel.connection.SetWriteDeadline(deadline)
	}
	if failure != nil {
		<-gate
		cancel()
		return nil, errors.New("设置电脑代理控制期限失败")
	}
	closed := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() { closed <- channel.Close() })
	return func() error {
		defer cancel()
		defer func() { <-gate }()
		var result error
		if !stop() {
			result = <-closed
		}
		if ctx.Err() != nil {
			result = errors.Join(result, ctx.Err())
		}
		return result
	}, nil
}

// Send 发送单个完整帧，不转移调用方描述符所有权，也不重试任何可能已送达的动作。
func (channel *Channel) Send(ctx context.Context, message Message, files []*os.File) (failure error) {
	if err := message.validate(); err != nil {
		return err
	}
	if message.Files != len(files) {
		return errors.New("电脑代理发送描述符数量与帧不符")
	}
	encoded, err := Encode(message)
	if err != nil {
		return err
	}
	descriptors := make([]int, 0, len(files))
	for _, file := range files {
		if file == nil {
			return errors.New("电脑代理发送描述符缺失")
		}
		descriptors = append(descriptors, int(file.Fd()))
	}
	var ancillary []byte
	if len(descriptors) > 0 {
		ancillary = unix.UnixRights(descriptors...)
	}
	finish, err := channel.begin(ctx, channel.writeGate, false)
	if err != nil {
		return err
	}
	defer func() {
		failure = errors.Join(failure, finish())
		if failure != nil {
			failure = errors.Join(failure, channel.Close())
		}
	}()
	count, controlCount, err := channel.connection.WriteMsgUnix(encoded, ancillary, nil)
	if err != nil || count != len(encoded) || controlCount != len(ancillary) {
		return errors.New("电脑代理控制发送失败，结果需要对账")
	}
	return nil
}

// Receive 原子设置接收描述符的 CLOEXEC；任何截断、取消或协议错误都会关闭已接收的描述符。
func (channel *Channel) Receive(ctx context.Context) (message Message, files []*os.File, failure error) {
	finish, err := channel.begin(ctx, channel.readGate, true)
	if err != nil {
		return message, nil, err
	}
	defer func() {
		failure = errors.Join(failure, finish())
		if failure != nil {
			failure = errors.Join(failure, CloseFiles(files), channel.Close())
			files = nil
			message = Message{}
		}
	}()
	encoded := make([]byte, MaximumBytes)
	ancillary := make([]byte, unix.CmsgSpace(MaximumFiles*4))
	raw, err := channel.connection.SyscallConn()
	if err != nil {
		return message, nil, errors.New("取得电脑代理接收入口失败")
	}
	var count, controlCount, flags int
	var receiveError error
	err = raw.Read(func(fd uintptr) bool {
		count, controlCount, flags, _, receiveError = unix.Recvmsg(int(fd), encoded, ancillary, unix.MSG_CMSG_CLOEXEC)
		return !errors.Is(receiveError, unix.EAGAIN) && !errors.Is(receiveError, unix.EWOULDBLOCK)
	})
	if err != nil || receiveError != nil {
		return message, nil, errors.New("电脑代理控制接收失败")
	}
	controls, err := unix.ParseSocketControlMessage(ancillary[:controlCount])
	if err != nil {
		return message, nil, errors.New("电脑代理描述符控制格式无效")
	}
	var invalidControl bool
	for _, control := range controls {
		if control.Header.Level != unix.SOL_SOCKET || control.Header.Type != unix.SCM_RIGHTS {
			invalidControl = true
			continue
		}
		descriptors, err := unix.ParseUnixRights(&control)
		if err != nil {
			invalidControl = true
			continue
		}
		for _, fd := range descriptors {
			files = append(files, os.NewFile(uintptr(fd), "agent-control"))
		}
	}
	if invalidControl || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || count == 0 || len(files) > MaximumFiles {
		return message, files, errors.New("电脑代理控制帧截断或描述符无效")
	}
	if err = Decode(encoded[:count], &message); err != nil {
		return message, files, err
	}
	if err = message.validate(); err != nil {
		return message, files, err
	}
	if message.Files != len(files) {
		return message, files, errors.New("电脑代理接收描述符数量与帧不符")
	}
	return message, files, nil
}
