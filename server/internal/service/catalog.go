package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"io.yueyang/tom/internal/config"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store"
)

// Field 保留 PATCH 的未提供、清空与赋值三种语义。
type Field[T any] struct {
	Set   bool
	Value T
}

func (f *Field[T]) UnmarshalJSON(encoded []byte) error {
	f.Set = true
	return json.Unmarshal(encoded, &f.Value)
}

type ProviderInput struct {
	Kind        string  `json:"kind"`
	DisplayName string  `json:"display_name"`
	BaseURL     string  `json:"base_url"`
	SecretID    *string `json:"secret_id"`
	IsEnabled   *bool   `json:"is_enabled"`
}
type ProviderPatch struct {
	Kind        Field[string]  `json:"kind"`
	DisplayName Field[string]  `json:"display_name"`
	BaseURL     Field[string]  `json:"base_url"`
	SecretID    Field[*string] `json:"secret_id"`
	IsEnabled   Field[bool]    `json:"is_enabled"`
}
type ModelInput struct {
	ProviderID          string `json:"provider_id"`
	ModelKey            string `json:"model_key"`
	DisplayName         string `json:"display_name"`
	ContextWindow       int    `json:"context_window"`
	MaxOutput           int    `json:"max_output"`
	InputPriceMicro     *int64 `json:"input_price_micro"`
	OutputPriceMicro    *int64 `json:"output_price_micro"`
	CacheReadPriceMicro *int64 `json:"cache_read_price_micro"`
	SupportsTools       *bool  `json:"supports_tools"`
	SupportsVision      *bool  `json:"supports_vision"`
	IsDefault           bool   `json:"is_default"`
}
type ModelPatch struct {
	ProviderID          Field[string] `json:"provider_id"`
	ModelKey            Field[string] `json:"model_key"`
	DisplayName         Field[string] `json:"display_name"`
	ContextWindow       Field[int]    `json:"context_window"`
	MaxOutput           Field[int]    `json:"max_output"`
	InputPriceMicro     Field[*int64] `json:"input_price_micro"`
	OutputPriceMicro    Field[*int64] `json:"output_price_micro"`
	CacheReadPriceMicro Field[*int64] `json:"cache_read_price_micro"`
	SupportsTools       Field[bool]   `json:"supports_tools"`
	SupportsVision      Field[bool]   `json:"supports_vision"`
	IsDefault           Field[bool]   `json:"is_default"`
}

// Catalog 管理供应商与模型，不负责执行模型调用。
type Catalog struct {
	db                                        *sql.DB
	deepSeekBaseURL                           string
	discoveryTimeout                          time.Duration
	deepSeekContextLimit, deepSeekOutputLimit int
}

// CatalogOptions 把供应商部署地址和发现时限注入服务，测试不访问真实供应商。
type CatalogOptions struct {
	DeepSeekBaseURL                           string
	DiscoveryTimeout                          time.Duration
	DeepSeekContextLimit, DeepSeekOutputLimit int
}

func NewCatalog(db *sql.DB, options ...CatalogOptions) *Catalog {
	c := &Catalog{db: db, deepSeekBaseURL: config.DefaultDeepSeekBaseURL, discoveryTimeout: 15 * time.Second}
	c.deepSeekContextLimit, c.deepSeekOutputLimit = config.DefaultDeepSeekContextLimit, config.DefaultDeepSeekOutputLimit
	if len(options) > 0 {
		if options[0].DeepSeekBaseURL != "" {
			c.deepSeekBaseURL = strings.TrimRight(options[0].DeepSeekBaseURL, "/")
		}
		if options[0].DiscoveryTimeout > 0 {
			c.discoveryTimeout = min(options[0].DiscoveryTimeout, 30*time.Second)
		}
		if options[0].DeepSeekContextLimit > 0 {
			c.deepSeekContextLimit = options[0].DeepSeekContextLimit
		}
		if options[0].DeepSeekOutputLimit > 0 {
			c.deepSeekOutputLimit = options[0].DeepSeekOutputLimit
		}
	}
	return c
}
func badInput(message string) error { return domain.NewError(400, "INVALID_INPUT", message) }
func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return domain.NewError(404, "NOT_FOUND", "The resource does not exist.")
	}
	return err
}
func boolean(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}
func price(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

// ValidateProviderURL 禁止 URL 凭据和查询参数；明文只用于本机无鉴权模型。
func ValidateProviderURL(address string) error {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return badInput("Provide a provider base URL without credentials or query parameters.")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	ip := net.ParseIP(parsed.Hostname())
	if parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())) {
		return nil
	}
	return badInput("Provider URLs must use HTTPS, except loopback endpoints.")
}
func validateProvider(p *domain.Provider) error {
	p.DisplayName = strings.TrimSpace(p.DisplayName)
	p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if p.Kind != "anthropic" && p.Kind != "openai_compat" {
		return badInput("Unsupported provider kind.")
	}
	if p.DisplayName == "" || len(p.DisplayName) > 120 {
		return badInput("Provide a display name of at most 120 bytes.")
	}
	return ValidateProviderURL(p.BaseURL)
}
func validateModel(m *domain.Model) error {
	m.ModelKey = strings.TrimSpace(m.ModelKey)
	m.DisplayName = strings.TrimSpace(m.DisplayName)
	if m.ProviderID == "" || m.ModelKey == "" || len(m.ModelKey) > 200 || m.DisplayName == "" || len(m.DisplayName) > 120 {
		return badInput("Provider, model key and display name are required.")
	}
	if m.ContextWindow < 256 || m.ContextWindow > 10000000 || m.MaxOutput < 1 || m.MaxOutput > m.ContextWindow {
		return badInput("Model context and output limits are invalid.")
	}
	for _, value := range []int64{m.InputPriceMicro, m.OutputPriceMicro, price(m.CacheReadPriceMicro)} {
		if value < 0 || value > 1000000000000 {
			return badInput("Model prices must be nonnegative and at most 1000000000000 micro-USD per million tokens.")
		}
	}
	return nil
}

type scanner interface{ Scan(...any) error }

const providerColumns = "id,kind,display_name,COALESCE(base_url,''),secret_id,is_enabled,created_at,extra_json"

func scanProvider(row scanner) (domain.Provider, error) {
	var p domain.Provider
	var created int64
	var extra string
	err := row.Scan(&p.ID, &p.Kind, &p.DisplayName, &p.BaseURL, &p.SecretID, &p.IsEnabled, &created, &extra)
	p.CreatedAt = time.UnixMilli(created).UTC()
	if err == nil {
		var metadata struct {
			Preset             string   `json:"preset"`
			ModelLimitDefaults []string `json:"model_limit_defaults"`
		}
		if err = json.Unmarshal([]byte(extra), &metadata); err == nil {
			p.Preset, p.ModelLimitDefaults = metadata.Preset, metadata.ModelLimitDefaults
		}
	}
	return p, notFound(err)
}

const modelColumns = "id,provider_id,model_key,display_name,context_window,max_output,input_price_micro,output_price_micro,cache_read_price_micro,supports_tools,supports_vision,is_default,created_at,is_price_estimated"

func scanModel(row scanner) (domain.Model, error) {
	var m domain.Model
	var created int64
	err := row.Scan(&m.ID, &m.ProviderID, &m.ModelKey, &m.DisplayName, &m.ContextWindow, &m.MaxOutput, &m.InputPriceMicro, &m.OutputPriceMicro, &m.CacheReadPriceMicro, &m.SupportsTools, &m.SupportsVision, &m.IsDefault, &created, &m.IsPriceEstimated)
	m.CreatedAt = time.UnixMilli(created).UTC()
	return m, notFound(err)
}
func (c *Catalog) Provider(ctx context.Context, id string) (domain.Provider, error) {
	p, err := scanProvider(c.db.QueryRowContext(ctx, "SELECT "+providerColumns+" FROM providers WHERE id=?", id))
	return c.providerPreset(p), err
}
func (c *Catalog) Model(ctx context.Context, id string) (domain.Model, error) {
	return scanModel(c.db.QueryRowContext(ctx, "SELECT "+modelColumns+" FROM models WHERE id=?", id))
}
func (c *Catalog) Providers(ctx context.Context, cursor string, limit int) ([]domain.Provider, error) {
	rows, err := c.db.QueryContext(ctx, "SELECT "+providerColumns+" FROM providers WHERE (?='' OR id<?) ORDER BY id DESC LIMIT ?", cursor, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []domain.Provider{}
	for rows.Next() {
		value, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, c.providerPreset(value))
	}
	return values, rows.Err()
}
func (c *Catalog) Models(ctx context.Context, cursor string, limit int) ([]domain.Model, error) {
	rows, err := c.db.QueryContext(ctx, "SELECT "+modelColumns+" FROM models WHERE (?='' OR id<?) ORDER BY id DESC LIMIT ?", cursor, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []domain.Model{}
	for rows.Next() {
		value, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
func (c *Catalog) CreateProvider(ctx context.Context, input ProviderInput, actor string) (domain.Provider, error) {
	p := domain.Provider{ID: ids.New(), Kind: input.Kind, DisplayName: input.DisplayName, BaseURL: input.BaseURL, SecretID: input.SecretID, IsEnabled: boolean(input.IsEnabled, true), CreatedAt: time.Now().UTC()}
	if err := validateProvider(&p); err != nil {
		return p, err
	}
	err := store.Transact(ctx, c.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO providers(id,kind,display_name,base_url,secret_id,is_enabled,created_at) VALUES(?,?,?,?,?,?,?)", p.ID, p.Kind, p.DisplayName, p.BaseURL, p.SecretID, p.IsEnabled, p.CreatedAt.UnixMilli()); err != nil {
			return store.ConstraintError(err)
		}
		return store.Audit(ctx, tx, actor, "provider.create", p.ID, map[string]string{"kind": p.Kind})
	})
	return c.providerPreset(p), err
}
func (c *Catalog) UpdateProvider(ctx context.Context, id string, patch ProviderPatch, actor string) (domain.Provider, error) {
	var p domain.Provider
	err := store.Transact(ctx, c.db, func(tx *sql.Tx) error {
		var err error
		p, err = scanProvider(tx.QueryRowContext(ctx, "SELECT "+providerColumns+" FROM providers WHERE id=?", id))
		if err != nil {
			return err
		}
		originalKind, originalURL := p.Kind, p.BaseURL
		if patch.Kind.Set {
			p.Kind = patch.Kind.Value
		}
		if patch.DisplayName.Set {
			p.DisplayName = patch.DisplayName.Value
		}
		if patch.BaseURL.Set {
			p.BaseURL = patch.BaseURL.Value
		}
		if patch.SecretID.Set {
			p.SecretID = patch.SecretID.Value
		}
		if patch.IsEnabled.Set {
			p.IsEnabled = patch.IsEnabled.Value
		}
		if err = validateProvider(&p); err != nil {
			return err
		}
		if p.Kind != originalKind || p.BaseURL != originalURL {
			// 通用 API 改换协议或地址后，原预设的专用请求参数不能继续影响新供应商。
			if _, err = tx.ExecContext(ctx, "UPDATE providers SET extra_json=json_remove(extra_json,'$.preset','$.model_limit_defaults') WHERE id=?", id); err != nil {
				return err
			}
			p.Preset, p.ModelLimitDefaults = "", nil
		}
		if _, err = tx.ExecContext(ctx, "UPDATE providers SET kind=?,display_name=?,base_url=?,secret_id=?,is_enabled=? WHERE id=?", p.Kind, p.DisplayName, p.BaseURL, p.SecretID, p.IsEnabled, id); err != nil {
			return store.ConstraintError(err)
		}
		return store.Audit(ctx, tx, actor, "provider.update", id, map[string]string{})
	})
	return c.providerPreset(p), err
}
func (c *Catalog) DeleteProvider(ctx context.Context, id, actor string) error {
	return store.Transact(ctx, c.db, func(tx *sql.Tx) error {
		if err := store.RequireChanged(tx.ExecContext(ctx, "DELETE FROM providers WHERE id=?", id)); err != nil {
			return err
		}
		return store.Audit(ctx, tx, actor, "provider.delete", id, map[string]string{})
	})
}
func (c *Catalog) CreateModel(ctx context.Context, input ModelInput, actor string) (domain.Model, error) {
	m := domain.Model{ID: ids.New(), ProviderID: input.ProviderID, ModelKey: input.ModelKey, DisplayName: input.DisplayName, ContextWindow: input.ContextWindow, MaxOutput: input.MaxOutput, InputPriceMicro: price(input.InputPriceMicro), OutputPriceMicro: price(input.OutputPriceMicro), CacheReadPriceMicro: input.CacheReadPriceMicro, SupportsTools: boolean(input.SupportsTools, true), SupportsVision: boolean(input.SupportsVision, true), IsDefault: input.IsDefault, IsPriceEstimated: input.InputPriceMicro == nil || input.OutputPriceMicro == nil, CreatedAt: time.Now().UTC()}
	if err := validateModel(&m); err != nil {
		return m, err
	}
	err := store.Transact(ctx, c.db, func(tx *sql.Tx) error {
		if m.IsDefault {
			if _, err := tx.ExecContext(ctx, "UPDATE models SET is_default=0 WHERE is_default=1"); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO models("+modelColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)", m.ID, m.ProviderID, m.ModelKey, m.DisplayName, m.ContextWindow, m.MaxOutput, m.InputPriceMicro, m.OutputPriceMicro, m.CacheReadPriceMicro, m.SupportsTools, m.SupportsVision, m.IsDefault, m.CreatedAt.UnixMilli(), m.IsPriceEstimated); err != nil {
			return store.ConstraintError(err)
		}
		if m.IsDefault {
			if err := bindDefaultBotModel(ctx, tx, m.ID, actor); err != nil {
				return err
			}
		}
		return store.Audit(ctx, tx, actor, "model.create", m.ID, map[string]string{"provider_id": m.ProviderID})
	})
	return m, err
}
func (c *Catalog) UpdateModel(ctx context.Context, id string, p ModelPatch, actor string) (domain.Model, error) {
	var m domain.Model
	err := store.Transact(ctx, c.db, func(tx *sql.Tx) error {
		var err error
		m, err = scanModel(tx.QueryRowContext(ctx, "SELECT "+modelColumns+" FROM models WHERE id=?", id))
		if err != nil {
			return err
		}
		if p.ProviderID.Set {
			m.ProviderID = p.ProviderID.Value
		}
		if p.ModelKey.Set {
			m.ModelKey = p.ModelKey.Value
		}
		if p.DisplayName.Set {
			m.DisplayName = p.DisplayName.Value
		}
		if p.ContextWindow.Set {
			m.ContextWindow = p.ContextWindow.Value
		}
		if p.MaxOutput.Set {
			m.MaxOutput = p.MaxOutput.Value
		}
		if p.InputPriceMicro.Set {
			m.InputPriceMicro = price(p.InputPriceMicro.Value)
		}
		if p.OutputPriceMicro.Set {
			m.OutputPriceMicro = price(p.OutputPriceMicro.Value)
		}
		if p.CacheReadPriceMicro.Set {
			m.CacheReadPriceMicro = p.CacheReadPriceMicro.Value
		}
		if (p.InputPriceMicro.Set && p.InputPriceMicro.Value == nil) || (p.OutputPriceMicro.Set && p.OutputPriceMicro.Value == nil) {
			m.IsPriceEstimated = true
		} else if p.InputPriceMicro.Set && p.OutputPriceMicro.Set {
			m.IsPriceEstimated = false
		}
		if p.SupportsTools.Set {
			m.SupportsTools = p.SupportsTools.Value
		}
		if p.SupportsVision.Set {
			m.SupportsVision = p.SupportsVision.Value
		}
		if p.IsDefault.Set {
			m.IsDefault = p.IsDefault.Value
		}
		if err = validateModel(&m); err != nil {
			return err
		}
		if m.IsDefault {
			if _, err = tx.ExecContext(ctx, "UPDATE models SET is_default=0 WHERE is_default=1"); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE models SET provider_id=?,model_key=?,display_name=?,context_window=?,max_output=?,input_price_micro=?,output_price_micro=?,cache_read_price_micro=?,supports_tools=?,supports_vision=?,is_default=?,is_price_estimated=? WHERE id=?", m.ProviderID, m.ModelKey, m.DisplayName, m.ContextWindow, m.MaxOutput, m.InputPriceMicro, m.OutputPriceMicro, m.CacheReadPriceMicro, m.SupportsTools, m.SupportsVision, m.IsDefault, m.IsPriceEstimated, id); err != nil {
			return store.ConstraintError(err)
		}
		if p.IsDefault.Set && m.IsDefault {
			if err = bindDefaultBotModel(ctx, tx, m.ID, actor); err != nil {
				return err
			}
		}
		if p.ContextWindow.Set && p.MaxOutput.Set {
			if _, err = tx.ExecContext(ctx, `UPDATE providers SET extra_json=json_set(extra_json,'$.model_limit_defaults',json((SELECT json_group_array(value) FROM json_each(providers.extra_json,'$.model_limit_defaults') WHERE value<>?))) WHERE id=? AND json_extract(extra_json,'$.preset')='deepseek'`, m.ModelKey, m.ProviderID); err != nil {
				return err
			}
		}
		return store.Audit(ctx, tx, actor, "model.update", id, map[string]string{})
	})
	return m, err
}
func (c *Catalog) DeleteModel(ctx context.Context, id, actor string) error {
	return store.Transact(ctx, c.db, func(tx *sql.Tx) error {
		var referenced bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM settings WHERE key='reviewer' AND json_extract(value_json,'$.model_id')=?)`, id).Scan(&referenced); err != nil {
			return err
		}
		if referenced {
			return domain.NewError(409, "REVIEWER_MODEL_IN_USE", "Change the reviewer model setting before deleting this model.")
		}
		if err := store.RequireChanged(tx.ExecContext(ctx, "DELETE FROM models WHERE id=?", id)); err != nil {
			return err
		}
		return store.Audit(ctx, tx, actor, "model.delete", id, map[string]string{})
	})
}

// ReviewModel 优先选择不同模型键的已启用模型；固定配置不可悄悄换成其他模型。
func (c *Catalog) ReviewModel(ctx context.Context, executionID string, preferredID *string) (domain.Model, domain.Provider, bool, error) {
	execution, err := c.Model(ctx, executionID)
	if err != nil {
		return domain.Model{}, domain.Provider{}, false, err
	}
	selected := execution
	if preferredID != nil {
		selected, err = c.Model(ctx, *preferredID)
	} else {
		selected, err = scanModel(c.db.QueryRowContext(ctx, "SELECT "+modelColumns+" FROM models WHERE model_key<>? AND provider_id IN (SELECT id FROM providers WHERE is_enabled=1) ORDER BY is_default DESC,id DESC LIMIT 1", execution.ModelKey))
		var missing *domain.Error
		if errors.As(err, &missing) && missing.Code == "NOT_FOUND" {
			selected, err = execution, nil
		}
	}
	if err != nil {
		return domain.Model{}, domain.Provider{}, false, err
	}
	spec, err := c.Provider(ctx, selected.ProviderID)
	if err != nil {
		return domain.Model{}, domain.Provider{}, false, err
	}
	self := selected.ModelKey == execution.ModelKey
	if !spec.IsEnabled {
		return selected, spec, self, domain.NewError(400, "PROVIDER_DISABLED", "The reviewer provider is disabled.")
	}
	return selected, spec, self, nil
}
