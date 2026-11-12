package identity

import (
	"context"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

type identityFixture struct {
	pool    *sql.DB
	service *Service
}
type testAccount struct{ id, username, password, activation string }

func randomValue(t *testing.T) string {
	t.Helper()
	value, err := secure.Token()
	if err != nil {
		t.Fatal("生成测试随机值失败")
	}
	return value
}

func attempt() Attempt {
	return Attempt{Source: netip.MustParseAddr("127.0.0.1"), RequestID: ids.New()}
}

func execute(t *testing.T, pool *sql.DB, query string, arguments ...any) {
	t.Helper()
	if _, err := pool.Exec(query, arguments...); err != nil {
		t.Fatal("准备或检查测试记录失败", err)
	}
}

func (fixture identityFixture) pending(t *testing.T, role string) testAccount {
	t.Helper()
	account := testAccount{id: ids.New(), password: randomValue(t), activation: randomValue(t)}
	account.username = strings.ToLower(account.id)
	node, image, computer := ids.New(), ids.New(), ids.New()
	execute(t, fixture.pool, `INSERT INTO tombot.users(id,username,display_name,role,status) VALUES($1,$2,'测试用户',$3,'provisioning')`, account.id, account.username, role)
	execute(t, fixture.pool, `INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2)`, image, "registry.example.test/computer@sha256:"+secure.Hash(randomValue(t)))
	execute(t, fixture.pool, `INSERT INTO tombot.computer_nodes(id,name,status,capacity,runtime_id,control_sequence,last_seen_at,control_expires_at,verified_image_id,capability_expires_at) VALUES($1,$1,'ready',10,$1,1,clock_timestamp(),clock_timestamp()+interval '5 minutes',$2,clock_timestamp()+interval '15 minutes')`, node, image)
	execute(t, fixture.pool, `INSERT INTO tombot.computers(user_id,id,node_id,image_id,status,container_id,volume_ref,cpu_millis,memory_bytes,disk_bytes,process_limit)
		VALUES($1,$2,$3,$4,'ready',$2,$2,1000,1073741824,1073741824,128)`, account.id, computer, node, image)
	execute(t, fixture.pool, `UPDATE tombot.users SET status='pending_activation' WHERE id=$1`, account.id)
	execute(t, fixture.pool, `INSERT INTO tombot.activation_tokens(id,user_id,token_digest,expires_at) VALUES($1,$2,$3,clock_timestamp()+interval '1 hour')`, ids.New(), account.id, digest(account.activation))
	return account
}

func (fixture identityFixture) active(t *testing.T) testAccount {
	t.Helper()
	account := fixture.pending(t, "user")
	if err := fixture.service.Activate(context.Background(), account.activation, account.password, "", attempt()); err != nil {
		t.Fatal(err)
	}
	return account
}

func loginRequest(account testAccount) LoginRequest {
	return LoginRequest{Username: account.username, Password: account.password, DeviceName: "测试客户端", Platform: "linux", Scope: "desktop"}
}

func (fixture identityFixture) login(t *testing.T, account testAccount) Session {
	t.Helper()
	session, err := fixture.service.Login(context.Background(), loginRequest(account), attempt())
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func requireError(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("错误类型不符：%v，预期 %v", err, want)
	}
}

func TestIdentityIntegration(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, name := cluster.Database(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	login := cluster.Login(t, string(postgres.IdentityRole))
	database, err := postgres.Open(ctx, postgres.Options{DSN: cluster.DSN(name, login), Role: postgres.IdentityRole, MaxConnections: 4,
		ConnectTimeout: 5 * time.Second, StatementTimeout: 5 * time.Second, LockTimeout: 3 * time.Second, TransactionTimeout: 10 * time.Second, ConnectionLifetime: time.Minute})
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
	service, err := New(database, Options{AccessLifetime: 5 * time.Minute, SessionLifetime: 24 * time.Hour, VerificationLifetime: 5 * time.Minute,
		RateWindow: time.Minute, AccountAttempts: 100, SourceAttempts: 1000, PasswordWorkers: 2, DeviceLimit: 20, EncryptionKey: key})
	if err != nil {
		t.Fatal(err)
	}
	fixture := identityFixture{pool, service}

	t.Run("激活原子性与电脑就绪", func(t *testing.T) {
		account := fixture.pending(t, "user")
		if _, err := service.Login(ctx, loginRequest(account), attempt()); err == nil {
			t.Fatal("未激活账户允许登录")
		}
		execute(t, pool, `UPDATE tombot.computers SET status='error' WHERE user_id=$1`, account.id)
		if err := service.Activate(ctx, account.activation, account.password, "", attempt()); err == nil {
			t.Fatal("电脑失败时仍激活成功")
		}
		var consumed bool
		var credentials int
		if err := pool.QueryRow(`SELECT consumed_at IS NOT NULL FROM tombot.activation_tokens WHERE user_id=$1`, account.id).Scan(&consumed); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(`SELECT count(*) FROM tombot.user_credentials WHERE user_id=$1`, account.id).Scan(&credentials); err != nil {
			t.Fatal(err)
		}
		if consumed || credentials != 0 {
			t.Fatal("失败激活留下凭据或消费记录")
		}
		execute(t, pool, `UPDATE tombot.computers SET status='ready' WHERE user_id=$1`, account.id)
		results := make(chan error, 2)
		for range 2 {
			go func() { results <- service.Activate(ctx, account.activation, account.password, "", attempt()) }()
		}
		success := 0
		for range 2 {
			err := <-results
			if err == nil {
				success++
			} else {
				requireError(t, err, ErrInvalidActivation)
			}
		}
		if success != 1 {
			t.Fatal("并发激活成功次数错误", success)
		}
		if err := service.Activate(ctx, account.activation, account.password, "", attempt()); !errors.Is(err, ErrInvalidActivation) {
			t.Fatal("已消费激活值未被拒绝", err)
		}
		fixture.login(t, account)
	})

	t.Run("过期激活值与权限范围", func(t *testing.T) {
		account := fixture.pending(t, "user")
		execute(t, pool, `UPDATE tombot.activation_tokens SET created_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE user_id=$1`, account.id)
		requireError(t, service.Activate(ctx, account.activation, account.password, "", attempt()), ErrInvalidActivation)
		account = fixture.active(t)
		request := loginRequest(account)
		request.Scope = "admin"
		request.Platform = "web"
		_, err := service.Login(ctx, request, attempt())
		requireError(t, err, ErrInvalidCredentials)
		session := fixture.login(t, account)
		principal, err := service.Authenticate(ctx, string(session.AccessToken), "desktop")
		if err != nil || principal.UserID != account.id {
			t.Fatal("未解析出所属用户", err)
		}
		_, err = service.Authenticate(ctx, string(session.RefreshToken), "desktop")
		requireError(t, err, ErrInvalidSession)
		_, err = service.Authenticate(ctx, string(session.AccessToken), "admin")
		requireError(t, err, ErrInvalidSession)
		requireError(t, service.RequireAdmin(ctx, principal, true), ErrForbidden)
		var stored []byte
		if err = pool.QueryRow(`SELECT token_digest FROM tombot.session_tokens WHERE session_id=$1 AND kind='access'`, principal.SessionID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if len(stored) != 32 || string(stored) == string(session.AccessToken) {
			t.Fatal("凭据未按摘要保存")
		}
		if strings.Contains(fmt.Sprintf("%+v %#v", session, session), string(session.AccessToken)) {
			t.Fatal("会话格式化暴露凭据")
		}
	})

	t.Run("设备所属权与撤销", func(t *testing.T) {
		first, second := fixture.active(t), fixture.active(t)
		sessionA, sessionB := fixture.login(t, first), fixture.login(t, second)
		devices, err := service.ListDevices(ctx, sessionA.Principal, "", 10)
		if err != nil || len(devices) != 1 || devices[0].ID != sessionA.Principal.DeviceID || !devices[0].IsCurrent {
			t.Fatal("设备列表串用户", err)
		}
		requireError(t, service.RevokeDevice(ctx, sessionA.Principal, sessionB.Principal.DeviceID, ids.New()), ErrForbidden)
		if _, err = service.Authenticate(ctx, string(sessionB.AccessToken), "desktop"); err != nil {
			t.Fatal("撤销越权影响其他用户", err)
		}
		if err = service.RevokeDevice(ctx, sessionA.Principal, sessionA.Principal.DeviceID, ids.New()); err != nil {
			t.Fatal(err)
		}
		_, err = service.Authenticate(ctx, string(sessionA.AccessToken), "desktop")
		requireError(t, err, ErrInvalidSession)
		_, err = service.Revalidate(ctx, sessionA.Principal)
		requireError(t, err, ErrInvalidSession)
	})

	t.Run("轮换与重放撤销必须提交", func(t *testing.T) {
		session := fixture.login(t, fixture.active(t))
		rotated, err := service.Refresh(ctx, string(session.RefreshToken), "desktop", attempt())
		if err != nil {
			t.Fatal(err)
		}
		if rotated.Principal.SessionID != session.Principal.SessionID || rotated.RefreshToken == session.RefreshToken {
			t.Fatal("续期未保留会话或未轮换")
		}
		_, err = service.Authenticate(ctx, string(session.AccessToken), "desktop")
		requireError(t, err, ErrInvalidSession)
		if _, err = service.Authenticate(ctx, string(rotated.AccessToken), "desktop"); err != nil {
			t.Fatal(err)
		}
		_, err = service.Refresh(ctx, string(session.RefreshToken), "desktop", attempt())
		requireError(t, err, ErrInvalidSession)
		_, err = service.Authenticate(ctx, string(rotated.AccessToken), "desktop")
		requireError(t, err, ErrInvalidSession)
		_, err = service.Refresh(ctx, string(rotated.RefreshToken), "desktop", attempt())
		requireError(t, err, ErrInvalidSession)
	})

	t.Run("并发续期仅签发一次并识别重放", func(t *testing.T) {
		session := fixture.login(t, fixture.active(t))
		type result struct {
			session Session
			err     error
		}
		results := make(chan result, 2)
		for range 2 {
			go func() {
				value, err := service.Refresh(ctx, string(session.RefreshToken), "desktop", attempt())
				results <- result{value, err}
			}()
		}
		success := 0
		var issued Session
		for range 2 {
			result := <-results
			if result.err == nil {
				success++
				issued = result.session
			} else {
				requireError(t, result.err, ErrInvalidSession)
			}
		}
		if success != 1 {
			t.Fatal("并发续期成功次数错误", success)
		}
		_, err := service.Authenticate(ctx, string(issued.AccessToken), "desktop")
		requireError(t, err, ErrInvalidSession)
	})

	t.Run("到期与禁用即时生效", func(t *testing.T) {
		account := fixture.active(t)
		session := fixture.login(t, account)
		execute(t, pool, `UPDATE tombot.session_tokens SET created_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE session_id=$1 AND kind='access'`, session.Principal.SessionID)
		_, err := service.Authenticate(ctx, string(session.AccessToken), "desktop")
		requireError(t, err, ErrInvalidSession)
		rotated, err := service.Refresh(ctx, string(session.RefreshToken), "desktop", attempt())
		if err != nil {
			t.Fatal(err)
		}
		execute(t, pool, `UPDATE tombot.users SET status='disabled',auth_version=auth_version+1 WHERE id=$1`, account.id)
		_, err = service.Authenticate(ctx, string(rotated.AccessToken), "desktop")
		requireError(t, err, ErrInvalidSession)
		_, err = service.Revalidate(ctx, session.Principal)
		requireError(t, err, ErrInvalidSession)
		_, err = service.Refresh(ctx, string(rotated.RefreshToken), "desktop", attempt())
		requireError(t, err, ErrInvalidSession)
		execute(t, pool, `UPDATE tombot.users SET status='active' WHERE id=$1`, account.id)
		_, err = service.Authenticate(ctx, string(rotated.AccessToken), "desktop")
		requireError(t, err, ErrInvalidSession)
		fixture.login(t, account)
	})

	t.Run("修改口令撤销全部会话", func(t *testing.T) {
		account := fixture.active(t)
		first, second := fixture.login(t, account), fixture.login(t, account)
		next := randomValue(t)
		requireError(t, service.ChangePassword(ctx, first.Principal, randomValue(t), next, "", attempt()), ErrInvalidCredentials)
		if err := service.ChangePassword(ctx, first.Principal, account.password, next, "", attempt()); err != nil {
			t.Fatal(err)
		}
		for _, session := range []Session{first, second} {
			_, err := service.Authenticate(ctx, string(session.AccessToken), "desktop")
			requireError(t, err, ErrInvalidSession)
		}
		_, err := service.Login(ctx, loginRequest(account), attempt())
		requireError(t, err, ErrInvalidCredentials)
		account.password = next
		session := fixture.login(t, account)
		if err := service.Logout(ctx, session.Principal, ids.New()); err != nil {
			t.Fatal(err)
		}
		_, err = service.Authenticate(ctx, string(session.AccessToken), "desktop")
		requireError(t, err, ErrInvalidSession)
	})

	t.Run("管理员登记原子消费验证信息", func(t *testing.T) {
		account := fixture.pending(t, "admin")
		requireError(t, service.Activate(ctx, account.activation, account.password, "", attempt()), ErrInvalidCredentials)
		encoded, err := service.BeginAdminEnrollment(ctx, account.activation, attempt())
		if err != nil {
			t.Fatal(err)
		}
		again, err := service.BeginAdminEnrollment(ctx, account.activation, attempt())
		if err != nil || encoded != again {
			t.Fatal("重复登记轮换了材料", err)
		}
		secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(string(encoded))
		if err != nil {
			t.Fatal("登记材料格式错误")
		}
		defer clear(secret)
		code := totpCode(secret, time.Now().Unix()/30)
		if err = service.Activate(ctx, account.activation, account.password, code, attempt()); err != nil {
			t.Fatal(err)
		}
		request := loginRequest(account)
		request.Scope = "admin"
		request.Platform = "web"
		request.Code = code
		_, err = service.Login(ctx, request, attempt())
		requireError(t, err, ErrInvalidCredentials)
		var ciphertext []byte
		if err = pool.QueryRow(`SELECT ciphertext FROM tombot.user_mfa WHERE user_id=$1`, account.id).Scan(&ciphertext); err != nil {
			t.Fatal(err)
		}
		if string(ciphertext) == string(secret) {
			t.Fatal("管理员验证材料以明文持久化")
		}
		_, err = service.BeginAdminEnrollment(ctx, account.activation, attempt())
		requireError(t, err, ErrInvalidActivation)
	})

	t.Run("管理员登录与再次验证重放", func(t *testing.T) {
		// 使用已完成登记的独立账户快照，不依赖上一条测试耗时恰好落在同一时间步。
		account := fixture.pending(t, "admin")
		secret, err := secure.Random(20)
		if err != nil {
			t.Fatal(err)
		}
		defer clear(secret)
		ciphertext, nonce, err := secure.Seal(service.options.EncryptionKey, secret, account.id)
		if err != nil {
			t.Fatal(err)
		}
		hash, err := hashPassword(account.password)
		if err != nil {
			t.Fatal(err)
		}
		execute(t, pool, `INSERT INTO tombot.user_credentials(user_id,password_hash) VALUES($1,$2)`, account.id, hash)
		execute(t, pool, `INSERT INTO tombot.user_mfa(user_id,ciphertext,nonce) VALUES($1,$2,$3)`, account.id, ciphertext, nonce)
		execute(t, pool, `UPDATE tombot.users SET status='active' WHERE id=$1`, account.id)
		request := loginRequest(account)
		request.Scope = "admin"
		request.Platform = "web"
		_, err = service.Login(ctx, request, attempt())
		requireError(t, err, ErrInvalidCredentials)
		request.Code = totpCode(secret, time.Now().Unix()/30)
		session, err := service.Login(ctx, request, attempt())
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.Login(ctx, request, attempt())
		requireError(t, err, ErrInvalidCredentials)
		if err = service.RequireAdmin(ctx, session.Principal, true); err != nil {
			t.Fatal(err)
		}
		execute(t, pool, `UPDATE tombot.user_sessions SET verified_at=clock_timestamp()-interval '10 minutes' WHERE id=$1`, session.Principal.SessionID)
		requireError(t, service.RequireAdmin(ctx, session.Principal, true), ErrForbidden)
		if err = service.RequireAdmin(ctx, session.Principal, false); err != nil {
			t.Fatal(err)
		}
		code := totpCode(secret, time.Now().Unix()/30+1)
		if err = service.Reauthenticate(ctx, session.Principal, account.password, code, attempt()); err != nil {
			t.Fatal(err)
		}
		if err = service.RequireAdmin(ctx, session.Principal, true); err != nil {
			t.Fatal(err)
		}
		requireError(t, service.Reauthenticate(ctx, session.Principal, account.password, code, attempt()), ErrInvalidCredentials)
	})

	t.Run("账户和来源限流跨实例共享", func(t *testing.T) {
		limited := *service
		limited.options.AccountAttempts = 2
		limited.options.SourceAttempts = 3
		account := fixture.active(t)
		request := loginRequest(account)
		request.Password = randomValue(t)
		source := Attempt{Source: netip.MustParseAddr("192.0.2.1"), RequestID: ids.New()}
		for range 2 {
			_, err := limited.Login(ctx, request, source)
			requireError(t, err, ErrInvalidCredentials)
		}
		request.Password = account.password
		_, err := limited.Login(ctx, request, source)
		requireError(t, err, ErrRateLimited)
		source.Source = netip.MustParseAddr("192.0.2.2")
		_, err = limited.Login(ctx, request, source)
		requireError(t, err, ErrRateLimited)
		source.Source = netip.MustParseAddr("192.0.2.1")
		request.Username = strings.ToLower(ids.New())
		_, err = limited.Login(ctx, request, source)
		requireError(t, err, ErrRateLimited)
		execute(t, pool, `UPDATE tombot.auth_rate_limits SET expires_at=clock_timestamp()-interval '1 second' WHERE bucket_digest=$1 OR bucket_digest=$2`, digest("subject:login:"+account.username), digest("source:"+source.Source.String()))
		request.Username = account.username
		if _, err = limited.Login(ctx, request, source); err != nil {
			t.Fatal("过期窗口未恢复", err)
		}
	})

	t.Run("并发尝试额度不能超发", func(t *testing.T) {
		limited := *service
		limited.options.AccountAttempts = 5
		limited.options.SourceAttempts = 100
		subject := "concurrent:" + ids.New()
		results := make(chan error, 20)
		for index := range 20 {
			go func() {
				source := netip.AddrFrom4([4]byte{198, 51, 100, byte(index + 1)})
				results <- limited.limitAttempt(ctx, subject, Attempt{Source: source, RequestID: ids.New()})
			}()
		}
		success := 0
		for range 20 {
			err := <-results
			if err == nil {
				success++
			} else {
				requireError(t, err, ErrRateLimited)
			}
		}
		if success != 5 {
			t.Fatal("跨连接并发额度错误", success)
		}
	})

	t.Run("数据库错误不能伪装认证成功", func(t *testing.T) {
		account := fixture.active(t)
		execute(t, pool, `REVOKE INSERT ON tombot.authentication_events FROM tombot_identity`)
		t.Cleanup(func() { execute(t, pool, `GRANT INSERT ON tombot.authentication_events TO tombot_identity`) })
		if _, err := service.Login(ctx, loginRequest(account), attempt()); err == nil {
			t.Fatal("审计写入失败仍登录成功")
		}
		var count int
		if err := pool.QueryRow(`SELECT count(*) FROM tombot.user_sessions WHERE user_id=$1`, account.id).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatal("失败登录留下有效会话")
		}
	})

	t.Run("设备上限与到期释放", func(t *testing.T) {
		limited := *service
		limited.options.DeviceLimit = 1
		account := fixture.active(t)
		first, err := limited.Login(ctx, loginRequest(account), attempt())
		if err != nil {
			t.Fatal(err)
		}
		_, err = limited.Login(ctx, loginRequest(account), attempt())
		requireError(t, err, ErrDeviceLimit)
		execute(t, pool, `UPDATE tombot.user_sessions SET created_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, first.Principal.SessionID)
		_, err = service.Authenticate(ctx, string(first.AccessToken), "desktop")
		requireError(t, err, ErrInvalidSession)
		_, err = service.Refresh(ctx, string(first.RefreshToken), "desktop", attempt())
		requireError(t, err, ErrInvalidSession)
		if _, err = limited.Login(ctx, loginRequest(account), attempt()); err != nil {
			t.Fatal("已到期会话未释放设备额度", err)
		}
	})

	t.Run("不存在账户同样受限且不暴露存在性", func(t *testing.T) {
		limited := *service
		limited.options.AccountAttempts = 1
		request := loginRequest(testAccount{username: strings.ToLower(ids.New()), password: randomValue(t)})
		_, err := limited.Login(ctx, request, attempt())
		requireError(t, err, ErrInvalidCredentials)
		_, err = limited.Login(ctx, request, attempt())
		requireError(t, err, ErrRateLimited)
	})

	t.Run("损坏凭据与错误密钥明确失败", func(t *testing.T) {
		account := fixture.active(t)
		execute(t, pool, `UPDATE tombot.user_credentials SET password_hash='invalid' WHERE user_id=$1`, account.id)
		_, err := service.Login(ctx, loginRequest(account), attempt())
		if err == nil || errors.Is(err, ErrInvalidCredentials) {
			t.Fatal("损坏凭据被当成普通认证结果", err)
		}
		admin := fixture.pending(t, "admin")
		if _, err = service.BeginAdminEnrollment(ctx, admin.activation, attempt()); err != nil {
			t.Fatal(err)
		}
		wrong := *service
		wrong.options.EncryptionKey, err = secure.Random(32)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = wrong.BeginAdminEnrollment(ctx, admin.activation, attempt()); err == nil {
			t.Fatal("错误主密钥仍解开了管理员材料")
		}
	})

	t.Run("认证角色不能提权或修改业务内容", func(t *testing.T) {
		account := fixture.active(t)
		for _, statement := range []string{`UPDATE tombot.users SET role='admin' WHERE id=$1`, `UPDATE tombot.users SET status='disabled' WHERE id=$1`, `SELECT id FROM tombot.bots WHERE user_id=$1`} {
			err := database.Platform(ctx, func(transaction *sql.Tx) error {
				_, err := transaction.ExecContext(ctx, statement, account.id)
				return err
			})
			if err == nil {
				t.Fatal("认证连接接受了越界操作")
			}
		}
	})
}

func TestIdentityInputAndPassword(t *testing.T) {
	if name, err := NormalizeUsername("  Test.User_1  "); err != nil || name != "test.user_1" {
		t.Fatal("账户名规范化错误")
	}
	for _, value := range []string{"a", "中文账户", "with space", strings.Repeat("a", 65)} {
		if _, err := NormalizeUsername(value); err == nil {
			t.Fatal("接受了无效账户名")
		}
	}
	password := randomValue(t)
	hash, err := hashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if valid, err := verifyPassword(hash, password); err != nil || !valid {
		t.Fatal("口令验证失败", err)
	}
	if valid, err := verifyPassword(hash, randomValue(t)); err != nil || valid {
		t.Fatal("错误口令被接受", err)
	}
	for _, encoded := range []string{"invalid", passwordParameters + "invalid", strings.Replace(hash, "m=65536", "m=2147483647", 1)} {
		if valid, err := verifyPassword(encoded, password); err == nil || valid {
			t.Fatal("未知或无界派生成本未拒绝")
		}
	}
	for _, value := range []string{"", strings.Repeat("a", 1025), string([]byte{0xff})} {
		requireError(t, validatePassword(value), ErrInvalidInput)
	}
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("接受了缺少数据库及限额的身份配置")
	}
}

func TestPasswordWorkCancellation(t *testing.T) {
	service := Service{passwordSlots: make(chan struct{}, 1)}
	service.passwordSlots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	requireError(t, service.passwordWork(ctx, func() error { t.Fatal("取消后执行口令计算"); return nil }), context.Canceled)
	<-service.passwordSlots
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		if err := service.passwordWork(context.Background(), func() error { return ErrInvalidInput }); !errors.Is(err, ErrInvalidInput) {
			t.Error(err)
		}
	}()
	workers.Wait()
	if len(service.passwordSlots) != 0 {
		t.Fatal("失败后未释放计算额度")
	}
}

func TestTOTPReferenceAndReplay(t *testing.T) {
	// 使用 RFC 6238 附录公开测试向量的字节序列，不使用真实认证材料。
	secret := make([]byte, 20)
	for index := range secret {
		secret[index] = byte('0' + (index+1)%10)
	}
	for _, fixture := range []struct {
		seconds int64
		suffix  string
	}{{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"}, {1234567890, "005924"}, {2000000000, "279037"}, {20000000000, "353130"}} {
		if actual := totpCode(secret, fixture.seconds/30); actual != fixture.suffix {
			t.Fatal("动态验证算法未通过公开向量")
		}
		step, valid := verifyTOTP(secret, fixture.suffix, time.Unix(fixture.seconds, 0), -1)
		if !valid || step != fixture.seconds/30 {
			t.Fatal("有效动态验证信息被拒绝")
		}
		if _, valid = verifyTOTP(secret, fixture.suffix, time.Unix(fixture.seconds, 0), step); valid {
			t.Fatal("动态验证信息可重放")
		}
		if _, valid = verifyTOTP(secret, fixture.suffix, time.Unix(fixture.seconds+90, 0), -1); valid {
			t.Fatal("过期动态验证信息被接受")
		}
	}
}
