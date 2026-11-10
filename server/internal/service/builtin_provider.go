package service

import "io.yueyang/tom/internal/config"

// BuiltinProvider 是服务端内置的模型供应商。接口地址不交给客户端填写。
type BuiltinProvider struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// BuiltinProviders 返回当前内置供应商。后续增加供应商时只扩展这个列表。
func BuiltinProviders() []BuiltinProvider {
	return []BuiltinProvider{{ID: "deepseek", Name: "DeepSeek"}}
}

// BuiltinBaseURL 返回内置供应商的接口地址。DeepSeek 使用 OpenAI 兼容根地址，模型列表是 GET /models。
func BuiltinBaseURL(id string, cfg config.Config) (string, bool) {
	if id != "deepseek" {
		return "", false
	}
	if cfg.DeepSeekBaseURL != "" {
		return cfg.DeepSeekBaseURL, true
	}
	return config.DefaultDeepSeekBaseURL, true
}
