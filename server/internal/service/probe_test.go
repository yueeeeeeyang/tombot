package service

import (
	"context"
	"errors"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit"
)

type probeStream func(context.Context, provider.Request, func(provider.Event) error) (provider.Usage, error)

func (f probeStream) Stream(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
	return f(ctx, req, emit)
}

func TestProbeUsageKnownPriceUnknownModelCancellationAndAuditRollback(t *testing.T) {
	db := testkit.Database(t)
	ctx := context.Background()
	catalog := NewCatalog(db)
	p, err := catalog.CreateProvider(ctx, ProviderInput{Kind: "openai_compat", DisplayName: "连通测试", BaseURL: "http://127.0.0.1:1234"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	price := int64(1000000)
	model, err := catalog.CreateModel(ctx, ModelInput{ProviderID: p.ID, ModelKey: "configured", DisplayName: "计价模型", ContextWindow: 8192, MaxOutput: 512, InputPriceMicro: &price, OutputPriceMicro: &price}, "user")
	if err != nil {
		t.Fatal(err)
	}
	factory := func(_ domain.Provider, _ []byte) provider.Provider {
		return probeStream(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
			if req.Model.MaxOutput != 1 {
				t.Fatal("连通性测试未限制输出")
			}
			return provider.Usage{InputTokens: 2, OutputTokens: 1}, nil
		})
	}
	if _, err = catalog.Probe(ctx, p.ID, "configured", "user", nil, factory); err != nil {
		t.Fatal(err)
	}
	var cost int64
	var estimated bool
	var modelID string
	if err = db.QueryRow("SELECT model_id,cost_micro,is_estimated FROM usage_events").Scan(&modelID, &cost, &estimated); err != nil || modelID != model.ID || cost != 3 || estimated {
		t.Fatal("已配置模型测试未正确计费", modelID, cost, estimated, err)
	}
	if _, err = catalog.Probe(ctx, p.ID, "unconfigured", "user", nil, factory); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT cost_micro,is_estimated FROM usage_events WHERE model_id=?", "probe:"+p.ID).Scan(&cost, &estimated); err != nil || cost != 0 || !estimated {
		t.Fatal("未知模型缺少估算标记", cost, estimated, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	defer cancel()
	interrupted := func(_ domain.Provider, _ []byte) provider.Provider {
		return probeStream(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
			cancel()
			return provider.Usage{InputTokens: 7}, ctx.Err()
		})
	}
	if _, err = catalog.Probe(canceled, p.ID, "configured", "user", nil, interrupted); !errors.Is(err, context.Canceled) {
		t.Fatal("取消错误未保留", err)
	}
	report, err := NewUsage(db).Report(ctx, UsageQuery{GroupBy: "bot"})
	if err != nil || report.Total.Calls != 3 || report.Total.CostMicro != 10 || report.Total.EstimatedCalls != 1 || len(report.Groups) != 1 || report.Groups[0].Name != "系统" {
		t.Fatal("测试费用未计入系统用量", report, err)
	}
	if _, err = db.Exec(`CREATE TRIGGER reject_probe_audit BEFORE INSERT ON audit_log WHEN NEW.action='provider.test' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.Probe(ctx, p.ID, "configured", "user", nil, factory); err == nil {
		t.Fatal("审计失败被忽略")
	}
	var count int
	if err = db.QueryRow("SELECT COUNT(*) FROM usage_events").Scan(&count); err != nil || count != 3 {
		t.Fatal("审计失败仍记入费用", count, err)
	}
	if _, err = db.Exec("DROP TRIGGER reject_probe_audit"); err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.UpdateProvider(ctx, p.ID, ProviderPatch{IsEnabled: Field[bool]{Set: true, Value: false}}, "user"); err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.Probe(ctx, p.ID, "configured", "user", nil, factory); err == nil {
		t.Fatal("停用供应商仍被调用")
	}
	if err = db.QueryRow("SELECT COUNT(*) FROM usage_events").Scan(&count); err != nil || count != 3 {
		t.Fatal("未调用仍计费", count, err)
	}
}
