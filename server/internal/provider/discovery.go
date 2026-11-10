package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"io.yueyang/tom/internal/domain"
)

const maxDiscoveredModels = 200
const maxDiscoveryBytes = 1 << 20

// DiscoveredModel 只包含上游模型发现接口实际返回的可用规格，不内置模型名单。
type DiscoveredModel struct {
	ID               string   `json:"id"`
	Object           string   `json:"object"`
	Name             string   `json:"name"`
	ContextWindow    *int     `json:"context_window"`
	MaxOutputTokens  *int     `json:"max_output_tokens"`
	InputModalities  []string `json:"input_modalities"`
	OutputModalities []string `json:"output_modalities"`
}

func invalidDiscovery() error {
	return domain.NewError(502, "PROVIDER_MODELS_INVALID", "供应商没有返回有效的模型列表或模型规格，请稍后重新同步。")
}

// DiscoverDeepSeek 使用一次有时限、禁止跳转的 GET 验证密钥并读取模型；错误不包含上游响应或密钥。
func DiscoverDeepSeek(parent context.Context, baseURL string, credential []byte, timeout time.Duration) (models []DiscoveredModel, result error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return nil, domain.NewError(400, "PROVIDER_URL_INVALID", "供应商地址无效。")
	}
	request.Header.Set("Authorization", "Bearer "+string(credential))
	request.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		if parent.Err() != nil {
			return nil, parent.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, domain.NewError(504, "PROVIDER_TIMEOUT", "获取供应商模型超时，请重试。")
		}
		return nil, domain.NewError(502, "PROVIDER_NETWORK", "无法连接供应商，请检查网络后重试。")
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			result = errors.Join(result, domain.NewError(502, "PROVIDER_NETWORK", "读取供应商模型失败，请重试。"))
		}
	}()
	if response.StatusCode != http.StatusOK {
		switch response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, domain.NewError(400, "PROVIDER_AUTH_FAILED", "API Key 验证失败，请检查后重试。")
		case http.StatusTooManyRequests:
			return nil, domain.NewError(429, "PROVIDER_RATE_LIMITED", "供应商请求过于频繁，请稍后重试。")
		default:
			return nil, domain.NewError(502, "PROVIDER_UNAVAILABLE", "供应商暂时无法提供模型列表，请稍后重试。")
		}
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maxDiscoveryBytes+1))
	if err != nil {
		if parent.Err() != nil {
			return nil, parent.Err()
		}
		if ctx.Err() != nil {
			return nil, domain.NewError(504, "PROVIDER_TIMEOUT", "获取供应商模型超时，请重试。")
		}
		return nil, invalidDiscovery()
	}
	if len(encoded) > maxDiscoveryBytes {
		return nil, invalidDiscovery()
	}
	var responseModels struct {
		Object string            `json:"object"`
		Data   []DiscoveredModel `json:"data"`
	}
	if err = json.Unmarshal(encoded, &responseModels); err != nil || responseModels.Object != "list" || len(responseModels.Data) == 0 || len(responseModels.Data) > maxDiscoveredModels {
		return nil, invalidDiscovery()
	}
	seen := make(map[string]bool, len(responseModels.Data))
	for index := range responseModels.Data {
		model := &responseModels.Data[index]
		if model.Object != "model" || model.ID == "" || strings.TrimSpace(model.ID) != model.ID || len(model.ID) > 120 || strings.ContainsFunc(model.ID, unicode.IsControl) || seen[model.ID] {
			return nil, invalidDiscovery()
		}
		seen[model.ID] = true
		if model.Name == "" {
			model.Name = model.ID
		}
		if len(model.Name) > 120 || strings.TrimSpace(model.Name) == "" || strings.ContainsFunc(model.Name, unicode.IsControl) || model.ContextWindow != nil && (*model.ContextWindow < 256 || *model.ContextWindow > 10000000) || model.MaxOutputTokens != nil && (*model.MaxOutputTokens < 1 || *model.MaxOutputTokens > 10000000) || model.ContextWindow != nil && model.MaxOutputTokens != nil && *model.MaxOutputTokens > *model.ContextWindow {
			return nil, invalidDiscovery()
		}
		if len(model.InputModalities) > 0 && !slices.Contains(model.InputModalities, "text") || len(model.OutputModalities) > 0 && !slices.Contains(model.OutputModalities, "text") {
			return nil, invalidDiscovery()
		}
	}
	return responseModels.Data, nil
}
