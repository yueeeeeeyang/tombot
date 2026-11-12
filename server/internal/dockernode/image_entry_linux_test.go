//go:build linux && imageintegration

package dockernode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/infra/ids"
)

type imageEntryFixture struct {
	manifest  ImageManifest
	path      string
	exists    atomic.Bool
	pulls     atomic.Int32
	inspects  atomic.Int32
	mode      atomic.Int32
	cancelled chan struct{}
}

func newImageEntryFixture(t *testing.T) *imageEntryFixture {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatal("镜像入口集成测试要求专用 Linux root 容器")
	}
	directory := filepath.Join("/run", "tombot-image-test-"+ids.New())
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	manifest := imageManifestFixture()
	manifest.StateDirectory = filepath.Join(directory, "state")
	manifest.Socket = filepath.Join(directory, "docker.sock")
	if err := os.Mkdir(manifest.StateDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	f := &imageEntryFixture{manifest: manifest, path: filepath.Join(directory, "manifest.json"), cancelled: make(chan struct{})}
	f.writeManifest(t)
	listener, err := net.Listen("unix", manifest.Socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(manifest.Socket, 0660); err != nil {
		t.Fatal(err)
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1.47/images/"+manifest.Reference+"/json":
			f.inspects.Add(1)
			if !f.exists.Load() {
				w.WriteHeader(404)
				return
			}
			if err := json.NewEncoder(w).Encode(testImage(manifest.Reference, manifest.Platform)); err != nil {
				t.Error(err)
			}
		case r.Method == "POST" && r.URL.Path == "/v1.47/images/create":
			f.pulls.Add(1)
			if r.URL.Query().Get("fromImage") != manifest.Reference || r.URL.Query().Get("platform") != manifest.Platform || r.Header.Get("X-Registry-Auth") != "" {
				t.Error("固定拉取参数或认证边界不符")
				w.WriteHeader(400)
				return
			}
			if f.mode.Load() == 1 {
				w.Header().Set("Content-Type", "application/json")
				if _, err := io.WriteString(w, "{\"status\":\"pulling\"}\n"); err != nil {
					t.Error(err)
					return
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(f.cancelled)
				return
			}
			f.exists.Store(true)
			if _, err := io.WriteString(w, "{\"status\":\"complete\"}\n"); err != nil {
				t.Error(err)
			}
		default:
			t.Error("镜像准备调用了额外引擎操作", r.Method, r.URL.Path)
			w.WriteHeader(400)
		}
	})}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	})
	return f
}
func (f *imageEntryFixture) writeManifest(t *testing.T) {
	t.Helper()
	encoded, err := json.Marshal(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
}
func imageEntryLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestLinuxImagePreparationEntry 使用真正的受保护文件、文件锁及 Unix HTTP，Docker 内容由明确替身提供。
func TestLinuxImagePreparationEntry(t *testing.T) {
	f := newImageEntryFixture(t)
	results := make(chan error, 8)
	for range 8 {
		go func() {
			report, err := PrepareImage(t.Context(), f.path, imageEntryLogger())
			if err == nil && (report.State != "available" || report.Scope != "image") {
				err = errors.New("回执范围错误")
			}
			results <- err
		}()
	}
	for range 8 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if f.pulls.Load() != 1 || f.inspects.Load() != 9 {
		t.Fatal("并发准备未串行核验或重复拉取", f.pulls.Load(), f.inspects.Load())
	}
	f.exists.Store(false)
	if _, err := PrepareImage(t.Context(), f.path, imageEntryLogger()); err != nil || f.pulls.Load() != 2 {
		t.Fatal("旧记录掩盖本地镜像缺失", err)
	}
}

func TestLinuxImagePreparationCancellation(t *testing.T) {
	f := newImageEntryFixture(t)
	f.mode.Store(1)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if report, err := PrepareImage(ctx, f.path, imageEntryLogger()); !errors.Is(err, context.DeadlineExceeded) || report != (ImagePreparation{}) {
		t.Fatal("取消下载仍成功", report, err)
	}
	select {
	case <-f.cancelled:
	case <-time.After(time.Second):
		t.Fatal("下载连接未随取消关闭")
	}
	journal, err := openJournal(f.manifest.StateDirectory, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := journal.root.Close(); err != nil {
			t.Error(err)
		}
	}()
	state, err := (imageJournal{journal}).load(f.manifest)
	if err != nil || state == nil || state.State != "failed" {
		t.Fatal("取消结果未持久记录", state, err)
	}
	if f.pulls.Load() != 1 {
		t.Fatal("取消后补发下载")
	}
}

func TestLinuxImagePreparationProtectedFiles(t *testing.T) {
	for _, kind := range []string{"mode", "link", "parent", "relative", "platform", "record", "node", "socket_permissions"} {
		t.Run(kind, func(t *testing.T) {
			f := newImageEntryFixture(t)
			if kind == "record" || kind == "node" {
				if _, err := PrepareImage(t.Context(), f.path, imageEntryLogger()); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "socket_permissions":
				if err := os.Chmod(f.manifest.Socket, 0777); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(f.path, 0644); err != nil {
					t.Fatal(err)
				}
			case "link":
				if err := os.Rename(f.path, f.path+".retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.path+".retained", f.path); err != nil {
					t.Fatal(err)
				}
			case "parent":
				if err := os.Chmod(filepath.Dir(f.path), 0777); err != nil {
					t.Fatal(err)
				}
			case "relative":
				f.path = "relative.json"
			case "platform":
				if f.manifest.Platform == "linux/arm64" {
					f.manifest.Platform = "linux/amd64"
				} else {
					f.manifest.Platform = "linux/arm64"
				}
				f.writeManifest(t)
			case "record":
				if err := os.WriteFile(filepath.Join(f.manifest.StateDirectory, f.manifest.ImageID+".image.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "node":
				f.manifest.NodeID = ids.New()
				f.writeManifest(t)
			}
			count := f.inspects.Load()
			if report, err := PrepareImage(t.Context(), f.path, imageEntryLogger()); err == nil || report != (ImagePreparation{}) {
				t.Fatal("不安全配置被接受", report, err)
			}
			if f.inspects.Load() != count {
				t.Fatal("配置或记录拒绝前接触 Docker")
			}
		})
	}
}
