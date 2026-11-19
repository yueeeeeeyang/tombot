package postgres

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"io.yueyang/tom/internal/noderelease"
	"io.yueyang/tom/internal/testkit/pgtest"
)

// 历史清单的空白和字段顺序由旧发布器决定，迁移按分片顺序识别原包并保留发布 ID。
func TestNodeReleaseCatalogMigration(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, _ := cluster.Database(t)
	before := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0083_" {
			break
		}
		name := "migrations/" + entry.Name()
		b, err := migrationFiles.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		before[name] = &fstest.MapFile{Data: b}
	}
	if err = migrate(t.Context(), pool, before); err != nil {
		t.Fatal(err)
	}
	chunks := []string{noderelease.Digest([]byte("第一片")), noderelease.Digest([]byte("第二片"))}
	payload, err := json.Marshal(map[string]any{"chunks": chunks})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string][]byte{"payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	id := noderelease.Digest(payload)
	if _, err = pool.Exec(`INSERT INTO tombot.node_releases(id,label,platform,envelope) VALUES ($1,'test','linux/amd64',$2)`, id, envelope); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	var artifact string
	if err = pool.QueryRow(`SELECT artifact FROM tombot.node_releases WHERE id=$1`, id).Scan(&artifact); err != nil || artifact != noderelease.Digest([]byte(strings.Join(chunks, "\n"))) {
		t.Fatal(artifact, err)
	}
}
