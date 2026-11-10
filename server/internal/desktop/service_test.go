package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/auth"
	"io.yueyang/tom/internal/domain"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/testkit"
)

func TestWorkspaceSeedSaveAndImport(t *testing.T) {
	service := newService(t, nil)
	ctx := context.Background()
	first, err := service.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Pristine || len(first.Workspace.Agents) != 8 || first.Workspace.Agents[0].Name != "小黑" || first.Workspace.Agents[0].Avatar != "bombay" {
		t.Fatalf("预设工作空间不正确: %+v", first.Workspace.Agents[0])
	}
	if len(first.Workspace.Agents[0].Messages) != 0 || first.Workspace.Agents[0].ComputerURL != "" {
		t.Fatal("预设猫咪不应带有消息或电脑地址")
	}
	first.Workspace.Agents[0].Draft = "还没发送"
	saved, err := service.Save(ctx, first.Workspace)
	if err != nil || saved.Pristine || saved.Workspace.Agents[0].Draft != "还没发送" {
		t.Fatal("保存失败", err, saved.Pristine)
	}
	again, err := service.Load(ctx)
	if err != nil || again.Workspace.Agents[0].Draft != "还没发送" || again.Pristine {
		t.Fatal("重新读取没有保留草稿", err)
	}
	imported := SeedWorkspace()
	imported.Agents[0].Name = "被拒绝的导入"
	if _, err = service.Import(ctx, imported); err == nil {
		t.Fatal("已有改动的工作空间不能被旧档覆盖")
	}
	fresh := newService(t, nil)
	imported.Agents[0].Name = "从旧档回来"
	loaded, err := fresh.Import(ctx, imported)
	if err != nil || loaded.Workspace.Agents[0].Name != "从旧档回来" || loaded.Pristine {
		t.Fatal("空白服务应接受一次旧档导入", err)
	}
}

func TestRejectsInvalidComputerURLAndDuplicateMessage(t *testing.T) {
	workspace := SeedWorkspace()
	workspace.Agents[0].ComputerURL = "javascript:alert(1)"
	if _, err := ParseWorkspace(mustJSON(t, workspace)); err == nil {
		t.Fatal("应拒绝非 HTTP 地址")
	}
	workspace = SeedWorkspace()
	workspace.Agents[0].ComputerURL = "https://user:secret@desktop.example.test/view"
	if _, err := ParseWorkspace(mustJSON(t, workspace)); err == nil {
		t.Fatal("应拒绝带账号的地址")
	}
	workspace = SeedWorkspace()
	workspace.Agents[0].ComputerURL = "https://desktop.example.test/view"
	workspace.Agents[0].Messages = []ChatMessage{{
		ID: "same", Role: "user", Content: "一句", CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Status: "unsent", Reactions: []Reaction{},
	}}
	workspace.Agents[1].Messages = []ChatMessage{{
		ID: "same", Role: "user", Content: "另一句", CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Status: "unsent", Reactions: []Reaction{},
	}}
	if _, err := ParseWorkspace(mustJSON(t, workspace)); err == nil {
		t.Fatal("应拒绝重复消息标识")
	}
}

func TestDeliverWithoutModelKeepsUnsent(t *testing.T) {
	completer := &fakeCompleter{reply: "不应该被调用"}
	service := newService(t, completer)
	ctx := context.Background()
	snapshot, err := service.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Workspace.Agents[0].Messages = []ChatMessage{{
		ID: "draft-1", Role: "user", Content: "你好", CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Status: "unsent", Reactions: []Reaction{},
	}}
	if _, err = service.Save(ctx, snapshot.Workspace); err != nil {
		t.Fatal(err)
	}
	_, err = service.Deliver(ctx, "assistant", "draft-1")
	var failure *domain.Error
	if !errors.As(err, &failure) || failure.Code != "MODEL_NOT_READY" {
		t.Fatal("未配置模型时应拒绝投递", err)
	}
	if completer.calls != 0 {
		t.Fatal("未配置模型不应访问供应商")
	}
	loaded, err := service.Load(ctx)
	if err != nil || loaded.Workspace.Agents[0].Messages[0].Status != "unsent" {
		t.Fatal("失败前的消息应保持未发送", err)
	}
}

func TestDeliverStoresReplyAndHidesAPIKey(t *testing.T) {
	completer := &fakeCompleter{reply: "我在。"}
	service := newService(t, completer)
	ctx := context.Background()
	view, err := service.SaveSettings(ctx, Settings{
		Language: "zh-CN", Provider: "custom", BaseURL: "http://127.0.0.1:9/v1", ModelName: "desk-model",
		Temperature: 0.7, TopP: 1, MaxOutputTokens: "128", IsStreaming: true,
	}, "secret-key", false)
	if err != nil || !view.HasAPIKey {
		t.Fatal("保存密钥失败", err)
	}
	encoded, err := json.Marshal(view)
	if err != nil || strings.Contains(string(encoded), "secret-key") {
		t.Fatal("接口响应包含了密钥")
	}
	snapshot, err := service.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Workspace.Agents[0].Messages = []ChatMessage{{
		ID: "draft-2", Role: "user", Content: "在吗", CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Status: "unsent", Reactions: []Reaction{},
	}}
	if _, err = service.Save(ctx, snapshot.Workspace); err != nil {
		t.Fatal(err)
	}
	delivered, err := service.Deliver(ctx, "assistant", "draft-2")
	if err != nil {
		t.Fatal(err)
	}
	messages := delivered.Workspace.Agents[0].Messages
	if len(messages) != 2 || messages[0].Status != "sent" || messages[1].Role != "assistant" || messages[1].Content != "我在。" {
		t.Fatalf("投递结果不正确: %+v", messages)
	}
	if completer.calls != 1 || completer.last.APIKey != "secret-key" || completer.last.Model != "desk-model" {
		t.Fatalf("模型请求不正确: %+v calls=%d", completer.last, completer.calls)
	}
	again, err := service.Deliver(ctx, "assistant", "draft-2")
	if err != nil || completer.calls != 1 || len(again.Workspace.Agents[0].Messages) != 2 {
		t.Fatal("已发送的消息不能再次调用模型", err, completer.calls)
	}
}

func TestDeliverFailureMarksMessageFailed(t *testing.T) {
	completer := &fakeCompleter{err: domain.NewError(502, "MODEL_UNAVAILABLE", "模型服务没有响应，请检查 API 地址后重试。")}
	service := newService(t, completer)
	ctx := context.Background()
	if _, err := service.SaveSettings(ctx, Settings{
		Language: "system", Provider: "openai", BaseURL: "https://models.example.test/v1", ModelName: "gpt-test",
		Temperature: 0.2, TopP: 1, MaxOutputTokens: "64", IsStreaming: false,
	}, "", false); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Workspace.Agents[0].Messages = []ChatMessage{{
		ID: "draft-3", Role: "user", Content: "试一次", CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Status: "unsent", Reactions: []Reaction{},
	}}
	if _, err = service.Save(ctx, snapshot.Workspace); err != nil {
		t.Fatal(err)
	}
	delivered, err := service.Deliver(ctx, "assistant", "draft-3")
	if err != nil || delivered.Code != "MODEL_FAILED" || delivered.Workspace.Agents[0].Messages[0].Status != "failed" {
		t.Fatal("供应商失败应标为发送失败", err, delivered.Code)
	}
}

func TestHTTPCompleterReadsAssistantText(t *testing.T) {
	var sawAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("路径错误 %s", r.URL.Path)
		}
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"  回应  "}}]}`)
	}))
	t.Cleanup(server.Close)
	reply, err := NewHTTPCompleter().Complete(context.Background(), Completion{
		BaseURL: server.URL + "/v1", Model: "m", MaxTokens: 16, Temperature: 0, TopP: 1, APIKey: "token",
		Messages: []Turn{{Role: "user", Content: "你好"}},
	})
	if err != nil || reply != "回应" || sawAuth != "Bearer token" {
		t.Fatal(reply, err, sawAuth)
	}
}

func newService(t *testing.T, completer Completer) *Service {
	t.Helper()
	db := testkit.Database(t)
	directory := t.TempDir()
	authentication, err := auth.New(db, directory)
	if err != nil {
		t.Fatal(err)
	}
	key, err := secure.Random(32)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(Dependencies{DB: db, Auth: authentication, Key: key, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Completer: completer})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

type fakeCompleter struct {
	reply string
	err   error
	calls int
	last  Completion
}

func (f *fakeCompleter) Complete(_ context.Context, request Completion) (string, error) {
	f.calls++
	f.last = request
	f.last.Messages = append([]Turn(nil), request.Messages...)
	if f.err != nil {
		return "", f.err
	}
	return f.reply, nil
}
