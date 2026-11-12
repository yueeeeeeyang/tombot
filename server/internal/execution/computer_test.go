package execution

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io.yueyang/tom/internal/botagent"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computercontrol"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

// sessionNode 是受控节点协议替身；这些测试验证控制面，不宣称实际 cgroup 或浏览器已清理。
type sessionNode struct {
	mu                              sync.Mutex
	states                          map[string]computer.SessionObservation
	opens, renews, closes           int
	failOpen, failClose, wrongClose bool
}

func (node *sessionNode) call(command computer.SessionCommand, action string) (computer.SessionObservation, error) {
	node.mu.Lock()
	defer node.mu.Unlock()
	if err := command.Validate(); err != nil {
		return computer.SessionObservation{}, err
	}
	if node.states == nil {
		node.states = map[string]computer.SessionObservation{}
	}
	old := node.states[command.Session.DesktopID]
	if old.Session.SessionID != "" {
		if old.Session.FencingToken > command.Session.FencingToken || old.Session.SessionID == command.Session.SessionID && old.Revision > command.Revision {
			return computer.SessionObservation{}, errors.New("旧控制命令")
		}
		if old.Session.SessionID != command.Session.SessionID && old.State != "drained" {
			return computer.SessionObservation{}, errors.New("旧会话仍占用桌面")
		}
		if old.Session.SessionID == command.Session.SessionID && old.State == "drained" && action != "close" {
			return computer.SessionObservation{}, errors.New("已结束会话不能重新打开")
		}
	}
	state := "active"
	switch action {
	case "open":
		node.opens++
	case "renew":
		node.renews++
		if old.Session != command.Session || old.State != "active" {
			return computer.SessionObservation{}, errors.New("会话不可续约")
		}
	case "close":
		node.closes++
		if node.failClose {
			return computer.SessionObservation{}, errors.New("节点清理失联")
		}
		state = "drained"
	}
	observation := computer.SessionObservation{Session: command.Session, Revision: command.Revision, State: state}
	node.states[command.Session.DesktopID] = observation
	if action == "open" && node.failOpen {
		return computer.SessionObservation{}, errors.New("开启回包丢失")
	}
	if action == "close" && node.wrongClose {
		observation.Session.Generation++
	}
	return observation, nil
}
func (node *sessionNode) Open(_ context.Context, command computer.SessionCommand) (computer.SessionObservation, error) {
	return node.call(command, "open")
}
func (node *sessionNode) Renew(_ context.Context, command computer.SessionCommand) (computer.SessionObservation, error) {
	return node.call(command, "renew")
}
func (node *sessionNode) Close(_ context.Context, command computer.SessionCommand) (computer.SessionObservation, error) {
	return node.call(command, "close")
}

func computerOptions() computercontrol.Options {
	return computercontrol.Options{LeaseLifetime: 3 * time.Second, WaitTimeout: 5 * time.Second, HoldTimeout: 20 * time.Second, NodeTimeout: 250 * time.Millisecond, RetryDelay: 100 * time.Millisecond, MaxWaiting: 5, MaxCleanupAttempts: 3}
}
func (fixture fixture) computers(t *testing.T, node computer.SessionRuntime, options computercontrol.Options) *computercontrol.Service {
	t.Helper()
	// 旧运行测试未涉及 Docker；本组使用符合真实引擎格式的容器身份。
	for _, userID := range []string{fixture.a.UserID, fixture.b.UserID} {
		digest := sha256.Sum256([]byte(userID))
		execute(t, fixture.pool, `UPDATE tombot.computers SET container_id=$2 WHERE user_id=$1`, userID, hex.EncodeToString(digest[:]))
	}
	service, err := computercontrol.New(fixture.tenant, fixture.dispatch, node, options, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return service
}
func (fixture fixture) sessionState(t *testing.T, lease Lease) string {
	t.Helper()
	var state string
	if err := fixture.pool.QueryRow(`SELECT status FROM tombot.computer_sessions WHERE user_id=$1 AND run_id=$2 AND job_fencing_token=$3 AND job_attempt=$4`, lease.UserID, lease.RunID, lease.FencingToken, lease.Attempt).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}
func (fixture fixture) allowCleanup(t *testing.T, lease Lease) {
	t.Helper()
	execute(t, fixture.pool, `UPDATE tombot.computer_sessions SET next_check_at=clock_timestamp() WHERE user_id=$1 AND run_id=$2`, lease.UserID, lease.RunID)
}

func TestComputerSessions(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("用户额度为一时，清理失败保留容量且其他用户独立执行", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		execute(t, fixture.pool, `UPDATE tombot.user_limits SET concurrent_desktops=1 WHERE user_id=$1`, fixture.a.UserID)
		node := &sessionNode{}
		computers := fixture.computers(t, node, computerOptions())
		fixture.send(t, fixture.a, fixture.first)
		first := fixture.claim(t)
		fixture.start(t, first)
		fixture.completeModel(t, first)
		fixture.send(t, fixture.a, fixture.second)
		second := fixture.claim(t)
		fixture.start(t, second)
		fixture.send(t, fixture.b, fixture.foreign)
		foreign := fixture.claim(t)
		fixture.start(t, foreign)
		one, err := computers.Acquire(ctx, computerReference(first))
		if err != nil {
			t.Fatal(err)
		}
		_, err = computers.Acquire(ctx, computerReference(second))
		expect(t, err, computercontrol.ErrWaiting)
		other, err := computers.Acquire(ctx, computerReference(foreign))
		if err != nil || other.ComputerID == one.ComputerID {
			t.Fatal("不同用户无法独立执行", err)
		}
		_, err = fixture.service.Complete(ctx, first, Outcome{Status: "succeeded", Answer: "完成"})
		expect(t, err, ErrComputerBusy)
		err = fixture.tenant.Tenant(ctx, first.UserID, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE tombot.jobs SET status='finished',lease_owner=NULL,lease_expires_at=NULL,fencing_token=fencing_token+1 WHERE user_id=$1 AND id=$2`, first.UserID, first.JobID)
			return err
		})
		if err == nil {
			t.Fatal("数据库允许绕过电脑清理完成任务")
		}
		execute(t, fixture.pool, `UPDATE tombot.jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 AND id=$2`, first.UserID, first.JobID)
		if count, err := fixture.service.Recover(ctx, 100); err != nil || count != 0 {
			t.Fatal("清理前恢复了旧任务", count, err)
		}
		node.failClose = true
		if count, err := computers.Reconcile(ctx, 100); !errors.Is(err, computercontrol.ErrNode) || count != 0 {
			t.Fatal("节点失败未保留占用", count, err)
		}
		if fixture.sessionState(t, first) != "quarantined" {
			t.Fatal("失联会话未隔离")
		}
		_, err = computers.Acquire(ctx, computerReference(second))
		expect(t, err, computercontrol.ErrWaiting)
		if count, err := fixture.service.Recover(ctx, 100); err != nil || count != 0 {
			t.Fatal("隔离会话仍被恢复", count, err)
		}
		node.failClose = false
		fixture.allowCleanup(t, first)
		if count, err := computers.Reconcile(ctx, 100); err != nil || count != 1 {
			t.Fatal("节点确认后未释放", count, err)
		}
		if count, err := fixture.service.Recover(ctx, 100); err != nil || count != 1 {
			t.Fatal("释放后未恢复", count, err)
		}
		two, err := computers.Acquire(ctx, computerReference(second))
		if err != nil || two.FencingToken != 1 || two.DesktopID == one.DesktopID {
			t.Fatal("其他桌面的代次未独立分配", err)
		}
		before := node.closes
		if err = computers.Release(ctx, computerReference(first)); err != nil || node.closes != before {
			t.Fatal("迟到清理触及新会话", err)
		}
	})
	t.Run("并发重复申请只开启一次，当前权限与会话绑定复验", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		node := &sessionNode{}
		computers := fixture.computers(t, node, computerOptions())
		fixture.send(t, fixture.a, fixture.first)
		lease := fixture.claim(t)
		fixture.start(t, lease)
		var wait sync.WaitGroup
		failures := make(chan error, 12)
		for range 12 {
			wait.Go(func() {
				_, err := computers.Acquire(ctx, computerReference(lease))
				if err != nil && !errors.Is(err, computercontrol.ErrWaiting) {
					failures <- err
				}
			})
		}
		wait.Wait()
		close(failures)
		for err := range failures {
			t.Fatal(err)
		}
		session, err := computers.Acquire(ctx, computerReference(lease))
		if err != nil || node.opens != 1 {
			t.Fatal("重复打开会话", node.opens, err)
		}
		wrong := computerReference(lease)
		wrong.UserID = fixture.b.UserID
		_, err = computers.Acquire(ctx, wrong)
		expect(t, err, computercontrol.ErrLeaseLost)
		digest := sha256.Sum256([]byte("file-read"))
		request := OperationRequest{StepKey: "tool.1.1", Kind: "computer", ToolName: "file_read", RequestDigest: digest[:], Computer: &session}
		altered := session
		altered.FencingToken++
		request.Computer = &altered
		_, _, err = fixture.service.BeginOperation(ctx, lease, request)
		expect(t, err, ErrLeaseLost)
		request.Computer = &session
		execute(t, fixture.pool, `UPDATE tombot.bots SET tool_permissions='[]' WHERE user_id=$1 AND id=$2`, lease.UserID, lease.BotID)
		_, _, err = fixture.service.BeginOperation(ctx, lease, request)
		expect(t, err, ErrUnavailable)
		execute(t, fixture.pool, `UPDATE tombot.bots SET tool_permissions='["file_read"]' WHERE user_id=$1 AND id=$2`, lease.UserID, lease.BotID)
		operation, created, err := fixture.service.BeginOperation(ctx, lease, request)
		if err != nil || !created {
			t.Fatal(err)
		}
		var stored string
		if err = fixture.pool.QueryRow(`SELECT computer_session_id FROM tombot.run_operations WHERE user_id=$1 AND id=$2`, lease.UserID, operation.ID).Scan(&stored); err != nil || stored != session.SessionID {
			t.Fatal("操作未绑定电脑会话", err)
		}
		if _, err = fixture.service.RecordOperation(ctx, lease, operation.ID, OperationResult{Status: "succeeded", Result: json.RawMessage(`{"content":"Verified contents.","is_error":false}`)}); err != nil {
			t.Fatal(err)
		}
		if err = computers.Release(ctx, computerReference(lease)); err != nil {
			t.Fatal(err)
		}
		_, err = computers.Acquire(ctx, computerReference(lease))
		expect(t, err, computercontrol.ErrUnavailable)
	})
	t.Run("开启回包丢失不能重复开启，错误清理回包不能释放", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		node := &sessionNode{failOpen: true, wrongClose: true}
		computers := fixture.computers(t, node, computerOptions())
		fixture.send(t, fixture.a, fixture.first)
		lease := fixture.claim(t)
		fixture.start(t, lease)
		_, err := computers.Acquire(ctx, computerReference(lease))
		expect(t, err, computercontrol.ErrNode)
		_, err = computers.Acquire(ctx, computerReference(lease))
		expect(t, err, computercontrol.ErrUnavailable)
		if node.opens != 1 {
			t.Fatal("未知开启被重放")
		}
		err = computers.Release(ctx, computerReference(lease))
		expect(t, err, computercontrol.ErrNode)
		if fixture.sessionState(t, lease) != "quarantined" {
			t.Fatal("错误实例清理被接受")
		}
		node.wrongClose = false
		fixture.allowCleanup(t, lease)
		if err = computers.Release(ctx, computerReference(lease)); err != nil {
			t.Fatal(err)
		}
		if fixture.sessionState(t, lease) != "released" {
			t.Fatal("幂等清理未确认")
		}
	})
	t.Run("释放事件回滚不交接，有限重试保留隔离", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		node := &sessionNode{}
		options := computerOptions()
		options.MaxCleanupAttempts = 2
		computers := fixture.computers(t, node, options)
		fixture.send(t, fixture.a, fixture.first)
		lease := fixture.claim(t)
		fixture.start(t, lease)
		if _, err := computers.Acquire(ctx, computerReference(lease)); err != nil {
			t.Fatal(err)
		}
		execute(t, fixture.pool, `CREATE FUNCTION tombot.reject_computer_release() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN IF NEW.kind='computer.session.updated' AND NEW.payload->>'state'='released' THEN RAISE EXCEPTION '清理事件失败'; END IF; RETURN NEW; END $body$`)
		execute(t, fixture.pool, `CREATE TRIGGER reject_computer_release BEFORE INSERT ON tombot.user_events FOR EACH ROW EXECUTE FUNCTION tombot.reject_computer_release()`)
		if err := computers.Release(ctx, computerReference(lease)); err == nil {
			t.Fatal("清理事件失败被忽略")
		}
		if fixture.sessionState(t, lease) != "draining" {
			t.Fatal("事务回滚仍释放了会话")
		}
		execute(t, fixture.pool, `DROP TRIGGER reject_computer_release ON tombot.user_events`)
		node.failClose = true
		fixture.allowCleanup(t, lease)
		expect(t, computers.Release(ctx, computerReference(lease)), computercontrol.ErrNode)
		fixture.allowCleanup(t, lease)
		expect(t, computers.Release(ctx, computerReference(lease)), computercontrol.ErrCleanupBlocked)
		if _, err := computers.Reconcile(ctx, 100); err != nil {
			t.Fatal(err)
		}
		if node.closes != 2 || fixture.sessionState(t, lease) != "quarantined" {
			t.Fatal("超过重试上限或错误释放")
		}
	})
	t.Run("额度排队取消无需节点操作", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		execute(t, fixture.pool, `UPDATE tombot.user_limits SET concurrent_desktops=1 WHERE user_id=$1`, fixture.a.UserID)
		node := &sessionNode{}
		options := computerOptions()
		options.MaxWaiting = 1
		computers := fixture.computers(t, node, options)
		fixture.send(t, fixture.a, fixture.first)
		first := fixture.claim(t)
		fixture.start(t, first)
		fixture.send(t, fixture.a, fixture.second)
		second := fixture.claim(t)
		fixture.start(t, second)
		if _, err := computers.Acquire(ctx, computerReference(first)); err != nil {
			t.Fatal(err)
		}
		_, err := computers.Acquire(ctx, computerReference(second))
		expect(t, err, computercontrol.ErrWaiting)
		if _, err = fixture.service.Cancel(ctx, fixture.a, second.RunID); err != nil {
			t.Fatal(err)
		}
		if count, err := computers.Reconcile(ctx, 100); err != nil || count != 1 {
			t.Fatal(count, err)
		}
		if node.closes != 0 || fixture.sessionState(t, second) != "released" {
			t.Fatal("排队取消错误清理了其他运行")
		}
		if _, err = fixture.service.Complete(ctx, second, Outcome{Status: "canceled", ErrorCode: "CANCELED"}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestComputerWorker(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("模型思考保留自身桌面且允许其他机器人并行，完成前定向清理", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		fixture.toolsModel(t)
		node := &sessionNode{}
		computers := fixture.computers(t, node, computerOptions())
		fixture.send(t, fixture.a, fixture.first)
		lease := fixture.claim(t)
		fixture.send(t, fixture.a, fixture.second)
		other := fixture.claim(t)
		fixture.start(t, other)
		calls := 0
		worker := fixture.workerWithComputers(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, _ modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
			calls++
			if calls == 1 {
				return emitTool(emit, `{"path":"notes.txt"}`)
			}
			if fixture.sessionState(t, lease) != "active" {
				t.Error("模型思考期间提前释放自身桌面")
			}
			_, err := computers.Acquire(ctx, computerReference(other))
			if err != nil {
				t.Error("其他机器人无法并行使用独立桌面", err)
			}
			return emitText(emit, "文件已检查。")
		}), computers, testTool{usesComputer: true, execute: func(_ context.Context, invocation Invocation) (botagent.ToolResult, error) {
			if invocation.Computer == nil || invocation.Computer.RunID != lease.RunID {
				t.Error("工具缺少电脑授权")
			}
			return botagent.ToolResult{Content: "Verified contents."}, nil
		}})
		run, err := worker.Process(ctx, lease)
		if err != nil || run.Status != "succeeded" || fixture.sessionState(t, lease) != "released" || node.closes != 1 {
			t.Fatal("运行未在清理后完成", run.Status, err)
		}
		if _, err = computers.Acquire(ctx, computerReference(other)); err != nil {
			t.Fatal("并行桌面受到其他运行清理影响", err)
		}
	})
	t.Run("等待桌面额度不占模型名额，取消阻止工具执行", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		execute(t, fixture.pool, `UPDATE tombot.user_limits SET concurrent_desktops=1 WHERE user_id=$1`, fixture.a.UserID)
		fixture.toolsModel(t)
		node := &sessionNode{}
		computers := fixture.computers(t, node, computerOptions())
		fixture.send(t, fixture.a, fixture.first)
		first := fixture.claim(t)
		fixture.start(t, first)
		if _, err := computers.Acquire(ctx, computerReference(first)); err != nil {
			t.Fatal(err)
		}
		fixture.send(t, fixture.a, fixture.second)
		second := fixture.claim(t)
		var calls, tools atomic.Int32
		worker := fixture.workerWithComputers(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, _ modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
			calls.Add(1)
			return emitTool(emit, `{"path":"notes.txt"}`)
		}), computers, testTool{usesComputer: true, execute: func(context.Context, Invocation) (botagent.ToolResult, error) {
			tools.Add(1)
			return botagent.ToolResult{Content: "Unexpected execution."}, nil
		}})
		done := make(chan error, 1)
		go func() {
			run, err := worker.Process(ctx, second)
			if err == nil && run.Status != "canceled" {
				err = errors.New("取消运行终态错误")
			}
			done <- err
		}()
		deadline := time.Now().Add(3 * time.Second)
		for {
			var waiting bool
			if err := fixture.pool.QueryRow(`SELECT EXISTS(SELECT 1 FROM tombot.computer_sessions WHERE user_id=$1 AND run_id=$2 AND status='waiting')`, second.UserID, second.RunID).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("未进入等待")
			}
			time.Sleep(10 * time.Millisecond)
		}
		var slots int
		if err := fixture.pool.QueryRow(`SELECT count(*) FROM tombot.usage_reservations WHERE user_id=$1 AND NOT slot_released`, second.UserID).Scan(&slots); err != nil || slots != 0 {
			t.Fatal("等待电脑占用模型槽", slots, err)
		}
		if _, err := fixture.service.Cancel(ctx, fixture.a, second.RunID); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("等待任务取消未结束")
		}
		if calls.Load() != 0 || tools.Load() != 0 || node.closes != 0 {
			t.Fatal("等待时重复调用模型、执行工具或清理其他运行")
		}
	})
}

func TestComputerBoundaries(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("最终回复检查后电脑到期，清理重试不能抹掉失效原因", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		node := &sessionNode{}
		computers := fixture.computers(t, node, computerOptions())
		fixture.send(t, fixture.a, fixture.first)
		lease := fixture.claim(t)
		fixture.start(t, lease)
		fixture.completeModel(t, lease)
		if _, err := computers.Acquire(ctx, computerReference(lease)); err != nil {
			t.Fatal(err)
		}
		if err := computers.Check(ctx, computerReference(lease)); err != nil {
			t.Fatal(err)
		}
		execute(t, fixture.pool, `UPDATE tombot.computer_sessions SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 AND run_id=$2`, lease.UserID, lease.RunID)
		node.failClose = true
		if _, err := computers.Reconcile(ctx, 100); !errors.Is(err, computercontrol.ErrNode) {
			t.Fatal(err)
		}
		node.failClose = false
		fixture.allowCleanup(t, lease)
		if _, err := computers.Reconcile(ctx, 100); err != nil {
			t.Fatal(err)
		}
		var code string
		if err := fixture.pool.QueryRow(`SELECT error_code FROM tombot.computer_sessions WHERE user_id=$1 AND run_id=$2`, lease.UserID, lease.RunID).Scan(&code); err != nil || code != "COMPUTER_LEASE_EXPIRED" {
			t.Fatal("清理失败改写了终止原因", code, err)
		}
		_, err := fixture.service.Complete(ctx, lease, Outcome{Status: "succeeded", Answer: "完成"})
		expect(t, err, ErrComputerLost)
		execute(t, fixture.pool, `UPDATE tombot.jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID)
		if count, err := fixture.service.Recover(ctx, 100); err != nil || count != 1 {
			t.Fatal(count, err)
		}
		var status string
		if err := fixture.pool.QueryRow(`SELECT status FROM tombot.runs WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID).Scan(&status); err != nil || status != "failed" {
			t.Fatal("失效电脑任务被重新排队", status, err)
		}
	})
	t.Run("额度等待队列有界，超时持久终止，用户与调度权限隔离", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		execute(t, fixture.pool, `UPDATE tombot.user_limits SET concurrent_desktops=1 WHERE user_id=$1`, fixture.a.UserID)
		node := &sessionNode{}
		options := computerOptions()
		options.MaxWaiting = 1
		options.WaitTimeout = time.Second
		computers := fixture.computers(t, node, options)
		fixture.send(t, fixture.a, fixture.first)
		first := fixture.claim(t)
		fixture.start(t, first)
		fixture.send(t, fixture.a, fixture.second)
		second := fixture.claim(t)
		fixture.start(t, second)
		spec := fixture.first.Spec
		spec.Name = "第三机器人"
		bot, _, err := fixture.catalog.Create(ctx, fixture.a, bots.CreateRequest{ClientRequestID: ids.New(), Spec: spec})
		if err != nil {
			t.Fatal(err)
		}
		fixture.send(t, fixture.a, bot)
		third := fixture.claim(t)
		fixture.start(t, third)
		if _, err = computers.Acquire(ctx, computerReference(first)); err != nil {
			t.Fatal(err)
		}
		_, err = computers.Acquire(ctx, computerReference(second))
		expect(t, err, computercontrol.ErrWaiting)
		_, err = computers.Acquire(ctx, computerReference(third))
		expect(t, err, computercontrol.ErrCapacity)
		if err = fixture.tenant.Tenant(ctx, fixture.b.UserID, func(tx *sql.Tx) error {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tombot.computer_sessions WHERE user_id=$1`, fixture.a.UserID).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return errors.New("跨用户读取到电脑会话")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err = fixture.dispatch.Platform(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `SELECT container_id FROM tombot.computer_sessions`)
			return err
		}); err == nil {
			t.Fatal("调度连接读取了节点执行身份")
		}
		time.Sleep(1100 * time.Millisecond)
		_, err = computers.Acquire(ctx, computerReference(second))
		expect(t, err, computercontrol.ErrUnavailable)
		if fixture.sessionState(t, second) != "released" {
			t.Fatal("等待超时未持久化")
		}
		_, err = computers.Acquire(ctx, computerReference(third))
		expect(t, err, computercontrol.ErrWaiting)
		if err = computers.Release(ctx, computerReference(first)); err != nil {
			t.Fatal(err)
		}
		if _, err = computers.Acquire(ctx, computerReference(third)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("电脑续约不延长占用上限，失效后必须确认清理", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		node := &sessionNode{}
		options := computerOptions()
		options.LeaseLifetime = time.Second
		options.HoldTimeout = 2 * time.Second
		options.NodeTimeout = 100 * time.Millisecond
		computers := fixture.computers(t, node, options)
		fixture.send(t, fixture.a, fixture.first)
		lease := fixture.claim(t)
		fixture.start(t, lease)
		if _, err := computers.Acquire(ctx, computerReference(lease)); err != nil {
			t.Fatal(err)
		}
		var original time.Time
		if err := fixture.pool.QueryRow(`SELECT hold_deadline_at FROM tombot.computer_sessions WHERE user_id=$1 AND run_id=$2`, lease.UserID, lease.RunID).Scan(&original); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for {
			time.Sleep(200 * time.Millisecond)
			err := computers.Renew(ctx, computerReference(lease))
			if errors.Is(err, computercontrol.ErrLeaseLost) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if time.Now().After(deadline) {
				t.Fatal("续约无限延长占用")
			}
		}
		var held time.Time
		if err := fixture.pool.QueryRow(`SELECT hold_deadline_at FROM tombot.computer_sessions WHERE user_id=$1 AND run_id=$2`, lease.UserID, lease.RunID).Scan(&held); err != nil || !held.Equal(original) {
			t.Fatal("占用上限发生变化", err)
		}
		if fixture.sessionState(t, lease) != "draining" || node.renews == 0 {
			t.Fatal("未经历有效续约及到期收尾")
		}
		if err := computers.Release(ctx, computerReference(lease)); err != nil {
			t.Fatal(err)
		}
		expect(t, computers.Renew(ctx, computerReference(lease)), computercontrol.ErrLeaseLost)
	})
	t.Run("Worker 清理失败不报成功，恢复复用结果不重放电脑动作", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		fixture.toolsModel(t)
		node := &sessionNode{failClose: true}
		computers := fixture.computers(t, node, computerOptions())
		fixture.send(t, fixture.a, fixture.first)
		lease := fixture.claim(t)
		calls, tools := 0, 0
		worker := fixture.workerWithComputers(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, _ modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
			calls++
			if calls == 1 {
				return emitTool(emit, `{"path":"notes.txt"}`)
			}
			return emitText(emit, "已完成检查。")
		}), computers, testTool{usesComputer: true, execute: func(context.Context, Invocation) (botagent.ToolResult, error) {
			tools++
			return botagent.ToolResult{Content: "Verified contents."}, nil
		}})
		if _, err := NewWorker(fixture.service, fixture.registry, worker.caller, []Tool{testTool{usesComputer: true}}, nil, worker.agents, worker.options, worker.logger); !errors.Is(err, ErrInvalidInput) {
			t.Fatal("电脑工具缺少执行权管理仍可启动", err)
		}
		if _, err := worker.Process(ctx, lease); !errors.Is(err, computercontrol.ErrNode) {
			t.Fatal("清理失败未报告", err)
		}
		var status string
		if err := fixture.pool.QueryRow(`SELECT status FROM tombot.runs WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID).Scan(&status); err != nil || status != "running" {
			t.Fatal("清理前报告运行完成", status, err)
		}
		execute(t, fixture.pool, `UPDATE tombot.jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID)
		if count, err := fixture.service.Recover(ctx, 100); err != nil || count != 0 {
			t.Fatal("清理前换代", count, err)
		}
		node.failClose = false
		fixture.allowCleanup(t, lease)
		if count, err := computers.Reconcile(ctx, 100); err != nil || count != 1 {
			t.Fatal("对账未释放", count, err)
		}
		if count, err := fixture.service.Recover(ctx, 100); err != nil || count != 1 {
			t.Fatal("运行未恢复", count, err)
		}
		execute(t, fixture.pool, `UPDATE tombot.jobs SET available_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID)
		next := fixture.claim(t)
		run, err := worker.Process(ctx, next)
		// 恢复必须启动新代次的容器 Agent；模型和工具只复用已知结果，不重放外部动作。
		if err != nil || run.Status != "succeeded" || calls != 2 || tools != 1 || node.opens != 2 {
			t.Fatal("恢复重放了模型或电脑动作", run.Status, calls, tools, node.opens, err)
		}
	})
}
