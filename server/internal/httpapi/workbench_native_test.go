package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net"
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

	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

// TestNativeWorkbenchSetup 用临时数据库和工作区提供真实 API。只把环回地址传入 Flutter 构建。
func TestNativeWorkbenchSetup(t *testing.T) {
	if os.Getenv("TOM_TEST_NATIVE_WORKBENCH") != "1" {
		t.Skip("未启用原生工作台验收")
	}
	target := os.Getenv("TOM_TEST_WORKBENCH_DEVICE")
	if target == "" {
		target = "macos"
	}
	platform := "ios"
	if target == "macos" {
		platform = "macos"
	}
	if strings.HasPrefix(target, "emulator-") {
		platform = "android"
	}
	if platform != "android" && runtime.GOOS != "darwin" {
		t.Fatal("本用例需要 macOS 和 Flutter 原生构建环境")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	var probes atomic.Int32
	var discoveries atomic.Int32
	apiKey := testkit.Random(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+apiKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == "GET" && r.URL.Path == "/v1/models" {
			discoveries.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if _, err := io.WriteString(w, `{"object":"list","data":[{"id":"deepseek-fixture","object":"model","name":"DeepSeek 验收模型","context_window":8192,"max_output_tokens":1024,"input_modalities":["text"]}]}`); err != nil {
				t.Error(err)
			}
			return
		}
		if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		probes.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	server, code := fixtureWithBrowser(t, true, nil, service.CatalogOptions{DeepSeekBaseURL: upstream.URL + "/v1"})
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	if err := server.Bots.EnsureDefault(ctx); err != nil {
		t.Fatal(err)
	}
	session, err := server.Auth.Login(ctx, password, "工作台验收", platform, nil)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	var issued atomic.Bool
	mux.HandleFunc("POST /fixture/session", func(w http.ResponseWriter, r *http.Request) {
		if !issued.CompareAndSwap(false, true) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"device_token": session.DeviceToken, "api_key": apiKey}); err != nil {
			t.Error(err)
		}
	})
	mux.Handle("/", server.Handler())
	api := httptest.NewServer(mux)
	defer api.Close()
	if platform == "android" {
		forwardEmulatorPort(t, ctx, target, api.Listener.Addr())
	}
	command := exec.CommandContext(ctx, "flutter", "test", "integration_test/workbench_setup_test.dart", "-d", target, "--dart-define=TOM_TEST_WORKBENCH_URL="+api.URL)
	command.Dir = filepath.Join("..", "..", "client")
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		t.Fatal("原生工作台验收失败", err)
	}
	if probes.Load() != 2 {
		t.Fatal("供应商连通性与默认助手聊天调用次数错误", probes.Load())
	}
	if discoveries.Load() != 1 {
		t.Fatal("供应商未通过真实模型发现接口接入", discoveries.Load())
	}
	snapshot, err := server.Conversations.Snapshot(ctx, "tom-direct")
	if err != nil || snapshot.ActiveTurnID != nil {
		t.Fatal("默认助手聊天没有完成", err)
	}
	userMessage, botReply := false, false
	for _, message := range snapshot.Messages {
		if len(message.Blocks) == 0 {
			continue
		}
		if message.SenderKind == "user" && message.Blocks[0].Text == "验证默认助手模型绑定" {
			userMessage = true
		}
		if message.SenderKind == "bot" && message.SenderBotID != nil && *message.SenderBotID == "tom" && message.Status == "complete" && message.Blocks[0].Text == "OK" {
			botReply = true
		}
	}
	if !userMessage || !botReply {
		t.Fatal("默认助手未通过原生界面完成真实模型对话")
	}
	bots, err := server.Bots.List(ctx, "", 10, false)
	if err != nil || len(bots) != 4 {
		t.Fatal("原生界面缺少默认助手或三个新建 Bot", err)
	}
	hues := map[int]bool{}
	directories := map[string]bool{}
	for _, bot := range bots {
		if bot.ModelID == nil || bot.ConversationID == "" {
			t.Fatal("缺少默认模型或主会话")
		}
		hues[bot.AccentHue] = true
		directories[bot.WorkspaceDir] = true
		for _, part := range []string{"inbox", "out", "notes", ".tom/memory"} {
			stat, err := os.Stat(filepath.Join(server.Config.WorkspaceDir, bot.WorkspaceDir, part))
			if err != nil || !stat.IsDir() {
				t.Fatal("Bot 工作区缺少目录", part, err)
			}
		}
	}
	if len(hues) != 4 || len(directories) != 4 {
		t.Fatal("身份色或工作区重复")
	}
}

// forwardEmulatorPort 仅暴露当前用例的环回端口，并在验收结束后撤销该条转发。
func forwardEmulatorPort(t *testing.T, ctx context.Context, target string, address net.Addr) {
	t.Helper()
	_, port, err := net.SplitHostPort(address.String())
	if err != nil {
		t.Fatal(err)
	}
	socket := "tcp:" + port
	if err := exec.CommandContext(ctx, "adb", "-s", target, "reverse", socket, socket).Run(); err != nil {
		t.Fatal("无法连接测试模拟器", err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := exec.CommandContext(cleanup, "adb", "-s", target, "reverse", "--remove", socket).Run(); err != nil {
			t.Error("撤销模拟器端口转发失败", err)
		}
	})
}
