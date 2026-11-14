package modelregistry

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestDisabledModelEditingAndDeletion(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	values := func(model Model) ModelRequest {
		return ModelRequest{ModelKey: model.ModelKey, DisplayName: "修改后的模型", ContextLimit: 32768, OutputLimit: 2048, SupportsTools: true, SupportsVision: true, ReasoningEffort: "high", Metadata: ModelMetadata{InputModalities: []string{"text", "image"}, OutputModalities: []string{"text"}, Effort: &ModelEffort{SupportedLevels: []string{"low", "high"}, DefaultLevel: "high"}}}
	}
	disable := func(f fixture) {
		t.Helper()
		if err := f.service.SetEnabled(ctx, f.actor, ids.New(), "model", f.model.ID, 1, false); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("仅停用可编辑且历史快照不变", func(t *testing.T) {
		f := newFixture(t, cluster)
		request := values(f.model)

		f.authorizer.denied.Store(true)
		_, denied := f.service.UpdateModel(ctx, f.actor, ids.New(), f.model.ID, 1, request)
		requireError(t, denied, identity.ErrForbidden)
		requireError(t, f.service.DeleteModel(ctx, f.actor, ids.New(), f.model.ID, 1), identity.ErrForbidden)
		f.authorizer.denied.Store(false)
		_, err := f.service.UpdateModel(ctx, f.actor, ids.New(), f.model.ID, 1, request)
		requireError(t, err, ErrModelEnabled)
		requireError(t, f.service.DeleteModel(ctx, f.actor, ids.New(), f.model.ID, 1), ErrModelEnabled)
		version := f.policy(t)
		disable(f)
		request.ModelKey = "new-model-key"
		updated, err := f.service.UpdateModel(ctx, f.actor, ids.New(), f.model.ID, 2, request)
		if err != nil || updated.IsEnabled || updated.Revision != 3 || updated.ProviderID != f.provider.ID || updated.ModelKey != request.ModelKey || updated.Metadata.Effort == nil {
			t.Fatal("编辑结果错误", err)
		}
		versions, err := f.service.ListVersions(ctx, f.actor, version.PolicyID, 0, 10)
		if err != nil || len(versions) != 1 || versions[0].Candidates[0].Configuration != version.Candidates[0].Configuration {
			t.Fatal("历史快照被改写", err)
		}
		requireError(t, f.service.DeleteModel(ctx, f.actor, ids.New(), f.model.ID, 3), ErrModelReferenced)
		_, err = f.service.UpdateModel(ctx, f.actor, ids.New(), f.model.ID, 2, request)
		requireError(t, err, ErrConflict)
		for _, query := range []string{
			`UPDATE tombot.models SET display_name='越过修订号' WHERE id=$1`,
			`UPDATE tombot.models SET model_key='同时启用',is_enabled=true,revision=revision+1 WHERE id=$1`,
			`UPDATE tombot.models SET created_at=created_at+interval '1 second',revision=revision+1 WHERE id=$1`,
			`DELETE FROM tombot.models WHERE id=$1`,
		} {
			if _, err = f.pool.Exec(query, f.model.ID); err == nil {
				t.Fatal("数据库允许越过模型保护")
			}
		}
	})
	t.Run("无引用可删除并持久化审计", func(t *testing.T) {
		f := newFixture(t, cluster)
		disable(f)
		requireError(t, f.service.DeleteModel(ctx, f.actor, ids.New(), f.model.ID, 1), ErrConflict)
		requestID := ids.New()
		if err := f.service.DeleteModel(ctx, f.actor, requestID, f.model.ID, 2); err != nil {
			t.Fatal(err)
		}
		models, err := f.service.ListModels(ctx, f.actor, f.provider.ID, "", 10)
		if err != nil || len(models) != 0 {
			t.Fatal("删除未生效", err)
		}
		var count int
		if err = f.pool.QueryRow(`SELECT count(*) FROM tombot.admin_audit_events WHERE request_id=$1 AND action='删除停用模型'`, requestID).Scan(&count); err != nil || count != 1 {
			t.Fatal("删除审计缺失", err)
		}
		requireError(t, f.service.DeleteModel(ctx, f.actor, ids.New(), f.model.ID, 2), ErrNotFound)
	})
	t.Run("校验边界、重名与并发修订", func(t *testing.T) {
		f := newFixture(t, cluster)
		disable(f)
		request := values(f.model)
		for _, change := range []func(*ModelRequest){
			func(r *ModelRequest) { r.ProviderID = ids.New() }, func(r *ModelRequest) { r.OutputLimit = r.ContextLimit + 1 }, func(r *ModelRequest) { r.ModelKey = "" }, func(r *ModelRequest) { r.ReasoningEffort = "max" }, func(r *ModelRequest) { r.Metadata.InputModalities = []string{"text"} },
		} {
			invalid := request
			change(&invalid)
			_, err := f.service.UpdateModel(ctx, f.actor, ids.New(), f.model.ID, 2, invalid)
			requireError(t, err, ErrInvalidInput)
		}
		duplicate := request
		duplicate.ProviderID = f.provider.ID
		duplicate.ModelKey = "duplicate"
		if _, err := f.service.CreateModel(ctx, f.actor, ids.New(), duplicate); err != nil {
			t.Fatal(err)
		}
		duplicate.ProviderID = ""
		_, err := f.service.UpdateModel(ctx, f.actor, ids.New(), f.model.ID, 2, duplicate)
		requireError(t, err, ErrConflict)
		results := make(chan error, 2)
		for range 2 {
			go func() {
				_, err := f.service.UpdateModel(ctx, f.actor, ids.New(), f.model.ID, 2, request)
				results <- err
			}()
		}
		successes := 0
		for range 2 {
			err := <-results
			if err == nil {
				successes++
			} else if !errors.Is(err, ErrConflict) {
				t.Fatal(err)
			}
		}
		if successes != 1 {
			t.Fatal("并发编辑没有仅提交一次")
		}
	})
	t.Run("审计失败回滚编辑及删除", func(t *testing.T) {
		f := newFixture(t, cluster)
		disable(f)
		execute(t, f.pool, `REVOKE INSERT ON tombot.admin_audit_events FROM tombot_control`)
		if _, err := f.service.UpdateModel(ctx, f.actor, ids.New(), f.model.ID, 2, values(f.model)); err == nil {
			t.Fatal("审计失败仍编辑成功")
		}
		if err := f.service.DeleteModel(ctx, f.actor, ids.New(), f.model.ID, 2); err == nil {
			t.Fatal("审计失败仍删除成功")
		}
		if err := f.service.control.Platform(ctx, func(tx *sql.Tx) error {
			var revision int64
			err := tx.QueryRowContext(ctx, `SELECT revision FROM tombot.models WHERE id=$1`, f.model.ID).Scan(&revision)
			if err == nil && revision != 2 {
				t.Error("失败事务未回滚")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
}
