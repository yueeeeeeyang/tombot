package clientrelease

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/noderelease"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestPlatformRelease(t *testing.T) {
	ctx := t.Context()
	cluster := pgtest.Start(t)
	pool, name := cluster.Database(t)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	db, err := postgres.Open(ctx, postgres.Options{DSN: cluster.DSN(name, cluster.Login(t, string(postgres.ControlRole))), Role: postgres.ControlRole, MaxConnections: 3, ConnectTimeout: 5 * time.Second, StatementTimeout: 5 * time.Second, LockTimeout: time.Second, TransactionTimeout: 10 * time.Second, ConnectionLifetime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	actor := ids.New()
	if _, err = pool.Exec(`INSERT INTO tombot.administrators(id,username,display_name,status) VALUES($1,'update-tester','发布测试','active')`, actor); err != nil {
		t.Fatal(err)
	}
	a := api{Service{db, t.TempDir()}, slog.New(slog.NewTextHandler(io.Discard, nil)), make(chan struct{}, 1)}
	chunk := []byte("安装包分片")
	digest := noderelease.Digest(chunk)
	m := manifest{details{"darwin-arm64", "1.10.0", "改进连接恢复", ".dmg", int64(len(chunk))}, []string{digest}}
	publish := func(m manifest) (string, error) { return a.publish(ctx, m, actor, ids.New()) }
	if _, err := publish(m); !errors.Is(err, errIncomplete) {
		t.Fatal("缺失分片仍可发布", err)
	}
	r := httptest.NewRequest("PUT", adminPath+"/parts/"+digest, bytes.NewReader(chunk))
	r.Header.Set("Content-Type", "application/octet-stream")
	w := httptest.NewRecorder()
	a.admin(w, r, identity.Attempt{RequestID: ids.New()}, actor)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := a.savePart(digest, []byte("错误内容")); !errors.Is(err, errInput) {
		t.Fatal("篡改分片被接受", err)
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", adminPath, bytes.NewReader(encoded))
	r.Header.Set("Content-Type", "application/json")
	a.admin(w, r, identity.Attempt{RequestID: ids.New()}, actor)
	var published struct {
		ID string `json:"id"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &published) != nil || published.ID == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := publish(m); !errors.Is(err, errDuplicate) {
		t.Fatal("覆盖已有版本", err)
	}
	older := m
	older.Version = "1.9.9"
	if _, err := publish(older); err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct{ platform, version, want string }{
		{"darwin-arm64", "1.9.9", "1.10.0"}, {"darwin-arm64", "1.10.0", ""}, {"darwin-arm64", "2.0.0", ""}, {"darwin-x64", "0.1.0", ""}, {"win32-arm64", "0.1.0", ""},
	} {
		result, err := a.latest(ctx, p.platform, p.version)
		if err != nil || (result == nil) != (p.want == "") || result != nil && (result.Version != p.want || result.Notes != m.Notes || result.ID != published.ID) {
			t.Fatalf("版本选择错误：%+v %+v %v", p, result, err)
		}
	}
	releases, err := a.list(ctx)
	if err != nil || len(releases) != 1 || releases[0] != m.details {
		t.Fatal(releases, err)
	}
	for _, p := range []struct {
		id, index string
		status    int
	}{{published.ID, "0", 200}, {published.ID, "1", 404}, {ids.New(), "0", 404}, {published.ID, "-1", 400}} {
		r = httptest.NewRequest("GET", publicPath, nil)
		r.SetPathValue("id", p.id)
		r.SetPathValue("index", p.index)
		w = httptest.NewRecorder()
		a.part(w, r)
		if w.Code != p.status || w.Code == 200 && !bytes.Equal(w.Body.Bytes(), chunk) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, change := range []func(*manifest){func(m *manifest) { m.Version = "01.1.1" }, func(m *manifest) { m.Extension = ".exe" }, func(m *manifest) { m.Platform = "../" }, func(m *manifest) { m.Notes = "  " }, func(m *manifest) { m.Notes = strings.Repeat("多", maxNotes+1) }, func(m *manifest) { m.Size = maxSize + 1 }, func(m *manifest) { m.Chunks = []string{"../"} }} {
		invalid := m
		change(&invalid)
		if _, err := publish(invalid); !errors.Is(err, errInput) {
			t.Fatal("非法发布未被拒绝", err)
		}
	}
}
