package accounts

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func expireNode(t *testing.T, fixture fixture) {
	t.Helper()
	execute(t, fixture.pool, `UPDATE tombot.computer_nodes SET control_sequence=control_sequence+1,last_seen_at=clock_timestamp()-interval '2 minutes',control_expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, fixture.nodeID)
}

func TestNodeControlLeaseGatesAccountAdmission(t *testing.T) {
	cluster := pgtest.Start(t)
	t.Run("失联禁止新建并显示离线", func(t *testing.T) {
		fixture := newFixture(t, cluster, true)
		expireNode(t, fixture)
		if _, err := fixture.service.Create(t.Context(), fixture.actor, fixture.request()); !errors.Is(err, ErrNoCapacity) {
			t.Fatal("过期控制节点接受新开户", err)
		}
		nodes, err := fixture.service.ListNodes(t.Context(), fixture.actor, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, node := range nodes {
			if node.ID == fixture.nodeID {
				found = true
				if node.Status != "offline" {
					t.Fatal("目录继续展示旧就绪")
				}
			}
		}
		if !found {
			t.Fatal("节点丢失")
		}
		var count, reserved int
		if err := fixture.pool.QueryRow(`SELECT (SELECT count(*) FROM tombot.user_creation_operations),reserved FROM tombot.computer_nodes WHERE id=$1`, fixture.nodeID).Scan(&count, &reserved); err != nil || count != 0 || reserved != 0 {
			t.Fatal("被拒请求遗留账户或容量", count, reserved, err)
		}
	})
	t.Run("失败重试同样检查控制租约", func(t *testing.T) {
		fixture := newFixture(t, cluster, true)
		operation := fixture.reserve(t)
		fixture.runtime.ensureError = &computer.Failure{Code: "test_failure", IsPermanent: true}
		fixture.process(t)
		fixture.process(t)
		expireNode(t, fixture)
		if _, err := fixture.service.Retry(t.Context(), fixture.actor, operation.ID, ids.New()); !errors.Is(err, ErrNoCapacity) {
			t.Fatal("过期节点允许重试开户", err)
		}
		fixture.assertState(t, operation, "failed", "provision_failed", "error", 0)
	})
	t.Run("等待行锁后重新判断过期", func(t *testing.T) {
		fixture := newFixture(t, cluster, true)
		execute(t, fixture.pool, `UPDATE tombot.computer_nodes SET control_sequence=control_sequence+1,last_seen_at=clock_timestamp(),control_expires_at=clock_timestamp()+interval '100 milliseconds' WHERE id=$1`, fixture.nodeID)
		transaction, err := fixture.pool.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := transaction.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				t.Error(err)
			}
		}()
		if _, err := transaction.ExecContext(t.Context(), `SELECT id FROM tombot.computer_nodes WHERE id=$1 FOR UPDATE`, fixture.nodeID); err != nil {
			t.Fatal(err)
		}
		request := fixture.request()
		done := make(chan error, 1)
		go func() {
			_, err := fixture.service.reserve(t.Context(), fixture.actor.UserID, request)
			done <- err
		}()
		timer := time.NewTimer(180 * time.Millisecond)
		<-timer.C
		if err := transaction.Commit(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if !errors.Is(err, ErrNoCapacity) {
				t.Fatal("锁前的有效租约在过期后被使用", err)
			}
		case <-time.After(time.Second):
			t.Fatal("准入未结束")
		}
	})
}

func TestNodeCapabilityProofGatesAccountAdmission(t *testing.T) {
	cluster := pgtest.Start(t)
	for _, scenario := range []string{"证据过期", "不同镜像", "失败重试"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newFixture(t, cluster, true)
			request := fixture.request()
			var operation Operation
			if scenario == "失败重试" {
				operation = fixture.reserve(t)
				fixture.runtime.ensureError = &computer.Failure{Code: "test_failure", IsPermanent: true}
				fixture.process(t)
				fixture.process(t)
			}
			if scenario == "不同镜像" {
				request.ImageID = ids.New()
				execute(t, fixture.pool, `INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2)`, request.ImageID, "example.test/other@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			} else {
				execute(t, fixture.pool, `UPDATE tombot.computer_nodes SET capability_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, fixture.nodeID)
			}
			var err error
			if scenario == "失败重试" {
				_, err = fixture.service.Retry(t.Context(), fixture.actor, operation.ID, ids.New())
			} else {
				_, err = fixture.service.Create(t.Context(), fixture.actor, request)
			}
			if !errors.Is(err, ErrNoCapacity) {
				t.Fatal("无有效镜像能力证据仍准入", err)
			}
			var reserved int
			if err = fixture.pool.QueryRow(`SELECT reserved FROM tombot.computer_nodes WHERE id=$1`, fixture.nodeID).Scan(&reserved); err != nil || reserved != 0 {
				t.Fatal("被拒准入仍占用容量", err)
			}
		})
	}
}
