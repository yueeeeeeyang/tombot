package deployment

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/noderelease"
)

func TestStageSignedFleetDownload(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := x509.Certificate{SerialNumber: big.NewInt(3), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, &cert, &cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"tomnode", "SHA256SUMS", "computer-image.tar", "node-release.json", "node-install.example.json", "安装说明.txt"} {
		if err = tw.WriteHeader(&tar.Header{Name: name, Size: 1, Mode: 0600}); err != nil {
			t.Fatal(err)
		}
		if _, err = tw.Write([]byte{7}); err != nil {
			t.Fatal(err)
		}
	}
	if err = tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = gz.Close(); err != nil {
		t.Fatal(err)
	}
	for _, corrupt := range []bool{false, true} {
		a := noderelease.Assignment{Job: ids.New()}
		m := noderelease.Manifest{Chunks: []string{noderelease.Digest(archive.Bytes())}, Size: int64(archive.Len())}
		h := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var sc noderelease.SignedCall
			if json.NewDecoder(r.Body).Decode(&sc) != nil || !sc.Valid(time.Now()) || sc.Call.Action != "part" || sc.Call.Job != a.Job || sc.Call.Index != 0 {
				t.Error("分片请求未绑定节点及任务")
				w.WriteHeader(400)
				return
			}
			b := bytes.Clone(archive.Bytes())
			if corrupt {
				b[0] ^= 1
			}
			if _, err := w.Write(b); err != nil {
				t.Error(err)
			}
		}))
		t.Cleanup(h.Close)
		c := updateClient{h.Client(), h.URL, ids.New(), "linux/amd64", tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}}
		parent := t.TempDir()
		dest := filepath.Join(parent, "release")
		if err := c.stage(t.Context(), dest, a, m); (err != nil) != corrupt {
			t.Fatal(corrupt, err)
		}
		files, err := os.ReadDir(parent)
		if err != nil || corrupt && len(files) != 0 || !corrupt && len(files) != 1 {
			t.Fatal("中断残留或目标缺失", files, err)
		}
	}
}
