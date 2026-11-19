package nodefleet

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	"os"

	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/noderelease"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestSinglePackageFleetDistribution(t *testing.T) {
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
	s := Service{db, t.TempDir()}
	admin := ids.New()
	part := standardArchive(t, "test", "")
	artifact := noderelease.Digest([]byte(noderelease.Digest(part)))
	upload := func() {
		t.Helper()
		if _, err := s.begin(admin, artifact, int64(len(part))); err != nil {
			t.Fatal(err)
		}
		if err := s.savePart(admin, artifact, "0", noderelease.Digest(part), part); err != nil {
			t.Fatal(err)
		}
	}
	upload()
	r, err := s.publish(ctx, admin, artifact)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := s.publish(ctx, admin, artifact); err != nil || again != r {
		t.Fatal("已确认的发布不能重取", again, err)
	}
	upload()
	if again, err := s.publish(ctx, admin, artifact); err != nil || again != r {
		t.Fatal("重复上传没有复用版本", again, err)
	}
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
	if err := os.Rename(s.archivePath(artifact), s.archivePath(artifact)+".saved"); err != nil {
		t.Fatal(err)
	}
	if err = s.dispatch(ctx, r.ID, []string{n}, false, admin, ids.New()); !errors.Is(err, ErrConflict) {
		t.Fatal("归档缺失仍下发", err)
	}
	if err := os.Rename(s.archivePath(artifact)+".saved", s.archivePath(artifact)); err != nil {
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
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &a) != nil || a.Release != r.ID || a.Artifact != artifact || a.Manifest.Validate(artifact) != nil || a.Manifest.Label != "test" {
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

// standardArchive 使用与标准打包一致的六个成员；可单独改变归档正文以检验接收端是否重算摘要。
func standardArchive(t *testing.T, label, changed string) []byte {
	t.Helper()
	image := []byte("节点镜像")
	header := make([]byte, 64)
	copy(header, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	header[16] = 2
	header[18] = 62
	profile, err := json.Marshal(noderelease.Profile{Version: 1, Platform: "linux/amd64", Label: label, ImageReference: "example.test/computer@sha256:" + noderelease.Digest(image), ArchiveSHA256: noderelease.Digest(image), Docker: json.RawMessage(`{"version":2,"timeout_seconds":300,"minimum_free_bytes":1073741824}`)})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"node-release.json": profile, "tomnode": header, "computer-image.tar": image, "安装说明.txt": []byte("说明"), "node-install.example.json": []byte("{}")}
	var sums strings.Builder
	for name, body := range files {
		sums.WriteString(noderelease.Digest(body) + "  " + name + "\n")
	}
	files["SHA256SUMS"] = []byte(sums.String())
	if changed != "" {
		files[changed] = []byte("内容已改变")
	}
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(body)), Mode: 0644}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(tw.Close(), gz.Close()); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestUploadValidationAndResume(t *testing.T) {
	s := Service{Directory: t.TempDir()}
	owner := ids.New()
	first := bytes.Repeat([]byte{3}, noderelease.ChunkSize)
	last := []byte{7}
	hash := noderelease.Digest([]byte(noderelease.Digest(first) + "\n" + noderelease.Digest(last)))
	size := int64(len(first) + len(last))
	if _, err := s.begin(owner, "../escape", size); !errors.Is(err, ErrInput) {
		t.Fatal(err)
	}
	if _, err := s.begin(owner, hash, noderelease.MaxSize+1); !errors.Is(err, ErrInput) {
		t.Fatal(err)
	}
	if _, err := s.begin(owner, hash, size); err != nil {
		t.Fatal(err)
	}
	if err := s.savePart(owner, hash, "1", noderelease.Digest(last), last); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err := s.savePart(owner, hash, "0", noderelease.Digest(last), first); !errors.Is(err, ErrInput) {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.savePart(owner, hash, "0", noderelease.Digest(first), first); err != nil {
			t.Fatal(err)
		}
	}
	if next, err := s.begin(owner, hash, size); err != nil || next.Next != 1 {
		t.Fatal(next, err)
	}
	if _, err := s.publish(t.Context(), owner, hash); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err := s.savePart(ids.New(), hash, "1", noderelease.Digest(last), last); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	for _, changed := range []string{"tomnode", "computer-image.tar", "SHA256SUMS"} {
		b := standardArchive(t, "v1", changed)
		d := noderelease.Digest(b)
		artifact := noderelease.Digest([]byte(d))
		if _, err := s.begin(owner, artifact, int64(len(b))); err != nil {
			t.Fatal(err)
		}
		if err := s.savePart(owner, artifact, "0", d, b); err != nil {
			t.Fatal(err)
		}
		if _, err := s.publish(t.Context(), owner, artifact); !errors.Is(err, noderelease.ErrPackage) {
			t.Fatal("错误归档被登记", changed, err)
		}
		if next, err := s.begin(owner, artifact, int64(len(b))); err != nil || next.Next != 0 {
			t.Fatal("损坏暂存不能重传", err)
		}
		if _, err := os.Stat(s.archivePath(artifact)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("验证失败仍发布", err)
		}
	}
}
