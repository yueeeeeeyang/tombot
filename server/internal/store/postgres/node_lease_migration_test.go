package postgres

import (
	"io/fs"
	"testing"
	"testing/fstest"
	"time"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestNodeControlLeaseForwardMigration(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, _ := cluster.Database(t)
	previous := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0045_" {
			continue
		}
		encoded, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		previous["migrations/"+entry.Name()] = &fstest.MapFile{Data: encoded}
	}
	if err := migrate(t.Context(), pool, previous); err != nil {
		t.Fatal(err)
	}
	nodes := map[string]string{ids.New(): "ready", ids.New(): "draining", ids.New(): "bootstrapping"}
	for id, status := range nodes {
		if _, err := pool.Exec(`INSERT INTO tombot.computer_nodes(id,name,status,capacity,reserved,generation,last_seen_at) VALUES($1,$1,$2,10,2,7,clock_timestamp())`, id, status); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := Migrate(t.Context(), pool); err != nil {
			t.Fatal(err)
		}
	}
	for id, before := range nodes {
		var status string
		var capacity, reserved, generation, sequence int
		var runtime, expires any
		var lastSeen time.Time
		if err := pool.QueryRow(`SELECT status,capacity,reserved,generation,runtime_id,control_sequence,control_expires_at,last_seen_at FROM tombot.computer_nodes WHERE id=$1`, id).Scan(&status, &capacity, &reserved, &generation, &runtime, &sequence, &expires, &lastSeen); err != nil {
			t.Fatal(err)
		}
		if before == "ready" {
			before = "bootstrapping"
		}
		if status != before || capacity != 10 || reserved != 2 || generation != 7 || runtime != nil || sequence != 0 || expires != nil || lastSeen.IsZero() {
			t.Fatal("迁移误造心跳或改变已分配资源", status)
		}
	}
}
