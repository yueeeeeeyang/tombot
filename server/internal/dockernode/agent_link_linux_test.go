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
)

type linkStore struct {
	mu    sync.Mutex
	state *computeragent.Snapshot
}

type linkCatalog struct {
	mu      sync.Mutex
	records map[string]*linkStore
}

func (catalog *linkCatalog) Desktop(id string) (computeragent.Store, error) {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	if catalog.records == nil {
		catalog.records = map[string]*linkStore{}
	}
	if catalog.records[id] == nil {
		catalog.records[id] = &linkStore{}
	}
	return catalog.records[id], nil
}

func (catalog *linkCatalog) Visit(ctx context.Context, visit func(computeragent.Snapshot) error) error {
	catalog.mu.Lock()
	records := make([]*linkStore, 0, len(catalog.records))
	for _, record := range catalog.records {
		records = append(records, record)
	}
	catalog.mu.Unlock()
	for _, record := range records {
		snapshot, err := record.Load(ctx)
		if err != nil {
			return err
		}
		if snapshot != nil {
			if err = visit(*snapshot); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func (store *linkStore) Load(context.Context) (*computeragent.Snapshot, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.state == nil {
		return nil, nil
	}
	copy := *store.state
	return &copy, nil
}
func (store *linkStore) Save(_ context.Context, state computeragent.Snapshot) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.state = &state
	return nil
}

type linkScopes struct{}
type linkScope struct {
	files  []*os.File
	mu     sync.Mutex
	closed bool
}

func (linkScopes) Open(_ processgroup.Reference, files []*os.File) (computeragent.Scope, error) {
	return &linkScope{files: files}, nil
}
func (*linkScope) Membership(string) (*os.File, error) {
	return nil, errors.New("协议联合测试不执行用户任务")
}
func (*linkScope) Revoke() error                   { return nil }
func (*linkScope) Terminate(context.Context) error { return nil }
func (scope *linkScope) Close() error {
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.closed {
		return nil
	}
	scope.closed = true
	return agentchannel.CloseFiles(scope.files)
}

type linkGroups struct {
	*fakeGroups
	rejectPeer bool
}

func (groups *linkGroups) Locate(ctx context.Context, session computer.ExecutionSession, pid int) (processgroup.Reference, error) {
	reference, err := groups.fakeGroups.Locate(ctx, session, pid)
	if groups.rejectPeer && pid != 4242 {
		reference.ParentInode++
	}
	return reference, err
}

// TestKernelNodeAgentBinding 使用真实协议和 socket，资源组明确使用替身，不作为内核回收证明。
func TestKernelNodeAgentBinding(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("节点协议联合测试要求专用 Linux root 容器")
	}
	for _, reject := range []bool{false, true} {
		name := "固定实例成功"
		if reject {
			name = "其他容器对端拒绝"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newFixture(t)
			session := fixture.prepareComputer(t, fixture.specification)
			resource := fixture.node.resource(fixture.specification)
			catalog := &linkCatalog{}
			store, err := catalog.Desktop(session.DesktopID)
			if err != nil {
				t.Fatal(err)
			}
			guard, err := computeragent.NewController(context.Background(), computeragent.Identity{UserID: session.UserID, ComputerID: session.ComputerID, NodeID: session.NodeID, Generation: session.Generation}, catalog, linkScopes{}, computeragent.Options{CallTimeout: time.Second, CleanupTimeout: time.Second, RetryDelay: 10 * time.Millisecond, CleanupAttempts: 2}, 4, sessionLogger())
			if err != nil {
				t.Fatal(err)
			}
			// 测试在同一 PID 空间运行，只有此测试服务采用本进程检查；正式入口固定 RequireHostPeer。
			probes, _ := newLinkProbeController(t, computeragent.Identity{UserID: session.UserID, ComputerID: session.ComputerID, NodeID: session.NodeID, Generation: session.Generation})
			server, err := computeragent.NewServer(guard, probes, newLinkBrowserController(t, computeragent.Identity{UserID: session.UserID, ComputerID: session.ComputerID, NodeID: session.NodeID, Generation: session.Generation}), func(peer agentchannel.Peer) error {
				if peer.PID != int32(os.Getpid()) || peer.UID != 0 || peer.GID != 0 {
					return errors.New("测试进程身份错误")
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
			listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: "/proc/self/fd/" + strconv.Itoa(int(parent.Fd())) + "/" + computeragent.SocketName, Net: "unixpacket"})
			if err != nil {
				t.Fatal(errors.Join(err, parent.Close()))
			}
			if err = os.Chmod(filepath.Join(resource.ControlPath, computeragent.SocketName), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			served, watched := make(chan error, 1), make(chan error, 1)
			go func() { served <- server.Serve(ctx, listener) }()
			go func() { watched <- guard.Serve(ctx) }()
			t.Cleanup(func() {
				cancel()
				for _, result := range []<-chan error{served, watched} {
					select {
					case err := <-result:
						if !errors.Is(err, context.Canceled) {
							t.Error(err)
						}
					case <-time.After(2 * time.Second):
						t.Error("协议联合测试未结束")
					}
				}
				if err := guard.Shutdown(context.Background()); err != nil {
					t.Error(err)
				}
				if err := parent.Close(); err != nil {
					t.Error(err)
				}
			})
			groups := &linkGroups{fakeGroups: &fakeGroups{path: t.TempDir()}, rejectPeer: reject}
			controller, err := NewSessions(context.Background(), fixture.node, groups, sessionOptions(), sessionLogger())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// 错误对端分支先证明拒绝，再恢复测试归属以完成真实协议清理。
				groups.rejectPeer = false
				if err := controller.Shutdown(context.Background()); err != nil {
					t.Error(err)
				}
			})
			command := commandFor(session)
			observation, err := controller.Open(context.Background(), command)
			if reject {
				if err == nil {
					t.Fatal("其他容器 root 对端获得授权")
				}
				state, err := store.Load(context.Background())
				if err != nil || state != nil {
					t.Fatal("对端拒绝之前已下发授权", err)
				}
				return
			}
			if err != nil || !observation.Matches(command, "active") {
				t.Fatal(observation, err)
			}
			other := commandFor(otherDesktop(session, 2))
			if _, err = controller.Open(context.Background(), other); err != nil {
				t.Fatal("同一容器第二桌面未获授权", err)
			}
			command.Revision++
			if _, err = controller.Renew(context.Background(), command); err != nil {
				t.Fatal(err)
			}
			state, err := store.Load(context.Background())
			if err != nil || state == nil || state.Command != command || state.Phase != "active" {
				t.Fatal(state, err)
			}
			if observation, err = controller.Close(context.Background(), command); err != nil || !observation.Matches(command, "drained") {
				t.Fatal(observation, err)
			}
			state, err = store.Load(context.Background())
			if err != nil || state.Phase != "closed" || groups.drains != 1 {
				t.Fatal(state, err)
			}
			otherStore, err := catalog.Desktop(other.Session.DesktopID)
			if err != nil {
				t.Fatal(err)
			}
			state, err = otherStore.Load(context.Background())
			if err != nil || state == nil || state.Command != other || state.Phase != "active" {
				t.Fatal("首个桌面关闭影响了另一桌面", err)
			}
			other.Revision++
			if _, err = controller.Renew(context.Background(), other); err != nil {
				t.Fatal(err)
			}
			if _, err = controller.Close(context.Background(), other); err != nil || groups.drains != 2 {
				t.Fatal(err)
			}
		})
	}
}
