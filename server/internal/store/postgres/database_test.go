package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"io.yueyang/tom/internal/computerdesktop"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func testOptions(dsn string, role Role) Options {
	return Options{DSN: dsn, Role: role, MaxConnections: 2, ConnectTimeout: 5 * time.Second,
		StatementTimeout: 5 * time.Second, LockTimeout: time.Second, TransactionTimeout: 10 * time.Second, ConnectionLifetime: time.Minute}
}

func testDatabase(t *testing.T, cluster *pgtest.Cluster, name string, role Role) *Database {
	t.Helper()
	login := cluster.Login(t, string(role))
	database, err := Open(context.Background(), testOptions(cluster.DSN(name, login), role))
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

func seedUser(t *testing.T, control *Database) string {
	t.Helper()
	userID := ids.New()
	err := control.Platform(context.Background(), func(transaction *sql.Tx) error {
		_, err := transaction.Exec(`INSERT INTO tombot.users(id,username,display_name,role,status) VALUES($1,$2,'测试用户','user','provisioning')`, userID, strings.ToLower(userID))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return userID
}

func TestPostgreSQLIntegration(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, name := cluster.Database(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal("重复初始化失败", err)
	}
	tenant := testDatabase(t, cluster, name, TenantRole)
	control := testDatabase(t, cluster, name, ControlRole)
	identity := testDatabase(t, cluster, name, IdentityRole)
	userA, userB := seedUser(t, control), seedUser(t, control)
	botA, botB := ids.New(), ids.New()
	for _, fixture := range []struct{ user, bot string }{{userA, botA}, {userB, botB}} {
		nodeID, imageID, computerID, desktopID := ids.New(), ids.New(), ids.New(), ids.New()
		if err := control.Platform(ctx, func(transaction *sql.Tx) error {
			if _, err := transaction.Exec(`INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2)`, imageID, "example.test/"+imageID+"@sha256:"+strings.Repeat("a", 64)); err != nil {
				return err
			}
			if _, err := transaction.Exec(`INSERT INTO tombot.computer_nodes(id,name,status,capacity,runtime_id,control_sequence,last_seen_at,control_expires_at,verified_image_id,capability_expires_at) VALUES($1,$1,'ready',1,$1,1,clock_timestamp(),clock_timestamp()+interval '5 minutes',$2,clock_timestamp()+interval '15 minutes')`, nodeID, imageID); err != nil {
				return err
			}
			_, err := transaction.Exec(`INSERT INTO tombot.computers(user_id,id,node_id,image_id,status,volume_ref,cpu_millis,memory_bytes,disk_bytes,process_limit) VALUES($1,$2,$3,$4,'pending',$2,1000,1073741824,1073741824,128)`, fixture.user, computerID, nodeID, imageID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := tenant.Tenant(ctx, fixture.user, func(transaction *sql.Tx) error {
			if _, err := transaction.Exec(`INSERT INTO tombot.bots(user_id,id,desktop_id,name) VALUES($1,$2,$3,'同名机器人')`, fixture.user, fixture.bot, desktopID); err != nil {
				return err
			}
			_, err := computerdesktop.Register(ctx, transaction, fixture.user, fixture.bot, desktopID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("行级隔离与连接复用", func(t *testing.T) {
		tenant.pool.SetMaxOpenConns(1)
		t.Cleanup(func() { tenant.pool.SetMaxOpenConns(2) })
		for _, userID := range []string{userA, userB, userA} {
			if err := tenant.Tenant(ctx, userID, func(transaction *sql.Tx) error {
				var count int
				if err := transaction.QueryRow(`SELECT count(*) FROM tombot.bots`).Scan(&count); err != nil {
					return err
				}
				if count != 1 {
					return fmt.Errorf("跨用户查询返回 %d 行", count)
				}
				var owner string
				if err := transaction.QueryRow(`SELECT user_id FROM tombot.bots`).Scan(&owner); err != nil {
					return err
				}
				if owner != userID {
					return errors.New("连接继承了其他用户上下文")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		var count int
		if err := tenant.pool.QueryRow(`SELECT count(*) FROM tombot.bots`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("无用户上下文仍能读取业务记录: %d %v", count, err)
		}
		if err := tenant.Tenant(ctx, "", func(*sql.Tx) error { t.Fatal("无身份进入事务"); return nil }); err == nil {
			t.Fatal("缺少用户身份未拒绝")
		}
		if err := tenant.Platform(ctx, func(*sql.Tx) error { return nil }); err == nil {
			t.Fatal("租户连接允许平台事务")
		}
	})

	t.Run("跨用户写入与关联被数据库拒绝", func(t *testing.T) {
		for _, query := range []struct {
			statement string
			arguments []any
			code      string
		}{
			{`INSERT INTO tombot.bots(user_id,id,desktop_id,name) VALUES($1,$2,$3,'越权机器人')`, []any{userB, ids.New(), ids.New()}, "42501"},
			{`INSERT INTO tombot.user_workspaces(user_id,active_bot_id) VALUES($1,$2)`, []any{userA, botB}, "23503"},
			{`INSERT INTO tombot.bots(user_id,id,desktop_id,name) VALUES($1,$2,$3,'同名机器人')`, []any{userA, ids.New(), ids.New()}, "23505"},
		} {
			err := tenant.Tenant(ctx, userA, func(transaction *sql.Tx) error {
				_, err := transaction.Exec(query.statement, query.arguments...)
				return err
			})
			var failure *pgconn.PgError
			if !errors.As(err, &failure) || failure.Code != query.code {
				t.Fatalf("数据库未按约束拒绝，预期 %s: %v", query.code, err)
			}
		}
		if err := tenant.Tenant(ctx, userA, func(transaction *sql.Tx) error {
			result, err := transaction.Exec(`UPDATE tombot.bots SET name='越权修改' WHERE id=$1`, botB)
			if err != nil {
				return err
			}
			count, err := result.RowsAffected()
			if err != nil || count != 0 {
				return errors.New("跨用户更新没有被隔离")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("角色最小权限", func(t *testing.T) {
		for _, database := range []*Database{control, identity} {
			if err := database.Platform(ctx, func(transaction *sql.Tx) error {
				_, err := transaction.Exec(`SELECT * FROM tombot.bots`)
				return err
			}); err == nil {
				t.Fatal("管理或身份连接获得用户内容访问权")
			}
		}
		for _, table := range []string{"model_credentials", "user_credentials", "session_tokens"} {
			if err := tenant.Tenant(ctx, userA, func(transaction *sql.Tx) error {
				_, err := transaction.Exec("SELECT * FROM tombot." + table)
				return err
			}); err == nil {
				t.Fatalf("租户连接允许读取 %s", table)
			}
		}
		for _, login := range []string{"test_owner", cluster.Login(t), cluster.Login(t, string(TenantRole), string(ControlRole))} {
			database, err := Open(ctx, testOptions(cluster.DSN(name, login), TenantRole))
			if err == nil {
				if closeErr := database.Close(); closeErr != nil {
					t.Error(closeErr)
				}
				t.Fatal("危险角色或用途不匹配未拒绝")
			}
		}
	})

	t.Run("业务与事件共同回滚", func(t *testing.T) {
		failure := errors.New("模拟业务中断")
		err := tenant.Tenant(ctx, userA, func(transaction *sql.Tx) error {
			if _, err := transaction.Exec(`UPDATE tombot.bots SET name='尚未提交' WHERE id=$1`, botA); err != nil {
				return err
			}
			if _, err := AppendEvent(ctx, transaction, userA, "bot.updated", botA, json.RawMessage(`{}`)); err != nil {
				return err
			}
			return failure
		})
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
		if err := tenant.Snapshot(ctx, userA, func(transaction *sql.Tx) error {
			var name string
			if err := transaction.QueryRow(`SELECT name FROM tombot.bots WHERE id=$1`, botA).Scan(&name); err != nil {
				return err
			}
			events, err := ReadEvents(ctx, transaction, userA, 0, 10)
			if err != nil {
				return err
			}
			if name != "同名机器人" || len(events) != 0 {
				return errors.New("失败事务留下业务或事件记录")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("事件头锁防止提交顺序反转", func(t *testing.T) {
		testEventCommitOrder(t, tenant, userA, userB)
	})
	t.Run("取消事务不遗留身份或修改", func(t *testing.T) {
		canceled, cancel := context.WithCancel(ctx)
		err := tenant.Tenant(canceled, userA, func(transaction *sql.Tx) error {
			if _, err := transaction.Exec(`UPDATE tombot.bots SET name='应被撤销' WHERE id=$1`, botA); err != nil {
				return err
			}
			cancel()
			return canceled.Err()
		})
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatal("取消事务未失败", err)
		}
		if err := tenant.Tenant(ctx, userA, func(transaction *sql.Tx) error {
			var name string
			if err := transaction.QueryRow(`SELECT name FROM tombot.bots WHERE id=$1`, botA).Scan(&name); err != nil {
				return err
			}
			if name != "同名机器人" {
				return errors.New("取消事务仍提交修改")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("初始化失败拒绝篡改与旧库", func(t *testing.T) {
		if _, err := pool.Exec(`UPDATE tombot.schema_migrations SET checksum=repeat('0',64)`); err != nil {
			t.Fatal(err)
		}
		if err := Migrate(ctx, pool); err == nil {
			t.Fatal("迁移摘要篡改未拒绝")
		}
		foreign, _ := cluster.Database(t)
		if _, err := foreign.Exec(`CREATE TABLE existing_records (id integer)`); err != nil {
			t.Fatal(err)
		}
		if err := Migrate(ctx, foreign); err == nil {
			t.Fatal("已有未知数据仍被初始化")
		}
		var present bool
		if err := foreign.QueryRow(`SELECT to_regclass('existing_records') IS NOT NULL`).Scan(&present); err != nil || !present {
			t.Fatal("初始化拒绝时改动了现有数据", err)
		}
	})

	t.Run("并发初始化只执行一份基线", func(t *testing.T) {
		fresh, _ := cluster.Database(t)
		results := make(chan error, 2)
		for range 2 {
			go func() { results <- Migrate(ctx, fresh) }()
		}
		for range 2 {
			if err := <-results; err != nil {
				t.Fatal("并发初始化失败", err)
			}
		}
		var count int
		manifest, err := readMigrations(migrationFiles)
		if err != nil {
			t.Fatal(err)
		}
		if err := fresh.QueryRow(`SELECT count(*) FROM tombot.schema_migrations`).Scan(&count); err != nil || count != len(manifest) {
			t.Fatal("并发初始化重复记录版本", count, err)
		}
	})

	t.Run("新脚本失败完整回滚", func(t *testing.T) {
		fresh, _ := cluster.Database(t)
		source := fstest.MapFS{}
		entries, err := fs.ReadDir(migrationFiles, "migrations")
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			source["migrations/"+entry.Name()] = &fstest.MapFile{Data: content}
		}
		source["migrations/9999_test_failure.sql"] = &fstest.MapFile{Data: []byte(`CREATE TABLE tombot.rollback_probe(id integer); SELECT missing_column;`)}
		if err := migrate(ctx, fresh, source); err == nil {
			t.Fatal("错误脚本未导致初始化失败")
		}
		var exists bool
		if err := fresh.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname='tombot')`).Scan(&exists); err != nil || exists {
			t.Fatal("失败后遗留部分数据库结构", err)
		}
		if err := Migrate(ctx, fresh); err != nil {
			t.Fatal("失败回滚后不能正常初始化", err)
		}
	})
}

func testEventCommitOrder(t *testing.T, database *Database, userA, userB string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	firstReady := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstResult, secondResult := make(chan error, 1), make(chan error, 1)
	go func() {
		firstResult <- database.Tenant(ctx, userA, func(transaction *sql.Tx) error {
			if _, err := AppendEvent(ctx, transaction, userA, "first", "test", json.RawMessage(`{}`)); err != nil {
				return err
			}
			close(firstReady)
			select {
			case <-releaseFirst:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-firstReady:
	case err := <-firstResult:
		t.Fatal("首个事件写入失败", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	secondStarted := make(chan struct{})
	go func() {
		secondResult <- database.Tenant(ctx, userA, func(transaction *sql.Tx) error {
			close(secondStarted)
			_, err := AppendEvent(ctx, transaction, userA, "second", "test", json.RawMessage(`{}`))
			return err
		})
	}()
	select {
	case <-secondStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// 第二个事务已进入，必须等待首事务提交才能取得该用户的后续序号。
	select {
	case err := <-secondResult:
		t.Fatal("后序事务越过未提交事件", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	for _, result := range []<-chan error{firstResult, secondResult} {
		if err := <-result; err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Tenant(ctx, userA, func(transaction *sql.Tx) error {
		events, err := ReadEvents(ctx, transaction, userA, 0, 10)
		if err != nil {
			return err
		}
		if len(events) != 2 || events[0].Sequence != 1 || events[0].Kind != "first" || events[1].Sequence != 2 || events[1].Kind != "second" {
			return fmt.Errorf("事件提交顺序不正确: %+v", events)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.Tenant(ctx, userB, func(transaction *sql.Tx) error {
		events, err := ReadEvents(ctx, transaction, userA, 0, 10)
		if err != nil {
			return err
		}
		if len(events) != 0 {
			return errors.New("用户乙读取了用户甲的事件")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestConfigurationAndMigrationValidation(t *testing.T) {
	for _, source := range []fstest.MapFS{
		{},
		{"migrations/0029_empty.sql": {Data: []byte(" ")}},
		{"migrations/0029_first.sql": {Data: []byte("SELECT 1")}, "migrations/0029_second.sql": {Data: []byte("SELECT 2")}},
		{"migrations/not_versioned.sql": {Data: []byte("SELECT 1")}},
	} {
		if _, err := readMigrations(source); err == nil {
			t.Fatal("无效迁移清单未被拒绝")
		}
	}
	for _, mutate := range []func(*Options){
		func(options *Options) { options.Role = "administrator" },
		func(options *Options) { options.MaxConnections = 0 },
		func(options *Options) { options.LockTimeout = time.Hour },
		func(options *Options) { options.DSN = "" },
		func(options *Options) { options.TransactionTimeout = 0 },
	} {
		options := testOptions("host=/tmp", TenantRole)
		mutate(&options)
		if _, err := Open(context.Background(), options); err == nil {
			t.Fatal("无效数据库参数被接受")
		}
	}
	if _, err := Open(context.Background(), testOptions("host=database.invalid sslmode=disable", TenantRole)); err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Fatal("允许远程明文数据库连接", err)
	}
}
