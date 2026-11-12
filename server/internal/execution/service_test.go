package execution

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/identity"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

type testAuthorizer struct{}

func (testAuthorizer) RequireAdmin(_ context.Context, principal identity.Principal, _ bool) error {
	if principal.Role != "admin" || principal.Scope != "admin" {
		return identity.ErrForbidden
	}
	return nil
}

type testVerifier struct{}

func (testVerifier) Verify(context.Context, modelregistry.Configuration, []byte) error { return nil }

type fixture struct {
	pool                   *sql.DB
	tenant, dispatch       *postgres.Database
	service                *Service
	catalog                *bots.Service
	registry               *modelregistry.Service
	actor                  identity.Principal
	policyID, modelID      string
	a, b                   identity.Principal
	first, second, foreign bots.Bot
}

func execute(t *testing.T, pool *sql.DB, statement string, arguments ...any) {
	t.Helper()
	if _, err := pool.Exec(statement, arguments...); err != nil {
		t.Fatal("准备运行测试失败", err)
	}
}
func newFixture(t *testing.T, cluster *pgtest.Cluster) fixture {
	t.Helper()
	ctx := context.Background()
	pool, name := cluster.Database(t)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	open := func(role postgres.Role) *postgres.Database {
		t.Helper()
		login := cluster.Login(t, string(role))
		database, err := postgres.Open(ctx, postgres.Options{DSN: cluster.DSN(name, login), Role: role, MaxConnections: 12, ConnectTimeout: 5 * time.Second, StatementTimeout: 5 * time.Second, LockTimeout: 2 * time.Second, TransactionTimeout: 10 * time.Second, ConnectionLifetime: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := database.Close(); err != nil {
				t.Error(err)
			}
		})
		return database
	}
	tenant, dispatch, control := open(postgres.TenantRole), open(postgres.DispatchRole), open(postgres.ControlRole)
	actor := identity.Principal{UserID: ids.New(), Role: "admin", Scope: "admin"}
	execute(t, pool, `INSERT INTO tombot.users(id,username,display_name,role,status) VALUES($1,$2,'运行测试管理员','admin','provisioning')`, actor.UserID, strings.ToLower(actor.UserID))
	key, err := secure.Random(32)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	keys, err := modelregistry.NewKeyring("fixture", map[string][]byte{"fixture": key})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := modelregistry.New(control, tenant, testAuthorizer{}, keys, testVerifier{}, modelregistry.Options{VerificationTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := registry.CreateProvider(ctx, actor, ids.New(), "运行模型供应商", "openai_compat", "https://model.example.test/v1")
	if err != nil {
		t.Fatal(err)
	}
	model, err := registry.CreateModel(ctx, actor, ids.New(), modelregistry.ModelRequest{ProviderID: provider.ID, ModelKey: "execution-fixture", DisplayName: "运行测试模型", ContextLimit: 4096, OutputLimit: 1024})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := registry.CreatePolicy(ctx, actor, ids.New(), "运行测试策略")
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := bots.New(tenant, bots.Options{MaxBots: 10, MaxGroups: 10, ToolNames: []string{"file_read", "computer_shell", "computer_screenshot", "computer_input"}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(tenant, dispatch, registry, Options{LeaseLifetime: 10 * time.Second, RecoveryDelay: time.Second, MaxAttempts: 3, MaxOperations: 8, ContextMessages: 20, ContextBytes: 128 << 10, Images: ImageOptions{MaxUserBytes: 64 << 20, MaxUserImages: 64, ContextImages: 2, Retention: 7 * 24 * time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	seed := func() identity.Principal {
		principal := identity.Principal{UserID: ids.New(), SessionID: ids.New(), DeviceID: ids.New(), Role: "user", Scope: "desktop", AuthVersion: 1}
		node, image, computer := ids.New(), ids.New(), ids.New()
		execute(t, pool, `INSERT INTO tombot.users(id,username,display_name,role,status) VALUES($1,$2,'运行用户','user','provisioning')`, principal.UserID, strings.ToLower(principal.UserID))
		execute(t, pool, `INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2)`, image, "example.test/computer@sha256:"+secure.Hash(image))
		execute(t, pool, `INSERT INTO tombot.computer_nodes(id,name,status,capacity,runtime_id,control_sequence,last_seen_at,control_expires_at,verified_image_id,capability_expires_at) VALUES($1,$1,'ready',1,$1,1,clock_timestamp(),clock_timestamp()+interval '5 minutes',$2,clock_timestamp()+interval '15 minutes')`, node, image)
		execute(t, pool, `INSERT INTO tombot.computers(user_id,id,node_id,image_id,status,container_id,volume_ref,cpu_millis,memory_bytes,disk_bytes,process_limit) VALUES($1,$2,$3,$4,'ready',$2,$2,1000,1073741824,1073741824,128)`, principal.UserID, computer, node, image)
		execute(t, pool, `UPDATE tombot.users SET status='active' WHERE id=$1`, principal.UserID)
		execute(t, pool, `INSERT INTO tombot.devices(user_id,id,name,platform) VALUES($1,$2,'运行设备','linux')`, principal.UserID, principal.DeviceID)
		execute(t, pool, `INSERT INTO tombot.user_sessions(user_id,id,device_id,auth_version,scope,expires_at) VALUES($1,$2,$3,1,'desktop',clock_timestamp()+interval '1 hour')`, principal.UserID, principal.SessionID, principal.DeviceID)
		execute(t, pool, `INSERT INTO tombot.user_limits(user_id,concurrent_model_calls,concurrent_desktops,queued_runs,monthly_token_limit) VALUES($1,2,2,10,100000)`, principal.UserID)
		return principal
	}
	create := func(principal identity.Principal, name string) bots.Bot {
		bot, _, err := catalog.Create(ctx, principal, bots.CreateRequest{ClientRequestID: ids.New(), Spec: bots.Spec{Name: name, SystemPrompt: "You are a research bot. Verify evidence.", ToolPermissions: []string{"file_read"}, ApprovalMode: "standard", MessagingPolicy: bots.MessagingPolicy{AcceptDM: true, CanInitiate: true, AcceptGroupInvite: true}}})
		if err != nil {
			t.Fatal(err)
		}
		return bot
	}
	a, b := seed(), seed()
	fixture := fixture{pool: pool, tenant: tenant, dispatch: dispatch, service: service, catalog: catalog, registry: registry, actor: actor, policyID: policy.ID, modelID: model.ID, a: a, b: b, first: create(a, "第一机器人"), second: create(a, "第二机器人"), foreign: create(b, "另一用户机器人")}
	fixture.publish(t)
	if err = registry.SetDefault(ctx, actor, ids.New(), policy.ID, fixture.revision(t)); err != nil {
		t.Fatal(err)
	}
	return fixture
}
func (fixture fixture) revision(t *testing.T) int64 {
	t.Helper()
	var revision int64
	if err := fixture.pool.QueryRow(`SELECT revision FROM tombot.model_policies WHERE id=$1`, fixture.policyID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return revision
}
func (fixture fixture) publish(t *testing.T) modelregistry.Version {
	t.Helper()
	version, err := fixture.registry.CreateDraft(context.Background(), fixture.actor, ids.New(), modelregistry.DraftRequest{PolicyID: fixture.policyID, ModelID: fixture.modelID, OutputLimit: 256})
	if err != nil {
		t.Fatal(err)
	}
	version, err = fixture.registry.Publish(context.Background(), fixture.actor, ids.New(), fixture.policyID, version.Number, fixture.revision(t))
	if err != nil {
		t.Fatal(err)
	}
	return version
}
func (fixture fixture) send(t *testing.T, principal identity.Principal, bot bots.Bot) Submission {
	t.Helper()
	submission, err := fixture.service.Send(context.Background(), principal, bot.ConversationID, SendRequest{ids.New(), "请检查这项任务。"})
	if err != nil {
		t.Fatal(err)
	}
	return submission
}
func (fixture fixture) claim(t *testing.T) Lease {
	t.Helper()
	lease, err := fixture.service.Claim(context.Background(), ids.New())
	if err != nil {
		t.Fatal(err)
	}
	return lease
}
func (fixture fixture) start(t *testing.T, lease Lease) Work {
	t.Helper()
	work, err := fixture.service.Start(context.Background(), lease)
	if err != nil {
		t.Fatal(err)
	}
	return work
}
func expect(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("预期 %v，实际 %v", want, err)
	}
}
func operationRequest(step string) OperationRequest {
	digest := sha256.Sum256([]byte(step))
	return OperationRequest{StepKey: step, Kind: "model", RequestDigest: digest[:], Model: &ModelCall{Configuration: modelregistry.Configuration{Protocol: "openai_compat", BaseURL: "https://model.example.test/v1", ModelKey: "execution-fixture", ContextLimit: 4096, OutputLimit: 256}, Lifetime: 30 * time.Second}}
}
func (fixture fixture) completeModel(t *testing.T, lease Lease) Operation {
	t.Helper()
	ctx := context.Background()
	if _, err := fixture.service.BindModel(ctx, lease); err != nil {
		t.Fatal(err)
	}
	operation, created, err := fixture.service.BeginOperation(ctx, lease, operationRequest("model.1"))
	if err != nil || !created {
		t.Fatal("模型操作开始失败", err)
	}
	operation, err = fixture.service.RecordOperation(ctx, lease, operation.ID, OperationResult{Status: "succeeded", Result: json.RawMessage(`{"answer":"已完成","usage":null}`)})
	if err != nil {
		t.Fatal(err)
	}
	return operation
}

func TestDurableExecution(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("消息事务幂等与隔离", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		request := SendRequest{ids.New(), "同一条消息"}
		first, err := fixture.service.Send(ctx, fixture.a, fixture.first.ConversationID, request)
		if err != nil || !first.IsCreated {
			t.Fatal(err)
		}
		again, err := fixture.service.Send(ctx, fixture.a, fixture.first.ConversationID, request)
		if err != nil || again.IsCreated || again.RunID != first.RunID || again.MessageID != first.MessageID {
			t.Fatal("重复消息未合并", err)
		}
		_, err = fixture.service.Send(ctx, fixture.a, fixture.first.ConversationID, SendRequest{request.ClientRequestID, "不同正文"})
		expect(t, err, ErrConflict)
		_, err = fixture.service.Send(ctx, fixture.a, fixture.first.ConversationID, SendRequest{ids.New(), "新回合"})
		expect(t, err, ErrBusy)
		_, err = fixture.service.Send(ctx, fixture.b, fixture.first.ConversationID, request)
		expect(t, err, ErrNotFound)
		_, err = fixture.service.Messages(ctx, fixture.b, fixture.first.ConversationID, 0, 10)
		expect(t, err, ErrNotFound)
		_, err = fixture.service.GetRun(ctx, fixture.b, first.RunID)
		expect(t, err, ErrNotFound)
		_, err = fixture.service.Cancel(ctx, fixture.b, first.RunID)
		expect(t, err, ErrNotFound)
		messages, err := fixture.service.Messages(ctx, fixture.a, fixture.first.ConversationID, 0, 10)
		if err != nil || len(messages) != 1 || messages[0].Sequence != 1 || messages[0].Blocks[0].Text != request.Content {
			t.Fatal("持久消息错误", err)
		}
		if err = fixture.service.MarkRead(ctx, fixture.a, fixture.first.ConversationID, 1); err != nil {
			t.Fatal(err)
		}
		expect(t, fixture.service.MarkRead(ctx, fixture.a, fixture.first.ConversationID, 2), ErrInvalidInput)
		if err = fixture.service.MarkRead(ctx, fixture.a, fixture.first.ConversationID, 0); err != nil {
			t.Fatal(err)
		}
		conversation, err := fixture.catalog.GetConversation(ctx, fixture.a, fixture.first.ConversationID)
		if err != nil || conversation.LastReadSequence != 1 {
			t.Fatal("已读位置倒退", err)
		}
		execute(t, fixture.pool, `UPDATE tombot.user_limits SET queued_runs=1 WHERE user_id=$1`, fixture.a.UserID)
		_, err = fixture.service.Send(ctx, fixture.a, fixture.second.ConversationID, SendRequest{ids.New(), "队列满"})
		expect(t, err, ErrCapacity)
	})
	t.Run("公平领取与并发互斥", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		fixture.send(t, fixture.a, fixture.first)
		fixture.send(t, fixture.a, fixture.second)
		fixture.send(t, fixture.b, fixture.foreign)
		first := fixture.claim(t)
		second := fixture.claim(t)
		if first.UserID == second.UserID {
			t.Fatal("连续优先同一用户，另一用户饥饿")
		}
		var wait sync.WaitGroup
		leases := make(chan Lease, 8)
		failures := make(chan error, 8)
		for range 8 {
			wait.Go(func() { lease, err := fixture.service.Claim(ctx, ids.New()); leases <- lease; failures <- err })
		}
		wait.Wait()
		close(leases)
		close(failures)
		claimed := 0
		for err := range failures {
			if err == nil {
				claimed++
			} else if !errors.Is(err, ErrNoWork) {
				t.Fatal(err)
			}
		}
		if claimed != 1 {
			t.Fatal("并发领取了重复任务", claimed)
		}
		for lease := range leases {
			if lease.JobID != "" && (lease.JobID == first.JobID || lease.JobID == second.JobID) {
				t.Fatal("已租用任务被再次领取")
			}
		}
		work := fixture.start(t, first)
		if work.Run.Status != "running" || len(work.Messages) != 1 {
			t.Fatal("领取后上下文不完整")
		}
		renewed, err := fixture.service.Renew(ctx, first)
		if err != nil || !renewed.ExpiresAt.After(first.ExpiresAt) {
			t.Fatal("续约失败", err)
		}
		forged := first
		forged.FencingToken++
		_, err = fixture.service.Start(ctx, forged)
		expect(t, err, ErrLeaseLost)
	})
	t.Run("固定版本操作去重与完成", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		submitted := fixture.send(t, fixture.a, fixture.first)
		lease := fixture.claim(t)
		work := fixture.start(t, lease)
		selection, err := fixture.service.BindModel(ctx, lease)
		if err != nil || selection.Version != 1 {
			t.Fatal(err)
		}
		fixture.publish(t)
		selectedAgain, err := fixture.service.BindModel(ctx, lease)
		if err != nil || selectedAgain.Version != 1 {
			t.Fatal("在途模型版本改变", err)
		}
		changed := fixture.first.Spec
		changed.SystemPrompt = "You are a newly configured bot."
		if _, err = fixture.catalog.Update(ctx, fixture.a, fixture.first.ID, bots.UpdateRequest{ExpectedRevision: fixture.first.Revision, Spec: changed}); err != nil {
			t.Fatal(err)
		}
		work = fixture.start(t, lease)
		if work.Bot.SystemPrompt != fixture.first.SystemPrompt || work.Selection == nil || work.Selection.Version != 1 {
			t.Fatal("运行配置快照改变")
		}
		request := operationRequest("model.1")
		operation, created, err := fixture.service.BeginOperation(ctx, lease, request)
		if err != nil || !created {
			t.Fatal(err)
		}
		replayed, created, err := fixture.service.BeginOperation(ctx, lease, request)
		if err != nil || created || replayed.ID != operation.ID || replayed.Status != "started" {
			t.Fatal("未知操作被允许重放", err)
		}
		_, _, err = fixture.service.BeginOperation(ctx, lease, operationRequest("model.2"))
		expect(t, err, ErrUncertain)
		_, err = fixture.service.Complete(ctx, lease, Outcome{Status: "succeeded", Answer: "过早完成"})
		expect(t, err, ErrUncertain)
		result := OperationResult{Status: "succeeded", Result: json.RawMessage(`{"answer":"已完成","usage":null}`)}
		if _, err = fixture.service.RecordOperation(ctx, lease, operation.ID, result); err != nil {
			t.Fatal(err)
		}
		if _, err = fixture.service.RecordOperation(ctx, lease, operation.ID, result); err != nil {
			t.Fatal("重复终态失败", err)
		}
		_, err = fixture.service.RecordOperation(ctx, lease, operation.ID, OperationResult{Status: "succeeded", Result: json.RawMessage(`{"answer":"覆盖"}`)})
		expect(t, err, ErrConflict)
		run, err := fixture.service.Complete(ctx, lease, Outcome{Status: "succeeded", Answer: "已完成"})
		if err != nil || run.Status != "succeeded" || run.FinishedAt == nil {
			t.Fatal("终态提交失败", err)
		}
		_, err = fixture.service.Complete(ctx, lease, Outcome{Status: "succeeded", Answer: "重复完成"})
		expect(t, err, ErrLeaseLost)
		messages, err := fixture.service.Messages(ctx, fixture.a, fixture.first.ConversationID, 0, 10)
		if err != nil || len(messages) != 2 || messages[0].Sequence != 2 || messages[0].RunID == nil || *messages[0].RunID != submitted.RunID {
			t.Fatal("回复或序号不完整", err)
		}
		older, err := fixture.service.Messages(ctx, fixture.a, fixture.first.ConversationID, 2, 10)
		if err != nil || len(older) != 1 || older[0].SenderKind != "user" {
			t.Fatal("消息分页错误", err)
		}
		fixture.send(t, fixture.a, fixture.first)
		next := fixture.claim(t)
		fixture.start(t, next)
		selectedAgain, err = fixture.service.BindModel(ctx, next)
		if err != nil || selectedAgain.Version != 2 {
			t.Fatal("新运行未使用新模型版本", err)
		}
	})
	t.Run("崩溃恢复与未知外部结果", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		submitted := fixture.send(t, fixture.a, fixture.first)
		lease := fixture.claim(t)
		fixture.start(t, lease)
		if count, err := fixture.service.Recover(ctx, 10); err != nil || count != 0 {
			t.Fatal("有效租约被恢复", count, err)
		}
		execute(t, fixture.pool, `UPDATE tombot.jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID)
		_, err := fixture.service.Renew(ctx, lease)
		expect(t, err, ErrLeaseLost)
		count, err := fixture.service.Recover(ctx, 10)
		if err != nil || count != 1 {
			t.Fatal("未开始调用的任务未恢复", count, err)
		}
		_, err = fixture.service.Claim(ctx, ids.New())
		expect(t, err, ErrNoWork)
		execute(t, fixture.pool, `UPDATE tombot.jobs SET available_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID)
		next := fixture.claim(t)
		if next.RunID != lease.RunID || next.FencingToken <= lease.FencingToken || next.Attempt != 2 {
			t.Fatal("恢复代次错误")
		}
		fixture.start(t, next)
		_, err = fixture.service.Start(ctx, lease)
		expect(t, err, ErrLeaseLost)
		if _, err = fixture.service.BindModel(ctx, next); err != nil {
			t.Fatal(err)
		}
		operation, _, err := fixture.service.BeginOperation(ctx, next, operationRequest("model.1"))
		if err != nil {
			t.Fatal(err)
		}
		execute(t, fixture.pool, `UPDATE tombot.jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 AND id=$2`, next.UserID, next.JobID)
		count, err = fixture.service.Recover(ctx, 10)
		if err != nil || count != 1 {
			t.Fatal(err)
		}
		run, err := fixture.service.GetRun(ctx, fixture.a, submitted.RunID)
		if err != nil || run.Status != "unknown" || run.ErrorCode != "EXTERNAL_RESULT_UNKNOWN" {
			t.Fatal("未知调用被重试或标成功", err)
		}
		_, err = fixture.service.Claim(ctx, ids.New())
		expect(t, err, ErrNoWork)
		_, err = fixture.service.RecordOperation(ctx, next, operation.ID, OperationResult{Status: "succeeded", Result: json.RawMessage(`{}`)})
		expect(t, err, ErrLeaseLost)
		var status string
		if err = fixture.pool.QueryRow(`SELECT status FROM tombot.run_operations WHERE user_id=$1 AND id=$2`, next.UserID, operation.ID).Scan(&status); err != nil || status != "unknown" {
			t.Fatal("旧持有者覆写未知结果", err)
		}
	})
	t.Run("取消排队和活动执行", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		queued := fixture.send(t, fixture.a, fixture.first)
		run, err := fixture.service.Cancel(ctx, fixture.a, queued.RunID)
		if err != nil || run.Status != "canceled" {
			t.Fatal(err)
		}
		if _, err = fixture.service.Cancel(ctx, fixture.a, queued.RunID); err != nil {
			t.Fatal("重复取消失败", err)
		}
		_, err = fixture.service.Claim(ctx, ids.New())
		expect(t, err, ErrNoWork)
		active := fixture.send(t, fixture.a, fixture.first)
		lease := fixture.claim(t)
		fixture.start(t, lease)
		if _, err = fixture.service.BindModel(ctx, lease); err != nil {
			t.Fatal(err)
		}
		operation, _, err := fixture.service.BeginOperation(ctx, lease, operationRequest("model.1"))
		if err != nil {
			t.Fatal(err)
		}
		run, err = fixture.service.Cancel(ctx, fixture.a, active.RunID)
		if err != nil || run.Status != "canceling" || run.FinishedAt != nil {
			t.Fatal("在途取消提前释放", err)
		}
		_, err = fixture.service.Renew(ctx, lease)
		expect(t, err, ErrCanceled)
		_, err = fixture.service.Complete(ctx, lease, Outcome{Status: "succeeded", Answer: "迟到"})
		expect(t, err, ErrCanceled)
		_, err = fixture.service.Complete(ctx, lease, Outcome{Status: "canceled", ErrorCode: "CANCELED"})
		expect(t, err, ErrUncertain)
		if _, err = fixture.service.RecordOperation(ctx, lease, operation.ID, OperationResult{Status: "unknown", ErrorCode: "CANCELED_REQUEST", Result: json.RawMessage(`{"usage":null}`)}); err != nil {
			t.Fatal(err)
		}
		run, err = fixture.service.Complete(ctx, lease, Outcome{Status: "canceled", ErrorCode: "CANCELED"})
		if err != nil || run.Status != "canceled" {
			t.Fatal(err)
		}
	})
	t.Run("事务失败与数据库保护", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		fixture.send(t, fixture.a, fixture.first)
		lease := fixture.claim(t)
		fixture.start(t, lease)
		operation := fixture.completeModel(t, lease)
		for _, statement := range []string{`UPDATE tombot.runs SET bot_snapshot='{}' WHERE user_id=$1 AND id=$2`, `UPDATE tombot.runs SET model_policy_id=NULL,model_version=NULL WHERE user_id=$1 AND id=$2`} {
			err := fixture.tenant.Tenant(ctx, lease.UserID, func(transaction *sql.Tx) error {
				_, err := transaction.ExecContext(ctx, statement, lease.UserID, lease.RunID)
				return err
			})
			var failure *pgconn.PgError
			if !errors.As(err, &failure) || failure.Code != "23514" {
				t.Fatal("固定运行配置被数据库允许改写", err)
			}
		}
		err := fixture.tenant.Tenant(ctx, lease.UserID, func(transaction *sql.Tx) error {
			_, err := transaction.ExecContext(ctx, `UPDATE tombot.run_operations SET result='{}' WHERE user_id=$1 AND id=$2`, lease.UserID, operation.ID)
			return err
		})
		var failure *pgconn.PgError
		if !errors.As(err, &failure) || failure.Code != "23514" {
			t.Fatal("操作终态可改写", err)
		}
		err = fixture.dispatch.Platform(ctx, func(transaction *sql.Tx) error {
			var count int
			return transaction.QueryRowContext(ctx, `SELECT count(*) FROM tombot.messages`).Scan(&count)
		})
		if err == nil {
			t.Fatal("调度角色可读取消息正文")
		}
		execute(t, fixture.pool, `CREATE FUNCTION tombot.reject_execution_event() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN RAISE EXCEPTION '执行事件写入失败'; END $body$`)
		execute(t, fixture.pool, `CREATE TRIGGER reject_execution_event BEFORE INSERT ON tombot.user_events FOR EACH ROW EXECUTE FUNCTION tombot.reject_execution_event()`)
		if _, err = fixture.service.Complete(ctx, lease, Outcome{Status: "succeeded", Answer: "应回滚"}); err == nil {
			t.Fatal("事件失败被忽略")
		}
		messages, err := fixture.service.Messages(ctx, fixture.a, fixture.first.ConversationID, 0, 10)
		if err != nil || len(messages) != 1 {
			t.Fatal("回复部分提交", err)
		}
		run, err := fixture.service.GetRun(ctx, fixture.a, lease.RunID)
		if err != nil || run.Status != "running" {
			t.Fatal("终态部分提交", err)
		}
		execute(t, fixture.pool, `DROP TRIGGER reject_execution_event ON tombot.user_events`)
		if _, err = fixture.service.Complete(ctx, lease, Outcome{Status: "succeeded", Answer: "已完成"}); err != nil {
			t.Fatal("修复后结果不能提交", err)
		}
	})
}
