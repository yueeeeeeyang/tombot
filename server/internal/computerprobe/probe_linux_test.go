//go:build linux

package computerprobe

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestNetworkInterfaceBoundaries(t *testing.T) {
	loopback := net.Interface{Name: "lo", Flags: net.FlagLoopback | net.FlagUp | net.FlagRunning}
	local := []net.Addr{&net.IPNet{IP: net.IPv4(127, 0, 0, 1), Mask: net.CIDRMask(8, 32)}}
	ipv6 := &net.IPNet{IP: net.IPv6loopback, Mask: net.CIDRMask(128, 128)}
	external := []net.Addr{&net.IPNet{IP: net.IPv4(192, 0, 2, 1), Mask: net.CIDRMask(24, 32)}}
	for _, test := range []struct {
		name      string
		network   net.Interface
		addresses []net.Addr
		loopback  bool
		valid     bool
	}{
		{"IPv4 环回", loopback, local, true, true},
		{"双栈环回", loopback, append(append([]net.Addr{}, local...), ipv6), true, true},
		{"未启用的内核隧道", net.Interface{Name: "tunl0", Flags: net.FlagPointToPoint}, nil, false, true},
		{"未启用的内核以太隧道", net.Interface{Name: "gretap0", Flags: net.FlagBroadcast | net.FlagMulticast}, nil, false, true},
		{"隧道启用", net.Interface{Name: "tunl0", Flags: net.FlagUp}, nil, false, false},
		{"非环回运行", net.Interface{Name: "eth0", Flags: net.FlagRunning}, nil, false, false},
		{"未启用但有地址", net.Interface{Name: "eth0"}, external, false, false},
		{"非环回伪装本地地址", net.Interface{Name: "eth0"}, local, false, false},
		{"非预期环回接口", net.Interface{Name: "other", Flags: net.FlagLoopback}, local, false, false},
		{"环回停用", net.Interface{Name: "lo", Flags: net.FlagLoopback}, local, false, false},
		{"环回名称伪装", net.Interface{Name: "lo", Flags: net.FlagUp}, local, false, false},
		{"环回无地址", loopback, nil, false, false},
		{"环回空地址", loopback, []net.Addr{nil}, false, false},
		{"环回外部地址", loopback, external, false, false},
		{"环回地址数量异常", loopback, []net.Addr{ipv6, ipv6, ipv6}, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			actual, err := checkInterface(test.network, test.addresses)
			if (err == nil) != test.valid || actual != test.loopback {
				t.Fatal("网络接口验收结果错误", actual, err)
			}
		})
	}
}

func TestRealTerminalAndFilesystem(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := checkTerminal(ctx); err != nil {
		t.Fatal("实际伪终端检查失败", err)
	}
	directory := t.TempDir()
	existing := filepath.Join(directory, "existing.txt")
	if err := os.WriteFile(existing, []byte("保留原有内容"), 0600); err != nil {
		t.Fatal(err)
	}
	filesystem, size, err := checkFiles(ctx, directory)
	if err != nil || filesystem == 0 || size == 0 {
		t.Fatal("实际共享文件读写失败", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 || entries[0].Name() != "existing.txt" {
		t.Fatal("探针遗留临时文件或改变既有目录", err)
	}
	if content, err := os.ReadFile(existing); err != nil || string(content) != "保留原有内容" {
		t.Fatal("探针修改已有文件", err)
	}
	if _, _, err := checkFiles(ctx, existing); err == nil {
		t.Fatal("普通文件被当作共享文件系统")
	}
}

func TestProbeCancellationAndDescriptorFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !errors.Is(checkTerminal(ctx), context.Canceled) || !errors.Is(checkNetwork(ctx), context.Canceled) {
		t.Fatal("取消后仍开始内核检查")
	}
	if _, _, err := checkFiles(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatal("取消后仍修改文件系统", err)
	}
	bounded, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if err := waitDescriptor(bounded, 1000000, unix.POLLIN); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("无效终端入口没有立即失败", err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(reader.Close(), writer.Close()); err != nil {
			t.Error(err)
		}
	})
	short, end := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer end()
	if err := waitDescriptor(short, int(reader.Fd()), unix.POLLIN); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("无数据时没有遵守取消期限", err)
	}
}
