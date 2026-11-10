package tools

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/service"
)

type toolReviewScript func(context.Context, provider.Request, func(provider.Event) error) (provider.Usage, error)

func (script toolReviewScript) Stream(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
	return script(ctx, req, emit)
}

func TestToolReviewerRiskApprovalRulesAndBudget(t *testing.T) {
	for _, mode := range []string{"raised", "failed", "consistent", "allow", "deny", "budget_after_review", "budget_after_approval"} {
		t.Run(mode, func(t *testing.T) {
			executor, bot, run := fixture(t, "loose")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			catalog := service.NewCatalog(executor.db)
			spec, err := catalog.CreateProvider(ctx, service.ProviderInput{Kind: "openai_compat", DisplayName: "模型服务", BaseURL: "http://127.0.0.1:1234"}, "user")
			if err != nil {
				t.Fatal(err)
			}
			price := int64(1_000_000)
			model, err := catalog.CreateModel(ctx, service.ModelInput{ProviderID: spec.ID, ModelKey: "fixture", DisplayName: "测试模型", ContextWindow: 8192, MaxOutput: 512, InputPriceMicro: &price, OutputPriceMicro: &price}, "user")
			if err != nil {
				t.Fatal(err)
			}
			run.ModelID = model.ID
			if _, err = executor.db.Exec("UPDATE runs SET model_id=?,budget_micro=100 WHERE id=?", model.ID, run.ID); err != nil {
				t.Fatal(err)
			}
			settings := service.NewSettings(executor.db)
			enabled := true
			if _, err = settings.Update(ctx, service.SettingsPatch{Reviewer: &service.ReviewerPatch{Enabled: service.Field[*bool]{Set: true, Value: &enabled}}}, "user"); err != nil {
				t.Fatal(err)
			}
			calls := 0
			script := toolReviewScript(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
				calls++
				encoded := `{"consistent":true,"risk":"high","reason":"请确认写入内容。"}`
				tokens := 1
				if mode == "failed" {
					encoded = "invalid"
				}
				if mode == "consistent" {
					encoded = `{"consistent":true,"risk":"low","reason":"符合本地写作要求。"}`
				}
				if mode == "budget_after_review" {
					tokens = 100
				}
				return provider.Usage{InputTokens: tokens}, emit(provider.Event{Kind: "text_delta", Text: encoded})
			})
			executor.reviewer, err = policy.NewReviewer(policy.ReviewerDependencies{DB: executor.db, Settings: settings, Catalog: catalog, Secrets: executor.secrets, Factory: func(domain.Provider, []byte) provider.Provider { return script }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "allow" || mode == "deny" {
				if _, err = executor.approvals.SaveRule(ctx, service.PolicyRule{BotID: bot.ID, ActionClass: "fs.write", Decision: mode}, "user"); err != nil {
					t.Fatal(err)
				}
			}
			type outcome struct {
				result domain.ToolResultBlock
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := executor.Execute(ctx, domain.ToolInvocation{Run: run, Bot: bot, Step: 0}, domain.ToolUseBlock{ID: ids.New(), Name: "file_write", Input: map[string]any{"path": "out/reviewed.md", "content": "报告正文"}})
				done <- outcome{result, err}
			}()
			if mode == "raised" || mode == "failed" || mode == "budget_after_approval" {
				approval := pending(t, executor)
				if _, err = executor.files.Read(ctx, bot.ID, "out/reviewed.md", 1, 1); err == nil {
					t.Fatal("审查审批前发生写入")
				}
				if approval.ReviewerNote == nil || !strings.Contains(*approval.ReviewerNote, "由执行模型自查") {
					t.Fatal("审批未保留自查说明")
				}
				if mode == "failed" && approval.RiskLevel != "medium" {
					t.Fatal("失败未按一档提升")
				}
				if mode == "budget_after_approval" {
					if _, err = executor.db.Exec("UPDATE runs SET cost_micro=budget_micro WHERE id=?", run.ID); err != nil {
						t.Fatal(err)
					}
				}
				if err = executor.approvals.Decide(ctx, approval.ID, service.ApprovalDecision{Decision: "approve"}, "user"); err != nil {
					t.Fatal(err)
				}
			}
			finished := <-done
			if strings.HasPrefix(mode, "budget_") {
				var public *domain.Error
				if !errors.As(finished.err, &public) || public.Code != "RUN_BUDGET_EXCEEDED" {
					t.Fatal("审查或等待期间费用未阻止副作用", finished.err)
				}
				var storedCode string
				if err := executor.db.QueryRow("SELECT error_code FROM tool_calls WHERE run_id=?", run.ID).Scan(&storedCode); err != nil || storedCode != "RUN_BUDGET_EXCEEDED" {
					t.Fatal("工具未保存预算停止原因", storedCode, err)
				}
			} else if finished.err != nil {
				t.Fatal(finished.err)
			}
			if mode == "deny" && (!finished.result.IsError || !strings.Contains(finished.result.Content, "TOOL_DENIED")) {
				t.Fatal("拒绝规则未生效")
			}
			if (mode == "allow" || mode == "deny") && calls != 0 {
				t.Fatal("显式规则仍调用了审查模型")
			}
			_, readErr := executor.files.Read(ctx, bot.ID, "out/reviewed.md", 1, 1)
			expectedWrite := mode != "deny" && !strings.HasPrefix(mode, "budget_")
			if expectedWrite != (readErr == nil) {
				t.Fatal("审查后副作用与决策不符", readErr)
			}
		})
	}
}
