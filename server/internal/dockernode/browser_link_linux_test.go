//go:build linux && agentintegration

package dockernode

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/processgroup"
	"io.yueyang/tom/internal/sharedbrowser"
	"io.yueyang/tom/internal/taskprocess"
)

// linkBrowserFactory 只验证双方持久屏障与真实 Unix 协议，明确不执行 Chrome 或证明资源组可用。
type linkBrowserFactory struct{}
type linkBrowserScope struct {
	files []*os.File
	done  chan struct{}
	once  sync.Once
}

func (linkBrowserFactory) OpenBrowser(_ taskprocess.BrowserRequest, files []*os.File, _ time.Duration) (computeragent.BrowserScope, error) {
	return &linkBrowserScope{files: files, done: make(chan struct{})}, nil
}
func (*linkBrowserScope) Start(context.Context, taskprocess.BrowserRequest, sharedbrowser.Options) (computeragent.BrowserService, error) {
	return linkUnavailableWindows{}, nil
}
func (s *linkBrowserScope) Done() <-chan struct{}       { return s.done }
func (s *linkBrowserScope) Revoke() error               { s.once.Do(func() { close(s.done) }); return nil }
func (s *linkBrowserScope) Drain(context.Context) error { return s.Revoke() }
func (s *linkBrowserScope) Close() error                { return agentchannel.CloseFiles(s.files) }

type linkUnavailableWindows struct{}

func (linkUnavailableWindows) Open(context.Context, sharedbrowser.Address) (sharedbrowser.Window, error) {
	return sharedbrowser.Window{}, sharedbrowser.ErrUnavailable
}
func (linkUnavailableWindows) List(context.Context, computer.ExecutionSession) ([]sharedbrowser.Window, error) {
	return nil, sharedbrowser.ErrUnavailable
}
func (linkUnavailableWindows) Execute(context.Context, sharedbrowser.Address, sharedbrowser.Command) (sharedbrowser.Window, error) {
	return sharedbrowser.Window{}, sharedbrowser.ErrUnavailable
}
func (linkUnavailableWindows) Screenshot(context.Context, sharedbrowser.Address) ([]byte, error) {
	return nil, sharedbrowser.ErrUnavailable
}
func (linkUnavailableWindows) RespondDialog(context.Context, sharedbrowser.Address, sharedbrowser.DialogReply) error {
	return sharedbrowser.ErrUnavailable
}
func (linkUnavailableWindows) CloseWindow(context.Context, sharedbrowser.Address) error {
	return sharedbrowser.ErrUnavailable
}
func (linkUnavailableWindows) ReleaseSession(context.Context, computer.ExecutionSession) error {
	return sharedbrowser.ErrUnavailable
}

func TestKernelNodeBrowserBinding(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("浏览器联合协议必须在专用 Linux root 容器运行")
	}
	// 使用生产 FileStore，父目录同样必须受保护；普通 /tmp 不能作为控制目录祖先。
	private, err := os.MkdirTemp("/run", "browser-link-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(private); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("TMPDIR", private)
	for _, reject := range []bool{false, true} {
		name := "固定实例成功"
		if reject {
			name = "错误对端拒绝"
		}
		t.Run(name, func(t *testing.T) {
			f, b, g, r, current, _ := newBrowserFixture(t)
			identity := f.node.agentConfiguration(r).Identity
			store, err := computeragent.NewFileStore(r.ControlPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			})
			options := computeragent.Options{CallTimeout: time.Second, CleanupTimeout: time.Second, RetryDelay: 10 * time.Millisecond, CleanupAttempts: 2}
			guard, err := computeragent.NewController(t.Context(), identity, &linkCatalog{}, linkScopes{}, options, 4, sessionLogger())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := guard.Shutdown(context.Background()); err != nil {
					t.Error(err)
				}
			})
			agentBrowsers, err := computeragent.NewBrowserController(t.Context(), identity, store, linkBrowserFactory{}, options, func(context.Context, computer.ExecutionSession) error { return sharedbrowser.ErrDenied }, sessionLogger(), func() { t.Error("协议测试浏览器失控") })
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				if err := agentBrowsers.Shutdown(ctx); err != nil {
					t.Error(err)
				}
			})
			probes, _ := newLinkProbeController(t, identity)
			server, err := computeragent.NewServer(guard, probes, agentBrowsers, func(peer agentchannel.Peer) error {
				if peer.PID != int32(os.Getpid()) || peer.UID != 0 || peer.GID != 0 {
					return errors.New("测试协议对端不符")
				}
				return nil
			}, time.Second, 4, sessionLogger())
			if err != nil {
				t.Fatal(err)
			}
			parent, err := os.Open(r.ControlPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := parent.Close(); err != nil {
					t.Error(err)
				}
			})
			listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: "/proc/self/fd/" + strconv.Itoa(int(parent.Fd())) + "/" + computeragent.SocketName, Net: "unixpacket"})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(r.ControlPath, computeragent.SocketName), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			served := make(chan error, 1)
			go func() { served <- server.Serve(ctx, listener) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-served:
					if !errors.Is(err, context.Canceled) {
						t.Error(err)
					}
				case <-time.After(2 * time.Second):
					t.Error("浏览器协议未结束")
				}
			})
			locate := b.locate
			b.locate = func(ctx context.Context, id string, pid int) (processgroup.ContainerReference, error) {
				ref, err := locate(ctx, id, pid)
				if reject && pid == os.Getpid() {
					ref.Inode++
				}
				return ref, err
			}
			b.stamp, b.invoke = agentchannel.FromDeadline, b.call
			err = b.ensure(t.Context(), r, current)
			if reject {
				if err == nil {
					t.Fatal("错误对端获得浏览器授权")
				}
				observed, err := store.LoadBrowser(t.Context())
				if err != nil || observed != nil {
					t.Fatal("拒绝对端仍消费代理授权", observed, err)
				}
				state, err := f.node.journal.readBrowser(r.Specification.UserID)
				if err != nil || state.Phase != "closed" || state.Closure != "isolated" {
					t.Fatal(state, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := b.ensure(t.Context(), r, current); err != nil {
				t.Fatal(err)
			}
			if err := b.retire(t.Context(), r); err != nil {
				t.Fatal(err)
			}
			observed, err := store.LoadBrowser(t.Context())
			if err != nil || observed == nil || observed.Phase != "closed" {
				t.Fatal(observed, err)
			}
			state, err := f.node.journal.readBrowser(r.Specification.UserID)
			if err != nil || state.Phase != "closed" || state.Closure != "agent" || state.Grant != observed.Grant || len(g.created) != 1 || len(g.drained) != 1 {
				t.Fatal("双方持久屏障不一致", state, err)
			}
		})
	}
}
