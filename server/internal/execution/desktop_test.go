package execution

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computercontrol"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func (fixture fixture) desktopRun(t *testing.T, bot bots.Bot) Lease {
	t.Helper()
	fixture.send(t, fixture.a, bot)
	lease := fixture.claim(t)
	fixture.start(t, lease)
	return lease
}

func (fixture fixture) extraBot(t *testing.T, name string) bots.Bot {
	t.Helper()
	spec := fixture.first.Spec
	spec.Name = name
	bot, _, err := fixture.catalog.Create(context.Background(), fixture.a, bots.CreateRequest{ClientRequestID: ids.New(), Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	return bot
}

func expectDesktopWait(t *testing.T, err error, reason string) {
	t.Helper()
	var waiting computercontrol.WaitError
	if !errors.Is(err, computercontrol.ErrWaiting) || !errors.As(err, &waiting) || waiting.Reason != reason {
		t.Fatal("等待原因不符", reason, err)
	}
}

func TestParallelDesktops(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("同电脑双桌面并行，隔离失败只占自身名额，代次独立递增", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		node := &sessionNode{}
		controller := fixture.computers(t, node, computerOptions())
		first := fixture.desktopRun(t, fixture.first)
		fixture.completeModel(t, first)
		second := fixture.desktopRun(t, fixture.second)
		third := fixture.desktopRun(t, fixture.extraBot(t, "第三桌面"))
		var group sync.WaitGroup
		failures := make(chan error, 2)
		for _, lease := range []Lease{first, second} {
			group.Go(func() { _, err := controller.Acquire(ctx, computerReference(lease)); failures <- err })
		}
		group.Wait()
		close(failures)
		for err := range failures {
			if err != nil {
				t.Fatal(err)
			}
		}
		one, err := controller.Acquire(ctx, computerReference(first))
		if err != nil {
			t.Fatal(err)
		}
		two, err := controller.Acquire(ctx, computerReference(second))
		if err != nil || one.ComputerID != two.ComputerID || one.DesktopID == two.DesktopID || one.UID == two.UID || one.GID != two.GID || one.FencingToken != 1 || two.FencingToken != 1 || node.opens != 2 {
			t.Fatal("未同时授予同电脑的独立桌面", err)
		}
		_, err = controller.Acquire(ctx, computerReference(third))
		expectDesktopWait(t, err, computercontrol.WaitUserCapacity)
		run, err := fixture.service.GetRun(ctx, fixture.a, third.RunID)
		if err != nil || run.ComputerWaitReason != computercontrol.WaitUserCapacity {
			t.Fatal("用户未获得额度等待原因", err)
		}
		var eventCount int
		if err = fixture.pool.QueryRow(`SELECT count(*) FROM tombot.user_events WHERE user_id=$1 AND kind='computer.session.updated' AND payload->>'run_id'=$2 AND payload->>'wait_reason'='user_capacity'`, third.UserID, third.RunID).Scan(&eventCount); err != nil || eventCount != 1 {
			t.Fatal("缺少等待原因事件", err)
		}
		_, err = controller.Acquire(ctx, computerReference(third))
		expectDesktopWait(t, err, computercontrol.WaitUserCapacity)
		var repeated int
		if err = fixture.pool.QueryRow(`SELECT count(*) FROM tombot.user_events WHERE user_id=$1 AND kind='computer.session.updated' AND payload->>'run_id'=$2 AND payload->>'wait_reason'='user_capacity'`, third.UserID, third.RunID).Scan(&repeated); err != nil || repeated != eventCount {
			t.Fatal("轮询重复写入等待事件", err)
		}
		node.failClose = true
		expect(t, controller.Release(ctx, computerReference(first)), computercontrol.ErrNode)
		if err = controller.Renew(ctx, computerReference(second)); err != nil {
			t.Fatal("其他桌面被清理失败阻塞", err)
		}
		_, err = controller.Acquire(ctx, computerReference(third))
		expectDesktopWait(t, err, computercontrol.WaitUserCapacity)
		node.failClose = false
		fixture.allowCleanup(t, first)
		if err = controller.Release(ctx, computerReference(first)); err != nil {
			t.Fatal(err)
		}
		if _, err = controller.Acquire(ctx, computerReference(third)); err != nil {
			t.Fatal(err)
		}
		run, err = fixture.service.GetRun(ctx, fixture.a, third.RunID)
		if err != nil || run.ComputerWaitReason != "" {
			t.Fatal("成功准入后保留了等待原因", err)
		}
		if _, err = fixture.service.Complete(ctx, first, Outcome{Status: "succeeded", Answer: "完成"}); err != nil {
			t.Fatal(err)
		}
		next := fixture.desktopRun(t, fixture.first)
		if err = controller.Release(ctx, computerReference(third)); err != nil {
			t.Fatal(err)
		}
		newSession, err := controller.Acquire(ctx, computerReference(next))
		if err != nil || newSession.DesktopIdentity != one.DesktopIdentity || newSession.FencingToken != one.FencingToken+1 {
			t.Fatal("同一桌面未沿用身份并增加代次", err)
		}
		before := node.closes
		if err = controller.Release(ctx, computerReference(first)); err != nil || node.closes != before {
			t.Fatal("旧清理作用于新桌面会话", err)
		}
		if err = controller.Check(ctx, computerReference(second)); err != nil {
			t.Fatal("其他桌面被错误关闭", err)
		}
	})
	t.Run("并发准入不超额度，旧等待优先且取消不阻塞空闲桌面", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		node := &sessionNode{}
		controller := fixture.computers(t, node, computerOptions())
		leases := []Lease{fixture.desktopRun(t, fixture.first), fixture.desktopRun(t, fixture.second), fixture.desktopRun(t, fixture.extraBot(t, "并发第三")), fixture.desktopRun(t, fixture.extraBot(t, "并发第四"))}
		var group sync.WaitGroup
		failures := make(chan error, len(leases))
		for _, lease := range leases {
			group.Go(func() { _, err := controller.Acquire(ctx, computerReference(lease)); failures <- err })
		}
		group.Wait()
		close(failures)
		active, waiting := 0, 0
		for err := range failures {
			if err == nil {
				active++
			} else if errors.Is(err, computercontrol.ErrWaiting) {
				waiting++
			} else {
				t.Fatal(err)
			}
		}
		if active != 2 || waiting != 2 || node.opens != 2 {
			t.Fatal("并发准入超额或丢失", active, waiting, node.opens)
		}
		var firstWaiting, secondWaiting Lease
		for _, lease := range leases {
			if fixture.sessionState(t, lease) == "active" {
				if err := controller.Release(ctx, computerReference(lease)); err != nil {
					t.Fatal(err)
				}
			} else if firstWaiting.RunID == "" {
				firstWaiting = lease
			} else {
				secondWaiting = lease
			}
		}
		var earlier bool
		if err := fixture.pool.QueryRow(`SELECT a.ticket<b.ticket FROM tombot.computer_sessions a JOIN tombot.computer_sessions b ON b.user_id=a.user_id WHERE a.user_id=$1 AND a.run_id=$2 AND b.run_id=$3`, fixture.a.UserID, firstWaiting.RunID, secondWaiting.RunID).Scan(&earlier); err != nil {
			t.Fatal(err)
		}
		if !earlier {
			firstWaiting, secondWaiting = secondWaiting, firstWaiting
		}
		execute(t, fixture.pool, `UPDATE tombot.user_limits SET concurrent_desktops=1 WHERE user_id=$1`, fixture.a.UserID)
		_, err := controller.Acquire(ctx, computerReference(secondWaiting))
		expectDesktopWait(t, err, computercontrol.WaitUserCapacity)
		if _, err = fixture.service.Cancel(ctx, fixture.a, firstWaiting.RunID); err != nil {
			t.Fatal(err)
		}
		if _, err = controller.Acquire(ctx, computerReference(secondWaiting)); err != nil {
			t.Fatal("已取消的其他桌面排队仍阻塞准入", err)
		}
	})
	t.Run("桌面归属与物理映射不能由工具授权替换", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		controller := fixture.computers(t, &sessionNode{}, computerOptions())
		lease := fixture.desktopRun(t, fixture.first)
		session, err := controller.Acquire(ctx, computerReference(lease))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte("desktop-bound-operation"))
		for _, change := range []func(*computer.ExecutionSession){
			func(value *computer.ExecutionSession) { value.DesktopID = fixture.second.DesktopID },
			func(value *computer.ExecutionSession) { value.UID++; value.DisplayNumber++ },
		} {
			forged := session
			change(&forged)
			_, _, err = fixture.service.BeginOperation(ctx, lease, OperationRequest{StepKey: "tool.1.1", Kind: "computer", ToolName: "file_read", RequestDigest: digest[:], Computer: &forged})
			expect(t, err, ErrLeaseLost)
		}
		for _, query := range []string{`UPDATE tombot.computer_sessions SET desktop_id=$2 WHERE user_id=$1`, `UPDATE tombot.desktop_execution_heads SET last_fencing_token=0 WHERE user_id=$1`} {
			err = fixture.tenant.Tenant(ctx, fixture.a.UserID, func(tx *sql.Tx) error {
				if query == `UPDATE tombot.computer_sessions SET desktop_id=$2 WHERE user_id=$1` {
					_, err := tx.Exec(query, fixture.a.UserID, fixture.second.DesktopID)
					return err
				}
				_, err := tx.Exec(query, fixture.a.UserID)
				return err
			})
			var failure *pgconn.PgError
			if !errors.As(err, &failure) || failure.Code != "23514" {
				t.Fatal("数据库允许替换身份或降低代次", err)
			}
		}
	})
	t.Run("数据库拒绝同桌面重复占用和绕过并发额度", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		controller := fixture.computers(t, &sessionNode{}, computerOptions())
		first := fixture.desktopRun(t, fixture.first)
		second := fixture.desktopRun(t, fixture.second)
		one, err := controller.Acquire(ctx, computerReference(first))
		if err != nil {
			t.Fatal(err)
		}
		copySession := `INSERT INTO tombot.computer_sessions(user_id,id,computer_id,node_id,container_id,generation,job_id,run_id,bot_id,job_owner,job_fencing_token,job_attempt,ticket,status,wait_deadline_at,desktop_id,linux_uid,shared_gid,display_number,fencing_token,revision,hold_deadline_at,lease_expires_at)
		SELECT user_id,$3,computer_id,node_id,container_id,generation,job_id,run_id,bot_id,job_owner,job_fencing_token,job_attempt+1,ticket+100,'starting',wait_deadline_at,desktop_id,linux_uid,shared_gid,display_number,fencing_token+1,revision,hold_deadline_at,lease_expires_at
		FROM tombot.computer_sessions WHERE user_id=$1 AND id=$2`
		err = fixture.tenant.Tenant(ctx, fixture.a.UserID, func(tx *sql.Tx) error {
			_, err := tx.Exec(copySession, fixture.a.UserID, one.SessionID, ids.New())
			return err
		})
		var failure *pgconn.PgError
		if !errors.As(err, &failure) || failure.Code != "23505" || failure.ConstraintName != "computer_sessions_desktop_exclusive" {
			t.Fatal("同一桌面允许双控制者", err)
		}
		execute(t, fixture.pool, `UPDATE tombot.user_limits SET concurrent_desktops=1 WHERE user_id=$1`, fixture.a.UserID)
		_, err = controller.Acquire(ctx, computerReference(second))
		expectDesktopWait(t, err, computercontrol.WaitUserCapacity)
		err = fixture.tenant.Tenant(ctx, fixture.a.UserID, func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE tombot.computer_sessions SET status='starting',wait_reason='',fencing_token=1,revision=1,hold_deadline_at=clock_timestamp()+interval '1 minute',lease_expires_at=clock_timestamp()+interval '10 seconds' WHERE user_id=$1 AND run_id=$2`, fixture.a.UserID, second.RunID)
			return err
		})
		if !errors.As(err, &failure) || failure.Code != "23514" {
			t.Fatal("数据库允许绕过用户并发额度", err)
		}
	})
}
