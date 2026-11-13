package accounts

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/identity"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func (fixture fixture) identity(t *testing.T) *identity.Service {
	t.Helper()
	role := fixture.cluster.Login(t, string(postgres.IdentityRole))
	database, err := postgres.Open(context.Background(), postgres.Options{DSN: fixture.cluster.DSN(fixture.name, role), Role: postgres.IdentityRole, MaxConnections: 4,
		ConnectTimeout: 5 * time.Second, StatementTimeout: 5 * time.Second, LockTimeout: time.Second, TransactionTimeout: 10 * time.Second, ConnectionLifetime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})
	key, err := secure.Random(32)
	if err != nil {
		t.Fatal(err)
	}
	service, err := identity.New(database, identity.Options{AccessLifetime: 5 * time.Minute, SessionLifetime: 24 * time.Hour, VerificationLifetime: 5 * time.Minute,
		RateWindow: time.Minute, AccountAttempts: 100, SourceAttempts: 1000, PasswordWorkers: 2, DeviceLimit: 20, EncryptionKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestActivationAndRecoveryIntegration(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("开户签发激活材料与登录完整联动", func(t *testing.T) {
		fixture := newFixture(t, cluster, true)
		identityService := fixture.identity(t)
		request := fixture.request()
		operation, err := fixture.service.reserve(ctx, fixture.actor.UserID, request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = fixture.service.IssueActivation(ctx, fixture.actor, operation.UserID, ids.New()); !errors.Is(err, ErrActivationUnavailable) {
			t.Fatal("电脑创建之前可签发激活材料", err)
		}
		fixture.process(t)
		first, err := fixture.service.IssueActivation(ctx, fixture.actor, operation.UserID, ids.New())
		if err != nil {
			t.Fatal(err)
		}
		second, err := fixture.service.IssueActivation(ctx, fixture.actor, operation.UserID, ids.New())
		if err != nil {
			t.Fatal(err)
		}
		if first.Value == second.Value || second.UserID != operation.UserID {
			t.Fatal("重复签发未轮换激活材料")
		}
		password, err := secure.Token()
		if err != nil {
			t.Fatal(err)
		}
		attempt := func() identity.Attempt {
			return identity.Attempt{Source: netip.MustParseAddr("127.0.0.1"), RequestID: ids.New()}
		}
		if err = identityService.Activate(ctx, string(first.Value), password, "", attempt()); !errors.Is(err, identity.ErrInvalidActivation) {
			t.Fatal("旧激活材料仍有效", err)
		}
		if err = identityService.Activate(ctx, string(second.Value), password, "", attempt()); err != nil {
			t.Fatal(err)
		}
		session, err := identityService.Login(ctx, identity.LoginRequest{Username: request.Username, Password: password, DeviceName: "开户联动测试", Platform: "linux", Scope: "desktop"}, attempt())
		if err != nil {
			t.Fatal(err)
		}
		principal, err := identityService.Authenticate(ctx, string(session.AccessToken), "desktop")
		if err != nil || principal.UserID != operation.UserID {
			t.Fatal("新会话未绑定开户用户", err)
		}
		if _, err = fixture.service.IssueActivation(ctx, fixture.actor, operation.UserID, ids.New()); !errors.Is(err, ErrActivationUnavailable) {
			t.Fatal("已激活用户仍可重新签发材料", err)
		}
		if _, err = fixture.service.Reconcile(ctx, fixture.actor, operation.ID, ids.New()); !errors.Is(err, ErrConflict) {
			t.Fatal("对账可清理已启用电脑", err)
		}
		fixture.assertState(t, operation, "succeeded", "active", "ready", 1)
	})
	t.Run("隔离恢复只做对账清理且不创建新实例", func(t *testing.T) {
		fixture := newFixture(t, cluster, true)
		operation := fixture.reserve(t)
		fixture.runtime.ensureError = &computer.Failure{IsPermanent: true}
		fixture.runtime.removeError = context.DeadlineExceeded
		for range 4 {
			fixture.process(t)
		}
		fixture.assertState(t, operation, "quarantined", "provision_failed", "quarantined", 1)
		fixture.runtime.removeError = nil
		reconciled, err := fixture.service.Reconcile(ctx, fixture.actor, operation.ID, ids.New())
		if !errors.Is(err, ErrPending) || reconciled.ID != operation.ID || reconciled.Attempt != 1 {
			t.Fatal("隔离对账未沿用原有操作", err)
		}
		fixture.process(t)
		fixture.assertState(t, operation, "failed", "provision_failed", "error", 0)
		if fixture.runtime.ensures != 1 {
			t.Fatal("对账阶段重新创建了电脑")
		}
		fixture.runtime.ensureError = nil
		if _, err = fixture.service.Retry(ctx, fixture.actor, operation.ID, ids.New()); !errors.Is(err, ErrPending) {
			t.Fatal(err)
		}
		fixture.process(t)
		fixture.assertState(t, operation, "succeeded", "pending_activation", "ready", 1)
	})
}
