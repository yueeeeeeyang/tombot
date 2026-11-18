package nodefleet

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/noderelease"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestSignedFleetDistribution(t *testing.T) {
	ctx := t.Context()
	cluster := pgtest.Start(t)
	pool, name := cluster.Database(t)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	db, err := postgres.Open(ctx, postgres.Options{DSN: cluster.DSN(name, cluster.Login(t, string(postgres.ControlRole))), Role: postgres.ControlRole, MaxConnections: 3, ConnectTimeout: time.Second * 5, StatementTimeout: time.Second * 5, LockTimeout: time.Second * 3, TransactionTimeout: time.Second * 10, ConnectionLifetime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := Service{db, t.TempDir(), func() (ed25519.PublicKey, error) { return pub, nil }}
	part := []byte("完整性测试分片")
	encoded, err := json.Marshal(noderelease.Manifest{Checksums: noderelease.Digest(part), Version: 1, Label: "test", Platform: "linux/amd64", Size: int64(len(part)), Chunks: []string{noderelease.Digest(part)}})
	if err != nil {
		t.Fatal(err)
	}
	e := noderelease.Envelope{Payload: encoded, Signature: ed25519.Sign(priv, encoded)}
	bad := e
	bad.Payload = append(bytes.Clone(encoded), ' ')
	if _, err = s.publish(ctx, bad); !errors.Is(err, ErrInput) {
		t.Fatal("篡改清单获准")
	}
	r, err := s.publish(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	admin := ids.New()
	n, image := ids.New(), ids.New()
	secondNode := ids.New()
	np, nk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, np, nk)
	if err != nil {
		t.Fatal(err)
	}
	pair := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: nk}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO tombot.administrators(id,username,display_name,status) VALUES($1,'fleet-test','测试管理员','active')`, []any{admin}},
		{`INSERT INTO tombot.computer_nodes(id,name,status,capacity) VALUES($1,'测试节点','bootstrapping',2)`, []any{n}},
		{`INSERT INTO tombot.computer_nodes(id,name,status,capacity) VALUES($1,'第二个测试节点','bootstrapping',2)`, []any{secondNode}},
		{`INSERT INTO tombot.computer_images(id,reference,is_enabled) VALUES($1,$2,true)`, []any{image, "example.test/computer@sha256:" + strings.Repeat("d", 64)}},
		{`INSERT INTO tombot.node_connections(node_id,url,certificate_fingerprint,image_id) VALUES($1,'https://node.example.test',$2,$3)`, []any{n, noderelease.Digest(der), image}},
		{`INSERT INTO tombot.node_connections(node_id,url,certificate_fingerprint,image_id) VALUES($1,'https://other-node.example.test',$2,$3)`, []any{secondNode, noderelease.Digest([]byte(secondNode)), image}},
	} {
		if _, err := pool.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	h := s.Agent(slog.New(slog.NewTextHandler(io.Discard, nil)))
	call := func(c noderelease.Call) *httptest.ResponseRecorder {
		t.Helper()
		c.Platform = "linux/amd64"
		req, err := c.Sign(pair)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequestWithContext(context.Background(), "POST", noderelease.AgentPath, bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, request)
		return w
	}
	if w := call(noderelease.Call{Node: n, Action: "poll"}); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if err = s.dispatch(ctx, r.ID, []string{n}, false, admin, ids.New()); !errors.Is(err, ErrConflict) {
		t.Fatal("分片缺失仍下发", err)
	}
	if err = s.savePart(ctx, r.ID, "0", []byte("坏")); !errors.Is(err, ErrInput) {
		t.Fatal("篡改分片被接受", err)
	}
	if err = s.savePart(ctx, r.ID, "0", part); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(`UPDATE tombot.computer_nodes SET reserved=1 WHERE id=$1`, n); err != nil {
		t.Fatal(err)
	}
	if err = s.dispatch(ctx, r.ID, []string{n}, false, admin, ids.New()); err != nil {
		t.Fatal(err)
	}
	w := call(noderelease.Call{Node: n, Action: "poll"})
	var a noderelease.Assignment
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &a) != nil || a.Release != r.ID {
		t.Fatal(w.Code, w.Body.String())
	}
	if err = s.dispatch(ctx, r.ID, []string{n}, false, admin, ids.New()); !errors.Is(err, ErrConflict) {
		t.Fatal("覆盖在途任务", err)
	}
	if w := call(noderelease.Call{Node: ids.New(), Action: "part", Job: a.Job}); w.Code != 403 {
		t.Fatal("跨节点下载", w.Code)
	}
	if w := call(noderelease.Call{Node: n, Action: "part", Job: a.Job}); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), part) {
		t.Fatal(w.Code)
	}
	for range 2 {
		if w := call(noderelease.Call{Node: n, Action: "report", Job: a.Job, State: "started"}); w.Code != 204 {
			t.Fatal(w.Code)
		}
	}
	status, err := s.status(ctx)
	if err != nil || len(status.Jobs) != 1 || status.Jobs[0].State != "started" {
		t.Fatal(status, err)
	}
	if err = s.dispatch(ctx, r.ID, nil, true, admin, ids.New()); err != nil {
		t.Fatal(err)
	}
	status, err = s.status(ctx)
	if err != nil || len(status.Jobs) != 1 || status.Jobs[0].State != "queued" {
		t.Fatal(status, err)
	}

	t.Run("目标节点已有任务时整批回滚", func(t *testing.T) {
		ctx := t.Context()
		if _, err := pool.ExecContext(ctx, `INSERT INTO tombot.node_update_agents(node_id,platform) VALUES($1,'linux/amd64')`, secondNode); err != nil {
			t.Fatal(err)
		}
		requestID := ids.New()
		if err := s.dispatch(ctx, r.ID, []string{n, secondNode}, false, admin, requestID); !errors.Is(err, ErrConflict) {
			t.Errorf("部分下发未报告冲突：%v", err)
		}
		var newJobs, auditEvents int
		if err := pool.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM tombot.node_update_jobs WHERE node_id=$1),
			(SELECT count(*) FROM tombot.admin_audit_events WHERE request_id=$2)`, secondNode, requestID).Scan(&newJobs, &auditEvents); err != nil {
			t.Fatal(err)
		}
		if newJobs != 0 || auditEvents != 0 {
			t.Fatalf("冲突后仍有 %d 条新任务和 %d 条审计记录", newJobs, auditEvents)
		}
	})
}
