package postgres

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestComputerCatalogMigration(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, _ := cluster.Database(t)
	previous := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0044_" {
			continue
		}
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		previous["migrations/"+entry.Name()] = &fstest.MapFile{Data: content}
	}
	if err := migrate(t.Context(), pool, previous); err != nil {
		t.Fatal(err)
	}
	nodeID, imageID := ids.New(), ids.New()
	reference := "registry.example.test/computer@sha256:" + strings.Repeat("a", 64)
	if _, err := pool.Exec(`INSERT INTO tombot.computer_nodes(id,name,endpoint,status,capacity,reserved,generation,last_seen_at)
		VALUES($1,'已有节点','https://unused.example.test','draining',10,2,4,clock_timestamp())`, nodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO tombot.computer_images(id,reference,is_enabled) VALUES($1,$2,false)`, imageID, reference); err != nil {
		t.Fatal(err)
	}
	var seen, created time.Time
	if err := pool.QueryRow(`SELECT last_seen_at,created_at FROM tombot.computer_nodes WHERE id=$1`, nodeID).Scan(&seen, &created); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Migrate(t.Context(), pool); err != nil {
			t.Fatal("目录前向迁移或重复校验失败", err)
		}
	}
	var nodeName, status, imageReference string
	var capacity, reserved, generation int
	var afterSeen, afterCreated time.Time
	var enabled bool
	if err := pool.QueryRow(`SELECT n.name,n.status,n.capacity,n.reserved,n.generation,n.last_seen_at,n.created_at,i.reference,i.is_enabled
		FROM tombot.computer_nodes n CROSS JOIN tombot.computer_images i WHERE n.id=$1 AND i.id=$2`, nodeID, imageID).
		Scan(&nodeName, &status, &capacity, &reserved, &generation, &afterSeen, &afterCreated, &imageReference, &enabled); err != nil {
		t.Fatal(err)
	}
	if nodeName != "已有节点" || status != "draining" || capacity != 10 || reserved != 2 || generation != 4 || !seen.Equal(afterSeen) || !created.Equal(afterCreated) || imageReference != reference || enabled {
		t.Fatal("目录迁移改变既有资源身份或运行状态")
	}
	var endpointExists bool
	if err := pool.QueryRow(`SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='tombot' AND table_name='computer_nodes' AND column_name='endpoint')`).Scan(&endpointExists); err != nil || endpointExists {
		t.Fatal("未删除旧端点字段", err)
	}
}
