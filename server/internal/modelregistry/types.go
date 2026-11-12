package modelregistry

import "time"

// Provider 保存不可变协议和地址；地址变化须创建新目录项，避免旧运行被重新路由。
type Provider struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Protocol  string    `json:"protocol"`
	BaseURL   string    `json:"base_url"`
	IsEnabled bool      `json:"is_enabled"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
}

// Model 保存供应商模型及能力上限，策略可以在此上限内降低输出长度。
type Model struct {
	ID             string `json:"id"`
	ProviderID     string `json:"provider_id"`
	ModelKey       string `json:"model_key"`
	DisplayName    string `json:"display_name"`
	ContextLimit   int    `json:"context_limit"`
	OutputLimit    int    `json:"output_limit"`
	SupportsTools  bool   `json:"supports_tools"`
	SupportsVision bool   `json:"supports_vision"`
	IsEnabled      bool   `json:"is_enabled"`
	Revision       int64  `json:"revision"`
}

// Credential 只包含版本元数据，不包含凭据前后缀、摘要或密文。
type Credential struct {
	ID          string    `json:"id"`
	ProviderID  string    `json:"provider_id"`
	Version     int64     `json:"version"`
	IsRevoked   bool      `json:"is_revoked"`
	IsDestroyed bool      `json:"is_destroyed"`
	CreatedAt   time.Time `json:"created_at"`
}

// Prices 价格单位为每百万 token 的微美元；缺失与零价明确区分。
type Prices struct {
	Input      *int64 `json:"input_micro_usd_per_million,omitempty"`
	Output     *int64 `json:"output_micro_usd_per_million,omitempty"`
	CacheRead  *int64 `json:"cache_read_micro_usd_per_million,omitempty"`
	CacheWrite *int64 `json:"cache_write_micro_usd_per_million,omitempty"`
}

// Configuration 为版本内完整快照，不存放任意 HTTP 头、凭据或客户端覆盖参数。
type Configuration struct {
	Protocol       string `json:"protocol"`
	BaseURL        string `json:"base_url"`
	ModelKey       string `json:"model_key"`
	ContextLimit   int    `json:"context_limit"`
	OutputLimit    int    `json:"output_limit"`
	SupportsTools  bool   `json:"supports_tools"`
	SupportsVision bool   `json:"supports_vision"`
	Prices         Prices `json:"prices"`
}

// Validate 检查固定配置的结构与数值边界；实际网络目标仍须由模型传输层验证。
func (configuration Configuration) Validate() error {
	if !configuration.valid() {
		return ErrInvalidInput
	}
	return nil
}

func (configuration Configuration) valid() bool {
	if (configuration.Protocol != "openai_compat" && configuration.Protocol != "anthropic") || !validURL(configuration.BaseURL) ||
		!validText(configuration.ModelKey, 256) || configuration.ContextLimit < 1 || configuration.ContextLimit > 100_000_000 ||
		configuration.OutputLimit < 1 || configuration.OutputLimit > configuration.ContextLimit {
		return false
	}
	for _, price := range []*int64{configuration.Prices.Input, configuration.Prices.Output, configuration.Prices.CacheRead, configuration.Prices.CacheWrite} {
		if price != nil && (*price < 0 || *price > 1_000_000_000_000) {
			return false
		}
	}
	return true
}

// Policy 以当前已发布版本供新运行绑定，停用不会改写已经发布的内容。
type Policy struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	IsDefault      bool   `json:"is_default"`
	IsEnabled      bool   `json:"is_enabled"`
	Revision       int64  `json:"revision"`
	CurrentVersion *int64 `json:"current_version"`
}

// Version 供管理员查看固定配置；凭据仅返回不可用于认证的记录标识。
type Version struct {
	PolicyID      string        `json:"policy_id"`
	Number        int64         `json:"version"`
	ProviderID    string        `json:"provider_id"`
	ModelID       string        `json:"model_id"`
	CredentialID  *string       `json:"credential_id"`
	Configuration Configuration `json:"configuration"`
	Status        string        `json:"status"`
	PublishedAt   *time.Time    `json:"published_at"`
}

// DraftRequest 每次产生一个新版本；旧草稿不能原地修改或复用版本号。
type DraftRequest struct {
	PolicyID     string  `json:"policy_id"`
	ModelID      string  `json:"model_id"`
	CredentialID *string `json:"credential_id"`
	OutputLimit  int     `json:"output_limit"`
	Prices       Prices  `json:"prices"`
}

// Selection 必须由调度器持久化到运行记录，普通用户接口不能接收此值作为模型选择。
type Selection struct {
	UserID   string `json:"user_id"`
	BotID    string `json:"bot_id"`
	PolicyID string `json:"policy_id"`
	Version  int64  `json:"version"`
}
