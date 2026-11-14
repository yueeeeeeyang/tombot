package modelregistry

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"io.yueyang/tom/internal/identity"
)

// ProbeResult 只返回固定分类与耗时，禁止包含上游正文、提示词或凭据。
type ProbeResult struct {
	Status     string `json:"status"`
	Message    string `json:"message"`
	DurationMS int64  `json:"duration_ms"`
}

// ModelTestResult 对应一次目录修订的即时测试，不修改模型能力或策略启用状态。
type ModelTestResult struct {
	ModelID          string      `json:"model_id"`
	ProviderRevision int64       `json:"provider_revision"`
	ModelRevision    int64       `json:"model_revision"`
	TestedAt         time.Time   `json:"tested_at"`
	Connectivity     ProbeResult `json:"connectivity"`
	Tools            ProbeResult `json:"tools"`
}

type modelTestSnapshot struct {
	providerID, credentialID        string
	providerRevision, modelRevision int64
	configuration                   Configuration
}

// readModelTestSnapshot 锁定本次读取的目录及凭据元数据；网络调用前必须结束事务。
func readModelTestSnapshot(ctx context.Context, tx *sql.Tx, modelID string) (modelTestSnapshot, error) {
	var snapshot modelTestSnapshot
	configuration := &snapshot.configuration
	err := tx.QueryRowContext(ctx, `SELECT p.id,p.revision,m.revision,p.protocol,p.base_url,m.model_key,m.context_limit,m.output_limit,m.reasoning_effort
	 FROM tombot.models m JOIN tombot.model_providers p ON p.id=m.provider_id WHERE m.id=$1 FOR SHARE OF p,m`, modelID).Scan(&snapshot.providerID, &snapshot.providerRevision, &snapshot.modelRevision, &configuration.Protocol, &configuration.BaseURL, &configuration.ModelKey, &configuration.ContextLimit, &configuration.OutputLimit, &configuration.ReasoningEffort)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshot, ErrNotFound
	}
	if err != nil {
		return snapshot, err
	}
	err = tx.QueryRowContext(ctx, `SELECT id FROM tombot.model_credentials WHERE provider_id=$1 AND revoked_at IS NULL AND destroyed_at IS NULL ORDER BY version DESC LIMIT 1 FOR SHARE`, snapshot.providerID).Scan(&snapshot.credentialID)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshot, ErrCredential
	}
	// 即使目录尚未声明工具能力，也允许管理员验证；结果不会自动修改声明。
	configuration.SupportsTools = true
	configuration.OutputLimit = min(configuration.OutputLimit, 1024)
	return snapshot, err
}

// TestModel 使用最新有效凭据测试连通性和工具调用，允许停用目录在启用前验证。
// 要求近期管理员验证及目录修订匹配；探测期间不持锁，结束后复验目录和凭据并写入审计。
func (service *Service) TestModel(ctx context.Context, actor identity.Principal, requestID, modelID string, providerRevision, modelRevision int64) (ModelTestResult, error) {
	if err := service.authorize(ctx, actor, requestID); err != nil {
		return ModelTestResult{}, err
	}
	if !validID(modelID) || providerRevision < 1 || modelRevision < 1 {
		return ModelTestResult{}, ErrInvalidInput
	}
	var initial modelTestSnapshot
	var credential []byte
	defer func() { clear(credential) }()
	err := service.transaction(ctx, func(tx *sql.Tx) error {
		var err error
		initial, err = readModelTestSnapshot(ctx, tx, modelID)
		if err != nil {
			return err
		}
		if initial.providerRevision != providerRevision || initial.modelRevision != modelRevision {
			return ErrConflict
		}
		credential, err = service.decrypt(ctx, tx, Candidate{ProviderID: initial.providerID, CredentialID: &initial.credentialID})
		return err
	})
	if err != nil {
		return ModelTestResult{}, err
	}
	probeContext, cancel := context.WithTimeout(ctx, service.options.VerificationTimeout)
	defer cancel()
	probe := func(kind, label string) ProbeResult {
		start := time.Now()
		err := service.verifier.Probe(probeContext, initial.configuration, credential, kind)
		result := ProbeResult{Status: "succeeded", Message: label + "测试通过", DurationMS: time.Since(start).Milliseconds()}
		if err != nil || probeContext.Err() != nil {
			result.Status = "failed"
			result.Message = label + "测试失败，请检查供应商配置、凭据和模型能力"
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(probeContext.Err(), context.DeadlineExceeded) {
				result.Message = label + "测试超时"
			}
		}
		return result
	}
	result := ModelTestResult{ModelID: modelID, ProviderRevision: providerRevision, ModelRevision: modelRevision}
	result.Connectivity = probe("connectivity", "连通性")
	result.Tools = ProbeResult{Status: "skipped", Message: "连通性未通过，未执行工具调用测试"}
	if result.Connectivity.Status == "succeeded" {
		result.Tools = probe("tools", "工具调用")
	}
	clear(credential)
	credential = nil
	if err = ctx.Err(); err != nil {
		return ModelTestResult{}, err
	}
	if err = service.authorize(ctx, actor, requestID); err != nil {
		return ModelTestResult{}, err
	}
	err = service.transaction(ctx, func(tx *sql.Tx) error {
		current, err := readModelTestSnapshot(ctx, tx, modelID)
		if err != nil {
			return err
		}
		if current != initial {
			return ErrConflict
		}
		outcome := "succeeded"
		if result.Connectivity.Status != "succeeded" || result.Tools.Status != "succeeded" {
			outcome = "failed"
		}
		return auditOutcome(ctx, tx, actor, requestID, "测试模型连通性和工具调用", modelID, outcome, map[string]any{"provider_id": initial.providerID, "provider_revision": providerRevision, "model_revision": modelRevision, "connectivity": result.Connectivity.Status, "tools": result.Tools.Status})
	})
	if err != nil {
		return ModelTestResult{}, err
	}
	result.TestedAt = time.Now().UTC()
	return result, nil
}
