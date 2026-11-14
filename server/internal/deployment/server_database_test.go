package deployment

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"io.yueyang/tom/internal/infra/ids"

	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func databaseScriptFixture(t *testing.T) (string, map[string]*url.URL) {
	t.Helper()
	identifier := make([]byte, 16)
	if _, err := rand.Read(identifier); err != nil {
		t.Fatal(err)
	}
	connections := map[string]*url.URL{}
	for _, purpose := range serverDatabasePurposes() {
		password := make([]byte, 32)
		if _, err := rand.Read(password); err != nil {
			t.Fatal(err)
		}
		connections[purpose] = &url.URL{Scheme: "postgres", User: url.UserPassword("tombot_"+purpose+"_login", hex.EncodeToString(password)), Host: "127.0.0.1:5432", Path: "/tombot"}
	}
	return hex.EncodeToString(identifier), connections
}

func runDatabaseScript(t *testing.T, cluster *pgtest.Cluster, script []byte) error {
	t.Helper()
	program := "psql"
	if directory := os.Getenv("TOM_TEST_POSTGRES_BIN"); directory != "" {
		program = filepath.Join(directory, "psql")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, program, "-X", "-v", "ON_ERROR_STOP=1", "--dbname="+cluster.DSN("postgres", "test_owner"))
	command.Stdin = strings.NewReader(string(script))
	// 验证器只经私有标准输入传入，测试失败也不打印 SQL 或认证摘要。
	return command.Run()
}

func TestServerDatabaseBootstrapRealPostgresAndRetry(t *testing.T) {
	cluster := pgtest.Start(t)
	id, connections := databaseScriptFixture(t)
	script, err := serverDatabaseScript(id, connections)
	if err != nil {
		t.Fatal(err)
	}
	for _, connection := range connections {
		password, _ := connection.User.Password()
		if strings.Contains(string(script), password) {
			t.Fatal("明文密码进入初始化 SQL")
		}
	}
	if err := runDatabaseScript(t, cluster, script); err != nil {
		t.Fatal("数据库初始化脚本失败", err)
	}
	if err := runDatabaseScript(t, cluster, script); err != nil {
		t.Fatal("重复初始化失败", err)
	}
	migration := cluster.DSN("tombot", "tombot_migration_login")
	if err := postgres.Initialize(t.Context(), migration); err != nil {
		t.Fatal("独立迁移身份无法初始化结构", err)
	}
	if err := postgres.CheckUpgrade(t.Context(), migration); err != nil {
		t.Fatal(err)
	}
	for _, role := range []postgres.Role{postgres.IdentityRole, postgres.ControlRole, postgres.TenantRole, postgres.DispatchRole} {
		database, err := postgres.Open(t.Context(), postgres.Options{DSN: cluster.DSN("tombot", string(role)+"_login"), Role: role, MaxConnections: 1, ConnectTimeout: 5 * time.Second, StatementTimeout: 5 * time.Second, LockTimeout: time.Second, TransactionTimeout: 10 * time.Second, ConnectionLifetime: time.Minute})
		if err != nil {
			t.Fatal("运行角色权限不符合要求", role, err)
		}
		if err := database.VerifyCurrent(t.Context()); err != nil {
			t.Error(err)
		}
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	}
	pool := pgtest.Connect(t, cluster.DSN("postgres", "test_owner"))
	var verifier string
	if err := pool.QueryRowContext(t.Context(), "SELECT rolpassword FROM pg_authid WHERE rolname='tombot_identity_login'").Scan(&verifier); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(verifier, "SCRAM-SHA-256$4096:") {
		t.Fatal("数据库未保存 SCRAM 认证摘要")
	}
	// 用真实 SCRAM 握手证明摘要可用，并确认错误口令被拒绝；仅修改隔离测试集群。
	address, err := url.Parse(cluster.DSN("postgres", "test_owner"))
	if err != nil {
		t.Fatal(err)
	}
	hbaPath := filepath.Join(address.Query().Get("host"), "cluster", "pg_hba.conf")
	hba, err := os.ReadFile(hbaPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hbaPath, append([]byte("local all tombot_identity_login scram-sha-256\n"), hba...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(t.Context(), "SELECT pg_reload_conf()"); err != nil {
		t.Fatal(err)
	}
	login, err := url.Parse(cluster.DSN("tombot", "tombot_identity_login"))
	if err != nil {
		t.Fatal(err)
	}
	login.User = url.UserPassword("tombot_identity_login", "incorrect-test-input")
	rejected := false
	for attempt := 0; attempt < 30; attempt++ {
		connection, err := pgx.Connect(t.Context(), login.String())
		if err != nil {
			rejected = true
			break
		}
		if err := connection.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !rejected {
		t.Fatal("SCRAM 认证未拒绝错误口令")
	}
	password, _ := connections["identity"].User.Password()
	login.User = url.UserPassword("tombot_identity_login", password)
	authenticated, err := pgx.Connect(t.Context(), login.String())
	if err != nil {
		t.Fatal("生成的 SCRAM 摘要无法认证")
	}
	if err := authenticated.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	otherID, _ := databaseScriptFixture(t)
	other, err := serverDatabaseScript(otherID, connections)
	if err != nil {
		t.Fatal(err)
	}
	if err := runDatabaseScript(t, cluster, other); err == nil {
		t.Fatal("其他安装接管了同名数据库")
	}
}

func TestServerManagedBackupRestoresAllTenantRows(t *testing.T) {
	cluster := pgtest.Start(t)
	id, connections := databaseScriptFixture(t)
	script, err := serverDatabaseScript(id, connections)
	if err != nil {
		t.Fatal(err)
	}
	if err := runDatabaseScript(t, cluster, script); err != nil {
		t.Fatal(err)
	}
	if err := postgres.Initialize(t.Context(), cluster.DSN("tombot", "tombot_migration_login")); err != nil {
		t.Fatal(err)
	}
	pool := pgtest.Connect(t, cluster.DSN("tombot", "test_owner"))
	for _, name := range []string{"backup_user_a", "backup_user_b"} {
		if _, err := pool.ExecContext(t.Context(), "INSERT INTO tombot.users(id,username,display_name,status) VALUES($1,$2,'备份测试','provisioning')", ids.New(), name); err != nil {
			t.Fatal(err)
		}
	}
	installer, _ := serverInstallFixture(t)
	installServerFixture(t, installer)
	api, err := installer.account(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := installer.loadServerConfig(api)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(installer.fs.resolve("/backup"), 0700); err != nil {
		t.Fatal(err)
	}
	installer.run = func(ctx context.Context, command serverCommand) ([]byte, error) {
		if command.program != "/usr/sbin/runuser" || !strings.Contains(strings.Join(command.arguments, " "), "-u postgres -- /usr/bin/pg_dump") {
			t.Fatal("本机备份没有使用隔离的管理员身份")
		}
		program := filepath.Join(os.Getenv("TOM_TEST_POSTGRES_BIN"), "pg_dump")
		if os.Getenv("TOM_TEST_POSTGRES_BIN") == "" {
			var err error
			program, err = exec.LookPath("pg_dump")
			if err != nil {
				t.Fatal(err)
			}
		}
		return runServerCommand(ctx, serverCommand{program: program, arguments: []string{"--format=custom", "--dbname=" + cluster.DSN("tombot", "test_owner")}, output: command.output, timeout: time.Minute})
	}
	if err := installer.dumpServerDatabase(t.Context(), configuration, "/backup"); err != nil {
		t.Fatal(err)
	}
	restored, database := cluster.Database(t)
	program := filepath.Join(os.Getenv("TOM_TEST_POSTGRES_BIN"), "pg_restore")
	if os.Getenv("TOM_TEST_POSTGRES_BIN") == "" {
		program, err = exec.LookPath("pg_restore")
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runServerCommand(t.Context(), serverCommand{program: program, arguments: []string{"--exit-on-error", "--dbname=" + cluster.DSN(database, "test_owner"), installer.fs.resolve("/backup/database.dump")}, output: io.Discard, timeout: time.Minute}); err != nil {
		t.Fatal("备份无法恢复", err)
	}
	var count int
	if err := restored.QueryRowContext(t.Context(), "SELECT count(*) FROM tombot.users").Scan(&count); err != nil || count != 2 {
		t.Fatal("没有完整恢复各租户数据", err)
	}
	if err := postgres.CheckUpgrade(t.Context(), cluster.DSN(database, "test_owner")); err != nil {
		t.Fatal("恢复后的迁移账本不完整", err)
	}
}

func TestServerDatabaseBootstrapRefusesExistingResources(t *testing.T) {
	for _, statement := range []string{"CREATE DATABASE tombot", "CREATE ROLE tombot_identity NOLOGIN"} {
		t.Run(statement, func(t *testing.T) {
			cluster := pgtest.Start(t)
			pool := pgtest.Connect(t, cluster.DSN("postgres", "test_owner"))
			if _, err := pool.ExecContext(t.Context(), statement); err != nil {
				t.Fatal(err)
			}
			id, connections := databaseScriptFixture(t)
			script, err := serverDatabaseScript(id, connections)
			if err != nil {
				t.Fatal(err)
			}
			if err := runDatabaseScript(t, cluster, script); err == nil {
				t.Fatal("覆盖了不属于安装器的资源")
			}
			var count int
			if err := pool.QueryRowContext(t.Context(), "SELECT count(*) FROM pg_roles WHERE rolname='tombot_migration_login'").Scan(&count); err != nil || count != 0 {
				t.Fatal("角色创建事务没有回滚", err)
			}
		})
	}
}

func TestServerUpgradeDatabaseCheckIsReadOnly(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, name := cluster.Database(t)
	connection := cluster.DSN(name, "test_owner")
	if err := postgres.CheckUpgrade(t.Context(), connection); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := pool.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname='tombot')").Scan(&exists); err != nil || exists {
		t.Fatal("预检意外执行迁移", err)
	}
	if err := postgres.Initialize(t.Context(), connection); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(t.Context(), "UPDATE tombot.schema_migrations SET checksum=repeat('0',64) WHERE version=(SELECT min(version) FROM tombot.schema_migrations)"); err != nil {
		t.Fatal(err)
	}
	if err := postgres.CheckUpgrade(t.Context(), connection); err == nil {
		t.Fatal("升级未拒绝篡改的历史")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := postgres.CheckUpgrade(ctx, connection); err == nil {
		t.Fatal("升级预检忽略取消")
	}
}
