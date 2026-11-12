package computeragent

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"

	"io.yueyang/tom/internal/computer"
)

// checkDesktopHandshake 只对已经验证内核对端的私有连接检查 RFB 3.8，不发送画面输入。
// ClientInit 的共享标记避免健康检查断开已连接的观察者，所有读取均固定长度或显式有界。
func checkDesktopHandshake(ctx context.Context, connection net.Conn) (failure error) {
	return withDesktopConnection(ctx, connection, 2*time.Second, func(context.Context) error { return desktopHandshake(connection) })
}

// withDesktopConnection 将阻塞读写与取消绑定；调用者持有并负责关闭已验证身份的连接。
func withDesktopConnection(ctx context.Context, connection net.Conn, timeout time.Duration, action func(context.Context) error) (failure error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	if connection.SetDeadline(deadline) != nil {
		return errors.New("设置桌面握手期限失败")
	}
	canceled := make(chan struct{})
	var cancelError error
	stop := context.AfterFunc(ctx, func() { cancelError = connection.SetDeadline(time.Now()); close(canceled) })
	defer func() {
		if !stop() {
			<-canceled
		}
		if cancelError != nil {
			failure = errors.Join(failure, errors.New("取消桌面握手失败"))
		}
	}()
	return errors.Join(action(ctx), ctx.Err())
}

func desktopHandshake(connection net.Conn) error {
	read := func(length int) ([]byte, error) {
		value := make([]byte, length)
		_, err := io.ReadFull(connection, value)
		if err != nil {
			return nil, errors.New("读取桌面握手失败")
		}
		return value, nil
	}
	write := func(value []byte) error {
		count, err := connection.Write(value)
		if err != nil || count != len(value) {
			return errors.New("发送桌面握手失败")
		}
		return nil
	}
	version, err := read(12)
	if err != nil {
		return err
	}
	if string(version) != "RFB 003.008\n" {
		return errors.New("桌面协议版本不符")
	}
	if err = write(version); err != nil {
		return err
	}
	security, err := read(2)
	if err != nil {
		return err
	}
	if security[0] != 1 || security[1] != 1 {
		return errors.New("私有桌面握手方式不符")
	}
	if err = write([]byte{1}); err != nil {
		return err
	}
	result, err := read(4)
	if err != nil {
		return err
	}
	if binary.BigEndian.Uint32(result) != 0 {
		return errors.New("私有桌面拒绝握手")
	}
	if err = write([]byte{1}); err != nil {
		return err
	}
	header, err := read(24)
	if err != nil {
		return err
	}
	if binary.BigEndian.Uint16(header[:2]) != computer.DesktopWidth || binary.BigEndian.Uint16(header[2:4]) != computer.DesktopHeight {
		return errors.New("桌面实际分辨率不符")
	}
	pixel := header[4:20]
	if pixel[0] != 32 || pixel[1] != 24 || pixel[2] > 1 || pixel[3] != 1 || binary.BigEndian.Uint16(pixel[4:6]) != 255 || binary.BigEndian.Uint16(pixel[6:8]) != 255 || binary.BigEndian.Uint16(pixel[8:10]) != 255 || pixel[10] > 24 || pixel[11] > 24 || pixel[12] > 24 || pixel[10]%8 != 0 || pixel[11]%8 != 0 || pixel[12]%8 != 0 || pixel[10] == pixel[11] || pixel[10] == pixel[12] || pixel[11] == pixel[12] {
		return errors.New("桌面像素格式不符")
	}
	if binary.BigEndian.Uint32(header[20:24]) != 3 {
		return errors.New("桌面名称长度不符")
	}
	name, err := read(3)
	if err != nil {
		return err
	}
	if string(name) != "Bot" {
		return errors.New("桌面名称不符")
	}
	return nil
}
