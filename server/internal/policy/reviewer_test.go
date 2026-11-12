package policy_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/secret"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
	"io.yueyang/tom/internal/usage"
)

type reviewScript func(context.Context, provider.Request, func(provider.Event) error) (provider.Usage, error)

func (s reviewScript) Stream(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
	return s(ctx, req, emit)
}

type reviewFixture struct {
	reviewer *policy.Reviewer
	db       *sql.DB
	settings *service.Settings
	catalog  *service.Catalog
	secrets  *secret.Manager
	run      domain.Run
	model    domain.Model
}

func newReviewFixture(t *testing.T, script reviewScript) reviewFixture {
	t.Helper()
	ctx := context.Background()
	db := testkit.Database(t)
	catalog := service.NewCatalog(db)
	spec, err := catalog.CreateProvider(ctx, service.ProviderInput{Kind: "openai_compat", DisplayName: "审查测试服务", BaseURL: "http://127.0.0.1:1234"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	price := int64(1_000_000)
	model, err := catalog.CreateModel(ctx, service.ModelInput{ProviderID: spec.ID, ModelKey: "executor", DisplayName: "执行模型", ContextWindow: 32768, MaxOutput: 2048, InputPriceMicro: &price, OutputPriceMicro: &price, IsDefault: true}, "user")
	if err != nil {
		t.Fatal(err)
	}
	bot, err := service.NewBots(db, bus.New(db), t.TempDir()).Create(ctx, service.BotInput{Template: "writer"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{ID: ids.New(), TurnID: ids.New(), BotID: bot.ID, ConversationID: bot.ConversationID, ModelID: model.ID}
	if _, err = db.Exec(`INSERT INTO turns(id,conversation_id,seq,source,status,created_at) VALUES(?,?,1,'user','running',0)`, run.TurnID, run.ConversationID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO runs(id,turn_id,conversation_id,bot_id,model_id,source,status,prompt,budget_micro,created_at) VALUES(?,?,?,?,?,'chat','running','生成本地报告',1000,0)`, run.ID, run.TurnID, run.ConversationID, run.BotID, model.ID); err != nil {
		t.Fatal(err)
	}
	settings := service.NewSettings(db)
	enabled := true
	if _, err = settings.Update(ctx, service.SettingsPatch{Reviewer: &service.ReviewerPatch{Enabled: service.Field[*bool]{Set: true, Value: &enabled}}}, "user"); err != nil {
		t.Fatal(err)
	}
	secrets := secret.New(db, []byte(testkit.Random(t))[:32])
	reviewer, err := policy.NewReviewer(policy.ReviewerDependencies{DB: db, Catalog: catalog, Settings: settings, Secrets: secrets, Factory: func(domain.Provider, []byte) provider.Provider { return script }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return reviewFixture{reviewer, db, settings, catalog, secrets, run, model}
}
func (f reviewFixture) review(ctx context.Context, risk string) (policy.ReviewDecision, error) {
	return f.reviewer.Review(ctx, f.run, ids.New(), "file_write", json.RawMessage(`{"path":"out/report.md","content":"报告正文"}`), policy.Action{Class: "fs.write", Pattern: "out/report.md", Risk: risk})
}

func TestReviewerIndependentSelectionChargesAndNeverLowersRisk(t *testing.T) {
	var selected string
	f := newReviewFixture(t, func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		selected = req.Model.ID
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 30*time.Second || len(req.Tools) != 0 || req.Model.MaxOutput > 1024 {
			t.Error("审查调用约束不完整")
		}
		var input map[string]any
		if err := json.Unmarshal([]byte(req.Messages[0].Blocks[0].Text), &input); err != nil || input["user_intent"] != "生成本地报告" || input["tool"] != "file_write" || input["target"] != "out/report.md" {
			t.Error("审查缺少必要上下文", err)
		}
		return provider.Usage{InputTokens: 3, OutputTokens: 2}, emit(provider.Event{Kind: "text_delta", Text: `{"consistent":true,"risk":"low","reason":"生成本地文件符合用户要求。"}`})
	})
	price := int64(2_000_000)
	model, err := f.catalog.CreateModel(context.Background(), service.ModelInput{ProviderID: f.model.ProviderID, ModelKey: "independent", DisplayName: "独立模型", ContextWindow: 8192, MaxOutput: 1024, InputPriceMicro: &price, OutputPriceMicro: &price}, "user")
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.review(context.Background(), "high")
	if err != nil || result.Action.Risk != "high" || result.Action.Review.SelfReview || selected != model.ID || !strings.HasPrefix(result.Action.ReviewerNote, "独立模型审查") {
		t.Fatal("独立审查或风险不降级失败", result, err)
	}
	var runCost, turnCost int64
	if err = f.db.QueryRow("SELECT r.cost_micro,t.cost_micro FROM runs r JOIN turns t ON r.turn_id=t.id WHERE r.id=?", f.run.ID).Scan(&runCost, &turnCost); err != nil || runCost != 10 || turnCost != 10 {
		t.Fatal("审查费用未合并", runCost, turnCost, err)
	}
}

func TestReviewerSelfReviewAndInconsistency(t *testing.T) {
	f := newReviewFixture(t, func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: `{"consistent":false,"risk":"low","reason":"操作偏离用户意图。"}`})
	})
	result, err := f.review(context.Background(), "low")
	if err != nil || result.Action.Risk != "high" || !result.Action.Review.SelfReview || !strings.HasPrefix(result.Action.ReviewerNote, "由执行模型自查") {
		t.Fatal("自查或意图不一致提示错误", result, err)
	}
}

func TestReviewerMalformedOutputForcesApproval(t *testing.T) {
	for _, encoded := range []string{"", `[]`, `null`, `{"consistent":true,"risk":"low"}`, `{"consistent":null,"risk":"low","reason":"说明"}`, `{"consistent":true,"risk":"low","reason":"说明","extra":1}`, `{"consistent":true,"consistent":false,"risk":"low","reason":"说明"}`, `{"consistent":true,"risk":"unknown","reason":"说明"}`, `{"consistent":true,"risk":"low","reason":""}`, `{"consistent":true,"risk":"low","reason":"说明"} {}`, "```json\n{}\n```", `{"consistent":true,"risk":"low","reason":"` + strings.Repeat("长", 1001) + `"}`} {
		t.Run(encoded[:min(30, len(encoded))], func(t *testing.T) {
			f := newReviewFixture(t, func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
				return provider.Usage{InputTokens: 2}, emit(provider.Event{Kind: "text_delta", Text: encoded})
			})
			result, err := f.review(context.Background(), "low")
			if err != nil || !result.RequireApproval || result.Action.Risk != "medium" || result.Action.Review.Status != "failed" || result.Action.Review.FailureCode != "REVIEWER_OUTPUT_INVALID" {
				t.Fatal("无效输出未转入保守审批", result, err)
			}
		})
	}
}

func TestReviewerCallFailureLimitsAndCancellation(t *testing.T) {
	for _, mode := range []string{"provider", "tool", "output", "truncated", "context", "disabled", "budget", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := newReviewFixture(t, func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
				calls++
				switch mode {
				case "tool":
					return provider.Usage{InputTokens: 1}, emit(provider.Event{Kind: "tool_use_start", ToolName: "file_write"})
				case "output":
					return provider.Usage{InputTokens: 1}, emit(provider.Event{Kind: "text_delta", Text: strings.Repeat("x", 8193)})
				case "truncated":
					if err := emit(provider.Event{Kind: "text_delta", Text: `{"consistent":true,"risk":"low","reason":"符合要求。"}`}); err != nil {
						return provider.Usage{InputTokens: 1}, err
					}
					return provider.Usage{InputTokens: 1}, emit(provider.Event{Kind: "stop", StopReason: "length"})
				case "canceled":
					cancel()
					return provider.Usage{InputTokens: 1}, ctx.Err()
				default:
					return provider.Usage{InputTokens: 1}, context.DeadlineExceeded
				}
			})
			switch mode {
			case "context":
				if _, err := f.catalog.UpdateModel(ctx, f.model.ID, service.ModelPatch{ContextWindow: service.Field[int]{Set: true, Value: 1024}, MaxOutput: service.Field[int]{Set: true, Value: 512}}, "user"); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if _, err := f.catalog.UpdateProvider(ctx, f.model.ProviderID, service.ProviderPatch{IsEnabled: service.Field[bool]{Set: true, Value: false}}, "user"); err != nil {
					t.Fatal(err)
				}
			case "budget":
				if _, err := f.db.Exec("UPDATE runs SET cost_micro=budget_micro WHERE id=?", f.run.ID); err != nil {
					t.Fatal(err)
				}
			}
			result, err := f.review(ctx, "medium")
			if mode == "canceled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("取消未传播", err)
				}
			} else if mode == "budget" {
				var public *domain.Error
				if !errors.As(err, &public) || public.Code != "RUN_BUDGET_EXCEEDED" {
					t.Fatal("预算未阻断", err)
				}
			} else if err != nil || !result.RequireApproval || result.Action.Risk != "high" || result.Action.Review.Status != "failed" {
				t.Fatal("失败未提升风险", result, err)
			}
			if (mode == "context" || mode == "disabled" || mode == "budget") && calls != 0 {
				t.Fatal("无效配置仍发起模型调用")
			}
			if mode == "canceled" {
				var cost int
				if err := f.db.QueryRow("SELECT cost_micro FROM runs WHERE id=?", f.run.ID).Scan(&cost); err != nil || cost != 1 {
					t.Fatal("取消丢失已产生费用", cost, err)
				}
			}
		})
	}
}

func TestReviewerDisabledDoesNotCallProvider(t *testing.T) {
	calls := 0
	f := newReviewFixture(t, func(context.Context, provider.Request, func(provider.Event) error) (provider.Usage, error) {
		calls++
		return provider.Usage{}, nil
	})
	disabled := false
	if _, err := f.settings.Update(context.Background(), service.SettingsPatch{Reviewer: &service.ReviewerPatch{Enabled: service.Field[*bool]{Set: true, Value: &disabled}}}, "user"); err != nil {
		t.Fatal(err)
	}
	result, err := f.review(context.Background(), "medium")
	if err != nil || calls != 0 || result.Action.Review != nil || result.Action.Risk != "medium" || result.Action.ReviewerNote != "" {
		t.Fatal("停用审查仍产生副作用", result, err)
	}
}

func TestReviewerLedgerAndAuditFailuresStopActions(t *testing.T) {
	for _, mode := range []string{"ledger", "audit", "invalid_usage", "budget_after_call"} {
		t.Run(mode, func(t *testing.T) {
			f := newReviewFixture(t, func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
				tokens := 1
				if mode == "invalid_usage" {
					tokens = -1
				}
				if mode == "budget_after_call" {
					tokens = 1000
				}
				return provider.Usage{InputTokens: tokens}, emit(provider.Event{Kind: "text_delta", Text: `{"consistent":true,"risk":"low","reason":"符合要求。"}`})
			})
			result, err := f.review(context.Background(), "low")
			if mode == "ledger" || mode == "audit" {
				if err != nil || result.Action.Risk != "low" || result.Action.Review == nil || result.Action.Review.Status != "completed" {
					t.Fatal("用量事件或审计已不再阻断审查", result, err)
				}
				return
			}
			if err == nil {
				t.Fatal("关键结算失败被允许继续")
			}
		})
	}
}

func TestSharedUsageLedgerConcurrentCosts(t *testing.T) {
	f := newReviewFixture(t, func(context.Context, provider.Request, func(provider.Event) error) (provider.Usage, error) {
		return provider.Usage{}, nil
	})
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			if _, err := usage.Record(context.Background(), f.db, f.run, f.model, provider.Usage{InputTokens: 1}); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	var runCost, turnCost int
	if err := f.db.QueryRow("SELECT r.cost_micro,t.cost_micro FROM runs r JOIN turns t ON t.id=r.turn_id WHERE r.id=?", f.run.ID).Scan(&runCost, &turnCost); err != nil || runCost != 16 || turnCost != 16 {
		t.Fatal("并发计费丢失", runCost, turnCost, err)
	}
}

func TestReviewerSecretLifetimeAndOutputRedaction(t *testing.T) {
	f := newReviewFixture(t, func(context.Context, provider.Request, func(provider.Event) error) (provider.Usage, error) {
		return provider.Usage{}, nil
	})
	ctx := context.Background()
	credential := testkit.Random(t)
	record, err := f.secrets.Create(ctx, "审查鉴权", credential, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.catalog.UpdateProvider(ctx, f.model.ProviderID, service.ProviderPatch{SecretID: service.Field[*string]{Set: true, Value: &record.ID}}, "user"); err != nil {
		t.Fatal(err)
	}
	var borrowed []byte
	f.reviewer, err = policy.NewReviewer(policy.ReviewerDependencies{DB: f.db, Settings: f.settings, Catalog: f.catalog, Secrets: f.secrets, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Factory: func(spec domain.Provider, key []byte) provider.Provider {
		borrowed = key
		if string(key) != credential {
			t.Error("鉴权注入错误")
		}
		return reviewScript(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
			encoded, err := json.Marshal(map[string]any{"consistent": true, "risk": "low", "reason": "供应商回显：" + string(key)})
			if err != nil {
				return provider.Usage{}, err
			}
			return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: string(encoded)})
		})
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.review(ctx, "low")
	if err != nil || strings.Contains(result.Action.ReviewerNote, credential) || !strings.Contains(result.Action.ReviewerNote, "[已脱敏]") {
		t.Fatal("审批提示泄露了鉴权材料", err)
	}
	for _, b := range borrowed {
		if b != 0 {
			t.Fatal("审查后未清除鉴权材料")
		}
	}
}
