package nodecatalog

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

// verificationRuntime 是生命周期边界替身；成功只用于检验发布规则，不代表真实 Docker 验收通过。
type verificationRuntime struct {
	mu                                               sync.Mutex
	heartbeat                                        computer.NodeHeartbeat
	capabilities                                     computer.Capabilities
	present                                          bool
	ensureCalls, removeCalls, deleteCalls            int
	unknownCreation, unknownRemoval, inspectionFails bool
	identities                                       []computer.Specification
}

func (node *verificationRuntime) Heartbeat(context.Context, computer.NodeQuery) (computer.NodeHeartbeat, error) {
	node.mu.Lock()
	defer node.mu.Unlock()
	return node.heartbeat, nil
}

func (node *verificationRuntime) observation(spec computer.Specification) computer.Observation {
	result := computer.Observation{OperationID: spec.OperationID, UserID: spec.UserID, ComputerID: spec.ComputerID, Generation: spec.Generation, FencingToken: spec.FencingToken, VolumeReference: spec.VolumeReference, State: "absent"}
	if node.present {
		result.State = "ready"
		result.ContainerID = strings.Repeat("a", 64)
		result.Capabilities = node.capabilities
	}
	return result
}

func (node *verificationRuntime) Inspect(_ context.Context, spec computer.Specification) (computer.Observation, error) {
	node.mu.Lock()
	defer node.mu.Unlock()
	if node.inspectionFails {
		return computer.Observation{}, errors.New("测试状态未知")
	}
	return node.observation(spec), nil
}

func (node *verificationRuntime) Ensure(_ context.Context, spec computer.Specification) (computer.Observation, error) {
	node.mu.Lock()
	defer node.mu.Unlock()
	node.ensureCalls++
	node.present = true
	node.identities = append(node.identities, spec)
	if node.unknownCreation {
		node.unknownCreation = false
		return computer.Observation{}, context.DeadlineExceeded
	}
	return node.observation(spec), nil
}

func (node *verificationRuntime) Remove(_ context.Context, spec computer.Specification) (computer.Observation, error) {
	node.mu.Lock()
	defer node.mu.Unlock()
	node.removeCalls++
	if node.present {
		node.deleteCalls++
	}
	node.present = false
	node.identities = append(node.identities, spec)
	if node.unknownRemoval {
		node.unknownRemoval = false
		return computer.Observation{}, context.DeadlineExceeded
	}
	return node.observation(spec), nil
}

func verifierFixture(t *testing.T, control *postgres.Database) (*Verifier, *verificationRuntime) {
	t.Helper()
	service, registration, heartbeat := heartbeatFixture(t, control)
	started, err := service.observationStart(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = service.recordHeartbeat(t.Context(), started, heartbeat, time.Minute); err != nil {
		t.Fatal(err)
	}
	node := &verificationRuntime{heartbeat: heartbeat, capabilities: computer.Capabilities{Terminal: true, Browser: true, Files: true, ProcessReaping: true, DiskQuota: true, NetworkIsolation: true}}
	options := VerificationOptions{ImageID: registration.ImageID, Resources: computer.Resources{CPUMillis: 1000, MemoryBytes: 2 << 30, DiskBytes: 1 << 30, ProcessLimit: 512}, CallTimeout: time.Second, LeaseDuration: 5 * time.Second, Interval: time.Minute, Lifetime: 2 * time.Minute, RetryDelay: time.Second, MaxAttempts: 3}
	verifier, err := NewVerifier(service, node, options, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return verifier, node
}

func processVerification(t *testing.T, verifier *Verifier) {
	t.Helper()
	if claimed, err := verifier.ProcessNext(t.Context()); err != nil || !claimed {
		t.Fatal("验收步骤未执行", claimed, err)
	}
}

func verificationState(t *testing.T, pool *sql.DB, verifier *Verifier) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(`SELECT state FROM tombot.node_verifications WHERE node_id=$1`, verifier.service.nodeID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func makeVerificationDue(t *testing.T, pool *sql.DB, verifier *Verifier) {
	t.Helper()
	execute(t, pool, `UPDATE tombot.node_verifications SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE node_id=$1`, verifier.service.nodeID)
}

func TestNodeVerificationLifecycle(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, name := cluster.Database(t)
	if err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	control := catalogDatabase(t, cluster, name, postgres.ControlRole)
	t.Run("清理前不就绪及镜像和期限准入", func(t *testing.T) {
		verifier, node := verifierFixture(t, control)
		processVerification(t, verifier)
		catalog, err := verifier.service.Get(t.Context(), verifier.options.ImageID)
		if err != nil || catalog.Node.Status != "bootstrapping" || verificationState(t, pool, verifier) != "cleaning" {
			t.Fatal(catalog, err)
		}
		var users int
		if err = pool.QueryRow(`SELECT count(*) FROM tombot.users`).Scan(&users); err != nil || users != 0 {
			t.Fatal("验收创建了业务账户", err)
		}
		processVerification(t, verifier)
		catalog, err = verifier.service.Get(t.Context(), verifier.options.ImageID)
		if err != nil || catalog.Node.Status != "ready" || verificationState(t, pool, verifier) != "succeeded" || node.present {
			t.Fatal(catalog, err)
		}
		if node.ensureCalls != 1 || node.removeCalls != 1 || node.identities[0].UserID != node.identities[1].UserID || node.identities[0].OperationID != node.identities[1].OperationID {
			t.Fatal("未清理同一验收资源")
		}
		var returned string
		if err = pool.QueryRow(`SELECT tombot.reserve_computer_node($1,$2)`, verifier.service.nodeID, verifier.options.ImageID).Scan(&returned); err != nil || returned != verifier.service.nodeID {
			t.Fatal("完整验收后无法准入", err)
		}
		other := registrationFixture()
		execute(t, pool, `INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2)`, other.ImageID, "example.test/"+strings.ToLower(other.ImageID)+"@sha256:"+strings.Repeat("b", 64))
		if err = pool.QueryRow(`SELECT tombot.reserve_computer_node($1,$2)`, verifier.service.nodeID, other.ImageID).Scan(&returned); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("未验收的镜像获得准入", err)
		}
		execute(t, pool, `UPDATE tombot.computer_nodes SET capability_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, verifier.service.nodeID)
		if err = pool.QueryRow(`SELECT tombot.reserve_computer_node($1,$2)`, verifier.service.nodeID, verifier.options.ImageID).Scan(&returned); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("能力证据过期仍准入", err)
		}
		catalog, err = verifier.service.Get(t.Context(), verifier.options.ImageID)
		if err != nil || catalog.Node.Status != "bootstrapping" {
			t.Fatal("能力过期仍显示就绪", catalog, err)
		}
	})
	t.Run("缺少浏览器能力时清理并失败", func(t *testing.T) {
		verifier, node := verifierFixture(t, control)
		node.capabilities.Browser = false
		processVerification(t, verifier)
		processVerification(t, verifier)
		if verificationState(t, pool, verifier) != "failed" || node.present || node.removeCalls != 1 {
			t.Fatal("未完整验收或未清理仍成功")
		}
		catalog, err := verifier.service.Get(t.Context(), verifier.options.ImageID)
		if err != nil || catalog.Node.Status != "bootstrapping" {
			t.Fatal(catalog, err)
		}
		if claimed, err := verifier.ProcessNext(t.Context()); err != nil || claimed {
			t.Fatal("失败没有有界检查间隔", err)
		}
	})
	t.Run("创建和删除未知结果先对账", func(t *testing.T) {
		verifier, node := verifierFixture(t, control)
		node.unknownCreation = true
		node.unknownRemoval = true
		processVerification(t, verifier)
		makeVerificationDue(t, pool, verifier)
		processVerification(t, verifier)
		processVerification(t, verifier)
		makeVerificationDue(t, pool, verifier)
		processVerification(t, verifier)
		if verificationState(t, pool, verifier) != "succeeded" || node.ensureCalls != 1 || node.removeCalls != 2 || node.deleteCalls != 1 {
			t.Fatal("未知结果导致重复副作用")
		}
	})
	t.Run("并发工作者不重复创建", func(t *testing.T) {
		verifier, node := verifierFixture(t, control)
		var wait sync.WaitGroup
		failures := make(chan error, 12)
		for range 12 {
			wait.Go(func() { _, err := verifier.ProcessNext(t.Context()); failures <- err })
		}
		wait.Wait()
		close(failures)
		for err := range failures {
			if err != nil {
				t.Fatal(err)
			}
		}
		for range 2 {
			if _, err := verifier.ProcessNext(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if verificationState(t, pool, verifier) != "succeeded" || node.ensureCalls != 1 || node.removeCalls != 1 {
			t.Fatal("并发验收重复分配")
		}
	})
	t.Run("查询持续失败保留隔离记录", func(t *testing.T) {
		verifier, node := verifierFixture(t, control)
		node.inspectionFails = true
		for range verifier.options.MaxAttempts {
			processVerification(t, verifier)
			makeVerificationDue(t, pool, verifier)
		}
		if verificationState(t, pool, verifier) != "quarantined" {
			t.Fatal("未知状态无限重试")
		}
		if claimed, err := verifier.ProcessNext(t.Context()); err != nil || claimed || node.ensureCalls != 0 {
			t.Fatal("隔离后创建替代资源", err)
		}
	})
	t.Run("旧租约回包不能覆盖新控制者", func(t *testing.T) {
		verifier, _ := verifierFixture(t, control)
		old, err := verifier.claim(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		execute(t, pool, `UPDATE tombot.node_verifications SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE node_id=$1`, verifier.service.nodeID)
		current, err := verifier.claim(t.Context())
		if err != nil || current == nil || current.specification.FencingToken <= old.specification.FencingToken {
			t.Fatal("未接管过期租约", err)
		}
		if err = verifier.advance(t.Context(), old, "cleaning", ""); !errors.Is(err, ErrVerificationLease) {
			t.Fatal("旧控制者写入成功", err)
		}
	})
	t.Run("下一轮复用独立身份且提升代次", func(t *testing.T) {
		verifier, node := verifierFixture(t, control)
		processVerification(t, verifier)
		processVerification(t, verifier)
		makeVerificationDue(t, pool, verifier)
		processVerification(t, verifier)
		first, next := node.identities[0], node.identities[2]
		if first.UserID != next.UserID || first.ComputerID != next.ComputerID || first.OperationID != next.OperationID || next.Generation != first.Generation+1 || next.Attempt != first.Attempt+1 || next.FencingToken <= first.FencingToken {
			t.Fatal("验收资源身份或代次复用错误")
		}
	})
	t.Run("从未创建的资源也须确认关闭代次", func(t *testing.T) {
		verifier, node := verifierFixture(t, control)
		claimed, err := verifier.claim(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err = verifier.advance(t.Context(), claimed, "cleaning", "capability_check_failed"); err != nil {
			t.Fatal(err)
		}
		processVerification(t, verifier)
		if verificationState(t, pool, verifier) != "failed" || node.removeCalls != 1 || node.deleteCalls != 0 {
			t.Fatal("物理不存在被当作代次已关闭")
		}
		makeVerificationDue(t, pool, verifier)
		processVerification(t, verifier)
		if node.identities[1].Generation != 2 || node.identities[1].Attempt != 2 || node.identities[1].OperationID != claimed.specification.OperationID {
			t.Fatal("未按正式节点契约开启下一代")
		}
	})
	t.Run("验收行锁等待后重查租约", func(t *testing.T) {
		verifier, _ := verifierFixture(t, control)
		claimed, err := verifier.claim(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		execute(t, pool, `UPDATE tombot.node_verifications SET lease_expires_at=clock_timestamp()+interval '100 milliseconds' WHERE node_id=$1`, verifier.service.nodeID)
		tx, err := pool.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				t.Error(err)
			}
		}()
		if _, err = tx.Exec(`SELECT node_id FROM tombot.node_verifications WHERE node_id=$1 FOR UPDATE`, verifier.service.nodeID); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- verifier.advance(t.Context(), claimed, "cleaning", "") }()
		timer := time.NewTimer(180 * time.Millisecond)
		<-timer.C
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		select {
		case err = <-done:
			if !errors.Is(err, ErrVerificationLease) {
				t.Fatal("锁等待后使用已过期租约", err)
			}
		case <-time.After(time.Second):
			t.Fatal("验收提交未退出")
		}
	})
	t.Run("取消不遗留工作循环", func(t *testing.T) {
		verifier, node := verifierFixture(t, control)
		node.inspectionFails = true
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := verifier.Run(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal("验收循环吞掉取消", err)
		}
		if node.ensureCalls != 0 {
			t.Fatal("取消后仍创建资源")
		}
	})
	t.Run("验收记录不能指向业务账户", func(t *testing.T) {
		verifier, node := verifierFixture(t, control)
		processVerification(t, verifier)
		owner := node.identities[0].UserID
		execute(t, pool, `INSERT INTO tombot.users(id,username,display_name,status) VALUES($1,$2,'业务账户','provisioning')`, owner, strings.ToLower(owner))
		if _, err := verifier.ProcessNext(t.Context()); !errors.Is(err, ErrConflict) {
			t.Fatal("验收记录覆盖了业务归属", err)
		}
		if node.removeCalls != 0 {
			t.Fatal("验收删除了业务资源")
		}
	})
}

func TestNodeVerificationInvalidation(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, name := cluster.Database(t)
	if err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	control := catalogDatabase(t, cluster, name, postgres.ControlRole)
	for _, scenario := range []string{"进程重启", "心跳中断恢复", "排空", "镜像停用", "能力过期", "未来能力时间", "实际进程已改变", "验收配置已改变"} {
		t.Run(scenario, func(t *testing.T) {
			verifier, node := verifierFixture(t, control)
			processVerification(t, verifier)
			switch scenario {
			case "进程重启", "心跳中断恢复":
				if scenario == "进程重启" {
					node.heartbeat.InstanceID = ids.New()
					node.heartbeat.Generation++
				} else {
					execute(t, pool, `UPDATE tombot.computer_nodes SET control_sequence=2,last_seen_at=clock_timestamp()-interval '2 minutes',control_expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, verifier.service.nodeID)
				}
				node.heartbeat.Sequence = 3
				started, err := verifier.service.observationStart(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if err = verifier.service.recordHeartbeat(t.Context(), started, node.heartbeat, time.Minute); err != nil {
					t.Fatal(err)
				}
			case "排空":
				execute(t, pool, `UPDATE tombot.computer_nodes SET status='draining' WHERE id=$1`, verifier.service.nodeID)
			case "镜像停用":
				execute(t, pool, `UPDATE tombot.computer_images SET is_enabled=false WHERE id=$1`, verifier.options.ImageID)
			case "能力过期":
				execute(t, pool, `UPDATE tombot.node_verifications SET verified_at=clock_timestamp()-interval '5 minutes' WHERE node_id=$1`, verifier.service.nodeID)
			case "未来能力时间":
				execute(t, pool, `UPDATE tombot.node_verifications SET verified_at=clock_timestamp()+interval '5 minutes' WHERE node_id=$1`, verifier.service.nodeID)
			case "实际进程已改变":
				node.heartbeat.InstanceID = ids.New()
				node.heartbeat.Generation++
			case "验收配置已改变":
				verifier.options.Resources.DiskBytes *= 2
			}
			processVerification(t, verifier)
			catalog, err := verifier.service.Get(t.Context(), verifier.options.ImageID)
			if err != nil || catalog.Node.Status == "ready" || verificationState(t, pool, verifier) != "failed" || node.present {
				t.Fatal("旧验收证据被发布或未清理", catalog, err)
			}
			if scenario == "排空" && catalog.Node.Status != "draining" {
				t.Fatal("验收解除排空")
			}
		})
	}
}
