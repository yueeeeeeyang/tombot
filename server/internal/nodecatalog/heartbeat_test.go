package nodecatalog

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func heartbeatFixture(t *testing.T, control *postgres.Database) (*Service, Registration, computer.NodeHeartbeat) {
	t.Helper()
	nodeID := ids.New()
	service, err := New(control, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	registration := registrationFixture()
	registration.Name = nodeID
	registration.ImageReference = "example.test/" + strings.ToLower(nodeID) + "@sha256:" + strings.Repeat("a", 64)
	if _, err := service.Register(t.Context(), registration, ids.New()); err != nil {
		t.Fatal(err)
	}
	return service, registration, computer.NodeHeartbeat{NodeID: nodeID, InstanceID: ids.New(), Generation: 1, Sequence: 1}
}

func TestHeartbeatDatabaseOrderingAndExpiry(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, name := cluster.Database(t)
	if err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	control := catalogDatabase(t, cluster, name, postgres.ControlRole)
	t.Run("不授予就绪及重复乱序", func(t *testing.T) {
		service, registration, heartbeat := heartbeatFixture(t, control)
		started, err := service.observationStart(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := service.recordHeartbeat(t.Context(), started, heartbeat, time.Minute); err != nil {
			t.Fatal(err)
		}
		catalog, err := service.Get(t.Context(), registration.ImageID)
		if err != nil || catalog.Node.Status != "bootstrapping" || catalog.Node.LastSeenAt == nil {
			t.Fatal(catalog, err)
		}
		if err := service.recordHeartbeat(t.Context(), started.Add(time.Second), heartbeat, time.Minute); !errors.Is(err, ErrStaleHeartbeat) {
			t.Fatal("重复心跳延长租约", err)
		}
		var expires time.Time
		if err := pool.QueryRow(`SELECT control_expires_at FROM tombot.computer_nodes WHERE id=$1`, service.nodeID).Scan(&expires); err != nil || !expires.Equal(started.Add(time.Minute)) {
			t.Fatal("重复结果改写期限", err)
		}
		var waiting sync.WaitGroup
		failures := make(chan error, 32)
		for sequence := int64(2); sequence <= 33; sequence++ {
			waiting.Go(func() {
				value := heartbeat
				value.Sequence = sequence
				err := service.recordHeartbeat(t.Context(), started, value, time.Minute)
				if errors.Is(err, ErrStaleHeartbeat) {
					err = nil
				}
				failures <- err
			})
		}
		waiting.Wait()
		close(failures)
		for err := range failures {
			if err != nil {
				t.Fatal(err)
			}
		}
		var sequence int64
		if err := pool.QueryRow(`SELECT control_sequence FROM tombot.computer_nodes WHERE id=$1`, service.nodeID).Scan(&sequence); err != nil || sequence != 33 {
			t.Fatal("并发发布回退了序号", sequence, err)
		}
	})
	t.Run("重启与旧进程", func(t *testing.T) {
		service, registration, heartbeat := heartbeatFixture(t, control)
		started, err := service.observationStart(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := service.recordHeartbeat(t.Context(), started, heartbeat, time.Minute); err != nil {
			t.Fatal(err)
		}
		// 本组预置能力验收结论；心跳本身不能授予它。
		execute(t, pool, `UPDATE tombot.computer_nodes SET status='ready',verified_image_id=$2,capability_expires_at=clock_timestamp()+interval '15 minutes' WHERE id=$1`, service.nodeID, registration.ImageID)
		changed := heartbeat
		changed.InstanceID = ids.New()
		changed.Sequence++
		if err := service.recordHeartbeat(t.Context(), started, changed, time.Minute); !errors.Is(err, ErrConflict) {
			t.Fatal("同代次换进程被接受", err)
		}
		changed.Generation++
		changed.Sequence = 1
		if err := service.recordHeartbeat(t.Context(), started, changed, time.Minute); err != nil {
			t.Fatal(err)
		}
		catalog, err := service.Get(t.Context(), registration.ImageID)
		if err != nil || catalog.Node.Generation != 2 || catalog.Node.Status != "bootstrapping" {
			t.Fatal("重启沿用旧就绪结论", catalog, err)
		}
		heartbeat.Sequence = 100
		if err := service.recordHeartbeat(t.Context(), started, heartbeat, time.Minute); !errors.Is(err, ErrStaleHeartbeat) {
			t.Fatal("旧进程覆盖新代次", err)
		}
		execute(t, pool, `UPDATE tombot.computer_nodes SET status='draining' WHERE id=$1`, service.nodeID)
		changed.Generation++
		changed.InstanceID = ids.New()
		if err := service.recordHeartbeat(t.Context(), started, changed, time.Minute); err != nil {
			t.Fatal(err)
		}
		catalog, err = service.Get(t.Context(), registration.ImageID)
		if err != nil || catalog.Node.Status != "draining" {
			t.Fatal("心跳解除排空", err)
		}
	})
	t.Run("过期和恢复不自动就绪", func(t *testing.T) {
		service, registration, heartbeat := heartbeatFixture(t, control)
		started, err := service.observationStart(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := service.recordHeartbeat(t.Context(), started, heartbeat, time.Minute); err != nil {
			t.Fatal(err)
		}
		execute(t, pool, `UPDATE tombot.computer_nodes SET status='ready',verified_image_id=$2,capability_expires_at=clock_timestamp()+interval '15 minutes',control_sequence=2,last_seen_at=clock_timestamp()-interval '2 minutes',control_expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, service.nodeID, registration.ImageID)
		catalog, err := service.Get(t.Context(), registration.ImageID)
		if err != nil || catalog.Node.Status != "offline" {
			t.Fatal("过期就绪未失效", catalog, err)
		}
		heartbeat.Sequence = 3
		if err := service.recordHeartbeat(t.Context(), started.Add(-2*time.Minute), heartbeat, time.Minute); !errors.Is(err, ErrStaleHeartbeat) {
			t.Fatal("迟到结果重新计时", err)
		}
		if err := service.recordHeartbeat(t.Context(), started, heartbeat, time.Minute); err != nil {
			t.Fatal(err)
		}
		catalog, err = service.Get(t.Context(), registration.ImageID)
		if err != nil || catalog.Node.Status != "bootstrapping" {
			t.Fatal("恢复通信直接恢复就绪", catalog, err)
		}
	})
	t.Run("行锁等待不刷新旧观察", func(t *testing.T) {
		service, _, heartbeat := heartbeatFixture(t, control)
		started, err := service.observationStart(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		transaction, err := pool.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transaction.ExecContext(t.Context(), `SELECT id FROM tombot.computer_nodes WHERE id=$1 FOR UPDATE`, service.nodeID); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			done <- service.recordHeartbeat(t.Context(), started.Add(-900*time.Millisecond), heartbeat, time.Second)
		}()
		timer := time.NewTimer(150 * time.Millisecond)
		<-timer.C
		if err := transaction.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, ErrStaleHeartbeat) {
			t.Fatal("锁等待延长观察有效期", err)
		}
	})
	t.Run("数据库拒绝绕过", func(t *testing.T) {
		service, _, heartbeat := heartbeatFixture(t, control)
		started, err := service.observationStart(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := service.recordHeartbeat(t.Context(), started, heartbeat, time.Minute); err != nil {
			t.Fatal(err)
		}
		for _, change := range []string{"generation=0", "runtime_id=NULL", "runtime_id='01K9AX6VSMKDBZ049G41FT4WHW'", "control_sequence=0", "control_expires_at=control_expires_at+interval '1 second'"} {
			if _, err := pool.Exec(`UPDATE tombot.computer_nodes SET `+change+` WHERE id=$1`, service.nodeID); err == nil {
				t.Fatal("数据库允许绕过控制版本", change)
			}
		}
		if _, err := pool.Exec(`INSERT INTO tombot.computer_nodes(id,name,status,capacity) VALUES($1,$1,'ready',1)`, ids.New()); err == nil {
			t.Fatal("NULL 约束允许无心跳就绪")
		}
	})
}

type heartbeatNode struct {
	sequence atomic.Int64
	instance string
	failure  error
	entered  chan struct{}
}

func (node *heartbeatNode) Heartbeat(ctx context.Context, query computer.NodeQuery) (computer.NodeHeartbeat, error) {
	sequence := node.sequence.Add(1)
	if node.entered != nil {
		select {
		case node.entered <- struct{}{}:
		default:
		}
	}
	return computer.NodeHeartbeat{NodeID: query.NodeID, InstanceID: node.instance, Generation: 1, Sequence: sequence}, errors.Join(node.failure, ctx.Err())
}

func TestHeartbeatObserverAndCancellation(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, name := cluster.Database(t)
	if err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	service, registration, heartbeat := heartbeatFixture(t, catalogDatabase(t, cluster, name, postgres.ControlRole))
	node := &heartbeatNode{instance: heartbeat.InstanceID}
	options := ObserverOptions{CallTimeout: time.Second, Interval: time.Second, Lifetime: 4 * time.Second}
	observer, err := NewObserver(service, node, options, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := observer.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	node.failure = errors.New("测试节点失联")
	if observer.Poll(t.Context()) == nil {
		t.Fatal("失联被当作心跳成功")
	}
	var sequence int64
	if err := pool.QueryRow(`SELECT control_sequence FROM tombot.computer_nodes WHERE id=$1`, service.nodeID).Scan(&sequence); err != nil || sequence != 1 {
		t.Fatal("失败更新了租约", err)
	}
	node.failure = nil
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if observer.Poll(ctx) == nil {
		t.Fatal("取消仍发布观察")
	}
	node.entered = make(chan struct{}, 1)
	ctx, cancel = context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- observer.Run(ctx) }()
	select {
	case <-node.entered:
	case <-time.After(time.Second):
		t.Fatal("心跳监督未运行")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	catalog, err := service.Get(t.Context(), registration.ImageID)
	if err != nil || catalog.Node.Status != "bootstrapping" {
		t.Fatal("监督误报完整就绪", err)
	}
	if _, err := NewObserver(service, node, ObserverOptions{}, observer.logger); err == nil {
		t.Fatal("无界监督配置被接受")
	}
}
