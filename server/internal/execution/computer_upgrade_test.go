package execution

import (
	"context"
	"errors"
	"testing"

	"io.yueyang/tom/internal/computercontrol"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func (fixture fixture) gateComputerImageUpgrade(t *testing.T, userID string) string {
	t.Helper()
	image := ids.New()
	execute(t, fixture.pool, `INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2)`, image, "example.test/computer@sha256:"+secure.Hash(image))
	execute(t, fixture.pool, `INSERT INTO tombot.computer_image_upgrades(user_id,computer_id,node_id,source_image_id,target_image_id,source_generation,source_container_id,source_specification,target_specification,state)
		SELECT user_id,id,node_id,image_id,$2,generation,container_id,'{}','{}','draining' FROM tombot.computers WHERE user_id=$1`, userID, image)
	execute(t, fixture.pool, `UPDATE tombot.computers SET status='stopping' WHERE user_id=$1`, userID)
	return image
}

func expectImageUpgradeWait(t *testing.T, err error) {
	t.Helper()
	var wait computercontrol.WaitError
	if !errors.As(err, &wait) || wait.Reason != computercontrol.WaitStartingDesktop {
		t.Fatal("未等待电脑镜像切换", err)
	}
}

func TestComputerImageUpgradeAdmission(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("新申请等待切换且不会绑定旧容器", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		node := &sessionNode{}
		computers := fixture.computers(t, node, computerOptions())
		fixture.send(t, fixture.a, fixture.first)
		lease := fixture.claim(t)
		fixture.start(t, lease)
		image := fixture.gateComputerImageUpgrade(t, lease.UserID)
		for _, state := range []string{"draining", "switching"} {
			execute(t, fixture.pool, `UPDATE tombot.computer_image_upgrades SET state=$2 WHERE user_id=$1`, lease.UserID, state)
			_, err := computers.Acquire(ctx, computerReference(lease))
			expectImageUpgradeWait(t, err)
		}
		execute(t, fixture.pool, `UPDATE tombot.computer_image_upgrades SET state='quarantined' WHERE user_id=$1`, lease.UserID)
		_, err := computers.Acquire(ctx, computerReference(lease))
		expect(t, err, computercontrol.ErrUnavailable)
		var rows int
		if err = fixture.pool.QueryRow(`SELECT count(*) FROM tombot.computer_sessions WHERE user_id=$1`, lease.UserID).Scan(&rows); err != nil || rows != 0 || node.opens != 0 {
			t.Fatal("升级前签发了旧容器会话", rows, err)
		}
		containerID := secure.Hash("new-computer-image")
		execute(t, fixture.pool, `UPDATE tombot.computers SET status='ready',generation=generation+1,image_id=$2,container_id=$3 WHERE user_id=$1`, lease.UserID, image, containerID)
		execute(t, fixture.pool, `UPDATE tombot.computer_image_upgrades SET state='succeeded' WHERE user_id=$1`, lease.UserID)
		session, err := computers.Acquire(ctx, computerReference(lease))
		if err != nil || session.Generation != 2 || session.ContainerID != containerID || node.opens != 1 {
			t.Fatal("切换后未绑定新容器", session.Generation, err)
		}
	})
	t.Run("旧等待会话释放，已授权会话可正常排空", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		node := &sessionNode{}
		computers := fixture.computers(t, node, computerOptions())
		execute(t, fixture.pool, `UPDATE tombot.user_limits SET concurrent_desktops=1 WHERE user_id=$1`, fixture.a.UserID)
		fixture.send(t, fixture.a, fixture.first)
		first := fixture.claim(t)
		fixture.start(t, first)
		fixture.send(t, fixture.a, fixture.second)
		second := fixture.claim(t)
		fixture.start(t, second)
		active, err := computers.Acquire(ctx, computerReference(first))
		if err != nil {
			t.Fatal(err)
		}
		_, err = computers.Acquire(ctx, computerReference(second))
		expect(t, err, computercontrol.ErrWaiting)
		fixture.gateComputerImageUpgrade(t, first.UserID)
		_, err = computers.Acquire(ctx, computerReference(second))
		expectImageUpgradeWait(t, err)
		if fixture.sessionState(t, second) != "released" || node.closes != 0 || node.opens != 1 {
			t.Fatal("旧排队项未释放或无端调用了节点")
		}
		session, err := computers.Acquire(ctx, computerReference(first))
		if err != nil || session != active {
			t.Fatal("升级中断了已确认的有效会话", err)
		}
		if err = computers.Release(ctx, computerReference(first)); err != nil || node.closes != 1 {
			t.Fatal("升级期间无法正常收尾已授权会话", err)
		}
	})
}

func TestRecoverComputerImageUpgradeSession(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	for _, confirmed := range []bool{true, false} {
		name := "节点确认后释放"
		if !confirmed {
			name = "额外尝试有上限"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newFixture(t, cluster)
			node := &sessionNode{}
			options := computerOptions()
			options.MaxCleanupAttempts = 1
			computers := fixture.computers(t, node, options)
			fixture.send(t, fixture.a, fixture.first)
			lease := fixture.claim(t)
			fixture.start(t, lease)
			reference := computerReference(lease)
			session, err := computers.Acquire(ctx, reference)
			if err != nil {
				t.Fatal(err)
			}
			fixture.gateComputerImageUpgrade(t, lease.UserID)
			expect(t, computers.RecoverUpgradeSession(ctx, reference), computercontrol.ErrUnavailable)
			if node.closes != 0 {
				t.Fatal("清理了仍有有效租约的会话")
			}
			execute(t, fixture.pool, `UPDATE tombot.jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID)
			node.failClose = true
			expect(t, computers.Release(ctx, reference), computercontrol.ErrNode)
			expect(t, computers.Release(ctx, reference), computercontrol.ErrCleanupBlocked)
			fixture.allowCleanup(t, lease)
			execute(t, fixture.pool, `UPDATE tombot.computer_image_upgrades SET source_generation=$2 WHERE user_id=$1`, lease.UserID, session.Generation+1)
			expect(t, computers.RecoverUpgradeSession(ctx, reference), computercontrol.ErrUnavailable)
			if node.closes != 1 || fixture.sessionState(t, lease) != "quarantined" {
				t.Fatal("不匹配的升级记录影响了隔离会话")
			}
			execute(t, fixture.pool, `UPDATE tombot.computer_image_upgrades SET source_generation=$2 WHERE user_id=$1`, lease.UserID, session.Generation)
			node.failClose = false
			node.wrongClose = true
			expect(t, computers.RecoverUpgradeSession(ctx, reference), computercontrol.ErrNode)
			if node.closes != 2 || fixture.sessionState(t, lease) != "quarantined" {
				t.Fatal("旧实例清理接受了错误的节点回包")
			}
			expect(t, computers.RecoverUpgradeSession(ctx, reference), computercontrol.ErrWaiting)
			fixture.allowCleanup(t, lease)
			if confirmed {
				node.wrongClose = false
				if err = computers.RecoverUpgradeSession(ctx, reference); err != nil || fixture.sessionState(t, lease) != "released" {
					t.Fatal("有效清理证据未能释放旧会话", err)
				}
				return
			}
			for range 2 {
				expect(t, computers.RecoverUpgradeSession(ctx, reference), computercontrol.ErrNode)
				fixture.allowCleanup(t, lease)
			}
			expect(t, computers.RecoverUpgradeSession(ctx, reference), computercontrol.ErrCleanupBlocked)
			if node.closes != 4 || fixture.sessionState(t, lease) != "quarantined" {
				t.Fatal("超过额外清理次数上限或失去隔离状态")
			}
		})
	}
}
