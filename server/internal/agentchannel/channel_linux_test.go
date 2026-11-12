//go:build linux

package agentchannel

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func socketPair(t *testing.T, kind int) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	descriptors, err := unix.Socketpair(unix.AF_UNIX, kind|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	var connections []*net.UnixConn
	for _, fd := range descriptors {
		file := os.NewFile(uintptr(fd), "control-pair")
		connection, err := net.FileConn(file)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			t.Fatal("创建测试控制连接失败", err, closeErr)
		}
		value, ok := connection.(*net.UnixConn)
		if !ok {
			t.Fatal("测试连接不是 Unix 套接字")
		}
		connections = append(connections, value)
	}
	return connections[0], connections[1]
}

func channels(t *testing.T) (*Channel, *Channel) {
	t.Helper()
	left, right := socketPair(t, unix.SOCK_SEQPACKET)
	first, peer, err := New(left, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := New(right, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if peer.PID != int32(os.Getpid()) || peer.UID != uint32(os.Geteuid()) || peer.GID != uint32(os.Getegid()) {
		t.Fatal("内核对端身份不符")
	}
	t.Cleanup(func() {
		if err := first.Close(); err != nil {
			t.Error(err)
		}
		if err := second.Close(); err != nil {
			t.Error(err)
		}
	})
	return first, second
}

func TestKernelDescriptorTransferAndCloexec(t *testing.T) {
	sender, receiver := channels(t)
	file, err := os.CreateTemp(t.TempDir(), "controller")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err = file.WriteString("control-example"); err != nil {
		t.Fatal(err)
	}
	request := testMessage()
	request.Files = 1
	if err := sender.Send(context.Background(), request, []*os.File{file}); err != nil {
		t.Fatal(err)
	}
	message, files, err := receiver.Receive(context.Background())
	if err != nil || message.ID != request.ID || len(files) != 1 {
		t.Fatal("描述符传递失败", err)
	}
	t.Cleanup(func() {
		if err := CloseFiles(files); err != nil {
			t.Error(err)
		}
	})
	flags, err := unix.FcntlInt(files[0].Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("接收描述符缺少执行时关闭标记", err)
	}
	content := make([]byte, 15)
	if count, err := files[0].ReadAt(content, 0); err != nil || count != len(content) || string(content) != "control-example" {
		t.Fatal("收到的不是同一个内核文件", err)
	}
	if _, err := file.Stat(); err != nil {
		t.Fatal("发送动作关闭了调用方描述符", err)
	}
}

func openReferences(t *testing.T, file *os.File) int {
	t.Helper()
	var original unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &original); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		var stat unix.Stat_t
		if err := unix.Stat(filepath.Join("/proc/self/fd", entry.Name()), &stat); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			t.Fatal(err)
		}
		if stat.Dev == original.Dev && stat.Ino == original.Ino {
			count++
		}
	}
	return count
}

func TestKernelMalformedFramesCloseReceivedDescriptors(t *testing.T) {
	for _, name := range []string{"数量不符", "控制截断", "内容截断", "重复键"} {
		t.Run(name, func(t *testing.T) {
			sender, receiver := channels(t)
			file, err := os.CreateTemp(t.TempDir(), "control")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := file.Close(); err != nil {
					t.Error(err)
				}
			})
			request := testMessage()
			request.Files = 1
			encoded, err := Encode(request)
			if err != nil {
				t.Fatal(err)
			}
			descriptors := []int{int(file.Fd())}
			switch name {
			case "数量不符":
				request.Files = 2
				encoded, err = Encode(request)
			case "控制截断":
				descriptors = []int{int(file.Fd()), int(file.Fd()), int(file.Fd()), int(file.Fd()), int(file.Fd()), int(file.Fd())}
			case "内容截断":
				encoded = []byte(strings.Repeat("x", MaximumBytes+100))
			case "重复键":
				encoded = []byte(strings.Replace(string(encoded), `"version":1`, `"version":1,"version":1`, 1))
			}
			if err != nil {
				t.Fatal(err)
			}
			before := openReferences(t, file)
			if _, _, err := sender.connection.WriteMsgUnix(encoded, unix.UnixRights(descriptors...), nil); err != nil {
				t.Fatal(err)
			}
			if _, received, err := receiver.Receive(context.Background()); err == nil || received != nil {
				t.Fatal("非法控制帧没有被拒绝并释放描述符", err)
			}
			if after := openReferences(t, file); after != before {
				t.Fatalf("非法帧泄漏描述符：%d -> %d", before, after)
			}
		})
	}
}

func TestKernelReceiveCancellation(t *testing.T) {
	_, receiver := channels(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := receiver.Receive(ctx); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("取消未传播", err)
		}
	case <-time.After(time.Second):
		t.Fatal("控制读取未响应取消")
	}
}

func TestKernelStreamSocketRejected(t *testing.T) {
	left, right := socketPair(t, unix.SOCK_STREAM)
	t.Cleanup(func() {
		if err := left.Close(); err != nil {
			t.Error(err)
		}
		if err := right.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, _, err := New(left, time.Second); err == nil {
		t.Fatal("丢失帧边界的连接被接受")
	}
}

func TestKernelDeadlineDoesNotExtendInTransit(t *testing.T) {
	original := time.Now().Add(100 * time.Millisecond)
	deadline, err := FromDeadline(original)
	if err != nil {
		t.Fatal(err)
	}
	local, err := deadline.Local(time.Second)
	if err != nil || local.After(original) {
		t.Fatal("共享单调时钟延长了原始期限", err)
	}
	wrong := deadline
	wrong.TimeNamespace++
	if _, err := wrong.Local(time.Second); err == nil {
		t.Fatal("不同时间命名空间被接受")
	}
	timer := time.NewTimer(time.Until(original) + time.Millisecond)
	defer timer.Stop()
	<-timer.C
	if _, err := deadline.Local(time.Second); err == nil {
		t.Fatal("传输期间过期的期限被重新计时")
	}
}
