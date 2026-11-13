package accounts

import (
	"context"
	"testing"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestDesktopConcurrencyProvisioning(t *testing.T) {
	fixture := newFixture(t, pgtest.Start(t), true)
	for _, maximum := range []int{0, -1, computer.MaxConcurrentDesktops + 1} {
		options := fixture.service.options
		options.DesktopConcurrency = maximum
		if _, err := New(fixture.service.database, fixture.runtime, fixture.authorizer, options); err == nil {
			t.Fatal("开户接受未配置或无界桌面并发额度", maximum)
		}
	}
	options := fixture.service.options
	options.DesktopConcurrency = computer.MaxConcurrentDesktops
	if _, err := New(fixture.service.database, fixture.runtime, fixture.authorizer, options); err != nil {
		t.Fatal("拒绝合法额度上界", err)
	}
	fixture.service.options.DesktopConcurrency = 7
	request := fixture.request()
	operation, err := fixture.service.reserve(context.Background(), fixture.actor.UserID, request)
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.options.DesktopConcurrency = 9
	replayed, err := fixture.service.reserve(context.Background(), fixture.actor.UserID, request)
	if err != nil || replayed.ID != operation.ID {
		t.Fatal("开户重试未复用操作", err)
	}
	var maximum int
	if err = fixture.pool.QueryRow(`SELECT concurrent_desktops FROM tombot.user_limits WHERE user_id=$1`, operation.UserID).Scan(&maximum); err != nil || maximum != 7 {
		t.Fatal("开户未保存显式额度或重试覆盖已分配额度", maximum, err)
	}
}
