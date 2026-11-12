//go:build linux && egressintegration

package dockernode

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/processgroup"
)

// TestKernelNodeEgressLifecycle 使用真实私有端点与代理；Docker 和 cgroup 定位替身不冒充宿主内核验收。
func TestKernelNodeEgressLifecycle(t *testing.T) {
	if os.Geteuid() != 0 || os.Getenv("TOM_EGRESS_TEST") != "1" {
		t.Fatal("节点出网验收必须在专用受限 root 容器运行")
	}
	directory, err := os.MkdirTemp("/run", "egress-node-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	fixture := newFixture(t)
	if err := fixture.node.journal.root.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.node.journal, err = openJournal(directory, 0)
	if err != nil {
		t.Fatal(err)
	}
	fixture.node.options.StateDirectory = directory
	if _, err := fixture.node.Ensure(t.Context(), fixture.specification); err != nil {
		t.Fatal(err)
	}
	resource := fixture.node.resource(fixture.specification)
	current := fixture.engine.containers[resource.Name]
	current.State.Pid = os.Getpid()
	fixture.engine.containers[resource.Name] = current
	controller := newEgress(fixture.node, sessionLogger())
	var checks atomic.Int32
	locate := func(ctx context.Context, id string, pid int) (processgroup.ContainerReference, error) {
		checks.Add(1)
		if pid != os.Getpid() {
			return processgroup.ContainerReference{}, errors.New("测试拒绝其他进程")
		}
		return processgroup.ContainerReference{ContainerID: id, Path: "docker/" + id, Device: 1, Inode: 2}, ctx.Err()
	}
	controller.locate = locate
	fixture.node.egress = controller
	if _, err := fixture.node.Inspect(t.Context(), fixture.specification); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(resource.ControlPath, computeragent.EgressSocketName)
	assertProxy := func() {
		t.Helper()
		connection, err := net.DialTimeout("unix", path, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := connection.Close(); err != nil {
				t.Error(err)
			}
		}()
		if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(connection, "GET http://127.0.0.1/ HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(bufio.NewReader(connection), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusForbidden {
			t.Fatal("通过实例身份后没有阻断环回目标", response.StatusCode)
		}
	}
	assertProxy()
	if checks.Load() < 3 {
		t.Fatal("真实连接未复验容器主进程与对端")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	if err := controller.shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	controller = newEgress(fixture.node, sessionLogger())
	controller.locate = locate
	fixture.node.egress = controller
	if err := fixture.node.restoreEgress(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertProxy()
	connection, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.node.Remove(t.Context(), fixture.specification); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	if _, err := connection.Read(one[:]); err == nil {
		t.Fatal("删除电脑后连接仍可用")
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("删除后遗留出网端点", err)
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatal("删除了持久端点锁", err)
	}
	if fixture.storage.removals != 1 {
		t.Fatal("出网收尾后没有完成生命周期删除")
	}
}
