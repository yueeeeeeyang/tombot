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
	"sync"
	"sync/atomic"
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
func pending(t *testing.T, e *Executor) service.ApprovalSummary {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		wake := e.events.Wake()
		items, err := e.approvals.List(context.Background(), "pending", "", 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) > 0 {
			return items[0]
		}
		select {
		case <-wake:
		case <-timer.C:
			t.Fatal("未创建审批")
		}
	}
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
	var audits int
	if err := e.db.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action='fs.write'").Scan(&audits); err != nil || audits != 2 {
		t.Fatal("文件审计缺失", err)
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan domain.ToolResultBlock, 1)
	failures := make(chan error, 1)
	call := domain.ToolUseBlock{ID: ids.New(), Name: "file_write", Input: map[string]any{"path": "notes/report.md", "content": "approved content", "create_dirs": true}}
	go func() {
		result, err := e.Execute(ctx, domain.ToolInvocation{Run: run, Bot: bot, Step: 0}, call)
		done <- result
		failures <- err
	}()
	approval := pending(t, e)
	if _, err := e.files.Read(context.Background(), bot.ID, "notes/report.md", 1, 1); err == nil {
		t.Fatal("未审批就发生写入")
	}
	var status string
	if err := e.db.QueryRow("SELECT status FROM runs WHERE id=?", run.ID).Scan(&status); err != nil || status != "waiting_approval" {
		t.Fatal("等待状态未持久化", err)
	}
	var successes atomic.Int32
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			err := e.approvals.Decide(context.Background(), approval.ID, service.ApprovalDecision{Decision: "approve", Remember: &service.RememberDecision{Scope: "bot"}}, "user")
			if err == nil {
				successes.Add(1)
			}
		})
	}
	workers.Wait()
	if successes.Load() != 1 {
		t.Fatal("审批重复决策成功")
	}
	if err := <-failures; err != nil {
		t.Fatal(err)
	}
	if result := <-done; result.IsError {
		t.Fatal(result.Content)
	}
	decision, err := e.approvals.PolicyDecision(context.Background(), bot.ID, policy.Action{Class: "fs.write_outside_out", Pattern: "notes/report.md"})
	if err != nil || decision != "allow" {
		t.Fatal("审批与规则未同时保存", err)
	}
	if err = e.db.QueryRow("SELECT status FROM runs WHERE id=?", run.ID).Scan(&status); err != nil || status != "running" {
		t.Fatal("审批后未恢复执行", err)
	}
	result := execute(t, e, bot, run, "file_write", map[string]any{"path": "notes/report.md", "content": "second write"})
	if result.IsError {
		t.Fatal("记忆规则未生效", result.Content)
	}
}

func TestApprovalDenialCancellationAndWhitelist(t *testing.T) {
	for _, mode := range []string{"deny", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			e, bot, run := fixture(t, "strict")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			results := make(chan domain.ToolResultBlock, 1)
			go func() {
				result, err := e.Execute(ctx, domain.ToolInvocation{Run: run, Bot: bot, Step: 0}, domain.ToolUseBlock{ID: ids.New(), Name: "file_write", Input: map[string]any{"path": "out/report", "content": "should not exist"}})
				results <- result
				done <- err
			}()
			approval := pending(t, e)
			if mode == "deny" {
				if err := e.approvals.Decide(ctx, approval.ID, service.ApprovalDecision{Decision: "deny"}, "user"); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			err := <-done
			result := <-results
			if mode == "deny" && (err != nil || !result.IsError || !strings.Contains(result.Content, "TOOL_DENIED")) {
				t.Fatal("拒绝没有回灌", err, result)
			}
			if mode == "cancel" && err == nil {
				t.Fatal("取消未传播")
			}
			if _, err = e.files.Read(context.Background(), bot.ID, "out/report", 1, 1); err == nil {
				t.Fatal("拒绝或取消仍发生副作用")
			}
			pending, err := e.approvals.List(context.Background(), "pending", "", 50)
			if err != nil || len(pending) != 0 {
				t.Fatal("残留待办", err)
			}
		})
	}
	e, bot, run := fixture(t, "standard")
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
	ctx := context.Background()
	target := "notes/report"
	for _, rule := range []service.PolicyRule{{BotID: "*", ActionClass: "fs.write_outside_out", Pattern: &target, Decision: "deny"}, {BotID: bot.ID, ActionClass: "fs.write_outside_out", Decision: "allow"}} {
		if _, err := e.approvals.SaveRule(ctx, rule, "user"); err != nil {
			t.Fatal(err)
		}
	}
	decision, err := e.approvals.PolicyDecision(ctx, bot.ID, policy.Action{Class: "fs.write_outside_out", Pattern: target})
	if err != nil || decision != "allow" {
		t.Fatal("Bot 规则没有优先", err)
	}
	if _, err = e.approvals.SaveRule(ctx, service.PolicyRule{BotID: bot.ID, ActionClass: "fs.write_outside_out", Pattern: &target, Decision: "deny"}, "user"); err != nil {
		t.Fatal(err)
	}
	decision, err = e.approvals.PolicyDecision(ctx, bot.ID, policy.Action{Class: "fs.write_outside_out", Pattern: target})
	if err != nil || decision != "deny" {
		t.Fatal("精确规则没有优先", err)
	}
}

func TestApprovalCannotAuthorizeChangedOutputPathRisk(t *testing.T) {
	root := t.TempDir()
	e, bot, run := fixtureAt(t, "strict", root)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	results := make(chan domain.ToolResultBlock, 1)
	go func() {
		result, err := e.Execute(ctx, domain.ToolInvocation{Run: run, Bot: bot, Step: 0}, domain.ToolUseBlock{ID: ids.New(), Name: "file_write", Input: map[string]any{"path": "out/link/report.md", "content": "should not be written", "create_dirs": true}})
		results <- result
		done <- err
	}()
	approval := pending(t, e)
	if err := os.Symlink("../notes", filepath.Join(root, bot.WorkspaceDir, "out/link")); err != nil {
		t.Fatal(err)
	}
	if err := e.approvals.Decide(ctx, approval.ID, service.ApprovalDecision{Decision: "approve"}, "user"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	result := <-results
	if !result.IsError || !strings.Contains(result.Content, "TOOL_TARGET_CHANGED") {
		t.Fatal("审批后目标变化仍获准", result)
	}
	if _, err := e.files.Read(ctx, bot.ID, "notes/report.md", 1, 1); err == nil {
		t.Fatal("变化后的路径被写入")
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
