package modelregistry

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/computerdesktop"
	"io.yueyang/tom/internal/identity"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

type testAuthorizer struct {
	actorID string
	denied  atomic.Bool
}

func (authorizer *testAuthorizer) RequireAdmin(_ context.Context, actor identity.Principal, _ bool) error {
	if authorizer.denied.Load() || actor.UserID != authorizer.actorID || actor.Role != "admin" || actor.Scope != "admin" {
		return identity.ErrForbidden
	}
	return nil
}

type testVerifier struct {
	calls     atomic.Int64
	verify    func(context.Context, Configuration, []byte) error
	discovery []DiscoveredModel
	discover  func(context.Context, string, string, []byte) error
}

// Discover 与 Verify 共用同一测试替身：发现调用只读，不改变目录。
func (verifier *testVerifier) Discover(ctx context.Context, protocol, baseURL string, credential []byte) ([]DiscoveredModel, error) {
	if verifier.discover != nil {
		if err := verifier.discover(ctx, protocol, baseURL, credential); err != nil {
			return nil, err
		}
	}
	if verifier.discovery == nil {
		return []DiscoveredModel{}, nil
	}
	return verifier.discovery, nil
}

func (verifier *testVerifier) Verify(ctx context.Context, configuration Configuration, credential []byte) error {
	verifier.calls.Add(1)
	if verifier.verify != nil {
		return verifier.verify(ctx, configuration, credential)
	}
	return nil
}

type fixture struct {
	pool       *sql.DB
	service    *Service
	authorizer *testAuthorizer
	verifier   *testVerifier
	actor      identity.Principal
	provider   Provider
	model      Model
	credential Credential
	plaintext  []byte
}

func execute(t *testing.T, pool *sql.DB, query string, arguments ...any) {
	t.Helper()
	if _, err := pool.Exec(query, arguments...); err != nil {
		t.Fatal("准备模型测试记录失败", err)
	}
}
func newFixture(t *testing.T, cluster *pgtest.Cluster) fixture {
	t.Helper()
	pool, name := cluster.Database(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	open := func(role postgres.Role) *postgres.Database {
		login := cluster.Login(t, string(role))
		database, err := postgres.Open(ctx, postgres.Options{DSN: cluster.DSN(name, login), Role: role, MaxConnections: 8, ConnectTimeout: 5 * time.Second, StatementTimeout: 5 * time.Second, LockTimeout: 2 * time.Second, TransactionTimeout: 10 * time.Second, ConnectionLifetime: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := database.Close(); err != nil {
				t.Error(err)
			}
		})
		return database
	}
	key, err := secure.Random(32)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	keys, err := NewKeyring("test-primary", map[string][]byte{"test-primary": key})
	if err != nil {
		t.Fatal(err)
	}
	actor := identity.Principal{UserID: ids.New(), Role: "admin", Scope: "admin"}
	execute(t, pool, `INSERT INTO tombot.users(id,username,display_name,status) VALUES($1,$2,'目录管理员','provisioning')`, actor.UserID, strings.ToLower(actor.UserID))
	authorizer := &testAuthorizer{actorID: actor.UserID}
	verifier := &testVerifier{}
	service, err := New(open(postgres.ControlRole), open(postgres.TenantRole), authorizer, keys, verifier, verifier, Options{VerificationTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := service.CreateProvider(ctx, actor, ids.New(), "测试供应商", "openai_compat", "https://model.example.test/v1", []byte("test-credential"))
	if err != nil {
		t.Fatal(err)
	}
	model, err := service.CreateModel(ctx, actor, ids.New(), ModelRequest{ProviderID: provider.ID, ModelKey: "example-model", DisplayName: "测试模型", ContextLimit: 8192, OutputLimit: 1024, SupportsTools: true, SupportsVision: true})
	if err != nil {
		t.Fatal(err)
	}
	// 登记供应商已在同一事务内写入首个凭据版本，测试直接复用它。
	plaintext := []byte("test-credential")
	t.Cleanup(func() { clear(plaintext) })
	var credential Credential
	credential.ProviderID = provider.ID
	if err := pool.QueryRowContext(ctx, `SELECT id,version,revoked_at IS NOT NULL,destroyed_at IS NOT NULL,created_at FROM tombot.model_credentials WHERE provider_id=$1`, provider.ID).
		Scan(&credential.ID, &credential.Version, &credential.IsRevoked, &credential.IsDestroyed, &credential.CreatedAt); err != nil {
		t.Fatal(err)
	}
	return fixture{pool, service, authorizer, verifier, actor, provider, model, credential, plaintext}
}
func (fixture fixture) user(t *testing.T) (string, string) {
	t.Helper()
	userID, nodeID, imageID, computerID, botID := ids.New(), ids.New(), ids.New(), ids.New(), ids.New()
	execute(t, fixture.pool, `INSERT INTO tombot.users(id,username,display_name,status) VALUES($1,$2,'模型用户','provisioning')`, userID, strings.ToLower(userID))
	execute(t, fixture.pool, `INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2)`, imageID, "example.test/computer@sha256:"+secure.Hash(imageID))
	execute(t, fixture.pool, `INSERT INTO tombot.computer_nodes(id,name,status,capacity,runtime_id,control_sequence,last_seen_at,control_expires_at,verified_image_id,capability_expires_at) VALUES($1,$1,'ready',1,$1,1,clock_timestamp(),clock_timestamp()+interval '5 minutes',$2,clock_timestamp()+interval '15 minutes')`, nodeID, imageID)
	execute(t, fixture.pool, `INSERT INTO tombot.computers(user_id,id,node_id,image_id,status,container_id,volume_ref,cpu_millis,memory_bytes,disk_bytes,process_limit) VALUES($1,$2,$3,$4,'ready',$2,$2,1000,1073741824,1073741824,128)`, userID, computerID, nodeID, imageID)
	execute(t, fixture.pool, `UPDATE tombot.users SET status='active' WHERE id=$1`, userID)
	desktopID := ids.New()
	err := fixture.service.tenant.Tenant(context.Background(), userID, func(transaction *sql.Tx) error {
		if _, err := transaction.Exec(`INSERT INTO tombot.bots(user_id,id,desktop_id,name) VALUES($1,$2,$3,'机器人')`, userID, botID, desktopID); err != nil {
			return err
		}
		_, err := computerdesktop.Register(context.Background(), transaction, userID, botID, desktopID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return userID, botID
}
func (fixture fixture) revision(t *testing.T, policyID string) int64 {
	t.Helper()
	var revision int64
	if err := fixture.pool.QueryRow(`SELECT revision FROM tombot.model_policies WHERE id=$1`, policyID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return revision
}
func (fixture fixture) draft(t *testing.T, policyID string, credentialID *string, output int) Version {
	t.Helper()
	version, err := fixture.service.CreateDraft(context.Background(), fixture.actor, ids.New(), DraftRequest{PolicyID: policyID, ModelID: fixture.model.ID, CredentialID: credentialID, OutputLimit: output})
	if err != nil {
		t.Fatal(err)
	}
	return version
}
func (fixture fixture) publish(t *testing.T, version Version) Version {
	t.Helper()
	published, err := fixture.service.Publish(context.Background(), fixture.actor, ids.New(), version.PolicyID, version.Number, fixture.revision(t, version.PolicyID))
	if err != nil {
		t.Fatal(err)
	}
	return published
}
func (fixture fixture) policy(t *testing.T) Version {
	t.Helper()
	policy, err := fixture.service.CreatePolicy(context.Background(), fixture.actor, ids.New(), ids.New())
	if err != nil {
		t.Fatal(err)
	}
	return fixture.publish(t, fixture.draft(t, policy.ID, &fixture.credential.ID, 512))
}
func requireError(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("预期 %v，实际 %v", want, err)
	}
}

func TestRegistryIntegration(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("目录分页和安全凭据元数据", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		providers, err := fixture.service.ListProviders(ctx, fixture.actor, "", 1)
		if err != nil || len(providers) != 1 {
			t.Fatal("供应商读取失败", err)
		}
		providers, err = fixture.service.ListProviders(ctx, fixture.actor, providers[0].ID, 1)
		if err != nil || len(providers) != 0 {
			t.Fatal("供应商游标重复", err)
		}
		models, err := fixture.service.ListModels(ctx, fixture.actor, fixture.provider.ID, "", 10)
		if err != nil || len(models) != 1 {
			t.Fatal("模型读取失败", err)
		}
		credentials, err := fixture.service.ListCredentials(ctx, fixture.actor, fixture.provider.ID, 0, 10)
		if err != nil || len(credentials) != 1 {
			t.Fatal("凭据元数据读取失败", err)
		}
		encoded, err := json.Marshal(credentials)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(encoded, fixture.plaintext) || bytes.Contains(encoded, []byte("ciphertext")) {
			t.Fatal("元数据包含凭据材料")
		}
		var ciphertext, nonce []byte
		if err = fixture.pool.QueryRow(`SELECT ciphertext,nonce FROM tombot.model_credentials WHERE id=$1`, fixture.credential.ID).Scan(&ciphertext, &nonce); err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(ciphertext, fixture.plaintext) || len(nonce) != 12 {
			t.Fatal("未以认证密文保存凭据")
		}
		fixture.authorizer.denied.Store(true)
		_, err = fixture.service.ListProviders(ctx, fixture.actor, "", 1)
		requireError(t, err, identity.ErrForbidden)
		_, err = fixture.service.CreatePolicy(ctx, fixture.actor, ids.New(), "拒绝策略")
		requireError(t, err, identity.ErrForbidden)
	})
	t.Run("策略优先级和不可用不回退", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		user, bot := fixture.user(t)
		otherUser, otherBot := fixture.user(t)
		_, err := fixture.service.Resolve(ctx, user, bot)
		requireError(t, err, ErrUnavailable)
		platform, userPolicy, botPolicy := fixture.policy(t), fixture.policy(t), fixture.policy(t)
		if err = fixture.service.SetDefault(ctx, fixture.actor, ids.New(), platform.PolicyID, fixture.revision(t, platform.PolicyID)); err != nil {
			t.Fatal(err)
		}
		check := func(expected string) {
			t.Helper()
			selected, err := fixture.service.Resolve(ctx, user, bot)
			if err != nil || selected.PolicyID != expected {
				t.Fatalf("策略优先级错误：%v %v", selected, err)
			}
		}
		check(platform.PolicyID)
		if err = fixture.service.Assign(ctx, fixture.actor, ids.New(), user, "", userPolicy.PolicyID, ""); err != nil {
			t.Fatal(err)
		}
		check(userPolicy.PolicyID)
		if err = fixture.service.Assign(ctx, fixture.actor, ids.New(), user, bot, botPolicy.PolicyID, ""); err != nil {
			t.Fatal(err)
		}
		check(botPolicy.PolicyID)
		err = fixture.service.Assign(ctx, fixture.actor, ids.New(), user, bot, platform.PolicyID, "")
		requireError(t, err, ErrConflict)
		_, err = fixture.service.Resolve(ctx, user, otherBot)
		requireError(t, err, ErrNotFound)
		err = fixture.service.Assign(ctx, fixture.actor, ids.New(), otherUser, bot, platform.PolicyID, "")
		requireError(t, err, ErrNotFound)
		if err = fixture.service.SetEnabled(ctx, fixture.actor, ids.New(), "policy", botPolicy.PolicyID, fixture.revision(t, botPolicy.PolicyID), false); err != nil {
			t.Fatal(err)
		}
		_, err = fixture.service.Resolve(ctx, user, bot)
		requireError(t, err, ErrUnavailable)
		if err = fixture.service.Assign(ctx, fixture.actor, ids.New(), user, bot, "", botPolicy.PolicyID); err != nil {
			t.Fatal(err)
		}
		check(userPolicy.PolicyID)
		if err = fixture.service.Assign(ctx, fixture.actor, ids.New(), user, "", "", userPolicy.PolicyID); err != nil {
			t.Fatal(err)
		}
		check(platform.PolicyID)
		if err = fixture.service.RevokeVersion(ctx, fixture.actor, ids.New(), platform.PolicyID, platform.Number); err != nil {
			t.Fatal(err)
		}
		_, err = fixture.service.Resolve(ctx, user, bot)
		requireError(t, err, ErrUnavailable)
	})
	t.Run("版本固定凭据轮换及撤销", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		user, bot := fixture.user(t)
		first := fixture.policy(t)
		if err := fixture.service.SetDefault(ctx, fixture.actor, ids.New(), first.PolicyID, fixture.revision(t, first.PolicyID)); err != nil {
			t.Fatal(err)
		}
		oldSelection, err := fixture.service.Resolve(ctx, user, bot)
		if err != nil {
			t.Fatal(err)
		}
		secret, err := secure.Token()
		if err != nil {
			t.Fatal(err)
		}
		nextPlaintext := []byte(secret)
		defer clear(nextPlaintext)
		nextCredential, err := fixture.service.AddCredential(ctx, fixture.actor, ids.New(), fixture.provider.ID, nextPlaintext)
		if err != nil {
			t.Fatal(err)
		}
		second := fixture.publish(t, fixture.draft(t, first.PolicyID, &nextCredential.ID, 256))
		selection, err := fixture.service.Resolve(ctx, user, bot)
		if err != nil || selection.Version != second.Number {
			t.Fatal("新运行没有绑定新版本", err)
		}
		var borrowed []byte
		if err = fixture.service.Use(ctx, oldSelection, func(_ context.Context, configuration Configuration, plaintext []byte) error {
			if configuration.OutputLimit != 512 || !bytes.Equal(plaintext, fixture.plaintext) {
				return errors.New("旧运行配置被改写")
			}
			borrowed = plaintext
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(borrowed, make([]byte, len(borrowed))) {
			t.Fatal("回调后凭据未清零")
		}
		calls := fixture.verifier.calls.Load()
		if _, err = fixture.service.Publish(ctx, fixture.actor, ids.New(), first.PolicyID, first.Number, 1); err != nil || fixture.verifier.calls.Load() != calls {
			t.Fatal("重复发布重放了探测", err)
		}
		if err = fixture.service.RevokeCredential(ctx, fixture.actor, ids.New(), fixture.credential.ID); err != nil {
			t.Fatal(err)
		}
		err = fixture.service.Use(ctx, oldSelection, func(context.Context, Configuration, []byte) error { t.Fatal("已撤销凭据进入调用"); return nil })
		requireError(t, err, ErrUnavailable)
		if err = fixture.service.Use(ctx, selection, func(_ context.Context, _ Configuration, plaintext []byte) error {
			if !bytes.Equal(plaintext, nextPlaintext) {
				return errors.New("新凭据未生效")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err = fixture.service.SetEnabled(ctx, fixture.actor, ids.New(), "provider", fixture.provider.ID, 1, false); err != nil {
			t.Fatal(err)
		}
		err = fixture.service.Use(ctx, selection, func(context.Context, Configuration, []byte) error {
			t.Fatal("已停用供应商进入调用")
			return nil
		})
		requireError(t, err, ErrUnavailable)
		err = fixture.service.SetEnabled(ctx, fixture.actor, ids.New(), "provider", fixture.provider.ID, 1, true)
		requireError(t, err, ErrConflict)
	})
	t.Run("探测不持事务且竞争后拒绝发布", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		policy, err := fixture.service.CreatePolicy(ctx, fixture.actor, ids.New(), "竞争策略")
		if err != nil {
			t.Fatal(err)
		}
		version := fixture.draft(t, policy.ID, &fixture.credential.ID, 512)
		revision := fixture.revision(t, policy.ID)
		started, release := make(chan struct{}), make(chan struct{})
		fixture.verifier.verify = func(ctx context.Context, _ Configuration, _ []byte) error {
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		finished := make(chan error, 1)
		go func() {
			_, err := fixture.service.Publish(ctx, fixture.actor, ids.New(), policy.ID, version.Number, revision)
			finished <- err
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("探测未开始")
		}
		fixture.draft(t, policy.ID, &fixture.credential.ID, 128)
		close(release)
		requireError(t, <-finished, ErrConflict)
		stored, err := fixture.service.GetVersion(ctx, fixture.actor, policy.ID, version.Number)
		if err != nil || stored.Status != "draft" {
			t.Fatal("竞争失败仍发布了版本", err)
		}
	})
	t.Run("探测失败脱敏与超时", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		policy, err := fixture.service.CreatePolicy(ctx, fixture.actor, ids.New(), "失败策略")
		if err != nil {
			t.Fatal(err)
		}
		version := fixture.draft(t, policy.ID, &fixture.credential.ID, 512)
		fixture.verifier.verify = func(_ context.Context, _ Configuration, plaintext []byte) error { return errors.New(string(plaintext)) }
		_, err = fixture.service.Publish(ctx, fixture.actor, ids.New(), policy.ID, version.Number, fixture.revision(t, policy.ID))
		requireError(t, err, ErrVerification)
		if strings.Contains(err.Error(), string(fixture.plaintext)) {
			t.Fatal("探测错误泄露凭据")
		}
		var count int
		if err = fixture.pool.QueryRow(`SELECT count(*) FROM tombot.admin_audit_events WHERE target_id=$1 AND result='failed'`, policy.ID).Scan(&count); err != nil || count != 1 {
			t.Fatal("失败审计未保存", err)
		}
		fixture.verifier.verify = func(ctx context.Context, _ Configuration, _ []byte) error { <-ctx.Done(); return ctx.Err() }
		_, err = fixture.service.Publish(ctx, fixture.actor, ids.New(), policy.ID, version.Number, fixture.revision(t, policy.ID))
		requireError(t, err, ErrVerification)
	})
	t.Run("探测期间撤销凭据不能发布", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		policy, err := fixture.service.CreatePolicy(ctx, fixture.actor, ids.New(), "撤销策略")
		if err != nil {
			t.Fatal(err)
		}
		version := fixture.draft(t, policy.ID, &fixture.credential.ID, 512)
		fixture.verifier.verify = func(ctx context.Context, _ Configuration, _ []byte) error {
			return fixture.service.RevokeCredential(ctx, fixture.actor, ids.New(), fixture.credential.ID)
		}
		_, err = fixture.service.Publish(ctx, fixture.actor, ids.New(), policy.ID, version.Number, fixture.revision(t, policy.ID))
		requireError(t, err, ErrUnavailable)
	})
	t.Run("并发版本号不重复", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		policy, err := fixture.service.CreatePolicy(ctx, fixture.actor, ids.New(), "并发策略")
		if err != nil {
			t.Fatal(err)
		}
		type result struct {
			number int64
			err    error
		}
		results := make(chan result, 8)
		for range 8 {
			go func() {
				version, err := fixture.service.CreateDraft(ctx, fixture.actor, ids.New(), DraftRequest{PolicyID: policy.ID, ModelID: fixture.model.ID, CredentialID: &fixture.credential.ID, OutputLimit: 512})
				results <- result{version.Number, err}
			}()
		}
		seen := map[int64]bool{}
		for range 8 {
			value := <-results
			if value.err != nil || seen[value.number] {
				t.Fatal("并发草稿版本分配失败", value.err)
			}
			seen[value.number] = true
		}
		for range 8 {
			go func() {
				credential, err := fixture.service.AddCredential(ctx, fixture.actor, ids.New(), fixture.provider.ID, fixture.plaintext)
				results <- result{credential.Version, err}
			}()
		}
		seen = map[int64]bool{}
		for range 8 {
			value := <-results
			if value.err != nil || seen[value.number] {
				t.Fatal("并发凭据版本分配失败", value.err)
			}
			seen[value.number] = true
		}
	})
	t.Run("撤销后退役清除密文并保持墓碑", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		version := fixture.policy(t)
		requireError(t, fixture.service.DestroyCredential(ctx, fixture.actor, ids.New(), fixture.credential.ID), ErrConflict)
		if err := fixture.service.RevokeCredential(ctx, fixture.actor, ids.New(), fixture.credential.ID); err != nil {
			t.Fatal(err)
		}
		requireError(t, fixture.service.DestroyCredential(ctx, fixture.actor, ids.New(), fixture.credential.ID), ErrConflict)
		if _, err := fixture.pool.Exec(`UPDATE tombot.model_credentials SET destroyed_at=transaction_timestamp(),nonce=NULL,ciphertext=NULL WHERE id=$1`, fixture.credential.ID); err == nil {
			t.Fatal("数据库允许销毁仍被策略引用的凭据")
		}
		if err := fixture.service.RevokeVersion(ctx, fixture.actor, ids.New(), version.PolicyID, version.Number); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := fixture.service.DestroyCredential(ctx, fixture.actor, ids.New(), fixture.credential.ID); err != nil {
				t.Fatal(err)
			}
		}
		var hasMaterial bool
		if err := fixture.pool.QueryRow(`SELECT nonce IS NOT NULL OR ciphertext IS NOT NULL FROM tombot.model_credentials WHERE id=$1`, fixture.credential.ID).Scan(&hasMaterial); err != nil || hasMaterial {
			t.Fatal("凭据退役后仍保留密文", err)
		}
		credentials, err := fixture.service.ListCredentials(ctx, fixture.actor, fixture.provider.ID, 0, 10)
		if err != nil || len(credentials) != 1 || !credentials[0].IsDestroyed || !credentials[0].IsRevoked {
			t.Fatal("退役元数据错误", err)
		}
		for _, query := range []string{`UPDATE tombot.model_credentials SET revoked_at=NULL WHERE id=$1`, `DELETE FROM tombot.model_credentials WHERE id=$1`} {
			if _, err := fixture.pool.Exec(query, fixture.credential.ID); err == nil {
				t.Fatal("数据库允许恢复或删除凭据墓碑")
			}
		}
	})
	t.Run("主密钥轮换及缺失历史密钥", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		user, bot := fixture.user(t)
		version := fixture.policy(t)
		selection := Selection{UserID: user, BotID: bot, PolicyID: version.PolicyID, Version: version.Number}
		key, err := secure.Random(32)
		if err != nil {
			t.Fatal(err)
		}
		defer clear(key)
		keys, err := NewKeyring("next", map[string][]byte{"next": key, "test-primary": fixture.service.keys.keys["test-primary"]})
		if err != nil {
			t.Fatal(err)
		}
		rotated, err := New(fixture.service.control, fixture.service.tenant, fixture.authorizer, keys, fixture.verifier, fixture.verifier, fixture.service.options)
		if err != nil {
			t.Fatal(err)
		}
		if err = rotated.Use(ctx, selection, func(_ context.Context, _ Configuration, plaintext []byte) error {
			if !bytes.Equal(plaintext, fixture.plaintext) {
				return errors.New("轮换后旧凭据无法解密")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		credential, err := rotated.AddCredential(ctx, fixture.actor, ids.New(), fixture.provider.ID, fixture.plaintext)
		if err != nil {
			t.Fatal(err)
		}
		var reference string
		if err = fixture.pool.QueryRow(`SELECT key_reference FROM tombot.model_credentials WHERE id=$1`, credential.ID).Scan(&reference); err != nil || reference != "next" {
			t.Fatal("新凭据未使用当前主密钥", err)
		}
		keys, err = NewKeyring("next", map[string][]byte{"next": key})
		if err != nil {
			t.Fatal(err)
		}
		missing, err := New(fixture.service.control, fixture.service.tenant, fixture.authorizer, keys, fixture.verifier, fixture.verifier, fixture.service.options)
		if err != nil {
			t.Fatal(err)
		}
		err = missing.Use(ctx, selection, func(context.Context, Configuration, []byte) error {
			t.Fatal("缺失历史主密钥仍调用模型")
			return nil
		})
		requireError(t, err, ErrCredential)
	})
	t.Run("版本分页无未知价格补零与停用用户", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		first := fixture.policy(t)
		second := fixture.publish(t, fixture.draft(t, first.PolicyID, &fixture.credential.ID, 256))
		versions, err := fixture.service.ListVersions(ctx, fixture.actor, first.PolicyID, 0, 1)
		if err != nil || len(versions) != 1 || versions[0].Number != second.Number || versions[0].Configuration.Prices.Input != nil {
			t.Fatal("版本分页或未知价格错误", err)
		}
		versions, err = fixture.service.ListVersions(ctx, fixture.actor, first.PolicyID, second.Number, 1)
		if err != nil || len(versions) != 1 || versions[0].Number != first.Number {
			t.Fatal("版本游标错误", err)
		}
		user, bot := fixture.user(t)
		if err = fixture.service.SetDefault(ctx, fixture.actor, ids.New(), first.PolicyID, fixture.revision(t, first.PolicyID)); err != nil {
			t.Fatal(err)
		}
		if _, err = fixture.service.Publish(ctx, fixture.actor, ids.New(), first.PolicyID, first.Number, 1); err != nil {
			t.Fatal(err)
		}
		selected, err := fixture.service.Resolve(ctx, user, bot)
		if err != nil || selected.Version != second.Number {
			t.Fatal("重复发布旧版本改变了新运行路由", err)
		}
		execute(t, fixture.pool, `UPDATE tombot.users SET status='disabled' WHERE id=$1`, user)
		_, err = fixture.service.Resolve(ctx, user, bot)
		requireError(t, err, ErrNotFound)
		err = fixture.service.Use(ctx, selected, func(context.Context, Configuration, []byte) error { t.Fatal("停用用户仍调用模型"); return nil })
		requireError(t, err, ErrNotFound)
	})
	t.Run("数据库拒绝改写历史与越权读取", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		version := fixture.policy(t)
		for _, statement := range []struct {
			query     string
			arguments []any
		}{
			{`UPDATE tombot.model_providers SET base_url='https://changed.example.test',revision=revision+1 WHERE id=$1`, []any{fixture.provider.ID}},
			{`UPDATE tombot.models SET model_key='changed',revision=revision+1 WHERE id=$1`, []any{fixture.model.ID}},
			{`UPDATE tombot.model_policy_versions SET configuration='{}' WHERE policy_id=$1`, []any{version.PolicyID}},
			{`UPDATE tombot.model_credentials SET ciphertext=nonce WHERE id=$1`, []any{fixture.credential.ID}},
			{`DELETE FROM tombot.model_policy_versions WHERE policy_id=$1`, []any{version.PolicyID}},
			{`UPDATE tombot.model_policies SET current_version=NULL,revision=revision+1 WHERE id=$1`, []any{version.PolicyID}},
		} {
			err := fixture.service.control.Platform(ctx, func(transaction *sql.Tx) error {
				_, err := transaction.ExecContext(ctx, statement.query, statement.arguments...)
				return err
			})
			if err == nil {
				t.Fatal("数据库允许改写历史", statement.query)
			}
		}
		user, _ := fixture.user(t)
		for _, table := range []string{"model_credentials", "model_providers", "model_policy_versions"} {
			err := fixture.service.tenant.Tenant(ctx, user, func(transaction *sql.Tx) error {
				_, err := transaction.ExecContext(ctx, `SELECT * FROM tombot.`+table)
				return err
			})
			if err == nil {
				t.Fatal("租户角色可以读取全局模型配置")
			}
		}
	})
	t.Run("密文替换拒绝且不返回部分成功", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		replacement := ids.New()
		execute(t, fixture.pool, `INSERT INTO tombot.model_credentials(id,provider_id,version,key_reference,nonce,ciphertext) SELECT $1,provider_id,2,key_reference,nonce,ciphertext FROM tombot.model_credentials WHERE id=$2`, replacement, fixture.credential.ID)
		policy, err := fixture.service.CreatePolicy(ctx, fixture.actor, ids.New(), "密文绑定策略")
		if err != nil {
			t.Fatal(err)
		}
		version := fixture.draft(t, policy.ID, &replacement, 512)
		_, err = fixture.service.Publish(ctx, fixture.actor, ids.New(), policy.ID, version.Number, fixture.revision(t, policy.ID))
		requireError(t, err, ErrCredential)
		if fixture.verifier.calls.Load() != 0 {
			t.Fatal("未认证的凭据进入探测")
		}
		execute(t, fixture.pool, `CREATE FUNCTION public.reject_model_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION '测试审计失败'; END $$;
			CREATE TRIGGER reject_model_audit BEFORE INSERT ON tombot.admin_audit_events FOR EACH ROW EXECUTE FUNCTION public.reject_model_audit()`)
		provider, err := fixture.service.CreateProvider(ctx, fixture.actor, ids.New(), "应回滚供应商", "anthropic", "https://rollback.example.test", []byte("test-credential"))
		if err == nil || provider.ID != "" {
			t.Fatal("审计失败仍返回成功")
		}
		var count int
		if err = fixture.pool.QueryRow(`SELECT count(*) FROM tombot.model_providers WHERE name='应回滚供应商'`).Scan(&count); err != nil || count != 0 {
			t.Fatal("审计失败未回滚目录", err)
		}
	})
}

func TestKeyringAndValidation(t *testing.T) {
	key, err := secure.Random(32)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	keys, err := NewKeyring("current", map[string][]byte{"current": key})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%+v %#v", keys, keys) != "模型主密钥集合（已隐藏） 模型主密钥集合（已隐藏）" {
		t.Fatal("调试输出未隐藏主密钥")
	}
	original := append([]byte(nil), keys.keys["current"]...)
	defer clear(original)
	clear(key)
	if !bytes.Equal(keys.keys["current"], original) {
		t.Fatal("主密钥引用了外部可变切片")
	}
	if _, err = NewKeyring("missing", map[string][]byte{"current": original}); err == nil {
		t.Fatal("缺失主密钥未拒绝")
	}
	for _, value := range []string{"http://model.example.test", "https://user@example.test", "https://example.test/?x=1", "https://example.test/#fragment", "https://example.test/?", "https://example.test/\n", "https://example.test\\path", "file:///tmp/model"} {
		if validURL(value) {
			t.Fatalf("不安全地址被接受：%q", value)
		}
	}
	if !validURL("https://model.example.test/v1") {
		t.Fatal("合法模型地址被拒绝")
	}
}
