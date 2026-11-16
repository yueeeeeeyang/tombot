package nodecatalog

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

type administrationAuthorizer struct {
	err    error
	recent bool
}

func (a *administrationAuthorizer) RequireAdmin(_ context.Context, _ identity.Principal, recent bool) error {
	a.recent = recent
	return a.err
}

func TestNodeAdministration(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, name := cluster.Database(t)
	if err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	control := catalogDatabase(t, cluster, name, postgres.ControlRole)
	actor := identity.Principal{UserID: ids.New()}
	execute(t, pool, `INSERT INTO tombot.administrators(id,username,display_name,status) VALUES($1,'node-admin','节点管理员','active')`, actor.UserID)
	auth := &administrationAuthorizer{}
	service, err := NewAdministration(control, auth)
	if err != nil {
		t.Fatal(err)
	}
	verifier, runtime := verifierFixture(t, control)
	nodeID := verifier.service.nodeID
	detail, err := service.GetNode(t.Context(), actor, nodeID)
	if err != nil || detail.ConfigurationVersion != 1 || detail.Verification != nil || detail.CanRetryVerification || detail.Diagnosis.Code != "waiting_verification" || auth.recent {
		t.Fatal(detail, err)
	}
	auth.err = identity.ErrForbidden
	if _, err = service.GetNode(t.Context(), actor, nodeID); !errors.Is(err, identity.ErrForbidden) {
		t.Fatal("未拒绝越权", err)
	}
	auth.err = identity.ErrVerificationRequired
	update := NodeUpdate{Name: "修改后的节点", Capacity: 5, ConfigurationVersion: 1}
	if _, err = service.UpdateNode(t.Context(), actor, nodeID, update, ids.New()); !errors.Is(err, identity.ErrVerificationRequired) || !auth.recent {
		t.Fatal("写入未要求近期验证", err)
	}
	auth.err = nil
	detail, err = service.UpdateNode(t.Context(), actor, nodeID, update, ids.New())
	if err != nil || detail.Name != update.Name || detail.Capacity != 5 || detail.ConfigurationVersion != 2 {
		t.Fatal(detail, err)
	}
	if _, err = service.UpdateNode(t.Context(), actor, nodeID, update, ids.New()); !errors.Is(err, ErrConflict) {
		t.Fatal("旧版本覆盖新配置", err)
	}
	update.ConfigurationVersion = 2
	if same, err := service.UpdateNode(t.Context(), actor, nodeID, update, ids.New()); err != nil || same.ConfigurationVersion != 2 {
		t.Fatal("同配置未幂等", same, err)
	}
	execute(t, pool, `UPDATE tombot.computer_nodes SET reserved=3 WHERE id=$1`, nodeID)
	update.Capacity = 2
	if _, err = service.UpdateNode(t.Context(), actor, nodeID, update, ids.New()); !errors.Is(err, ErrConflict) {
		t.Fatal("容量低于已预留", err)
	}
	for _, bad := range []NodeUpdate{{Name: "", Capacity: 5, ConfigurationVersion: 2}, {Name: " 带空格", Capacity: 5, ConfigurationVersion: 2}, {Name: "正常", Capacity: 10001, ConfigurationVersion: 2}, {Name: "正常", Capacity: 5}} {
		if _, err = service.UpdateNode(t.Context(), actor, nodeID, bad, ids.New()); !errors.Is(err, ErrInvalidInput) {
			t.Fatal("未校验配置", bad, err)
		}
	}
	processVerification(t, verifier)
	processVerification(t, verifier)
	detail, err = service.GetNode(t.Context(), actor, nodeID)
	if err != nil || detail.Status != "ready" || detail.Verification == nil || !detail.Verification.Capabilities.Ready() {
		t.Fatal(detail, err)
	}
	update.Capacity = 5
	update.IsDraining = true
	detail, err = service.UpdateNode(t.Context(), actor, nodeID, update, ids.New())
	if err != nil || detail.Status != "draining" || detail.CapabilityExpiresAt != nil || detail.Verification.IsValid || detail.Reserved != 3 {
		t.Fatal("暂停未撤回准入或改变预留", detail, err)
	}
	update.IsDraining = false
	update.ConfigurationVersion = detail.ConfigurationVersion
	detail, err = service.UpdateNode(t.Context(), actor, nodeID, update, ids.New())
	if err != nil || detail.Status != "bootstrapping" || detail.CapabilityExpiresAt != nil {
		t.Fatal("恢复接收跳过验收", detail, err)
	}
	if runtime.present {
		t.Fatal("未清理验收资源")
	}
	for _, code := range []error{nil, context.DeadlineExceeded, errors.New("外部错误内容"), &computer.Failure{Code: "container_identity_mismatch"}, &computer.Failure{Code: "invalid\ncontent"}} {
		got := verificationFailureCode(code)
		if got != "node_operation_unknown" && got != "node_call_timeout" && got != "container_identity_mismatch" {
			t.Fatal("错误码泄露原始信息", got)
		}
	}
	t.Run("并发编辑不能互相覆盖", func(t *testing.T) {
		// 使用无共享状态的授权器，专门验证数据库行锁与版本冲突。
		concurrent, err := NewAdministration(control, statelessAdministrator{})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		results := make(chan error, 2)
		for _, name := range []string{"并发节点甲", "并发节点乙"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := concurrent.UpdateNode(t.Context(), actor, nodeID, NodeUpdate{Name: name, Capacity: 5, ConfigurationVersion: detail.ConfigurationVersion}, ids.New())
				results <- err
			}()
		}
		wg.Wait()
		close(results)
		succeeded, conflicted := 0, 0
		for err := range results {
			if err == nil {
				succeeded++
			} else if errors.Is(err, ErrConflict) {
				conflicted++
			} else {
				t.Fatal(err)
			}
		}
		if succeeded != 1 || conflicted != 1 {
			t.Fatal(succeeded, conflicted)
		}
	})
	if _, err = service.GetNode(t.Context(), actor, ids.New()); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err = service.GetNode(t.Context(), actor, "bad"); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
}

type statelessAdministrator struct{}

func (statelessAdministrator) RequireAdmin(context.Context, identity.Principal, bool) error {
	return nil
}

func TestRetryQuarantinedNodePreservesOriginalResources(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, name := cluster.Database(t)
	if err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	control := catalogDatabase(t, cluster, name, postgres.ControlRole)
	actor := identity.Principal{UserID: ids.New()}
	execute(t, pool, `INSERT INTO tombot.administrators(id,username,display_name,status) VALUES($1,'retry-admin','节点管理员','active')`, actor.UserID)
	service, err := NewAdministration(control, statelessAdministrator{})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"checking", "cleaning"} {
		t.Run(phase, func(t *testing.T) {
			verifier, runtime := verifierFixture(t, control)
			if phase == "cleaning" {
				processVerification(t, verifier)
			}
			runtime.inspectionError = &computer.Failure{Code: "container_identity_mismatch"}
			if phase == "cleaning" {
				runtime.removalError = runtime.inspectionError
			}
			for range verifier.options.MaxAttempts {
				makeVerificationDue(t, pool, verifier)
				processVerification(t, verifier)
			}
			before, err := service.GetNode(t.Context(), actor, verifier.service.nodeID)
			if err != nil || before.Diagnosis.Code != "quarantined" || !before.CanRetryVerification || before.Verification.NodeErrorCode == nil || *before.Verification.NodeErrorCode != "container_identity_mismatch" {
				t.Fatal(before, err)
			}
			var original []byte
			if err := pool.QueryRow(`SELECT specification FROM tombot.node_verifications WHERE node_id=$1`, before.ID).Scan(&original); err != nil {
				t.Fatal(err)
			}
			if _, err = service.RetryNodeVerification(t.Context(), actor, before.ID, before.Verification.FencingToken+1, ids.New()); !errors.Is(err, ErrConflict) {
				t.Fatal("接受过时重试", err)
			}
			after, err := service.RetryNodeVerification(t.Context(), actor, before.ID, before.Verification.FencingToken, ids.New())
			if err != nil || after.Verification.State != "cleaning" || after.Status == "ready" || after.CanRetryVerification {
				t.Fatal(after, err)
			}
			var preserved []byte
			if err := pool.QueryRow(`SELECT specification FROM tombot.node_verifications WHERE node_id=$1`, before.ID).Scan(&preserved); err != nil || string(original) != string(preserved) {
				t.Fatal("重试改变原资源身份", err)
			}
			if _, err = service.RetryNodeVerification(t.Context(), actor, before.ID, before.Verification.FencingToken, ids.New()); !errors.Is(err, ErrConflict) {
				t.Fatal("重复重试未被拦截", err)
			}
			runtime.inspectionError = nil
			runtime.removalError = nil
			processVerification(t, verifier)
			if phase == "checking" {
				// 原资源清理确认后，下一轮才允许创建；不从隔离状态直接重放 Ensure。
				if runtime.ensureCalls != 0 {
					t.Fatal("清理前重新创建了隔离资源")
				}
				makeVerificationDue(t, pool, verifier)
				processVerification(t, verifier)
				processVerification(t, verifier)
			}
			final, err := service.GetNode(t.Context(), actor, before.ID)
			if err != nil || final.Status != "ready" || runtime.ensureCalls != 1 || runtime.deleteCalls != 1 {
				t.Fatal(final, err, runtime.ensureCalls, runtime.deleteCalls)
			}
			var audits int
			if err := pool.QueryRow(`SELECT count(*) FROM tombot.admin_audit_events WHERE target_id=$1 AND action='重新核对节点验收'`, before.ID).Scan(&audits); err != nil || audits != 1 {
				t.Fatal("审计未单次记录", audits, err)
			}
		})
	}
}

func TestNodeDiagnosisNeverInfersReadiness(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Minute)
	past := now.Add(-time.Minute)
	node := NodeDetail{Node: Node{Status: "bootstrapping", LastSeenAt: &now}, ControlExpiresAt: &future}
	if diagnoseNode(node, now).Code != "waiting_verification" {
		t.Fatal(node)
	}
	node.Verification = &NodeVerification{State: "quarantined"}
	if diagnoseNode(node, now).Code != "quarantined" {
		t.Fatal(node)
	}
	node.ControlExpiresAt = &past
	if diagnoseNode(node, now).Code != "offline" {
		t.Fatal(node)
	}
	node.IsDraining = true
	if diagnoseNode(node, now).Code != "draining" {
		t.Fatal(node)
	}
}
