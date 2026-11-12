package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/secret"
	"io.yueyang/tom/internal/store"
)

type ProviderPreset struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

func ProviderPresets() []ProviderPreset {
	return []ProviderPreset{{ID: "deepseek", DisplayName: "DeepSeek"}}
}

type ProviderConnectInput struct {
	Preset     string `json:"preset"`
	APIKey     string `json:"api_key"`
	ProviderID string `json:"provider_id"`
}

type ProviderConnectResult struct {
	Provider           domain.Provider `json:"provider"`
	Models             []domain.Model  `json:"models"`
	ModelLimitDefaults []string        `json:"model_limit_defaults"`
}

// providerPreset 只向客户端暴露服务端支持的预设，旧手工录入的同端点供应商也可直接重新同步。
func (c *Catalog) providerPreset(p domain.Provider) domain.Provider {
	if p.Kind == "openai_compat" && (p.Preset == "deepseek" || p.BaseURL == c.deepSeekBaseURL || p.BaseURL == c.deepSeekBaseURL+"/v1") {
		p.Preset = "deepseek"
	} else {
		p.Preset = ""
	}
	return p
}

func connectionChanged() error {
	return domain.NewError(409, "PROVIDER_CONNECTION_CHANGED", "同步期间供应商或密钥已更新，请重新同步。")
}

// ConnectProvider 先通过真实模型发现验证密钥，再原子提交密钥、供应商及模型；不自动重试写操作。
func (c *Catalog) ConnectProvider(ctx context.Context, input ProviderConnectInput, actor string, secrets *secret.Manager) (result ProviderConnectResult, err error) {
	if input.Preset != "deepseek" {
		return result, domain.NewError(400, "PROVIDER_PRESET_INVALID", "请选择支持的供应商。")
	}
	if secrets == nil {
		return result, domain.NewError(500, "SECRET_UNAVAILABLE", "密钥服务不可用。")
	}
	if err = ValidateProviderURL(c.deepSeekBaseURL); err != nil {
		return result, err
	}
	if c.deepSeekContextLimit < 256 || c.deepSeekContextLimit > 10000000 || c.deepSeekOutputLimit < 1 || c.deepSeekOutputLimit > c.deepSeekContextLimit {
		return result, badInput("DeepSeek 应用运行限额无效。")
	}
	input.APIKey = strings.TrimSpace(input.APIKey)
	if input.APIKey != "" && (len(input.APIKey) < 8 || len(input.APIKey) > 16<<10 || strings.ContainsFunc(input.APIKey, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })) {
		return result, domain.NewError(400, "SECRET_INVALID", "请输入有效的 API Key。")
	}
	var original domain.Provider
	if input.ProviderID == "" {
		original, err = scanProvider(c.db.QueryRowContext(ctx, "SELECT "+providerColumns+" FROM providers WHERE kind='openai_compat' AND (base_url IN (?,?) OR json_extract(extra_json,'$.preset')='deepseek') ORDER BY created_at,id LIMIT 1", c.deepSeekBaseURL, c.deepSeekBaseURL+"/v1"))
		var missing *domain.Error
		if errors.As(err, &missing) && missing.Code == "NOT_FOUND" {
			err = nil
		}
	} else {
		original, err = c.Provider(ctx, input.ProviderID)
	}
	if err != nil {
		return result, err
	}
	original = c.providerPreset(original)
	if original.ID != "" && original.Preset != "deepseek" {
		return result, domain.NewError(400, "PROVIDER_PRESET_MISMATCH", "该供应商不属于 DeepSeek，无法通过快速接入修改。")
	}
	if input.APIKey == "" && (original.SecretID == nil || *original.SecretID == "") {
		return result, domain.NewError(400, "SECRET_REQUIRED", "请输入 API Key。")
	}
	var originalNonce []byte
	if original.SecretID != nil {
		if err = c.db.QueryRowContext(ctx, "SELECT nonce FROM secrets WHERE id=?", *original.SecretID).Scan(&originalNonce); err != nil {
			return result, notFound(err)
		}
	}
	var discovered []provider.DiscoveredModel
	discover := func(credential []byte) error {
		var err error
		discovered, err = provider.DiscoverDeepSeek(ctx, c.deepSeekBaseURL, credential, c.discoveryTimeout)
		return err
	}
	if input.APIKey != "" {
		credential := []byte(input.APIKey)
		err = discover(credential)
		clear(credential)
	} else {
		err = secrets.Use(ctx, *original.SecretID, actor, "", discover)
	}
	if err != nil {
		return result, err
	}
	err = store.Transact(ctx, c.db, func(tx *sql.Tx) error {
		p := original
		isNew := p.ID == ""
		if isNew {
			p = domain.Provider{ID: "deepseek", Preset: "deepseek", Kind: "openai_compat", DisplayName: "DeepSeek", BaseURL: c.deepSeekBaseURL, IsEnabled: true, CreatedAt: time.Now().UTC()}
		} else {
			current, err := scanProvider(tx.QueryRowContext(ctx, "SELECT "+providerColumns+" FROM providers WHERE id=?", p.ID))
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(c.providerPreset(current), original) {
				return connectionChanged()
			}
			if p.SecretID != nil {
				var nonce []byte
				if err = tx.QueryRowContext(ctx, "SELECT nonce FROM secrets WHERE id=?", *p.SecretID).Scan(&nonce); err != nil {
					return notFound(err)
				}
				if !bytes.Equal(nonce, originalNonce) {
					return connectionChanged()
				}
			}
		}
		if input.APIKey != "" {
			var references int
			if p.SecretID != nil {
				if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM providers WHERE secret_id=?", *p.SecretID).Scan(&references); err != nil {
					return err
				}
			}
			if p.SecretID != nil && references == 1 {
				if err := secrets.RotateInTransaction(ctx, tx, *p.SecretID, input.APIKey, actor); err != nil {
					return err
				}
			} else {
				metadata, err := secrets.CreateInTransaction(ctx, tx, "DeepSeek", input.APIKey, actor)
				if err != nil {
					return err
				}
				p.SecretID = &metadata.ID
			}
		}
		p.BaseURL, p.Preset, p.IsEnabled = c.deepSeekBaseURL, "deepseek", true
		if isNew {
			if _, err := tx.ExecContext(ctx, "INSERT INTO providers(id,kind,display_name,base_url,secret_id,is_enabled,created_at) VALUES(?,?,?,?,?,?,?)", p.ID, p.Kind, p.DisplayName, p.BaseURL, p.SecretID, p.IsEnabled, p.CreatedAt.UnixMilli()); err != nil {
				return store.ConstraintError(err)
			}
		}
		models, defaults, err := c.syncDiscoveredModels(ctx, tx, p, discovered, actor)
		if err != nil {
			return err
		}
		p.ModelLimitDefaults = defaults
		extra, err := json.Marshal(map[string]any{"preset": "deepseek", "model_limit_defaults": defaults})
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE providers SET base_url=?,secret_id=?,is_enabled=1,extra_json=? WHERE id=?", p.BaseURL, p.SecretID, string(extra), p.ID); err != nil {
			return store.ConstraintError(err)
		}
		if err = store.Audit(ctx, tx, actor, "provider.connect", p.ID, map[string]any{"preset": "deepseek", "model_count": len(models)}); err != nil {
			return err
		}
		result = ProviderConnectResult{Provider: p, Models: models, ModelLimitDefaults: defaults}
		return nil
	})
	return result, err
}

// syncDiscoveredModels 保留模型 ID、显式价格和已有默认选择；已下架但仍被引用的模型要求先解除引用。
func (c *Catalog) syncDiscoveredModels(ctx context.Context, tx *sql.Tx, p domain.Provider, discovered []provider.DiscoveredModel, actor string) ([]domain.Model, []string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT "+modelColumns+" FROM models WHERE provider_id=? LIMIT 201", p.ID)
	if err != nil {
		return nil, nil, err
	}
	existing := map[string]domain.Model{}
	for rows.Next() {
		m, scanErr := scanModel(rows)
		if scanErr != nil {
			return nil, nil, errors.Join(scanErr, rows.Close())
		}
		existing[m.ModelKey] = m
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return nil, nil, err
	}
	if len(existing) > 200 {
		return nil, nil, badInput("该供应商的模型数量超出单次同步上限。")
	}
	keys := make(map[string]bool, len(discovered))
	for _, model := range discovered {
		keys[model.ID] = true
	}
	removedIDs := []any{}
	for key, model := range existing {
		if !keys[key] {
			removedIDs = append(removedIDs, model.ID)
		}
	}
	if len(removedIDs) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(removedIDs)), ",")
		arguments := append(append([]any{}, removedIDs...), removedIDs...)
		var referenced bool
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM bots WHERE model_id IN ("+placeholders+") UNION ALL SELECT 1 FROM settings WHERE key='reviewer' AND json_extract(value_json,'$.model_id') IN ("+placeholders+"))", arguments...).Scan(&referenced); err != nil {
			return nil, nil, err
		}
		if referenced {
			return nil, nil, domain.NewError(409, "PROVIDER_MODEL_IN_USE", "供应商已移除仍被 Bot 或审查设置使用的模型，请先修改模型引用再同步。")
		}
	}
	for key, model := range existing {
		if keys[key] {
			continue
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM models WHERE id=?", model.ID); err != nil {
			return nil, nil, store.ConstraintError(err)
		}
		if err = store.Audit(ctx, tx, actor, "model.delete", model.ID, map[string]string{"provider_id": p.ID}); err != nil {
			return nil, nil, err
		}
	}
	var hasDefault bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM models WHERE is_default=1)").Scan(&hasDefault); err != nil {
		return nil, nil, err
	}
	models := make([]domain.Model, 0, len(discovered))
	defaults := []string{}
	for _, offered := range discovered {
		model, found := existing[offered.ID]
		if !found {
			model = domain.Model{ID: ids.New(), ProviderID: p.ID, ModelKey: offered.ID, ContextWindow: c.deepSeekContextLimit, MaxOutput: c.deepSeekOutputLimit, IsPriceEstimated: true, CreatedAt: time.Now().UTC()}
		}
		model.DisplayName = offered.Name
		model.SupportsTools = true
		model.SupportsVision = slices.Contains(offered.InputModalities, "image")
		if offered.ContextWindow != nil {
			model.ContextWindow = *offered.ContextWindow
		}
		if offered.MaxOutputTokens != nil {
			model.MaxOutput = *offered.MaxOutputTokens
			if offered.ContextWindow == nil && !found {
				model.MaxOutput = min(model.MaxOutput, c.deepSeekOutputLimit)
			}
		}
		model.MaxOutput = min(model.MaxOutput, model.ContextWindow)
		if offered.ContextWindow == nil || offered.MaxOutputTokens == nil {
			if !found || slices.Contains(p.ModelLimitDefaults, offered.ID) {
				defaults = append(defaults, offered.ID)
			}
		}
		selectedDefault := !hasDefault
		if selectedDefault {
			model.IsDefault, hasDefault = true, true
		}
		if err = validateModel(&model); err != nil {
			return nil, nil, err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO models("+modelColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(provider_id,model_key) DO UPDATE SET display_name=excluded.display_name,context_window=excluded.context_window,max_output=excluded.max_output,supports_tools=excluded.supports_tools,supports_vision=excluded.supports_vision,is_default=excluded.is_default", model.ID, model.ProviderID, model.ModelKey, model.DisplayName, model.ContextWindow, model.MaxOutput, model.InputPriceMicro, model.OutputPriceMicro, model.CacheReadPriceMicro, model.SupportsTools, model.SupportsVision, model.IsDefault, model.CreatedAt.UnixMilli(), model.IsPriceEstimated)
		if err != nil {
			return nil, nil, store.ConstraintError(err)
		}
		if selectedDefault {
			if err = bindDefaultBotModel(ctx, tx, model.ID, actor); err != nil {
				return nil, nil, err
			}
		}
		if err = store.Audit(ctx, tx, actor, "model.sync", model.ID, map[string]string{"provider_id": p.ID}); err != nil {
			return nil, nil, err
		}
		models = append(models, model)
	}
	return models, defaults, nil
}
