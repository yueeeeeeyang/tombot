package computeragent

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func desktopHeader() []byte {
	header := make([]byte, 24)
	binary.BigEndian.PutUint16(header[:2], 1280)
	binary.BigEndian.PutUint16(header[2:4], 800)
	copy(header[4:20], []byte{32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0})
	binary.BigEndian.PutUint32(header[20:], 3)
	return header
}

func serveTestRFB(connection net.Conn, header []byte) (failure error) {
	defer func() { failure = errors.Join(failure, connection.Close()) }()
	return writeTestHandshake(connection, header)
}

func writeTestHandshake(connection net.Conn, header []byte) error {
	if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	if _, err := io.WriteString(connection, "RFB 003.008\n"); err != nil {
		return err
	}
	version := make([]byte, 12)
	if _, err := io.ReadFull(connection, version); err != nil {
		return err
	}
	if string(version) != "RFB 003.008\n" {
		return errors.New("客户端协议版本错误")
	}
	if _, err := connection.Write([]byte{1, 1}); err != nil {
		return err
	}
	choice := make([]byte, 1)
	if _, err := io.ReadFull(connection, choice); err != nil {
		return err
	}
	if choice[0] != 1 {
		return errors.New("客户端握手方式错误")
	}
	if _, err := connection.Write(make([]byte, 4)); err != nil {
		return err
	}
	if _, err := io.ReadFull(connection, choice); err != nil {
		return err
	}
	if choice[0] != 1 {
		return errors.New("探测请求会断开其他桌面观察者")
	}
	_, err := connection.Write(append(header, []byte("Bot")...))
	return err
}

func TestDesktopHandshakeBoundaries(t *testing.T) {
	for name, mutate := range map[string]func([]byte){
		"正常": nil, "错误宽度": func(b []byte) { b[0] = 0 }, "错误色深": func(b []byte) { b[5] = 8 }, "重复颜色位移": func(b []byte) { b[14] = b[15] }, "超长名称": func(b []byte) { binary.BigEndian.PutUint32(b[20:], 1<<30) },
	} {
		t.Run(name, func(t *testing.T) {
			client, server := net.Pipe()
			header := desktopHeader()
			if mutate != nil {
				mutate(header)
			}
			done := make(chan error, 1)
			go func() { done <- serveTestRFB(server, header) }()
			err := checkDesktopHandshake(context.Background(), client)
			if closeError := client.Close(); closeError != nil {
				t.Fatal(closeError)
			}
			serverError := <-done
			if mutate == nil {
				if err != nil || serverError != nil {
					t.Fatal(err, serverError)
				}
			} else if err == nil {
				t.Fatal("接受了错误桌面握手")
			}
		})
	}
	client, server := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- checkDesktopHandshake(ctx, client) }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("取消握手被报告为成功")
		}
	case <-time.After(time.Second):
		t.Fatal("握手取消未唤醒读取")
	}
	if err := errors.Join(client.Close(), server.Close()); err != nil {
		t.Fatal(err)
	}
}
