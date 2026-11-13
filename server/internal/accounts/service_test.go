package accounts

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/identity"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

type testAuthorizer struct {
	userID string
	denied bool
}

func (authorizer *testAuthorizer) RequireAdmin(_ context.Context, principal identity.Principal, _ bool) error {
	if authorizer.denied || principal.UserID == "" || principal.UserID != authorizer.userID || principal.Scope != "admin" {
		return identity.ErrForbidden
	}
	return nil
}

type testRuntime struct {
	mu                sync.Mutex
	resources         map[string]string
	ensures           int
	removals          int
	ensureError       error
	inspectError      error
	removeError       error
	missingCapability bool
	wrongOwner        bool
	started           chan struct{}
	release           chan struct{}
}

func observation(specification computer.Specification, state string) computer.Observation {
	value := computer.Observation{OperationID: specification.OperationID, UserID: specification.UserID, ComputerID: specification.ComputerID,
		Generation: specification.Generation, FencingToken: specification.FencingToken, State: state, VolumeReference: specification.VolumeReference,
		Capabilities: computer.Capabilities{Terminal: true, Browser: true, Files: true, ProcessReaping: true, DiskQuota: true, NetworkIsolation: true}}
	if state != "absent" {
		value.ContainerID = secure.Hash(specification.ComputerID)
	}
	return value
}

func (runtime *testRuntime) Inspect(_ context.Context, specification computer.Specification) (computer.Observation, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.inspectError != nil {
		return computer.Observation{}, runtime.inspectError
	}
	state := runtime.resources[specification.ComputerID]
	if state == "" {
		state = "absent"
	}
	value := observation(specification, state)
	if runtime.wrongOwner {
		value.UserID = ids.New()
	}
	if runtime.missingCapability {
		value.Capabilities.Browser = false
	}
	return value, nil
}

func (runtime *testRuntime) Ensure(ctx context.Context, specification computer.Specification) (computer.Observation, error) {
	runtime.mu.Lock()
	runtime.ensures++
	started, release := runtime.started, runtime.release
	runtime.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		case <-ctx.Done():
			return computer.Observation{}, ctx.Err()
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return computer.Observation{}, ctx.Err()
		}
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state := "ready"
	var permanent *computer.Failure
	if errors.As(runtime.ensureError, &permanent) && permanent.IsPermanent {
		state = "present"
	}
	runtime.resources[specification.ComputerID] = state
	value := observation(specification, state)
	if runtime.missingCapability {
		value.Capabilities.Browser = false
	}
	return value, runtime.ensureError
}

func (runtime *testRuntime) Remove(_ context.Context, specification computer.Specification) (computer.Observation, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.removals++
	if runtime.removeError != nil {
		return computer.Observation{}, runtime.removeError
	}
	delete(runtime.resources, specification.ComputerID)
	return observation(specification, "absent"), nil
}

type fixture struct {
	pool                  *sql.DB
	service               *Service
	runtime               *testRuntime
	authorizer            *testAuthorizer
	actor                 identity.Principal
	nodeID, imageID, name string
	cluster               *pgtest.Cluster
}

func execute(t *testing.T, pool *sql.DB, query string, arguments ...any) {
	t.Helper()
	if _, err := pool.Exec(query, arguments...); err != nil {
		t.Fatal("准备或检查开户测试记录失败", err)
	}
}

func newFixture(t *testing.T, cluster *pgtest.Cluster, withAdmin bool) fixture {
	t.Helper()
	pool, name := cluster.Database(t)
	if err := postgres.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	role := cluster.Login(t, string(postgres.ControlRole))
	database, err := postgres.Open(context.Background(), postgres.Options{DSN: cluster.DSN(name, role), Role: postgres.ControlRole, MaxConnections: 8,
		ConnectTimeout: 5 * time.Second, StatementTimeout: 5 * time.Second, LockTimeout: time.Second, TransactionTimeout: 10 * time.Second, ConnectionLifetime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})
	runtime := &testRuntime{resources: make(map[string]string)}
	authorizer := &testAuthorizer{}
	service, err := New(database, runtime, authorizer, Options{WorkerID: ids.New(), Workers: 2, CallTimeout: time.Second, LeaseDuration: 3 * time.Second, OperationTimeout: 10 * time.Second,
		RequestWait: 30 * time.Millisecond, PollInterval: time.Millisecond, RetryDelay: time.Millisecond, MaxReconciliations: 3, ActivationLifetime: time.Hour, ModelConcurrency: 2, DesktopConcurrency: 2, QueuedRuns: 100})
	if err != nil {
		t.Fatal(err)
	}
	fixture := fixture{pool: pool, service: service, runtime: runtime, authorizer: authorizer, nodeID: ids.New(), imageID: ids.New(), name: name, cluster: cluster}
	execute(t, pool, `INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2)`, fixture.imageID, "registry.example.test/computer@sha256:"+secure.Hash(ids.New()))
	execute(t, pool, `INSERT INTO tombot.computer_nodes(id,name,status,capacity,runtime_id,control_sequence,last_seen_at,control_expires_at,verified_image_id,capability_expires_at) VALUES($1,$1,'ready',20,$1,1,clock_timestamp(),clock_timestamp()+interval '5 minutes',$2,clock_timestamp()+interval '15 minutes')`, fixture.nodeID, fixture.imageID)
	if withAdmin {
		admin, node, computerID := ids.New(), ids.New(), ids.New()
		execute(t, pool, `INSERT INTO tombot.users(id,username,display_name,status) VALUES($1,$2,'管理员测试快照','provisioning')`, admin, strings.ToLower(admin))
		execute(t, pool, `INSERT INTO tombot.computer_nodes(id,name,status,capacity,reserved,runtime_id,control_sequence,last_seen_at,control_expires_at,verified_image_id,capability_expires_at) VALUES($1,$1,'ready',1,1,$1,1,clock_timestamp(),clock_timestamp()+interval '5 minutes',$2,clock_timestamp()+interval '15 minutes')`, node, fixture.imageID)
		execute(t, pool, `INSERT INTO tombot.computers(user_id,id,node_id,image_id,status,container_id,volume_ref,cpu_millis,memory_bytes,disk_bytes,process_limit)
			VALUES($1,$2,$3,$4,'ready',$2,$2,1000,1073741824,1073741824,128)`, admin, computerID, node, fixture.imageID)
		execute(t, pool, `UPDATE tombot.users SET status='active' WHERE id=$1`, admin)
		fixture.actor = identity.Principal{UserID: admin, Role: "admin", Scope: "admin"}
		authorizer.userID = admin
	}
	return fixture
}

func (fixture fixture) request() CreateRequest {
	return CreateRequest{Username: strings.ToLower(ids.New()), DisplayName: "新用户", NodeID: fixture.nodeID, ImageID: fixture.imageID,
		Resources: computer.Resources{CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 2 << 30, ProcessLimit: 128}, IdempotencyKey: ids.New(), RequestID: ids.New()}
}

func (fixture fixture) reserve(t *testing.T) Operation {
	t.Helper()
	operation, err := fixture.service.reserve(context.Background(), fixture.actor.UserID, fixture.request())
	if err != nil {
		t.Fatal(err)
	}
	return operation
}

func (fixture fixture) process(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		claimed, err := fixture.service.ProcessNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if claimed {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("没有领取到预期的开户操作")
		case <-time.After(time.Millisecond):
		}
	}
}

func (fixture fixture) assertState(t *testing.T, operation Operation, status, userStatus, computerStatus string, reserved int) {
	t.Helper()
	var actualStatus, actualUser, actualComputer string
	var actualReserved int
	err := fixture.pool.QueryRow(`SELECT o.status,u.status,c.status,n.reserved FROM tombot.user_creation_operations o JOIN tombot.users u ON u.id=o.user_id
		JOIN tombot.computers c ON c.user_id=u.id JOIN tombot.computer_nodes n ON n.id=c.node_id WHERE o.id=$1`, operation.ID).Scan(&actualStatus, &actualUser, &actualComputer, &actualReserved)
	if err != nil {
		t.Fatal(err)
	}
	if actualStatus != status || actualUser != userStatus || actualComputer != computerStatus || actualReserved != reserved {
		t.Fatalf("开户状态不符：%s %s %s %d", actualStatus, actualUser, actualComputer, actualReserved)
	}
}

func (fixture fixture) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- fixture.service.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Error("开户工作池异常退出", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("开户工作池未停止")
		}
	})
}

func TestAccountProvisioningIntegration(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("同步成功必须等待电脑能力就绪", func(t *testing.T) {
		fixture := newFixture(t, cluster, true)
		fixture.service.options.RequestWait = 2 * time.Second
		fixture.runtime.started = make(chan struct{}, 1)
		fixture.runtime.release = make(chan struct{})
		fixture.start(t)
		type result struct {
			operation Operation
			err       error
		}
		returned := make(chan result, 1)
		go func() {
			operation, err := fixture.service.Create(ctx, fixture.actor, fixture.request())
			returned <- result{operation, err}
		}()
		select {
		case <-fixture.runtime.started:
		case <-time.After(time.Second):
			t.Fatal("开户没有调用节点")
		}
		select {
		case <-returned:
			t.Fatal("电脑尚未就绪就返回创建结果")
		default:
		}
		close(fixture.runtime.release)
		select {
		case result := <-returned:
			if result.err != nil {
				t.Fatal(result.err)
			}
			fixture.assertState(t, result.operation, "succeeded", "pending_activation", "ready", 1)
		case <-time.After(3 * time.Second):
			t.Fatal("就绪后未返回创建结果")
		}
	})
	t.Run("请求断开后持久操作继续且重复请求不重建", func(t *testing.T) {
		fixture := newFixture(t, cluster, true)
		fixture.runtime.started = make(chan struct{}, 1)
		fixture.runtime.release = make(chan struct{})
		fixture.start(t)
		request := fixture.request()
		operation, err := fixture.service.Create(ctx, fixture.actor, request)
		if !errors.Is(err, ErrPending) || operation.ID == "" {
			t.Fatal("等待超时没有返回同一操作", err)
		}
		close(fixture.runtime.release)
		fixture.service.options.RequestWait = time.Second
		repeated, err := fixture.service.Create(ctx, fixture.actor, request)
		if err != nil || repeated.ID != operation.ID || repeated.UserID != operation.UserID {
			t.Fatal("重连没有复用原操作", err)
		}
		fixture.runtime.mu.Lock()
		calls := fixture.runtime.ensures
		fixture.runtime.mu.Unlock()
		if calls != 1 {
			t.Fatal("重复请求重复供给", calls)
		}
	})
	t.Run("并发幂等及用户名冲突完整回滚", func(t *testing.T) {
		fixture := newFixture(t, cluster, true)
		request := fixture.request()
		type result struct {
			operation Operation
			err       error
		}
		results := make(chan result, 8)
		for range 8 {
			go func() {
				operation, err := fixture.service.reserve(ctx, fixture.actor.UserID, request)
				results <- result{operation, err}
			}()
		}
		var id string
		for range 8 {
			result := <-results
			if result.err != nil {
				t.Fatal(result.err)
			}
			if id != "" && id != result.operation.ID {
				t.Fatal("同一请求产生多个操作")
			}
			id = result.operation.ID
		}
		changed := request
		changed.DisplayName = "另一请求"
		if _, err := fixture.service.reserve(ctx, fixture.actor.UserID, changed); !errors.Is(err, ErrConflict) {
			t.Fatal("相同幂等键接受不同内容", err)
		}
		changed = request
		changed.IdempotencyKey = ids.New()
		if _, err := fixture.service.reserve(ctx, fixture.actor.UserID, changed); !errors.Is(err, ErrConflict) {
			t.Fatal("重复用户名未拒绝", err)
		}
		var reserved, operations int
		if err := fixture.pool.QueryRow(`SELECT reserved,(SELECT count(*) FROM tombot.user_creation_operations) FROM tombot.computer_nodes WHERE id=$1`, fixture.nodeID).Scan(&reserved, &operations); err != nil {
			t.Fatal(err)
		}
		if reserved != 1 || operations != 1 {
			t.Fatal("冲突请求泄漏容量或账户", reserved, operations)
		}
	})
	t.Run("容量与权限在建账户前检查", func(t *testing.T) {
		fixture := newFixture(t, cluster, true)
		fixture.authorizer.denied = true
		if _, err := fixture.service.Create(ctx, fixture.actor, fixture.request()); !errors.Is(err, identity.ErrForbidden) {
			t.Fatal("未拒绝无权限创建", err)
		}
		fixture.authorizer.denied = false
		execute(t, fixture.pool, `UPDATE tombot.computer_nodes SET status='offline' WHERE id=$1`, fixture.nodeID)
		if _, err := fixture.service.Create(ctx, fixture.actor, fixture.request()); !errors.Is(err, ErrNoCapacity) {
			t.Fatal("离线节点仍受理开户", err)
		}
		var count int
		if err := fixture.pool.QueryRow(`SELECT count(*) FROM tombot.user_creation_operations`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatal("拒绝前已写入开户记录")
		}
	})
	t.Run("节点成功后回包丢失先对账", func(t *testing.T) {
		fixture := newFixture(t, cluster, true)
		operation := fixture.reserve(t)
		fixture.runtime.ensureError = context.DeadlineExceeded
		fixture.process(t)
		fixture.assertState(t, operation, "reconciling", "provisioning", "provisioning", 1)
		fixture.process(t)
		fixture.assertState(t, operation, "succeeded", "pending_activation", "ready", 1)
		if fixture.runtime.ensures != 1 {
			t.Fatal("结果未知时重复创建电脑")
		}
	})
	t.Run("最终提交失败通过租约恢复已创建电脑", func(t *testing.T) {
		fixture := newFixture(t, cluster, true)
		operation := fixture.reserve(t)
		execute(t, fixture.pool, `REVOKE INSERT ON tombot.admin_audit_events FROM tombot_control`)
		if claimed, err := fixture.service.ProcessNext(ctx); !claimed || err == nil {
			t.Fatal("审计故障未阻断最终提交", err)
		}
		execute(t, fixture.pool, `GRANT INSERT ON tombot.admin_audit_events TO tombot_control`)
		fixture.assertState(t, operation, "running", "provisioning", "provisioning", 1)
		execute(t, fixture.pool, `UPDATE tombot.user_creation_operations SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, operation.ID)
		fixture.process(t)
		fixture.assertState(t, operation, "succeeded", "pending_activation", "ready", 1)
		if fixture.runtime.ensures != 1 {
			t.Fatal("提交恢复重新创建了电脑")
		}
	})
	t.Run("明确失败清理后才释放容量并允许重试", func(t *testing.T) {
		fixture := newFixture(t, cluster, true)
		operation := fixture.reserve(t)
		fixture.runtime.ensureError = &computer.Failure{Code: "probe_failed", IsPermanent: true}
		fixture.process(t)
		fixture.assertState(t, operation, "compensating", "provisioning", "provisioning", 1)
		if _, err := fixture.service.IssueActivation(ctx, fixture.actor, operation.UserID, ids.New()); !errors.Is(err, ErrActivationUnavailable) {
			t.Fatal("失败供给用户可获取激活材料", err)
		}
		fixture.process(t)
		fixture.assertState(t, operation, "failed", "provision_failed", "error", 0)
		if fixture.runtime.removals != 1 {
			t.Fatal("未验证清理")
		}
		if claimed, err := fixture.service.ProcessNext(ctx); claimed || err != nil {
			t.Fatal("失败操作被自动重复消费", err)
		}
		fixture.runtime.ensureError = nil
		retried, err := fixture.service.Retry(ctx, fixture.actor, operation.ID, ids.New())
		if !errors.Is(err, ErrPending) || retried.UserID != operation.UserID || retried.ComputerID != operation.ComputerID || retried.Attempt != 2 {
			t.Fatal("重试没有保留所属资源", err)
		}
		fixture.process(t)
		fixture.assertState(t, operation, "succeeded", "pending_activation", "ready", 1)
		var generation int64
		if err := fixture.pool.QueryRow(`SELECT generation FROM tombot.computers WHERE user_id=$1`, operation.UserID).Scan(&generation); err != nil {
			t.Fatal(err)
		}
		if generation != 2 {
			t.Fatal("重试未提升实例代次")
		}
	})
	t.Run("清理不确定保持容量并隔离", func(t *testing.T) {
		fixture := newFixture(t, cluster, true)
		operation := fixture.reserve(t)
		fixture.runtime.ensureError = &computer.Failure{IsPermanent: true}
		fixture.runtime.removeError = context.DeadlineExceeded
		for range 4 {
			fixture.process(t)
		}
		fixture.assertState(t, operation, "quarantined", "provision_failed", "quarantined", 1)
		if _, err := fixture.service.Retry(ctx, fixture.actor, operation.ID, ids.New()); !errors.Is(err, ErrQuarantined) {
			t.Fatal("未知资源可直接重建", err)
		}
	})
	t.Run("节点回包所属关系和能力必须完整", func(t *testing.T) {
		fixture := newFixture(t, cluster, true)
		operation := fixture.reserve(t)
		fixture.runtime.wrongOwner = true
		fixture.process(t)
		fixture.assertState(t, operation, "quarantined", "provision_failed", "quarantined", 1)
		if fixture.runtime.ensures != 0 || fixture.runtime.removals != 0 {
			t.Fatal("错误资源所属关系触发了创建或删除")
		}
		other := newFixture(t, cluster, true)
		second := other.reserve(t)
		other.runtime.missingCapability = true
		other.process(t)
		other.assertState(t, second, "compensating", "provisioning", "provisioning", 1)
		other.process(t)
		other.assertState(t, second, "failed", "provision_failed", "error", 0)
	})
	t.Run("过期操作不再发起创建", func(t *testing.T) {
		fixture := newFixture(t, cluster, true)
		operation := fixture.reserve(t)
		execute(t, fixture.pool, `UPDATE tombot.user_creation_operations SET deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, operation.ID)
		fixture.process(t)
		fixture.process(t)
		fixture.assertState(t, operation, "failed", "provision_failed", "error", 0)
		if fixture.runtime.ensures != 0 || fixture.runtime.removals != 0 {
			t.Fatal("已核实不存在的过期操作仍修改节点")
		}
	})
	t.Run("过期工作者不能覆盖新租约", func(t *testing.T) {
		fixture := newFixture(t, cluster, true)
		operation := fixture.reserve(t)
		old, err := fixture.service.claimNext(ctx)
		if err != nil || old == nil {
			t.Fatal(err)
		}
		execute(t, fixture.pool, `UPDATE tombot.user_creation_operations SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, operation.ID)
		next, err := fixture.service.claimNext(ctx)
		if err != nil || next == nil {
			t.Fatal(err)
		}
		if next.specification.FencingToken <= old.specification.FencingToken {
			t.Fatal("接管没有提升操作代次")
		}
		if err := fixture.service.succeed(ctx, old, observation(old.specification, "ready")); !errors.Is(err, ErrLeaseLost) {
			t.Fatal("旧工作者仍能提交就绪", err)
		}
		fixture.assertState(t, operation, "running", "provisioning", "provisioning", 1)
		if err := fixture.service.succeed(ctx, next, observation(next.specification, "ready")); err != nil {
			t.Fatal(err)
		}
		fixture.assertState(t, operation, "succeeded", "pending_activation", "ready", 1)
	})
}
