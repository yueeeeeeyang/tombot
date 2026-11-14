package modelregistry

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestMultiModelVersions(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("继承参数并验证所有候选", func(t *testing.T) {
		f := newFixture(t, cluster)
		model, err := f.service.CreateModel(ctx, f.actor, ids.New(), ModelRequest{ProviderID: f.provider.ID, ModelKey: "vision", DisplayName: "视觉模型", ContextLimit: 8192, OutputLimit: 2048, SupportsTools: true, SupportsVision: true, ReasoningEffort: "high", Metadata: ModelMetadata{Effort: &ModelEffort{SupportedLevels: []string{"low", "high"}, DefaultLevel: "high"}}})
		if err != nil {
			t.Fatal(err)
		}
		p, err := f.service.CreatePolicy(ctx, f.actor, ids.New(), "多模型策略")
		if err != nil {
			t.Fatal(err)
		}
		v, err := f.service.CreateVersion(ctx, f.actor, ids.New(), VersionRequest{PolicyID: p.ID, RoutingMode: "weighted", Candidates: []CandidateRequest{{ModelID: f.model.ID, Priority: 1, Weight: 8}, {ModelID: model.ID, Priority: 2, Weight: 2}}})
		if err != nil {
			t.Fatal(err)
		}
		if len(v.Candidates) != 2 || v.Candidates[0].Configuration.OutputLimit != f.model.OutputLimit || v.Candidates[1].Configuration.OutputLimit != 2048 || v.Candidates[1].Configuration.ReasoningEffort != "high" || v.Candidates[1].CredentialID == nil || *v.Candidates[1].CredentialID != f.credential.ID {
			t.Fatal("候选未继承配置")
		}
		enabled := f.enableVersion(t, v)
		if f.verifier.calls.Load() != 2 {
			t.Fatal("未验证每一个候选")
		}
		listed, err := f.service.ListVersions(ctx, f.actor, p.ID, 0, 10)
		if err != nil || len(listed) != 1 || !reflect.DeepEqual(listed[0].Candidates, enabled.Candidates) {
			t.Fatal("候选快照分页丢失", err)
		}
	})
	t.Run("任一候选失败不启用且审计保留", func(t *testing.T) {
		f := newFixture(t, cluster)
		p, err := f.service.CreatePolicy(ctx, f.actor, ids.New(), "失败策略")
		if err != nil {
			t.Fatal(err)
		}
		previous := f.enableVersion(t, f.createVersion(t, p.ID, &f.credential.ID, 128))
		model, err := f.service.CreateModel(ctx, f.actor, ids.New(), ModelRequest{ProviderID: f.provider.ID, ModelKey: "rejected", DisplayName: "失败模型", ContextLimit: 4096, OutputLimit: 512})
		if err != nil {
			t.Fatal(err)
		}
		v, err := f.service.CreateVersion(ctx, f.actor, ids.New(), VersionRequest{PolicyID: p.ID, RoutingMode: "priority", Candidates: []CandidateRequest{{ModelID: f.model.ID, Priority: 1, Weight: 1}, {ModelID: model.ID, Priority: 2, Weight: 1}}})
		if err != nil {
			t.Fatal(err)
		}
		f.verifier.verify = func(_ context.Context, c Configuration, _ []byte) error {
			if c.ModelKey == "rejected" {
				return errors.New("测试拒绝")
			}
			return nil
		}
		_, err = f.service.EnableVersion(ctx, f.actor, ids.New(), p.ID, v.Number, f.revision(t, p.ID))
		requireError(t, err, ErrVerification)
		stored, err := f.service.GetVersion(ctx, f.actor, p.ID, v.Number)
		if err != nil || stored.IsEnabled || stored.VerifiedAt != nil {
			t.Fatal("部分通过仍启用", err)
		}
		previous, err = f.service.GetVersion(ctx, f.actor, p.ID, previous.Number)
		if err != nil || !previous.IsEnabled {
			t.Fatal("验证失败停用了原版本", err)
		}
		var count int
		if err = f.pool.QueryRow(`SELECT count(*) FROM tombot.admin_audit_events WHERE target_id=$1 AND result='failed'`, p.ID).Scan(&count); err != nil || count != 1 {
			t.Fatal("缺少失败审计", err)
		}
	})
	t.Run("候选校验与不可变", func(t *testing.T) {
		f := newFixture(t, cluster)
		v := f.policy(t)
		base := CandidateRequest{ModelID: f.model.ID, Priority: 1, Weight: 1}
		for _, request := range []VersionRequest{
			{PolicyID: v.PolicyID, RoutingMode: "random", Candidates: []CandidateRequest{base}},
			{PolicyID: v.PolicyID, RoutingMode: "priority"},
			{PolicyID: v.PolicyID, RoutingMode: "priority", Candidates: []CandidateRequest{base, base}},
			{PolicyID: v.PolicyID, RoutingMode: "priority", Candidates: []CandidateRequest{{ModelID: f.model.ID, Priority: 0, Weight: 1}}},
			{PolicyID: v.PolicyID, RoutingMode: "priority", Candidates: []CandidateRequest{{ModelID: f.model.ID, Priority: 1, Weight: 1001}}},
			{PolicyID: v.PolicyID, RoutingMode: "priority", Candidates: []CandidateRequest{{ModelID: f.model.ID, Priority: 1, Weight: 1, OutputLimit: routingInt(f.model.OutputLimit + 1)}}},
		} {
			_, err := f.service.CreateVersion(ctx, f.actor, ids.New(), request)
			requireError(t, err, ErrInvalidInput)
		}
		if _, err := f.pool.Exec(`UPDATE tombot.model_policy_candidates SET weight=2 WHERE policy_id=$1`, v.PolicyID); err == nil {
			t.Fatal("历史权重可改写")
		}
		if _, err := f.pool.Exec(`INSERT INTO tombot.model_policy_candidates SELECT policy_id,version,2,provider_id,model_id,credential_id,configuration,priority,weight FROM tombot.model_policy_candidates WHERE policy_id=$1`, v.PolicyID); err == nil {
			t.Fatal("可向历史版本追加候选")
		}
		f.authorizer.denied.Store(true)
		_, err := f.service.EnableVersion(ctx, f.actor, ids.New(), v.PolicyID, v.Number, f.revision(t, v.PolicyID))
		requireError(t, err, identity.ErrForbidden)
	})
	t.Run("自由启用旧版本且并发只允许一项成功", func(t *testing.T) {
		f := newFixture(t, cluster)
		first := f.policy(t)
		second := f.enableVersion(t, f.createVersion(t, first.PolicyID, &f.credential.ID, 256))
		revision := f.revision(t, first.PolicyID)
		_, err := f.service.EnableVersion(ctx, f.actor, ids.New(), first.PolicyID, first.Number, revision-1)
		requireError(t, err, ErrConflict)
		if _, err := f.service.EnableVersion(ctx, f.actor, ids.New(), first.PolicyID, first.Number, revision); err != nil {
			t.Fatal(err)
		}
		third := f.createVersion(t, first.PolicyID, &f.credential.ID, 128)
		if third.IsEnabled || third.VerifiedAt != nil {
			t.Fatal("新版本没有默认停用")
		}
		third = f.enableVersion(t, third)
		revision = f.revision(t, first.PolicyID)
		var wg sync.WaitGroup
		results := make(chan error, 2)
		for _, v := range []Version{first, second} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := f.service.EnableVersion(ctx, f.actor, ids.New(), v.PolicyID, v.Number, revision)
				results <- err
			}()
		}
		wg.Wait()
		close(results)
		succeeded := 0
		for err := range results {
			if err == nil {
				succeeded++
			} else {
				requireError(t, err, ErrConflict)
			}
		}
		if succeeded != 1 {
			t.Fatal("并发启用未串行")
		}
		var active int64
		if err := f.pool.QueryRow(`SELECT current_version FROM tombot.model_policies WHERE id=$1`, first.PolicyID).Scan(&active); err != nil || active == third.Number {
			t.Fatal("未切换启用版本", err)
		}
		if err := f.service.DisableVersion(ctx, f.actor, ids.New(), first.PolicyID, active, f.revision(t, first.PolicyID)); err != nil {
			t.Fatal(err)
		}
		versions, err := f.service.ListVersions(ctx, f.actor, first.PolicyID, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, version := range versions {
			if version.IsEnabled {
				t.Fatal("停用后列表仍显示启用")
			}
		}
		calls := f.verifier.calls.Load()
		enabled, err := f.service.EnableVersion(ctx, f.actor, ids.New(), first.PolicyID, active, f.revision(t, first.PolicyID))
		if err != nil || !enabled.IsEnabled || f.verifier.calls.Load() != calls {
			t.Fatal("停用版本无法重新启用或重复验证", err)
		}
		versions, err = f.service.ListVersions(ctx, f.actor, first.PolicyID, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, version := range versions {
			if version.IsEnabled {
				count++
			}
		}
		if count != 1 {
			t.Fatal("同策略启用了多个版本")
		}
		// 过期停用不能覆盖其他管理员的选择，停用非当前版本也不能清空当前版本。
		revision = f.revision(t, first.PolicyID)
		requireError(t, f.service.DisableVersion(ctx, f.actor, ids.New(), first.PolicyID, active, revision-1), ErrConflict)
		if err := f.service.DisableVersion(ctx, f.actor, ids.New(), first.PolicyID, third.Number, revision); err != nil {
			t.Fatal(err)
		}
		stored, err := f.service.GetVersion(ctx, f.actor, first.PolicyID, active)
		if err != nil || !stored.IsEnabled || f.revision(t, first.PolicyID) != revision {
			t.Fatal("停用非当前版本影响了启用选择", err)
		}
		if err := f.service.DisableVersion(ctx, f.actor, ids.New(), first.PolicyID, active, revision); err != nil {
			t.Fatal(err)
		}
		revision = f.revision(t, first.PolicyID)
		if err := f.service.DisableVersion(ctx, f.actor, ids.New(), first.PolicyID, active, revision); err != nil || f.revision(t, first.PolicyID) != revision {
			t.Fatal("重复停用不是幂等操作", err)
		}
	})
}

func TestWeightedCandidateBoundaries(t *testing.T) {
	candidates := []Candidate{{ModelID: "first", Weight: 8}, {ModelID: "second", Weight: 2}}
	for ticket := range 10 {
		want := "first"
		if ticket >= 8 {
			want = "second"
		}
		if weightedCandidate(candidates, ticket).ModelID != want {
			t.Fatal("权重边界错误", ticket)
		}
	}
}
