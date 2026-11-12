package service

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/secret"
	ledger "io.yueyang/tom/internal/usage"
)

type ProbeResult struct {
	LatencyMS int64          `json:"latency_ms"`
	Usage     provider.Usage `json:"usage"`
}
type ProviderFactory func(domain.Provider, []byte) provider.Provider

// Probe 使用真实最小生成请求确认连通与鉴权；不保存测试文本或凭据。
func (c *Catalog) Probe(ctx context.Context, id, modelKey, actor string, secrets *secret.Manager, factory ProviderFactory) (ProbeResult, error) {
	started := time.Now()
	p, err := c.Provider(ctx, id)
	if err != nil {
		return ProbeResult{}, err
	}
	if !p.IsEnabled {
		return ProbeResult{}, domain.NewError(400, "PROVIDER_DISABLED", "Enable the provider before testing.")
	}
	if modelKey == "" {
		err = c.db.QueryRowContext(ctx, "SELECT model_key FROM models WHERE provider_id=? ORDER BY is_default DESC,id DESC LIMIT 1", id).Scan(&modelKey)
		if errors.Is(err, sql.ErrNoRows) {
			return ProbeResult{}, domain.NewError(400, "MODEL_NOT_CONFIGURED", "Add a model or provide a model key to test.")
		}
		if err != nil {
			return ProbeResult{}, err
		}
	}
	if len(modelKey) > 200 {
		return ProbeResult{}, badInput("Model key is too long.")
	}
	model := domain.Model{ID: "probe:" + id, ModelKey: modelKey, IsPriceEstimated: true}
	var modelID string
	err = c.db.QueryRowContext(ctx, "SELECT id FROM models WHERE provider_id=? AND model_key=? ORDER BY id LIMIT 1", id, modelKey).Scan(&modelID)
	if err == nil {
		model, err = c.Model(ctx, modelID)
		if err != nil {
			return ProbeResult{}, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ProbeResult{}, err
	}
	// 已配置模型提供价格快照，但连通性请求始终只要求一个输出 token。
	model.MaxOutput = 1
	timeout, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var usage provider.Usage
	called := false
	execute := func(credential []byte) error {
		var err error
		called = true
		usage, err = factory(p, credential).Stream(timeout, provider.Request{Model: model, Messages: []provider.Message{{Role: "user", Blocks: []domain.MessageBlock{{Kind: "text", Text: "Reply briefly."}}}}}, func(provider.Event) error { return nil })
		return err
	}
	if p.SecretID != nil {
		err = secrets.Use(timeout, *p.SecretID, actor, "", execute)
	} else {
		err = execute(nil)
	}
	result := ProbeResult{LatencyMS: time.Since(started).Milliseconds(), Usage: usage}
	if called {
		persist, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		err = errors.Join(err, ledger.RecordProbe(persist, c.db, model, usage, id, actor, result.LatencyMS))
	}
	return result, err
}
