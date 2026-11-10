package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// TestNativeChatAttachment 验证系统面板选择的文件真的经过客户端上传、单聊消息和供应商请求。
func TestNativeChatAttachment(t *testing.T) {
	if os.Getenv("TOM_TEST_NATIVE_ATTACHMENT") != "1" && os.Getenv("TOM_TEST_NATIVE_ATTACHMENT") != "widgets" {
		t.Skip("未启用 macOS 系统附件面板验收")
	}
	if runtime.GOOS != "darwin" {
		t.Fatal("此验收需要 macOS 环境")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	server, setup := fixture(t, true)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, setup); err != nil {
		t.Fatal(err)
	}
	session, err := server.Auth.Login(ctx, password, "原生附件验收", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	const filename = "原生附件验收.txt"
	const content = "从系统选择器读取的原始中文正文。"
	selected := filepath.Join(t.TempDir(), filename)
	if err = os.WriteFile(selected, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 128<<10))
		if err != nil {
			t.Error(err)
			return
		}
		if r.URL.Path != "/v1/chat/completions" || !strings.Contains(string(body), content) || !strings.Contains(string(body), filename) {
			t.Error("供应商未收到所选文件的正文和文件名")
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"已核对原生附件。\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	spec, err := server.Catalog.CreateProvider(ctx, service.ProviderInput{Kind: "openai_compat", DisplayName: "附件验收上游", BaseURL: upstream.URL + "/v1"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = server.Catalog.CreateModel(ctx, service.ModelInput{ProviderID: spec.ID, ModelKey: "fixture", DisplayName: "验收模型", ContextWindow: 8192, MaxOutput: 512, IsDefault: true}, "user"); err != nil {
		t.Fatal(err)
	}
	bot, err := server.Bots.Create(ctx, service.BotInput{Template: "writer"}, "user")
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
		if err := json.NewEncoder(w).Encode(session); err != nil {
			t.Error(err)
		}
	})
	mux.Handle("/", server.Handler())
	api := httptest.NewServer(mux)
	defer api.Close()
	t.Logf("在 macOS 系统文件面板中选择 %s", selected)
	clientDir := filepath.Join("..", "..", "client")
	widgets := os.Getenv("TOM_TEST_NATIVE_ATTACHMENT") == "widgets"
	args := []string{"build", "macos", "--debug", "--target", "integration_test/attachment_test.dart", "--dart-define=TOM_TEST_ATTACHMENT_PANEL=true"}
	if widgets {
		args = []string{"test", "integration_test/attachment_test.dart", "-d", "macos"}
	}
	args = append(args, "--dart-define=TOM_TEST_ATTACHMENT_URL="+api.URL, "--dart-define=TOM_TEST_ATTACHMENT_BOT="+bot.ID)
	command := exec.CommandContext(ctx, "flutter", args...)
	command.Dir = clientDir
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err = command.Run(); err != nil {
		t.Fatal("原生附件应用构建失败", err)
	}
	if !widgets {
		appCtx, stopApp := context.WithCancel(ctx)
		app := exec.CommandContext(appCtx, filepath.Join(clientDir, "build", "macos", "Build", "Products", "Debug", "tom.app", "Contents", "MacOS", "tom"))
		if err = app.Start(); err != nil {
			stopApp()
			t.Fatal("无法打开验收应用", err)
		}
		done := make(chan error, 1)
		go func() { done <- app.Wait(); close(done) }()
		defer func() { stopApp(); <-done }()
		t.Log("验收应用已启动，请选择附件并在聊天输入框发送。")
		for {
			wake := server.Bus.Wake()
			snapshot, err := server.Conversations.Snapshot(ctx, bot.ConversationID)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Messages) >= 2 && snapshot.ActiveTurnID == nil {
				botReply := snapshot.Messages[len(snapshot.Messages)-1]
				if botReply.Status != "complete" || len(botReply.Blocks) != 1 || botReply.Blocks[0].Text != "已核对原生附件。" {
					t.Fatal("原生消息未得到完整模型回答")
				}
				break
			}
			select {
			case err := <-done:
				t.Fatal("验收应用提前退出", err)
			case <-ctx.Done():
				t.Fatal("未在期限内收到系统面板上传的消息", ctx.Err())
			case <-wake:
			}
		}
	}
	if calls.Load() != 1 {
		t.Fatal("实际模型调用次数错误", calls.Load())
	}
	snapshot, err := server.Conversations.Snapshot(ctx, bot.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Messages) != 2 || len(snapshot.Messages[0].Blocks) != 1 || snapshot.Messages[0].Blocks[0].Artifact == nil {
		t.Fatal("只含附件的消息未正确持久化")
	}
	ref := snapshot.Messages[0].Blocks[0].Artifact
	if ref.BotID != bot.ID || ref.Bytes != len(content) || snapshot.Messages[1].Status != "complete" {
		t.Fatal("附件归属、长度或响应状态不符")
	}
}
