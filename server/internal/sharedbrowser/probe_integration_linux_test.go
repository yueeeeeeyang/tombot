//go:build linux && browserintegration

package sharedbrowser

import (
	"context"
	"errors"
	"github.com/chromedp/chromedp"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/egressproxy"
	"io.yueyang/tom/internal/infra/ids"
)

// TestSharedChromeCapabilityProbe 使用真实 Chrome、私有 Unix 代理及环回转接；断开代理必须使下次验收失败。
func TestSharedChromeCapabilityProbe(t *testing.T) {
	options, _, sessions := testOptions()
	options.CallTimeout = 20 * time.Second

	directory, err := os.MkdirTemp("/workspace", "browser-probe-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	if err = os.Chown(directory, computer.BrowserUID, computer.DesktopSharedGID); err != nil {
		t.Fatal(err)
	}
	private, err := os.MkdirTemp("/run", "probe-egress-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(private); err != nil {
			t.Error(err)
		}
	})
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(private, "egress.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(listener.Addr().String(), 0600); err != nil {
		t.Fatal(err)
	}
	var peers atomic.Int32
	proxy, err := egressproxy.New(egressproxy.Options{Ports: []uint16{80, 443}, ConnectTimeout: time.Second, HeaderTimeout: time.Second, IdleTimeout: time.Second, RequestTimeout: 5 * time.Second, TunnelTimeout: 5 * time.Second, ShutdownTimeout: 2 * time.Second, MaximumConnections: 32, MaximumRequestBytes: 4096, MaximumResponseBytes: 16384, MaximumTunnelBytes: 16384}, egressproxy.Owner{UserID: options.UserID, ComputerID: options.ComputerID, Generation: 1, ContainerID: strings.Repeat("a", 64)}, func(ctx context.Context, peer egressproxy.Peer) error {
		if peer.PID != int32(os.Getpid()) {
			return errors.New("测试对端身份不符")
		}
		peers.Add(1)
		return ctx.Err()
	}, options.Logger)
	if err != nil {
		t.Fatal(err)
	}
	proxyContext, stopProxy := context.WithCancel(t.Context())
	proxyDone := make(chan error, 1)
	go func() { proxyDone <- proxy.Serve(proxyContext, listener) }()
	t.Cleanup(func() {
		stopProxy()
		select {
		case err := <-proxyDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("测试代理未退出")
		}
	})
	bridge, err := egressproxy.NewBridge(egressproxy.BridgeOptions{ConnectTimeout: time.Second, IdleTimeout: 2 * time.Second, Lifetime: 30 * time.Second, ShutdownTimeout: 3 * time.Second, MaximumConnections: 32, MaximumTransferBytes: 65536}, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", listener.Addr().String())
	}, options.Logger)
	if err != nil {
		t.Fatal(err)
	}
	loopback, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bridgeContext, stopBridge := context.WithCancel(t.Context())
	bridgeDone := make(chan error, 1)
	go func() { bridgeDone <- bridge.Serve(bridgeContext, loopback) }()
	t.Cleanup(func() {
		stopBridge()
		select {
		case err := <-bridgeDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("测试转接未退出")
		}
	})
	live := launchTestBrowserWithArguments(t, filepath.Join(directory, "profile"), options, []string{"--proxy-server=http://" + loopback.Addr().String(), "--proxy-bypass-list=<-loopback>"})
	service := live.service
	// 正常业务窗口必须在连续验收之后仍存在，不允许用关闭整个 Chrome 完成成功清理。
	address := Address{Session: sessions[0], WindowID: ids.New()}
	if _, err = service.Open(t.Context(), address); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		challenge := ids.New()
		evidence, err := service.Probe(t.Context(), challenge)
		if err != nil || evidence.Validate(challenge) != nil {
			tab := chromedp.FromContext(service.root).Target
			diagnostic, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if navigateErr := probeNavigate(diagnostic, tab, "chrome://sandbox"); navigateErr == nil {
				if waitErr := probeWait(diagnostic, tab, `document.body && document.body.innerText.includes('sandboxed.')`); waitErr == nil {
					var report string
					if readErr := probeEvaluate(diagnostic, tab, `document.body.innerText`, &report); readErr == nil {
						t.Log(report)
					} else {
						t.Log(readErr)
					}
				} else {
					t.Log(waitErr)
				}
			} else {
				t.Log(navigateErr)
			}
			t.Fatal("真实浏览器能力检查失败", evidence, err)
		}
		service.mu.Lock()
		remaining := len(service.probeTargets)
		service.mu.Unlock()
		if remaining != 0 {
			t.Fatal("探针窗口未清理")
		}
		windows, err := service.List(t.Context(), sessions[0])
		if err != nil || len(windows) != 1 || windows[0].ID != address.WindowID {
			t.Fatal("验收影响了业务窗口", windows, err)
		}
	}
	if peers.Load() < 2 {
		t.Fatal("未经过真实 Unix 出网代理")
	}
	stopBridge()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if evidence, err := service.Probe(ctx, ids.New()); err == nil || evidence.ChallengeID != "" {
		t.Fatal("代理断开后仍返回就绪", evidence, err)
	}
}
