package httpapi

import (
	"context"
	"encoding/json"
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

	"io.yueyang/tom/internal/domain"
)

// runNativeBotScenario 只传本机地址和业务标识；设备授权由临时环回实例在运行时签发一次。
func runNativeBotScenario(t *testing.T, server *Server, bot domain.Bot, device, scenario string) {
	t.Helper()
	target := os.Getenv("TOM_TEST_BOT_DEVICE")
	if target == "" {
		target = "macos"
	}
	android := strings.HasPrefix(target, "emulator-")
	if !android && (target != "macos" || runtime.GOOS != "darwin") {
		t.Fatal("请选择 macOS 或专用 Android 模拟器")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	var issued atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /fixture/session", func(w http.ResponseWriter, r *http.Request) {
		if !issued.CompareAndSwap(false, true) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(map[string]string{"device_token": device}); err != nil {
			t.Error(err)
		}
	})
	mux.Handle("/", server.Handler())
	host := httptest.NewServer(mux)
	defer host.Close()
	if android {
		forwardEmulatorPort(t, ctx, target, host.Listener.Addr())
	}
	cmd := exec.CommandContext(ctx, "flutter", "test", "integration_test/"+scenario, "-d", target, "--dart-define=TOM_TEST_BOT_SERVER="+host.URL, "--dart-define=TOM_TEST_BOT_ID="+bot.ID)
	cmd.Dir = filepath.Join("..", "..", "client")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatal("原生客户端验收失败", err)
	}
}
