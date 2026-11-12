package nodeapp

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadyNotificationIsControlOnly(t *testing.T) {
	// Unix 路径有内核长度上限，macOS 的测试目录过长时使用独立短目录。
	directory, err := os.MkdirTemp("/tmp", "tomnode-notify-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	}()
	path := filepath.Join(directory, "notify.sock")
	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := NotifyReady(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	message := make([]byte, 1024)
	count, _, err := listener.ReadFromUnix(message)
	if err != nil || !strings.HasPrefix(string(message[:count]), "READY=1\nSTATUS=") || !strings.Contains(string(message[:count]), "用户电脑仍需完整能力检查") {
		t.Fatal("通知错误", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if NotifyReady(ctx, path) == nil {
		t.Fatal("取消后仍发送通知")
	}
	if err := NotifyReady(t.Context(), ""); err != nil {
		t.Fatal("前台启动被拒绝", err)
	}
	for _, invalid := range []string{"relative", "/run/../run/notify", "@", "/run/a\n", strings.Repeat("x", 101), filepath.Join(directory, "missing.sock")} {
		if NotifyReady(t.Context(), invalid) == nil {
			t.Fatal("无效通知端点被接受")
		}
	}
}
