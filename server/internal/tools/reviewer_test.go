package tools

import (
	"context"
	"errors"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/policy"
)

func TestToolReviewerRiskApprovalRulesAndBudget(t *testing.T) {
	executor, bot, run := fixture(t, "loose")
	ctx := context.Background()
	decision, err := executor.approvals.PolicyDecision(ctx, bot.ID, policy.Action{Class: "fs.write", Pattern: "out/reviewed.md"})
	if err != nil || decision != "" {
		t.Fatal("策略不再提供拒绝规则", decision, err)
	}
	result := execute(t, executor, bot, run, "file_write", map[string]any{"path": "out/reviewed.md", "content": "报告正文"})
	if result.IsError {
		t.Fatal(result.Content)
	}
	if _, err = executor.files.Read(ctx, bot.ID, "out/reviewed.md", 1, 1); err != nil {
		t.Fatal("写入未立即执行", err)
	}
	if _, err = executor.db.Exec("UPDATE runs SET budget_micro=100, cost_micro=100 WHERE id=?", run.ID); err != nil {
		t.Fatal(err)
	}
	blocked, err := executor.Execute(ctx, domain.ToolInvocation{Run: run, Bot: bot, Step: 1}, domain.ToolUseBlock{ID: ids.New(), Name: "file_write", Input: map[string]any{"path": "out/blocked.md", "content": "不应写入"}})
	var public *domain.Error
	if !errors.As(err, &public) || public.Code != "RUN_BUDGET_EXCEEDED" {
		t.Fatal("超出预算仍执行工具", err, blocked)
	}
	if _, err = executor.files.Read(ctx, bot.ID, "out/blocked.md", 1, 1); err == nil {
		t.Fatal("预算用尽仍写入文件")
	}
}
