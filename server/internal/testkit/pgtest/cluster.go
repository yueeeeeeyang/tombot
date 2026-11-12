// Package pgtest 为集成测试启动独立 PostgreSQL，不连接开发者现有数据库。
package pgtest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"io.yueyang/tom/internal/infra/ids"
)

// Cluster 持有测试专用进程及私有 Unix 套接字目录，生命周期绑定测试。
type Cluster struct {
	socket string
	owner  string
	pool   *sql.DB
}

// Start 初始化空集群并启动真实进程；缺少工具或启动失败会使测试失败，绝不跳过验证。
func Start(t testing.TB) *Cluster {
	t.Helper()
	lookup := func(name string) string {
		if directory := os.Getenv("TOM_TEST_POSTGRES_BIN"); directory != "" {
			return filepath.Join(directory, name)
		}
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("PostgreSQL 集成测试需要 %s；请设置 TOM_TEST_POSTGRES_BIN 或 PATH", name)
		}
		return path
	}
	// macOS 套接字路径有长度限制，使用权限为 0700 的短独立目录。
	directory, err := os.MkdirTemp("/tmp", "tombot-pg-")
	if err != nil {
		t.Fatal("创建 PostgreSQL 测试目录失败", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error("删除 PostgreSQL 测试目录失败", err)
		}
	})
	dataDirectory := filepath.Join(directory, "cluster")
	owner := "test_owner"
	run := func(program string, arguments ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if output, err := exec.CommandContext(ctx, program, arguments...).CombinedOutput(); err != nil {
			t.Fatalf("PostgreSQL 测试进程 %s 失败: %v\n%s", filepath.Base(program), err, output)
		}
	}
	// 只允许私有目录内的本地测试身份，关闭 TCP，避免产生外部可访问的无鉴权实例。
	run(lookup("initdb"), "-D", dataDirectory, "-U", owner, "--auth-local=trust", "--auth-host=reject", "--no-locale", "--encoding=UTF8")
	configuration := fmt.Sprintf("\nlisten_addresses = ''\nunix_socket_directories = '%s'\nlog_statement = 'none'\nlog_parameter_max_length_on_error = 0\n", directory)
	file, err := os.OpenFile(filepath.Join(dataDirectory, "postgresql.conf"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString(configuration)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("写入测试集群配置失败: %v %v", writeErr, closeErr)
	}
	control := lookup("pg_ctl")
	run(control, "-D", dataDirectory, "-l", filepath.Join(directory, "postgres.log"), "-w", "-t", "20", "start")
	t.Cleanup(func() { run(control, "-D", dataDirectory, "-w", "-t", "20", "-m", "immediate", "stop") })
	cluster := &Cluster{socket: directory, owner: owner}
	cluster.pool = Connect(t, cluster.DSN("postgres", owner))
	return cluster
}

// DSN 构造测试集群连接信息，不包含口令且只能通过私有 Unix 套接字访问。
func (cluster *Cluster) DSN(database, role string) string {
	address := url.URL{Scheme: "postgres", User: url.User(role), Path: "/" + database}
	query := url.Values{"host": {cluster.socket}, "sslmode": {"disable"}}
	address.RawQuery = query.Encode()
	return address.String()
}

// Database 为子测试创建全新数据库，返回其迁移连接和库名；不复用应用数据。
func (cluster *Cluster) Database(t testing.TB) (*sql.DB, string) {
	t.Helper()
	name := "test_" + strings.ToLower(ids.New())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := cluster.pool.ExecContext(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal("创建独立测试数据库失败", err)
	}
	return Connect(t, cluster.DSN(name, cluster.owner)), name
}

// Login 创建仅继承指定权限组的测试登录角色，便于验证真实数据库授权。
func (cluster *Cluster) Login(t testing.TB, groups ...string) string {
	t.Helper()
	name := "login_" + strings.ToLower(ids.New())
	statement := "CREATE ROLE " + pgx.Identifier{name}.Sanitize() + " LOGIN"
	if len(groups) > 0 {
		quoted := make([]string, len(groups))
		for index, group := range groups {
			quoted[index] = pgx.Identifier{group}.Sanitize()
		}
		statement += " IN ROLE " + strings.Join(quoted, ",")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := cluster.pool.ExecContext(ctx, statement); err != nil {
		t.Fatal("创建测试登录角色失败", err)
	}
	return name
}

// Connect 建立测试专用连接并登记清理；连接失败立即终止当前测试。
func Connect(t testing.TB, dsn string) *sql.DB {
	t.Helper()
	configuration, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("解析测试连接失败")
	}
	configuration.ConnectTimeout = 5 * time.Second
	configuration.RuntimeParams["statement_timeout"] = "10000"
	pool := stdlib.OpenDB(*configuration)
	pool.SetMaxOpenConns(4)
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error("关闭测试数据库连接失败", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.PingContext(ctx); err != nil {
		t.Fatal("连接测试数据库失败")
	}
	return pool
}
