package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/browser"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/secret"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
	"io.yueyang/tom/internal/testkit/browserfake"
	"io.yueyang/tom/internal/workspace"
)

func fixture(t *testing.T, strictness string) (*Executor, domain.Bot, domain.Run) {
	return fixtureAt(t, strictness, t.TempDir())
}

func fixtureAt(t *testing.T, strictness, path string) (*Executor, domain.Bot, domain.Run) {
	t.Helper()
	ctx := context.Background()
	db := testkit.Database(t)
	events := bus.New(db)
	if err := os.Mkdir(filepath.Join(path, "shared"), 0700); err != nil {
		t.Fatal(err)
	}
	storage, err := workspace.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Error(err)
		}
	})
	bots := service.NewBots(db, events, path)
	bot, err := bots.Create(ctx, service.BotInput{Template: "writer", ToolPolicy: &domain.ToolPolicy{Approval: strictness, AllowedTools: []string{"file_read", "file_write", "file_edit", "file_list", "file_glob", "file_search"}}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{ID: ids.New(), TurnID: ids.New(), ConversationID: bot.ConversationID, BotID: bot.ID, ModelID: "fixture"}
	if _, err = db.Exec(`INSERT INTO turns(id,conversation_id,seq,source,status,created_at) VALUES(?,?,1,'user','running',0)`, run.TurnID, run.ConversationID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO runs(id,turn_id,conversation_id,bot_id,source,status,model_id,prompt,created_at) VALUES(?,?,?,?,'chat','running','fixture','Write a report.',0)`, run.ID, run.TurnID, run.ConversationID, bot.ID); err != nil {
		t.Fatal(err)
	}
	terminals, err := service.NewTerminals(ctx, db, events, bots, storage, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := terminals.Shutdown(shutdown); err != nil {
			t.Error(err)
		}
	})
	secrets := secret.New(db, []byte(testkit.Random(t))[:32])
	reviewer, err := policy.NewReviewer(policy.ReviewerDependencies{DB: db, Settings: service.NewSettings(db), Catalog: service.NewCatalog(db), Secrets: secrets, Factory: func(spec domain.Provider, key []byte) provider.Provider { return provider.NewHTTP(spec, key) }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	browsing, err := service.NewBrowser(ctx, service.BrowserDependencies{DB: db, Bots: bots, Events: events, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Factory: func(ctx context.Context, options browser.Options) (service.BrowserEngine, error) {
		return browserfake.New(options), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := browsing.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	executor, err := New(db, events, storage, service.NewApprovals(db, events), secrets, terminals, browsing, reviewer, service.NewMemories(db, bots, storage, events))
	if err != nil {
		t.Fatal(err)
	}
	return executor, bot, run
}
func execute(t *testing.T, e *Executor, bot domain.Bot, run domain.Run, name string, input any) domain.ToolResultBlock {
	t.Helper()
	result, err := e.Execute(context.Background(), domain.ToolInvocation{Run: run, Bot: bot, Step: 0}, domain.ToolUseBlock{ID: ids.New(), Name: name, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func TestFilesExecuteAuditAndTruncation(t *testing.T) {
	e, bot, run := fixture(t, "standard")
	result := execute(t, e, bot, run, "file_write", map[string]any{"path": "out/report.md", "content": "Original report"})
	if result.IsError {
		t.Fatal(result.Content)
	}
	result = execute(t, e, bot, run, "file_edit", map[string]any{"path": "out/report.md", "old_text": "Original", "new_text": "Updated"})
	if result.IsError {
		t.Fatal(result.Content)
	}
	result = execute(t, e, bot, run, "file_read", map[string]any{"path": "out/report.md"})
	if result.IsError || !strings.Contains(result.Content, "Updated report") {
		t.Fatal(result)
	}
	var saved string
	if err := e.db.QueryRow("SELECT input_json FROM tool_calls WHERE tool_name='file_write'").Scan(&saved); err != nil || strings.Contains(saved, "Original report") {
		t.Fatal("调用摘要泄露正文", err)
	}
	result = execute(t, e, bot, run, "file_write", map[string]any{"path": "out/large.md", "content": strings.Repeat("long line\n", 10000)})
	if result.IsError {
		t.Fatal(result)
	}
	result = execute(t, e, bot, run, "file_read", map[string]any{"path": "out/large.md"})
	var output map[string]any
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil {
		t.Fatal(err)
	}
	if output["truncated"] != true {
		t.Fatal("行分页没有标记截断")
	}
	result = execute(t, e, bot, run, "file_write", map[string]any{"path": "out/wide.md", "content": strings.Repeat("wide", 20000)})
	if result.IsError {
		t.Fatal(result)
	}
	result = execute(t, e, bot, run, "file_read", map[string]any{"path": "out/wide.md"})
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil {
		t.Fatal(err)
	}
	path, ok := output["output_path"].(string)
	if !ok || len(result.Content) > 32<<10 {
		t.Fatal("超长工具结果未落盘", result.IsError)
	}
	if _, err := e.files.Read(context.Background(), bot.ID, path, 1, 1); err != nil {
		t.Fatal("完整工具输出不可读取", err)
	}
}

func TestApprovalDecisionRememberAndConflicts(t *testing.T) {
	e, bot, run := fixture(t, "standard")
	result := execute(t, e, bot, run, "file_write", map[string]any{"path": "notes/report.md", "content": "approved content", "create_dirs": true})
	if result.IsError {
		t.Fatal(result.Content)
	}
	file, err := e.files.Read(context.Background(), bot.ID, "notes/report.md", 1, 1)
	if err != nil || !strings.Contains(file.Content, "approved content") {
		t.Fatal("写入未立即执行", err)
	}
	var status string
	if err = e.db.QueryRow("SELECT status FROM runs WHERE id=?", run.ID).Scan(&status); err != nil || status != "running" {
		t.Fatal("执行把运行标成等待审批", status, err)
	}
	decision, err := e.approvals.PolicyDecision(context.Background(), bot.ID, policy.Action{Class: "fs.write_outside_out", Pattern: "notes/report.md"})
	if err != nil || decision != "" {
		t.Fatal("策略仍返回拒绝或放行规则", decision, err)
	}
}

func TestApprovalDenialCancellationAndWhitelist(t *testing.T) {
	e, bot, run := fixture(t, "strict")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.Execute(ctx, domain.ToolInvocation{Run: run, Bot: bot, Step: 0}, domain.ToolUseBlock{ID: ids.New(), Name: "file_write", Input: map[string]any{"path": "out/report", "content": "should not exist"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("取消未传播", err)
	}
	if _, err = e.files.Read(context.Background(), bot.ID, "out/report", 1, 1); err == nil {
		t.Fatal("取消仍发生副作用")
	}
	e, bot, run = fixture(t, "standard")
	bot.ToolPolicy.AllowedTools = nil
	result := execute(t, e, bot, run, "file_write", map[string]any{"path": "out/report", "content": "blocked"})
	if !result.IsError || !strings.Contains(result.Content, "TOOL_NOT_ALLOWED") {
		t.Fatal(result)
	}
	bot.ToolPolicy.AllowedTools = []string{"file_read"}
	result = execute(t, e, bot, run, "file_read", map[string]any{"path": "out/report", "unknown": true})
	if !result.IsError || !strings.Contains(result.Content, "INVALID_TOOL_INPUT") {
		t.Fatal(result)
	}
}

func TestPolicyPrecedence(t *testing.T) {
	e, bot, _ := fixture(t, "standard")
	decision, err := e.approvals.PolicyDecision(context.Background(), bot.ID, policy.Action{Class: "fs.write_outside_out", Pattern: "notes/report"})
	if err != nil || decision != "" {
		t.Fatal("策略仍返回拒绝规则", decision, err)
	}
}

func TestApprovalCannotAuthorizeChangedOutputPathRisk(t *testing.T) {
	root := t.TempDir()
	e, bot, run := fixtureAt(t, "strict", root)
	linkParent := filepath.Join(root, bot.WorkspaceDir, "out")
	if err := os.MkdirAll(linkParent, 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(linkParent, "link")); err != nil {
		t.Fatal(err)
	}
	result := execute(t, e, bot, run, "file_write", map[string]any{"path": "out/link/report.md", "content": "should not be written", "create_dirs": true})
	if !result.IsError {
		t.Fatal("符号链接逃逸被接受", result.Content)
	}
	if _, err := os.Stat(filepath.Join(outside, "report.md")); err == nil {
		t.Fatal("工作区外被写入")
	}
}

// 运行事件写入失败时，工具状态和外部文件操作必须都停在执行边界之前。
func TestToolStatusWriteFailurePreventsFileAction(t *testing.T) {
	e, bot, run := fixture(t, "loose")
	if _, err := e.db.Exec(`CREATE TRIGGER reject_running_event BEFORE INSERT ON events WHEN NEW.kind='tool.status' AND json_extract(NEW.payload_json,'$.status')='running' BEGIN SELECT RAISE(ABORT,'reject tool transition'); END`); err != nil {
		t.Fatal(err)
	}
	call := domain.ToolUseBlock{ID: ids.New(), Name: "file_write", Input: map[string]any{"path": "out/rollback.txt", "content": "不应发布"}}
	if _, err := e.Execute(t.Context(), domain.ToolInvocation{Run: run, Bot: bot}, call); err == nil {
		t.Fatal("事件失败却继续执行")
	}
	var state string
	if err := e.db.QueryRow("SELECT status FROM tool_calls WHERE id=?", call.ID).Scan(&state); err != nil || state != "failed" {
		t.Fatal("失败调用未收尾", state, err)
	}
	_, err := e.files.Download(t.Context(), bot.ID, "out/rollback.txt")
	var public *domain.Error
	if !errors.As(err, &public) || public.Code != "FILE_NOT_FOUND" {
		t.Fatal("状态回滚时仍产生文件", err)
	}
}
