package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/testkit"
)

// TestNativeAuditAndLargeDownload 以真实 HTTP 服务和 macOS 客户端验证审计导出、分块写盘与原始字节。
func TestNativeAuditAndLargeDownload(t *testing.T) {
	if os.Getenv("TOM_TEST_NATIVE_AUDIT") != "1" {
		t.Skip("未启用 macOS 审计与下载验收")
	}
	if runtime.GOOS != "darwin" {
		t.Fatal("需要 macOS 原生 Flutter 环境")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	server, code := fixture(t, true)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := server.Auth.Login(ctx, password, "原生审计验收", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 53 {
		if err = server.Audit.BeginExport(ctx, "user"); err != nil {
			t.Fatal(err)
		}
	}
	const size = 65<<20 + 1
	f, err := os.Create(filepath.Join(server.Config.WorkspaceDir, "shared", "large.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteAt([]byte{173}, size-1); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	var issued atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /fixture/session", func(w http.ResponseWriter, r *http.Request) {
		if !issued.CompareAndSwap(false, true) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(session); err != nil {
			t.Error(err)
		}
	})
	mux.Handle("/", server.Handler())
	api := httptest.NewServer(mux)
	defer api.Close()
	args := []string{"test", "integration_test/audit_download_test.dart", "-d", "macos", "--dart-define=TOM_TEST_AUDIT_URL=" + api.URL}
	var output string
	if os.Getenv("TOM_TEST_NATIVE_PICKER") == "1" {
		output, err = os.MkdirTemp("/private/tmp", "tom-picker-")
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.RemoveAll(output); err != nil {
				t.Error(err)
			}
		}()
		args = []string{"build", "macos", "--debug", "--target", "integration_test/audit_download_test.dart", "--dart-define=TOM_TEST_AUDIT_URL=" + api.URL, "--dart-define=TOM_TEST_NATIVE_PICKER=true"}
	}
	command := exec.CommandContext(ctx, "flutter", args...)
	command.Dir = filepath.Join("..", "..", "client")
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		t.Fatal("原生审计与下载验收失败", err)
	}
	if output != "" {
		t.Logf("验收应用已构建，请打开 tom.app，在审计页导出到 %s", output)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				t.Fatal("未在指定目录观察到已完成的审计导出", ctx.Err())
			case <-ticker.C:
				names, err := filepath.Glob(filepath.Join(output, "tom-download-*", "tom-audit.json"))
				if err != nil {
					t.Fatal(err)
				}
				if len(names) == 0 {
					continue
				}
				body, err := os.ReadFile(names[0])
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				var entries []struct{ Action string }
				if err = json.Unmarshal(body, &entries); err != nil || len(entries) < 56 || entries[0].Action != "audit.export" || strings.Contains(string(body), session.DeviceToken) {
					t.Fatal("系统面板导出不完整或未脱敏")
				}
				t.Logf("系统面板已授权，导出文件包含 %d 条完整记录", len(entries))
				return
			}
		}
	}
	if !issued.Load() {
		t.Fatal("原生验收未连接实例")
	}
}
