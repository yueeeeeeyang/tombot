package modelregistry

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"io.yueyang/tom/internal/identity"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestUpdateDisabledProvider(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("状态校验及保留目录和历史快照", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		version := fixture.policy(t)
		update := func(revision int64, name, protocol, url string, credential []byte) (Provider, error) {
			return fixture.service.UpdateProvider(ctx, fixture.actor, ids.New(), fixture.provider.ID, revision, name, protocol, url, credential)
		}
		_, err := update(1, "新名称", "responses", "https://model.example.test", nil)
		requireError(t, err, ErrProviderEnabled)
		if err = fixture.service.SetEnabled(ctx, fixture.actor, ids.New(), "provider", fixture.provider.ID, 1, false); err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			name, protocol, url string
			revision            int64
			want                error
		}{
			{"新名称", "responses", "https://model.example.test", 1, ErrConflict},
			{"", "responses", "https://model.example.test", 2, ErrInvalidInput},
			{"新名称", "invalid", "https://model.example.test", 2, ErrInvalidInput},
			{"新名称", "responses", "http://model.example.test", 2, ErrInvalidInput},
			{"新名称", "responses", "https://other.example.test", 2, ErrNewCredentialRequired},
			{"新名称", "responses", "https://model.example.test:444", 2, ErrNewCredentialRequired},
		} {
			_, err = update(test.revision, test.name, test.protocol, test.url, nil)
			requireError(t, err, test.want)
		}
		fixture.authorizer.denied.Store(true)
		_, err = update(2, "新名称", "responses", "https://model.example.test", nil)
		requireError(t, err, identity.ErrForbidden)
		fixture.authorizer.denied.Store(false)
		updated, err := update(2, "新名称", "responses", "https://model.example.test", nil)
		if err != nil || updated.ID != fixture.provider.ID || updated.IsEnabled || updated.Revision != 3 || updated.Name != "新名称" || updated.Protocol != "responses" {
			t.Fatal("停用供应商编辑失败", err)
		}
		models, err := fixture.service.ListModels(ctx, fixture.actor, fixture.provider.ID, "", 10)
		if err != nil || len(models) != 1 || models[0].ID != fixture.model.ID {
			t.Fatal("编辑破坏模型目录", err)
		}
		versions, err := fixture.service.ListVersions(ctx, fixture.actor, version.PolicyID, 0, 10)
		if err != nil || len(versions) != 1 || versions[0].Candidates[0].Configuration != version.Candidates[0].Configuration {
			t.Fatal("历史策略被改写", err)
		}
		if err = fixture.service.SetEnabled(ctx, fixture.actor, ids.New(), "provider", fixture.provider.ID, 3, true); err != nil {
			t.Fatal(err)
		}
		stored, err := fixture.service.GetVersion(ctx, fixture.actor, version.PolicyID, version.Number)
		if err != nil || stored.Candidates[0].Configuration != version.Candidates[0].Configuration {
			t.Fatal("历史版本查询未保留快照", err)
		}
		_, err = fixture.service.EnableVersion(ctx, fixture.actor, ids.New(), version.PolicyID, version.Number, fixture.revision(t, version.PolicyID))
		requireError(t, err, ErrUnavailable)
		fixture.verifier.discover = func(_ context.Context, protocol, baseURL string, plaintext []byte) error {
			if protocol != "responses" || baseURL != "https://model.example.test" || !bytes.Equal(plaintext, fixture.plaintext) {
				t.Error("编辑后协议或原凭据绑定错误")
			}
			return nil
		}
		if _, err = fixture.service.ListRemoteModels(ctx, fixture.actor, fixture.provider.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = update(3, "新名称", "responses", "https://model.example.test", nil); !errors.Is(err, ErrConflict) {
			t.Fatal("过期编辑被重放", err)
		}
		if err = fixture.service.control.Platform(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE tombot.model_providers SET protocol='anthropic',revision=revision+1 WHERE id=$1`, fixture.provider.ID)
			return err
		}); err == nil {
			t.Fatal("数据库允许编辑启用供应商")
		}
		if err = fixture.service.SetEnabled(ctx, fixture.actor, ids.New(), "provider", fixture.provider.ID, 4, false); err != nil {
			t.Fatal(err)
		}
		for _, query := range []string{
			`UPDATE tombot.model_providers SET name='非法并发启用',is_enabled=true,revision=revision+1 WHERE id=$1`,
			`UPDATE tombot.model_providers SET name='缺失修订号' WHERE id=$1`,
			`UPDATE tombot.model_providers SET created_at=created_at+interval '1 second',revision=revision+1 WHERE id=$1`,
		} {
			if _, err = fixture.pool.Exec(query, fixture.provider.ID); err == nil {
				t.Fatal("数据库允许越过停用编辑边界")
			}
		}
	})
	t.Run("跨来源更新凭据与审计原子回滚", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		if err := fixture.service.SetEnabled(ctx, fixture.actor, ids.New(), "provider", fixture.provider.ID, 1, false); err != nil {
			t.Fatal(err)
		}
		token, err := secure.Token()
		if err != nil {
			t.Fatal(err)
		}
		update := func() (Provider, error) {
			return fixture.service.UpdateProvider(ctx, fixture.actor, ids.New(), fixture.provider.ID, 2, "另一个供应商", "responses", "https://other.example.test", []byte(token))
		}
		execute(t, fixture.pool, `CREATE FUNCTION public.reject_edit_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION '测试审计失败'; END $$; CREATE TRIGGER reject_edit_audit BEFORE INSERT ON tombot.admin_audit_events FOR EACH ROW EXECUTE FUNCTION public.reject_edit_audit()`)
		if value, err := update(); err == nil || value.ID != "" {
			t.Fatal("审计失败返回成功")
		}
		var unchanged bool
		if err = fixture.pool.QueryRow(`SELECT p.revision=2 AND p.protocol='openai_compat' AND c.revoked_at IS NULL AND (SELECT count(*) FROM tombot.model_credentials WHERE provider_id=p.id)=1 FROM tombot.model_providers p JOIN tombot.model_credentials c ON c.provider_id=p.id WHERE p.id=$1`, fixture.provider.ID).Scan(&unchanged); err != nil || !unchanged {
			t.Fatal("凭据及配置未整体回滚", err)
		}
		execute(t, fixture.pool, `DROP TRIGGER reject_edit_audit ON tombot.admin_audit_events; DROP FUNCTION public.reject_edit_audit()`)
		if _, err = update(); err != nil {
			t.Fatal(err)
		}
		credentials, err := fixture.service.ListCredentials(ctx, fixture.actor, fixture.provider.ID, 0, 10)
		if err != nil || len(credentials) != 2 || credentials[0].IsRevoked || !credentials[1].IsRevoked {
			t.Fatal("跨来源凭据状态错误", err)
		}
		if err = fixture.service.SetEnabled(ctx, fixture.actor, ids.New(), "provider", fixture.provider.ID, 3, true); err != nil {
			t.Fatal(err)
		}
		fixture.verifier.discover = func(_ context.Context, _, _ string, plaintext []byte) error {
			if !bytes.Equal(plaintext, []byte(token)) {
				t.Error("新主机收到旧凭据")
			}
			return nil
		}
		if _, err = fixture.service.ListRemoteModels(ctx, fixture.actor, fixture.provider.ID); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("并发编辑仅成功一次", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		if err := fixture.service.SetEnabled(ctx, fixture.actor, ids.New(), "provider", fixture.provider.ID, 1, false); err != nil {
			t.Fatal(err)
		}
		var wait sync.WaitGroup
		outcomes := make(chan error, 2)
		for range 2 {
			wait.Go(func() {
				_, err := fixture.service.UpdateProvider(ctx, fixture.actor, ids.New(), fixture.provider.ID, 2, "新名称", "responses", "https://model.example.test", nil)
				outcomes <- err
			})
		}
		wait.Wait()
		close(outcomes)
		successes := 0
		for err := range outcomes {
			if err == nil {
				successes++
			} else {
				requireError(t, err, ErrConflict)
			}
		}
		if successes != 1 {
			t.Fatal("并发编辑覆盖其他修订")
		}
		if _, err := fixture.service.UpdateProvider(ctx, fixture.actor, ids.New(), ids.New(), 1, "不存在", "responses", "https://model.example.test", nil); !errors.Is(err, ErrNotFound) {
			t.Fatal("未知目录未拒绝", err)
		}
	})
}
