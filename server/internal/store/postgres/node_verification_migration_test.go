package postgres

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestNodeVerificationForwardMigration(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, _ := cluster.Database(t)
	previous := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0046_" {
			continue
		}
		encoded, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		previous["migrations/"+entry.Name()] = &fstest.MapFile{Data: encoded}
	}
	if err = migrate(t.Context(), pool, previous); err != nil {
		t.Fatal(err)
	}
	node, draining := ids.New(), ids.New()
	for id, status := range map[string]string{node: "ready", draining: "draining"} {
		if _, err = pool.Exec(`INSERT INTO tombot.computer_nodes(id,name,status,capacity,reserved,runtime_id,control_sequence,last_seen_at,control_expires_at) VALUES($1,$1,$2,10,2,$1,1,clock_timestamp(),clock_timestamp()+interval '5 minutes')`, id, status); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err = Migrate(t.Context(), pool); err != nil {
			t.Fatal(err)
		}
	}
	for id, expected := range map[string]string{node: "bootstrapping", draining: "draining"} {
		var status, runtime string
		var reserved int
		var image, expires any
		if err = pool.QueryRow(`SELECT status,reserved,runtime_id,verified_image_id,capability_expires_at FROM tombot.computer_nodes WHERE id=$1`, id).Scan(&status, &reserved, &runtime, &image, &expires); err != nil {
			t.Fatal(err)
		}
		if status != expected || reserved != 2 || runtime != id || image != nil || expires != nil {
			t.Fatal("前向迁移误造能力证据或影响既有分配")
		}
	}
	var count int
	if err = pool.QueryRow(`SELECT count(*) FROM tombot.node_verifications`).Scan(&count); err != nil || count != 0 {
		t.Fatal("迁移创建了虚假验收", err)
	}
	if _, err = pool.Exec(`UPDATE tombot.computer_nodes SET status='ready' WHERE id=$1`, node); err == nil {
		t.Fatal("没有能力证据也可直接就绪")
	}
}
