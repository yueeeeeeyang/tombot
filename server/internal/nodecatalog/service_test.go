package nodecatalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func registrationFixture() Registration {
	return Registration{Name: "生产节点", Capacity: 20, ImageID: ids.New(), ImageReference: "registry.example.test/computer@sha256:" + strings.Repeat("a", 64)}
}

func TestRegistrationValidation(t *testing.T) {
	registration := registrationFixture()
	if registration.Validate() != nil {
		t.Fatal("合法目录被拒绝")
	}
	for name, change := range map[string]func(*Registration){
		"无镜像身份": func(r *Registration) { r.ImageID = "" },
		"非规范身份": func(r *Registration) { r.ImageID = "0000000000000000000000000a" },
		"浮动镜像":  func(r *Registration) { r.ImageReference = "registry.example.test/computer:latest" },
		"无容量":   func(r *Registration) { r.Capacity = 0 },
		"容量越界":  func(r *Registration) { r.Capacity = 10001 },
		"空名称":   func(r *Registration) { r.Name = "" },
		"边界空格":  func(r *Registration) { r.Name += " " },
		"控制字符":  func(r *Registration) { r.Name += "\n" },
		"过长名称":  func(r *Registration) { r.Name = strings.Repeat("节", 121) },
		"非法编码":  func(r *Registration) { r.Name = string([]byte{255}) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := registration
			change(&changed)
			if !errors.Is(changed.Validate(), ErrInvalidInput) {
				t.Fatal("错误目录字段被接受")
			}
		})
	}
	if _, err := New(nil, ids.New()); err == nil {
		t.Fatal("缺失控制连接仍建立目录服务")
	}
}

func catalogDatabase(t *testing.T, cluster *pgtest.Cluster, name string, role postgres.Role) *postgres.Database {
	t.Helper()
	login := cluster.Login(t, string(role))
	database, err := postgres.Open(t.Context(), postgres.Options{DSN: cluster.DSN(name, login), Role: role, MaxConnections: 8, ConnectTimeout: 5 * time.Second, StatementTimeout: 5 * time.Second, LockTimeout: 3 * time.Second, TransactionTimeout: 10 * time.Second, ConnectionLifetime: time.Minute})
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

func execute(t *testing.T, pool *sql.DB, statement string, arguments ...any) {
	t.Helper()
	if _, err := pool.Exec(statement, arguments...); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentCatalogRegistration(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, name := cluster.Database(t)
	if err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	control := catalogDatabase(t, cluster, name, postgres.ControlRole)
	// 每轮都是首次登记，不能用已存在行的重试掩盖多个唯一索引的首次写入竞争。
	const rounds, requests = 12, 32
	for round := range rounds {
		service, err := New(control, ids.New())
		if err != nil {
			t.Fatal(err)
		}
		registration := registrationFixture()
		registration.Name = fmt.Sprintf("并发登记节点 %d", round)
		registration.ImageReference = fmt.Sprintf("registry.example.test/computer@sha256:%064x", round+1)
		start := make(chan struct{})
		failures := make(chan error, requests)
		var wait sync.WaitGroup
		for range requests {
			wait.Go(func() {
				<-start
				catalog, err := service.Register(t.Context(), registration, ids.New())
				if err == nil && (catalog.Node.ID != service.nodeID || catalog.Image.ID != registration.ImageID || catalog.Node.Status != "bootstrapping") {
					err = errors.New("并发登记返回不同目录或虚假就绪")
				}
				failures <- err
			})
		}
		close(start)
		wait.Wait()
		close(failures)
		for err := range failures {
			if err != nil {
				t.Fatalf("第 %d 轮首次登记失败：%v", round, err)
			}
		}
	}
	var nodes, images, audits int
	if err := pool.QueryRow(`SELECT (SELECT count(*) FROM tombot.computer_nodes),(SELECT count(*) FROM tombot.computer_images),(SELECT count(*) FROM tombot.admin_audit_events)`).Scan(&nodes, &images, &audits); err != nil || nodes != rounds || images != rounds || audits != 2*rounds {
		t.Fatal("并发登记没有保持资源和审计幂等", nodes, images, audits, err)
	}
}

func TestCatalogIntegration(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, name := cluster.Database(t)
	if err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	control := catalogDatabase(t, cluster, name, postgres.ControlRole)
	nodeID := ids.New()
	service, err := New(control, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	registration := registrationFixture()
	if _, err := service.Get(t.Context(), registration.ImageID); !errors.Is(err, ErrNotFound) {
		t.Fatal("未登记目录被自动创建", err)
	}
	if _, err := service.Register(t.Context(), registration, ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("缺少审计请求仍写入目录", err)
	}
	var wait sync.WaitGroup
	failures := make(chan error, 16)
	for range 16 {
		wait.Go(func() {
			catalog, err := service.Register(t.Context(), registration, ids.New())
			if err == nil && (catalog.Node.ID != nodeID || catalog.Node.Status != "bootstrapping" || catalog.Node.Reserved != 0 || catalog.Node.Generation != 1 || catalog.Node.LastSeenAt != nil || catalog.Image.ID != registration.ImageID || !catalog.Image.IsEnabled) {
				err = errors.New("登记误报节点就绪或返回错误身份")
			}
			failures <- err
		})
	}
	wait.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertCounts := func(nodes, images, audits int) {
		t.Helper()
		var actualNodes, actualImages, actualAudits int
		if err := pool.QueryRow(`SELECT (SELECT count(*) FROM tombot.computer_nodes),(SELECT count(*) FROM tombot.computer_images),(SELECT count(*) FROM tombot.admin_audit_events)`).Scan(&actualNodes, &actualImages, &actualAudits); err != nil || nodes != actualNodes || images != actualImages || audits != actualAudits {
			t.Fatal("登记原子性、幂等或审计不符", actualNodes, actualImages, actualAudits, err)
		}
	}
	assertCounts(1, 1, 2)
	t.Run("重复登记不覆盖运行状态或重新启用镜像", func(t *testing.T) {
		execute(t, pool, `UPDATE tombot.computer_nodes SET status='draining',reserved=1,generation=3,last_seen_at=clock_timestamp() WHERE id=$1`, nodeID)
		execute(t, pool, `UPDATE tombot.computer_images SET is_enabled=false WHERE id=$1`, registration.ImageID)
		catalog, err := service.Register(t.Context(), registration, ids.New())
		if err != nil || catalog.Node.Status != "draining" || catalog.Node.Reserved != 1 || catalog.Node.Generation != 3 || catalog.Node.LastSeenAt == nil || catalog.Image.IsEnabled {
			t.Fatal("重复登记覆盖已有状态", err)
		}
		assertCounts(1, 1, 2)
	})
	t.Run("冲突原子回滚", func(t *testing.T) {
		changed := registration
		changed.Name = "另一个名称"
		changed.ImageID = ids.New()
		changed.ImageReference = strings.Replace(registration.ImageReference, "/computer@", "/other@", 1)
		if _, err := service.Register(t.Context(), changed, ids.New()); !errors.Is(err, ErrConflict) {
			t.Fatal("名称冲突仍登记镜像", err)
		}
		changed = registration
		changed.Capacity++
		if _, err := service.Register(t.Context(), changed, ids.New()); !errors.Is(err, ErrConflict) {
			t.Fatal("容量冲突覆盖原目录", err)
		}
		other, err := New(control, ids.New())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := other.Register(t.Context(), registration, ids.New()); !errors.Is(err, ErrConflict) {
			t.Fatal("名称唯一冲突未拒绝", err)
		}
		changed = registration
		changed.Name = "新节点"
		changed.ImageReference = strings.Replace(registration.ImageReference, "/computer@", "/other@", 1)
		if _, err := other.Register(t.Context(), changed, ids.New()); !errors.Is(err, ErrConflict) {
			t.Fatal("镜像引用冲突仍创建节点", err)
		}
		changed.ImageReference = registration.ImageReference
		changed.ImageID = ids.New()
		if _, err := other.Register(t.Context(), changed, ids.New()); !errors.Is(err, ErrConflict) {
			t.Fatal("相同镜像引用登记不同 ID", err)
		}
		assertCounts(1, 1, 2)
	})
	t.Run("审计失败不遗留部分目录且错误脱敏", func(t *testing.T) {
		other, err := New(control, ids.New())
		if err != nil {
			t.Fatal(err)
		}
		changed := registrationFixture()
		changed.Name = "不应输出的目录字段"
		changed.ImageReference = strings.Replace(changed.ImageReference, "/computer@", "/audit@", 1)
		execute(t, pool, `REVOKE INSERT ON tombot.admin_audit_events FROM tombot_control`)
		t.Cleanup(func() { execute(t, pool, `GRANT INSERT ON tombot.admin_audit_events TO tombot_control`) })
		catalog, err := other.Register(t.Context(), changed, ids.New())
		if err == nil || catalog.Node.ID != "" || strings.Contains(err.Error(), changed.Name) || strings.Contains(err.Error(), changed.ImageReference) {
			t.Fatal("审计失败仍返回成功或泄漏目录内容", err)
		}
		assertCounts(1, 1, 2)
	})
	t.Run("取消不改变目录", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := service.Register(ctx, registration, ids.New()); !errors.Is(err, context.Canceled) {
			t.Fatal("取消登记仍执行", err)
		}
		assertCounts(1, 1, 2)
	})
	t.Run("数据库保护固定身份和最小权限", func(t *testing.T) {
		for statement, expected := range map[string]string{
			`UPDATE tombot.computer_nodes SET id=id||'changed'`:                                       "23514",
			`UPDATE tombot.computer_images SET id=id||'changed'`:                                      "23514",
			`UPDATE tombot.computer_images SET reference=replace(reference,'/computer@','/changed@')`: "23514",
			`DELETE FROM tombot.computer_nodes`:                                                       "42501",
			`DELETE FROM tombot.computer_images`:                                                      "42501",
		} {
			err := control.Platform(t.Context(), func(tx *sql.Tx) error { _, err := tx.Exec(statement); return err })
			var state interface{ SQLState() string }
			if !errors.As(err, &state) || state.SQLState() != expected {
				t.Fatal("数据库目录保护没有按预期拒绝", statement, err)
			}
		}
		if err := control.Platform(t.Context(), func(tx *sql.Tx) error {
			if _, err := tx.Exec(`UPDATE tombot.computer_nodes SET status='offline'`); err != nil {
				return err
			}
			_, err := tx.Exec(`UPDATE tombot.computer_images SET is_enabled=true`)
			return err
		}); err != nil {
			t.Fatal("固定身份保护错误阻止合法状态变更", err)
		}
		tenant := catalogDatabase(t, cluster, name, postgres.TenantRole)
		if _, err := New(tenant, nodeID); err == nil {
			t.Fatal("普通用户连接获得部署目录权限")
		}
		if err := tenant.Tenant(t.Context(), ids.New(), func(tx *sql.Tx) error {
			var count int
			return tx.QueryRow(`SELECT count(*) FROM tombot.computer_nodes`).Scan(&count)
		}); err == nil {
			t.Fatal("普通用户可读取部署目录")
		}
		var endpointExists bool
		if err := pool.QueryRow(`SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='tombot' AND table_name='computer_nodes' AND column_name='endpoint')`).Scan(&endpointExists); err != nil || endpointExists {
			t.Fatal("遗留节点网络端点未移除", err)
		}
		assertCounts(1, 1, 2)
	})
}
