package deployment

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/pbkdf2"
	"io.yueyang/tom/internal/linuxhost"
	"io.yueyang/tom/internal/serverapp"
	"io.yueyang/tom/internal/store/postgres"
)

func serverDatabasePurposes() [5]string {
	return [5]string{"migration", "identity", "control", "tenant", "dispatch"}
}

func (installer serverInstaller) ensureServerDatabase(ctx context.Context, record serverInstallation) error {
	if err := installer.ensurePostgresPackages(ctx); err != nil {
		return err
	}
	connections := map[string]*url.URL{}
	for _, purpose := range serverDatabasePurposes() {
		encoded, err := installer.fs.readServerFile(fileRule{path: serverDirectory + "/" + purpose + ".connection", maximum: 16 << 10, private: true, api: true}, account{uid: record.UID, gid: record.GID})
		if err != nil {
			return err
		}
		connection, err := url.Parse(strings.TrimSpace(string(encoded)))
		clear(encoded)
		if err != nil || connection.User == nil || connection.User.Username() != "tombot_"+purpose+"_login" || connection.Host != "127.0.0.1:5432" || connection.Path != "/tombot" {
			return errors.New("自动管理的数据库连接文件已变化，未修改数据库角色")
		}
		password, present := connection.User.Password()
		if !present || len(password) != 64 {
			return errors.New("自动数据库认证文件无效")
		}
		connections[purpose] = connection
	}
	script, err := serverDatabaseScript(record.ID, connections)
	if err != nil {
		return err
	}
	defer clear(script)
	if _, err := installer.run(ctx, serverCommand{program: "/usr/sbin/runuser", arguments: []string{"-u", "postgres", "--", "/usr/bin/psql", "-X", "--no-password", "-v", "ON_ERROR_STOP=1", "-h", "/var/run/postgresql", "-p", "5432", "-d", "postgres"}, input: script, timeout: time.Minute}); err != nil {
		return errors.New("初始化本机 PostgreSQL 失败；已有数据库、角色或认证规则冲突时不会接管，请检查 PostgreSQL 服务状态")
	}
	// 重跑只复用原密码，不执行 ALTER PASSWORD；认证失败明确阻止启动。
	for _, purpose := range serverDatabasePurposes() {
		configuration, err := pgx.ParseConfig(connections[purpose].String())
		if err != nil {
			return errors.New("数据库连接文件无效")
		}
		configuration.ConnectTimeout = 5 * time.Second
		connection, err := pgx.ConnectConfig(ctx, configuration)
		if err != nil {
			return errors.New("新建数据库身份无法连接；未重置密码，请检查本机 PostgreSQL 认证规则")
		}
		if err := connection.Close(ctx); err != nil {
			return errors.New("关闭数据库核验连接失败")
		}
	}
	return nil
}

// serverDatabaseScript 只初始化数据库容器和登录角色，业务结构始终由不可变迁移创建。
// 随机安装标识用于拒绝接管同名资源；角色创建在同一事务，数据库创建通过所有者恢复。
func serverDatabaseScript(id string, connections map[string]*url.URL) ([]byte, error) {
	marker := sqlLiteral("tombot-install:" + id)
	var script strings.Builder
	script.WriteString("SET log_statement = 'none'; SET log_min_duration_statement = -1; SET log_min_duration_sample = -1; SET log_transaction_sample_rate = 0; SET log_min_error_statement = 'panic';\nSET statement_timeout = '45s'; SET lock_timeout = '10s';\nSELECT pg_advisory_lock(hashtextextended('tombot.server.install', 0));\nBEGIN;\n")
	fmt.Fprintf(&script, `DO $guard$ BEGIN
IF EXISTS (SELECT 1 FROM pg_database d WHERE d.datname='tombot' AND
 (pg_get_userbyid(d.datdba)<>'tombot_migration_login' OR shobj_description(d.datdba,'pg_authid') IS DISTINCT FROM %s)) THEN
 RAISE EXCEPTION '已有数据库不属于本次安装'; END IF;
END $guard$;
`, marker)
	for _, purpose := range serverDatabasePurposes() {
		if purpose != "migration" {
			writeServerRole(&script, "tombot_"+purpose, marker, "NOLOGIN")
		}
		connection := connections[purpose]
		if connection == nil || connection.User == nil {
			return nil, errors.New("缺少数据库身份配置")
		}
		password, present := connection.User.Password()
		if !present || password == "" {
			return nil, errors.New("缺少数据库认证材料")
		}
		verifier, err := serverSCRAMVerifier(password)
		if err != nil {
			return nil, err
		}
		membership := ""
		if purpose != "migration" {
			membership = " IN ROLE tombot_" + purpose
		}
		writeServerRole(&script, "tombot_"+purpose+"_login", marker, "LOGIN PASSWORD "+sqlLiteral(verifier)+membership)
	}
	script.WriteString("COMMIT;\nSELECT 'CREATE DATABASE tombot OWNER tombot_migration_login' WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname='tombot')\n\\gexec\nREVOKE ALL ON DATABASE tombot FROM PUBLIC;\nGRANT CONNECT ON DATABASE tombot TO tombot_migration_login,tombot_identity_login,tombot_control_login,tombot_tenant_login,tombot_dispatch_login;\n")
	return []byte(script.String()), nil
}

func writeServerRole(script *strings.Builder, name, marker, attributes string) {
	fmt.Fprintf(script, `DO $role$ BEGIN
IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname=%s) THEN
 IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname=%s AND shobj_description(oid,'pg_authid')=%s
 AND NOT (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)) THEN
 RAISE EXCEPTION '已有数据库角色不属于本次安装或权限异常'; END IF;
ELSE
 CREATE ROLE %s NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS INHERIT %s;
 COMMENT ON ROLE %s IS %s;
END IF; END $role$;
`, sqlLiteral(name), sqlLiteral(name), marker, name, attributes, name, marker)
}

func sqlLiteral(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

func serverSCRAMVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", errors.New("生成数据库认证盐失败")
	}
	salted := pbkdf2.Key([]byte(password), salt, 4096, 32, sha256.New)
	defer clear(salted)
	client := hmac.New(sha256.New, salted)
	client.Write([]byte("Client Key"))
	stored := sha256.Sum256(client.Sum(nil))
	server := hmac.New(sha256.New, salted)
	server.Write([]byte("Server Key"))
	encode := base64.StdEncoding.EncodeToString
	return "SCRAM-SHA-256$4096:" + encode(salt) + "$" + encode(stored[:]) + ":" + encode(server.Sum(nil)), nil
}

func (installer serverInstaller) migrationConnection(configuration serverapp.Config) (string, error) {
	api, err := installer.account(context.Background(), false)
	if err != nil {
		return "", err
	}
	encoded, err := installer.fs.readServerFile(fileRule{path: configuration.Database.MigrationDSNFile, maximum: 16 << 10, private: true, api: true}, api)
	defer clear(encoded)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(encoded)), nil
}

func (installer serverInstaller) checkServerDatabase(ctx context.Context, configuration serverapp.Config) error {
	connection, err := installer.migrationConnection(configuration)
	if err != nil {
		return err
	}
	return postgres.CheckUpgrade(ctx, connection)
}

// dumpServerDatabase 使用受保护的口令文件，命令参数只包含去掉密码的 URI。
// 高级配置的迁移连接要求 PostgreSQL URI，避免猜测或重写任意连接参数。
func (installer serverInstaller) dumpServerDatabase(ctx context.Context, configuration serverapp.Config, destination string) (result error) {
	connection, err := installer.migrationConnection(configuration)
	if err != nil {
		return err
	}
	parsed, err := url.Parse(connection)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.User == nil || parsed.Hostname() == "" {
		return errors.New("自动升级备份要求迁移连接使用完整 PostgreSQL URI")
	}
	record, exists, err := installer.loadServerRecord()
	if err != nil || !exists {
		return errors.New("备份缺少安装记录")
	}
	if record.ManagedDatabase && parsed.Host == "127.0.0.1:5432" && parsed.Path == "/tombot" {
		// FORCE RLS 阻止普通迁移身份导出全部租户；仅安装器借助本机 postgres 身份备份，业务身份不提权。
		return installer.dumpLocalServerDatabase(ctx, destination)
	}
	password, present := parsed.User.Password()
	if !present || password == "" || parsed.Query().Has("password") {
		return errors.New("迁移 URI 必须通过独立登录密码认证，不支持查询参数中的密码")
	}
	port := parsed.Port()
	if port == "" {
		port = "5432"
	}
	escape := func(value string) string { return strings.NewReplacer("\\", "\\\\", ":", "\\:").Replace(value) }
	if strings.ContainsAny(password, "\r\n") {
		return errors.New("数据库备份认证材料格式无效")
	}
	passwordFile := filepath.Join(destination, "backup.pgpass")
	content := []byte(strings.Join([]string{escape(parsed.Hostname()), port, escape(strings.TrimPrefix(parsed.Path, "/")), escape(parsed.User.Username()), escape(password)}, ":") + "\n")
	defer clear(content)
	if err := writeConfigurationFile(installer.fs.resolve(passwordFile), content, 0600); err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(installer.fs.resolve(passwordFile)); err != nil {
			result = errors.Join(result, errors.New("删除备份临时认证文件失败"))
		}
	}()
	parsed.User = url.User(parsed.User.Username())
	file, err := os.OpenFile(installer.fs.resolve(filepath.Join(destination, "database.dump")), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("创建数据库备份失败")
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	if _, err := installer.run(ctx, serverCommand{program: "/usr/bin/pg_dump", arguments: []string{"--format=custom", "--no-password", "--dbname=" + parsed.String()}, output: file, environment: []string{"PGPASSFILE=" + passwordFile}, timeout: time.Hour}); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return errors.New("数据库备份刷盘失败")
	}
	return nil
}

func (installer serverInstaller) dumpLocalServerDatabase(ctx context.Context, destination string) (result error) {
	file, err := os.OpenFile(installer.fs.resolve(filepath.Join(destination, "database.dump")), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("创建本机数据库备份失败")
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	_, err = installer.run(ctx, serverCommand{program: "/usr/sbin/runuser", arguments: []string{"-u", "postgres", "--", "/usr/bin/pg_dump", "--format=custom", "--no-password", "--host=/var/run/postgresql", "--port=5432", "--dbname=tombot"}, output: file, timeout: time.Hour})
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return errors.New("本机数据库备份刷盘失败")
	}
	return nil
}

// ensurePostgresPackages 使用宿主维护的软件包，不锁定发行版版本；仅首次初始化空的 RPM 集群。
func (installer serverInstaller) ensurePostgresPackages(ctx context.Context) error {
	encoded, err := os.ReadFile(installer.fs.resolve("/etc/os-release"))
	if err != nil {
		return errors.New("无法读取 Linux 发行信息")
	}
	release, err := linuxhost.ParseRelease(encoded)
	if err != nil {
		return err
	}
	names := []string{"postgresql", "postgresql-client"}
	if release.Family == "dnf" {
		names = []string{"postgresql-server", "postgresql"}
	}
	var missing []string
	for _, name := range names {
		query := serverCommand{program: "/usr/bin/dpkg-query", arguments: []string{"-W", "-f=${db:Status-Status}\n", name}, timeout: 30 * time.Second}
		expected := "installed\n"
		if release.Family == "dnf" {
			query.program = "/usr/bin/rpm"
			query.arguments = []string{"-q", "--qf", "%{NAME}\n", name}
			expected = name + "\n"
		}
		output, err := installer.run(ctx, query)
		if err != nil {
			var failure *serverCommandExit
			if !errors.As(err, &failure) || failure.code != 1 {
				return err
			}
			missing = append(missing, name)
		} else if string(output) != expected {
			return errors.New("PostgreSQL 软件包状态异常，未覆盖或修复宿主包数据库")
		}
	}
	if len(missing) > 0 {
		missing = append(missing, "ca-certificates")
		if release.Family == "apt" {
			for _, args := range [][]string{{"update"}, append([]string{"install", "--yes", "--no-remove", "--no-upgrade"}, missing...)} {
				args = append([]string{"-o", "DPkg::Lock::Timeout=60", "-o", "Acquire::Retries=2", "-o", "APT::Update::Error-Mode=any", "-o", "Acquire::http::Timeout=30", "-o", "Acquire::https::Timeout=30"}, args...)
				if _, err := installer.run(ctx, serverCommand{program: release.Manager(), arguments: args, timeout: 30 * time.Minute, output: io.Discard}); err != nil {
					return err
				}
			}
		} else {
			args := append([]string{"--assumeyes", "--setopt=timeout=30", "--setopt=retries=2", "install"}, missing...)
			if _, err := installer.run(ctx, serverCommand{program: release.Manager(), arguments: args, timeout: 30 * time.Minute, output: io.Discard}); err != nil {
				return err
			}
		}
	}
	if release.Family == "dnf" {
		cluster := installer.fs.resolve("/var/lib/pgsql/data")
		if _, err := os.Lstat(filepath.Join(cluster, "PG_VERSION")); errors.Is(err, os.ErrNotExist) {
			entries, err := os.ReadDir(cluster)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return errors.New("无法检查 PostgreSQL 数据目录")
			}
			if len(entries) != 0 {
				return errors.New("PostgreSQL 数据目录非空且缺少有效集群，未覆盖")
			}
			// 初始化选项只作用于新集群；不重写已有数据库的认证规则。
			if _, err := installer.run(ctx, serverCommand{program: "/usr/bin/postgresql-setup", arguments: []string{"--initdb"}, environment: []string{"PGSETUP_INITDB_OPTIONS=--auth-local=peer --auth-host=scram-sha-256"}, timeout: 2 * time.Minute}); err != nil {
				return err
			}
		} else if err != nil {
			return errors.New("无法检查 PostgreSQL 集群版本")
		}
	}
	if _, err := installer.runSystemctl(ctx, 90*time.Second, "enable", "--now", "postgresql.service"); err != nil {
		return err
	}
	return nil
}
