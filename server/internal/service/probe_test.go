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
	if model.ID == "" {
		t.Fatal("已配置模型丢失")
	}
	if _, err = catalog.Probe(ctx, p.ID, "unconfigured", "user", nil, factory); err != nil {
		t.Fatal(err)
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
	if _, err = catalog.UpdateProvider(ctx, p.ID, ProviderPatch{IsEnabled: Field[bool]{Set: true, Value: false}}, "user"); err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.Probe(ctx, p.ID, "configured", "user", nil, factory); err == nil {
		t.Fatal("停用供应商仍被调用")
	}
}
