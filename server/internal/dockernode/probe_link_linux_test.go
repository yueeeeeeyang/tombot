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
	"io.yueyang/tom/internal/computerprobe"
	"io.yueyang/tom/internal/processgroup"
	"io.yueyang/tom/internal/taskprocess"
)

type linkProbeStore struct {
	mu    sync.Mutex
	state *computeragent.ProbeSnapshot
}

func (store *linkProbeStore) LoadProbe(context.Context) (*computeragent.ProbeSnapshot, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.state == nil {
		return nil, nil
	}
	copy := *store.state
	return &copy, nil
}
func (store *linkProbeStore) SaveProbe(_ context.Context, state computeragent.ProbeSnapshot) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.state = &state
	return nil
}

type linkProbeScopes struct{}
type linkProbeScope struct{ linkScope }

func (linkProbeScopes) OpenProbe(_ processgroup.ServiceReference, files []*os.File) (computeragent.ProbeScope, error) {
	return &linkProbeScope{linkScope{files: files}}, nil
}
func (*linkProbeScope) Run(ctx context.Context, request taskprocess.ProbeRequest, _ *os.File) (computerprobe.Evidence, error) {
	return probeEvidence(request.Group.ID), ctx.Err()
}
func (*linkProbeScope) Drain(ctx context.Context) error { return ctx.Err() }

func newLinkProbeController(t *testing.T, identity computeragent.Identity) (*computeragent.ProbeController, *linkProbeStore) {
	t.Helper()
	store := &linkProbeStore{}
	controller, err := computeragent.NewProbeController(t.Context(), identity, store, linkProbeScopes{}, computeragent.Options{CallTimeout: time.Second, CleanupTimeout: time.Second, RetryDelay: 10 * time.Millisecond, CleanupAttempts: 2}, sessionLogger(), func() { t.Error("测试探针失控") })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := controller.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return controller, store
}

// TestKernelNodeProbeBinding 真实发送 Unix 控制帧和描述符；仅进程资源组与工作者采用显式替身。
func TestKernelNodeProbeBinding(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("节点探针联合测试要求专用 Linux root 容器")
	}
	for _, reject := range []bool{false, true} {
		name := "固定实例成功"
		if reject {
			name = "错误对端拒绝"
		}
		t.Run(name, func(t *testing.T) {
			f, probes, groups, resource, current := newProbeFixture(t)
			spec := resource.Specification
			identity := computeragent.Identity{UserID: spec.UserID, ComputerID: spec.ComputerID, NodeID: spec.NodeID, Generation: spec.Generation}
			agentProbes, store := newLinkProbeController(t, identity)
			guard, err := computeragent.NewController(t.Context(), identity, &linkCatalog{}, linkScopes{}, computeragent.Options{CallTimeout: time.Second, CleanupTimeout: time.Second, RetryDelay: 10 * time.Millisecond, CleanupAttempts: 2}, 4, sessionLogger())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := guard.Shutdown(context.Background()); err != nil {
					t.Error(err)
				}
			})
			server, err := computeragent.NewServer(guard, agentProbes, newLinkBrowserController(t, identity), func(peer agentchannel.Peer) error {
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
			listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: "/proc/self/fd/" + strconv.Itoa(int(parent.Fd())) + "/" + computeragent.SocketName, Net: "unixpacket"})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(resource.ControlPath, computeragent.SocketName), 0600); err != nil {
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
					t.Error("探针协议未结束")
				}
			})
			locate := probes.locate
			probes.locate = func(ctx context.Context, id string, pid int) (processgroup.ContainerReference, error) {
				ref, err := locate(ctx, id, pid)
				if reject && pid == os.Getpid() {
					ref.Inode++
				}
				return ref, err
			}
			probes.stamp, probes.invoke = agentchannel.FromDeadline, probes.call
			f.node.probes = probes
			evidence, err := probes.check(t.Context(), resource, current)
			if len(groups.drained) != 1 {
				t.Fatal("协议之后未宿主回收")
			}
			state, loadErr := store.LoadProbe(t.Context())
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if reject {
				if err == nil || evidence != (computerprobe.Evidence{}) || state != nil {
					t.Fatal("错误对端获得授权", state, err)
				}
				return
			}
			if err != nil || state == nil || state.Phase != "closed" || evidence.Validate(state.Grant.Request.Group.ID) != nil {
				t.Fatal(state, evidence, err)
			}
			hostState, err := f.node.journal.readProbe(spec.UserID)
			if err != nil || hostState.Phase != "closed" || hostState.Grant != state.Grant {
				t.Fatal("双方屏障不一致", hostState, err)
			}
		})
	}
}

// 此夹具不调用浏览器动作；意外调用必须失败，不能伪造浏览器已经就绪。
type unusedLinkBrowser struct{}

func (unusedLinkBrowser) LoadBrowser(ctx context.Context) (*computeragent.BrowserSnapshot, error) {
	return nil, ctx.Err()
}
func (unusedLinkBrowser) SaveBrowser(context.Context, computeragent.BrowserSnapshot) error {
	return errors.New("节点协议夹具没有浏览器持久操作")
}
func (unusedLinkBrowser) OpenBrowser(taskprocess.BrowserRequest, []*os.File, time.Duration) (computeragent.BrowserScope, error) {
	return nil, errors.New("节点协议夹具没有浏览器执行")
}
func newLinkBrowserController(t *testing.T, identity computeragent.Identity) *computeragent.BrowserController {
	t.Helper()
	controller, err := computeragent.NewBrowserController(t.Context(), identity, unusedLinkBrowser{}, unusedLinkBrowser{}, computeragent.Options{CallTimeout: time.Second, CleanupTimeout: time.Second, RetryDelay: 10 * time.Millisecond, CleanupAttempts: 2}, func(context.Context, computer.ExecutionSession) error {
		return errors.New("节点协议夹具没有浏览器授权")
	}, sessionLogger(), func() { t.Error("测试浏览器失控") })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := controller.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return controller
}
