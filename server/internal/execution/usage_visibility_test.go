package execution

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"testing"

	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestModelAdmissionFailureDiagnostics(t *testing.T) {
	fixture := newFixture(t, pgtest.Start(t))
	fixture.send(t, fixture.a, fixture.plainBot(t))
	lease := fixture.claim(t)
	// 注入含原始内容的数据库错误，确认日志只保留安全分类且不调用外部模型。
	execute(t, fixture.pool, `CREATE FUNCTION tombot.reject_test_usage() RETURNS trigger LANGUAGE plpgsql AS $body$
		BEGIN RAISE EXCEPTION '原始数据库内容不可外传' USING ERRCODE='23514'; END $body$;
		CREATE TRIGGER reject_test_usage BEFORE INSERT ON tombot.usage_reservations FOR EACH ROW EXECUTE FUNCTION tombot.reject_test_usage()`)
	calls := 0
	worker := fixture.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, _ modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
		calls++
		return emitText(emit, "不应调用")
	}))
	var logs bytes.Buffer
	worker.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	run, err := worker.Process(context.Background(), lease)
	if err != nil || run.Status != "failed" || run.ErrorCode != "EXECUTION_FAILED" || calls != 0 {
		t.Fatal("记账失败未阻止模型调用", run.Status, run.ErrorCode, calls, err)
	}
	for _, expected := range []string{"模型操作记账失败", "SQLSTATE 23514", lease.UserID, lease.RunID} {
		if !strings.Contains(logs.String(), expected) {
			t.Fatal("缺少记账诊断字段", expected)
		}
	}
	if strings.Contains(logs.String(), "原始数据库内容不可外传") {
		t.Fatal("原始数据库错误进入日志")
	}
	if known, held := fixture.totals(t, lease.UserID); known != 0 || held != 0 {
		t.Fatal("记账失败留下用量预留", known, held)
	}
}

func TestModelUsageVerifierTenantIsolation(t *testing.T) {
	cluster := pgtest.Start(t)
	fixture := newFixture(t, cluster)
	fixture.send(t, fixture.a, fixture.first)
	first := fixture.claim(t)
	fixture.prepareModel(t, first)
	fixture.send(t, fixture.b, fixture.foreign)
	second := fixture.claim(t)
	fixture.prepareModel(t, second)
	var database, owner string
	var bypass, isSuper, isTenant, isControl bool
	if err := fixture.pool.QueryRow(`SELECT current_database(),r.rolname,r.rolsuper,r.rolbypassrls,
		pg_has_role(r.oid,'tombot_tenant','MEMBER'),pg_has_role(r.oid,'tombot_control','MEMBER')
		FROM pg_proc p JOIN pg_roles r ON r.oid=p.proowner WHERE p.oid='tombot.check_usage_binding()'::regprocedure`).Scan(&database, &owner, &isSuper, &bypass, &isTenant, &isControl); err != nil {
		t.Fatal(err)
	}
	if isSuper || bypass || isTenant || isControl {
		t.Fatal("用量验证依赖了额外角色或绕过隔离权限")
	}
	connection := pgtest.Connect(t, cluster.DSN(database, owner))
	for _, userID := range []string{"", fixture.a.UserID, fixture.b.UserID} {
		t.Run(userID, func(t *testing.T) {
			ctx := context.Background()
			transaction, err := connection.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := transaction.Rollback(); err != nil {
					t.Error(err)
				}
			})
			if _, err = transaction.ExecContext(ctx, `SELECT set_config('tombot.user_id',$1,true)`, userID); err != nil {
				t.Fatal(err)
			}
			var total, foreign int
			if err = transaction.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER(WHERE user_id<>$1) FROM tombot.model_routes`, userID).Scan(&total, &foreign); err != nil {
				t.Fatal(err)
			}
			want := 1
			if userID == "" {
				want = 0
			}
			if total != want || foreign != 0 {
				t.Fatal("用量验证身份读取了其他用户或无身份路由", total, foreign)
			}
		})
	}
}
