package modelregistry

import (
	"io.yueyang/tom/internal/provider"
	"time"
)

// Provider 保存供应商当前目录；停用后可编辑，历史策略仍使用独立配置快照。
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
	ID              string        `json:"id"`
	ProviderID      string        `json:"provider_id"`
	ModelKey        string        `json:"model_key"`
	DisplayName     string        `json:"display_name"`
	ContextLimit    int           `json:"context_limit"`
	OutputLimit     int           `json:"output_limit"`
	SupportsTools   bool          `json:"supports_tools"`
	SupportsVision  bool          `json:"supports_vision"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
	IsEnabled       bool          `json:"is_enabled"`
	Revision        int64         `json:"revision"`
	Metadata        ModelMetadata `json:"metadata"`
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
	Protocol        string `json:"protocol"`
	BaseURL         string `json:"base_url"`
	ModelKey        string `json:"model_key"`
	ContextLimit    int    `json:"context_limit"`
	OutputLimit     int    `json:"output_limit"`
	SupportsTools   bool   `json:"supports_tools"`
	SupportsVision  bool   `json:"supports_vision"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	Prices          Prices `json:"prices"`
}

// Validate 检查固定配置的结构与数值边界；实际网络目标仍须由模型传输层验证。
func (configuration Configuration) Validate() error {
	if !configuration.valid() {
		return ErrInvalidInput
	}
	return nil
}

func (configuration Configuration) valid() bool {
	if provider.GenerationPath(configuration.Protocol) == "" || !validURL(configuration.BaseURL) ||
		!validText(configuration.ModelKey, 256) || configuration.ContextLimit < 1 || configuration.ContextLimit > 100_000_000 ||
		configuration.OutputLimit < 1 || configuration.OutputLimit > configuration.ContextLimit || configuration.ReasoningEffort != "" && !metadataIdentifier.MatchString(configuration.ReasoningEffort) {
		return false
	}
	for _, price := range []*int64{configuration.Prices.Input, configuration.Prices.Output, configuration.Prices.CacheRead, configuration.Prices.CacheWrite} {
		if price != nil && (*price < 0 || *price > 1_000_000_000_000) {
			return false
		}
	}
	return true
}

// Policy 以当前已启用版本供新运行绑定，停用不会改写已经启用的内容。
type Policy struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	IsDefault      bool   `json:"is_default"`
	IsEnabled      bool   `json:"is_enabled"`
	Revision       int64  `json:"revision"`
	CurrentVersion *int64 `json:"current_version"`
}

// Candidate 保存版本内独立模型快照，优先级越小越优先，权重用于新任务分配。
type Candidate struct {
	Position      int           `json:"position"`
	ProviderID    string        `json:"provider_id"`
	ModelID       string        `json:"model_id"`
	CredentialID  *string       `json:"credential_id"`
	Configuration Configuration `json:"configuration"`
	Priority      int           `json:"priority"`
	Weight        int           `json:"weight"`
}

// Version 仅有启用和停用状态；验证时间用于首次启用检查，候选和路由配置不可改写。
type Version struct {
	PolicyID    string      `json:"policy_id"`
	Number      int64       `json:"version"`
	RoutingMode string      `json:"routing_mode"`
	Candidates  []Candidate `json:"candidates"`
	IsEnabled   bool        `json:"is_enabled"`
	VerifiedAt  *time.Time  `json:"verified_at"`
}

// CandidateRequest 的可选覆盖为空时继承模型配置和供应商最新有效凭据。
type CandidateRequest struct {
	ModelID         string  `json:"model_id"`
	CredentialID    *string `json:"credential_id,omitempty"`
	OutputLimit     *int    `json:"output_limit,omitempty"`
	ReasoningEffort *string `json:"reasoning_effort,omitempty"`
	Priority        int     `json:"priority"`
	Weight          int     `json:"weight"`
	Prices          Prices  `json:"prices"`
}

// VersionRequest 每次创建不可变版本；路由模式仅接受优先级、加权分配和轮询。
type VersionRequest struct {
	PolicyID    string             `json:"policy_id"`
	RoutingMode string             `json:"routing_mode"`
	Candidates  []CandidateRequest `json:"candidates"`
}

// Requirements 由受信执行器根据消息和授权工具生成，不能由用户指定模型来绕过筛选。
type Requirements struct {
	Tools       bool
	Vision      bool
	InputTokens int
}

// Selection 仅供执行器使用；策略版本绑定运行，具体模型经持久路由选择后填写。
type Selection struct {
	UserID   string `json:"user_id"`
	BotID    string `json:"bot_id"`
	PolicyID string `json:"policy_id"`
	Version  int64  `json:"version"`
	RunID    string `json:"run_id"`
	ModelID  string `json:"model_id"`
}
