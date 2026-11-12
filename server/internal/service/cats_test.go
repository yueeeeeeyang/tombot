package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/testkit"
)

func TestWorkspaceSeedAndSave(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	first, err := service.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Pristine || len(first.Workspace.Agents) != 0 || first.Workspace.ActiveAgentID != "" {
		t.Fatalf("首次工作空间应为空: %+v", first.Workspace)
	}
	created := Workspace{Version: 5, ActiveAgentID: "cat-1", Agents: []Agent{{
		ID: "cat-1", Name: "阿花", Description: "一起写", Group: "未分组", Avatar: "calico", ModelID: "写作模型",
		Messages: []ChatMessage{}, Artifacts: []Artifact{},
	}}}
	savedFirst, err := service.Save(ctx, created)
	if err != nil || savedFirst.Workspace.LeadAgentID != "cat-1" {
		t.Fatal("第一只猫应成为主猫", err, savedFirst.Workspace.LeadAgentID)
	}
	second := savedFirst.Workspace
	second.Agents = append(second.Agents, Agent{
		ID: "cat-2", Name: "阿白", Description: "第二只", Group: "未分组", Avatar: "bombay", ModelID: "写作模型",
		Messages: []ChatMessage{}, Artifacts: []Artifact{},
	})
	second.ActiveAgentID = "cat-2"
	savedSecond, err := service.Save(ctx, second)
	if err != nil || savedSecond.Workspace.LeadAgentID != "cat-1" {
		t.Fatal("再招一只不应换掉主猫", err, savedSecond.Workspace.LeadAgentID)
	}
	first.Workspace = SeedWorkspace()
	first.Workspace.Agents[0].Draft = "还没发送"
	saved, err := service.Save(ctx, first.Workspace)
	if err != nil || saved.Pristine || saved.Workspace.Agents[0].Draft != "还没发送" {
		t.Fatal("保存失败", err, saved.Pristine)
	}
	again, err := service.Load(ctx)
	if err != nil || again.Workspace.Agents[0].Draft != "还没发送" || again.Pristine {
		t.Fatalf("重新读取没有保留草稿 %+v %v", again.Workspace.Agents[0], err)
	}
	var draft string
	if err = service.db.QueryRow(`SELECT draft FROM bots WHERE desktop_id='assistant'`).Scan(&draft); err != nil || draft != "还没发送" {
		t.Fatal("草稿应写在 bots.draft", err, draft)
	}
	if _, err = service.db.Exec(`SELECT 1 FROM desktop_state`); err == nil {
		t.Fatal("desktop_state 应该已经删除")
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

func TestPrepareDispatchWithoutModelKeepsUnsent(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	snapshot, err := service.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Workspace = SeedWorkspace()
	snapshot.Workspace.Agents[0].Messages = []ChatMessage{{
		ID: "draft-1", Role: "user", Content: "你好", CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Status: "unsent", Reactions: []Reaction{},
	}}
	if _, err = service.Save(ctx, snapshot.Workspace); err != nil {
		t.Fatal(err)
	}
	_, err = service.PrepareDispatch(ctx, "assistant", "draft-1")
	var failure *domain.Error
	if !errors.As(err, &failure) || failure.Code != "MODEL_NOT_READY" {
		t.Fatal("未配置模型时应拒绝投递", err)
	}
	loaded, err := service.Load(ctx)
	if err != nil || loaded.Workspace.Agents[0].Messages[0].Status != "unsent" {
		t.Fatal("失败前的消息应保持未发送", err)
	}
}

func TestApplyRunPersistsAssistantReply(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	snapshot, err := service.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Workspace = SeedWorkspace()
	snapshot.Workspace.Agents[0].Messages = []ChatMessage{{
		ID: "draft-2", Role: "user", Content: "在吗", CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Status: "unsent", Reactions: []Reaction{},
	}}
	saved, err := service.Save(ctx, snapshot.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := service.ApplyRun(ctx, "assistant", "draft-2", saved.Revision, "我在。", nil)
	if err != nil {
		t.Fatal(err)
	}
	messages := applied.Workspace.Agents[0].Messages
	if len(messages) != 2 || messages[0].Status != "sent" || messages[1].Content != "我在。" || messages[1].Role != "assistant" {
		t.Fatalf("运行结果没有写回: %+v", messages)
	}
}

func TestModelFormDoesNotEchoAPIKey(t *testing.T) {
	service := newService(t)
	view, err := service.SaveModelForm(context.Background(), ModelForm{
		Language: "zh-CN", Provider: "custom", BaseURL: "http://127.0.0.1:9/v1", ModelName: "desk-model",
	}, "secret-key", false)
	if err != nil || !view.HasAPIKey {
		t.Fatal("保存密钥失败", err)
	}
	encoded, err := json.Marshal(view)
	if err != nil || strings.Contains(string(encoded), "secret-key") {
		t.Fatal("接口响应包含了密钥")
	}
}

func TestApplyRunFailureKeepsPartialReply(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	snapshot, err := service.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Workspace = SeedWorkspace()
	snapshot.Workspace.Agents[0].Messages = []ChatMessage{{
		ID: "draft-3", Role: "user", Content: "试一次", CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Status: "unsent", Reactions: []Reaction{},
	}}
	saved, err := service.Save(ctx, snapshot.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := service.ApplyRun(ctx, "assistant", "draft-3", saved.Revision, "说到一半", domain.NewError(502, "RUN_INTERRUPTED", "中断"))
	if err != nil || applied.Code != "MODEL_FAILED" || applied.Workspace.Agents[0].Messages[0].Status != "failed" {
		t.Fatal("中断应标为失败", err, applied.Code)
	}
	if applied.Workspace.Agents[0].Messages[1].Content != "说到一半" || applied.Workspace.Agents[0].Messages[1].Status != "failed" {
		t.Fatal("已生成的正文应保留")
	}
}

func TestBuiltinProviderListsModelsFromMock(t *testing.T) {
	credential := "test-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/models" || r.Header.Get("Authorization") != "Bearer "+credential {
			http.NotFound(w, r)
			return
		}
		if _, err := fmt.Fprint(w, `{"object":"list","data":[{"id":"deepseek-chat","object":"model","name":"DeepSeek Chat"}]}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	service := newService(t)
	models, err := service.ListBuiltinModels(context.Background(), credential, server.URL)
	if err != nil || len(models) != 1 || models[0].ID != "deepseek-chat" {
		t.Fatal("内置提供商没有从模拟接口拉取模型", err, models)
	}
	if _, err = service.ListBuiltinModels(context.Background(), "", server.URL); err == nil {
		t.Fatal("没有密钥时不应假装拉到模型")
	}
	rejected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer rejected.Close()
	_, err = service.ListBuiltinModels(context.Background(), credential, rejected.URL)
	var domainError *domain.Error
	if !errors.As(err, &domainError) || domainError.Message != "API Key 验证失败，请检查后重试。" || strings.Contains(domainError.Message, credential) {
		t.Fatal("无效密钥应返回不含密钥的中文错误", err)
	}
}

func newService(t *testing.T) *Service {
	t.Helper()
	db := testkit.Database(t)
	key, err := secure.Random(32)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(Dependencies{DB: db, Key: key, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
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

func TestWelcomeProjectionAndPreconditions(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	if _, err := service.Save(ctx, SeedWorkspace()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PrepareWelcome(ctx, "missing"); err == nil {
		t.Fatal("不存在的猫咪应失败")
	}
	if _, err := service.PrepareWelcome(ctx, "assistant"); err == nil {
		t.Fatal("未配置模型应失败")
	}
	var conversationID string
	if err := service.db.QueryRow(`SELECT c.id FROM conversations c JOIN bots b ON c.bot_id=b.id WHERE b.desktop_id='assistant' AND c.kind='direct'`).Scan(&conversationID); err != nil {
		t.Fatal(err)
	}
	for index, sender := range []string{"system", "bot"} {
		id := fmt.Sprintf("welcome-projection-%d", index)
		blocks := `[{"kind":"text","text":"模型欢迎内容"}]`
		if sender == "system" {
			blocks = `[{"kind":"text","text":"Greet the user in Chinese."}]`
		}
		if _, err := service.db.Exec(`INSERT INTO messages(id,conversation_id,seq,sender_kind,blocks_json,status,created_at) VALUES(?,?,?,?,?,'complete',?)`, id, conversationID, index+1, sender, blocks, time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := service.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	messages := snapshot.Workspace.Agents[0].Messages
	if len(messages) != 1 || messages[0].Role != "assistant" || messages[0].Content != "模型欢迎内容" {
		t.Fatal("系统触发不能显示为消息", messages)
	}
	dispatch, err := service.PrepareWelcome(ctx, "assistant")
	if err != nil || !dispatch.Done {
		t.Fatal("已有会话不应触发欢迎", dispatch, err)
	}
}
