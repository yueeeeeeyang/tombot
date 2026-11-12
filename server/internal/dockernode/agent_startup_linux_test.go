//go:build linux && agentintegration

package dockernode

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/processgroup"
)

// TestKernelNodeAgentStartup 使用真实 socket 和控制协议验证延迟监听；Docker 与资源组明确使用替身。
func TestKernelNodeAgentStartup(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("启动接入联合测试要求专用 Linux root 容器")
	}
	for _, scenario := range []string{"missing", "refused", "unsafe", "wrong_peer"} {
		t.Run(scenario, func(t *testing.T) {
			fixture, startup, resource, current := newAgentStartupFixture(t)
			identity := fixture.node.agentConfiguration(resource).Identity
			catalog := &linkCatalog{}
			guard, err := computeragent.NewController(t.Context(), identity, catalog, linkScopes{}, computeragent.Options{CallTimeout: time.Second, CleanupTimeout: time.Second, RetryDelay: 10 * time.Millisecond, CleanupAttempts: 2}, 4, sessionLogger())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := guard.Shutdown(ctx); err != nil {
					t.Error(err)
				}
			})
			probes, _ := newLinkProbeController(t, identity)
			server, err := computeragent.NewServer(guard, probes, newLinkBrowserController(t, identity), func(peer agentchannel.Peer) error {
				if peer.PID != int32(os.Getpid()) || peer.UID != 0 || peer.GID != 0 {
					return errors.New("测试协议对端不符")
				}
				return nil
			}, time.Second, 2, sessionLogger())
			if err != nil {
				t.Fatal(err)
			}
			parent, err := os.Open(resource.ControlPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := parent.Close(); err != nil {
					t.Error(err)
				}
			})
			path := filepath.Join(resource.ControlPath, computeragent.SocketName)
			listen := func() *net.UnixListener {
				t.Helper()
				listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: "/proc/self/fd/" + strconv.Itoa(int(parent.Fd())) + "/" + computeragent.SocketName, Net: "unixpacket"})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0600); err != nil {
					t.Fatal(errors.Join(err, listener.Close()))
				}
				return listener
			}
			serve := func() {
				t.Helper()
				listener := listen()
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- server.Serve(ctx, listener) }()
				t.Cleanup(func() {
					cancel()
					select {
					case err := <-done:
						if !errors.Is(err, context.Canceled) {
							t.Error(err)
						}
					case <-time.After(2 * time.Second):
						t.Error("控制协议未结束")
					}
				})
			}
			switch scenario {
			case "refused":
				listener := listen()
				listener.SetUnlinkOnClose(false)
				if err := listener.Close(); err != nil {
					t.Fatal(err)
				}
			case "unsafe":
				if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong_peer":
				serve()
				locate := startup.locate
				startup.locate = func(ctx context.Context, id string, pid int) (processgroup.ContainerReference, error) {
					ref, err := locate(ctx, id, pid)
					if pid == os.Getpid() {
						ref.Inode++
					}
					return ref, err
				}
			}
			attempts := 0
			isDelayed := scenario == "missing" || scenario == "refused"
			startup.check = func(ctx context.Context, resource Resource, expected processgroup.ContainerReference) error {
				attempts++
				err := startup.call(ctx, resource, expected)
				if attempts == 1 && isDelayed {
					if !isAgentNotListening(err) {
						t.Fatal("实际未监听错误丢失可等待分类", err)
					}
					if scenario == "refused" {
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
					}
					serve()
				}
				return err
			}
			err = startup.wait(t.Context(), resource, current)
			if isDelayed {
				if err != nil || attempts != 2 {
					t.Fatal("延迟监听未接入", attempts, err)
				}
			} else if err == nil || attempts != 1 {
				t.Fatal("不安全入口未立即拒绝", attempts, err)
			}
			if len(catalog.records) != 0 {
				t.Fatal("控制检查生成了桌面状态")
			}
			if state, err := fixture.node.journal.readBrowser(identity.UserID); err != nil || state != nil {
				t.Fatal("控制检查生成了浏览器授权", state, err)
			}
		})
	}
}
