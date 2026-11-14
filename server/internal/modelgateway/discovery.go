package modelgateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"unicode"

	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
)

var (
	ErrDiscoveryAuthentication = errors.New("供应商拒绝了模型列表凭据")
	ErrDiscoveryRateLimited    = errors.New("供应商模型列表请求受到限流")
	ErrDiscoveryUnsupported    = errors.New("供应商地址不支持模型列表接口")
)

const (
	maxDiscoveredModels = 200
	maxDiscoveryBytes   = 1 << 20
)

// Discover 通过同一套出口策略读取供应商模型列表；校验目标地址，不跟随跳转，不保留凭据。
// 保留上游明确提供的规格，缺失字段不填默认值，也不按模型名称猜测能力。
func (gateway *Gateway) Discover(parent context.Context, protocol, baseURL string, credential []byte) (models []modelregistry.DiscoveredModel, failure error) {
	if provider.GenerationPath(protocol) == "" || len(credential) == 0 || len(credential) > 8192 {
		return nil, ErrInvalidInput
	}
	for _, value := range credential {
		if value < 33 || value > 126 {
			return nil, ErrInvalidInput
		}
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/models"
	ctx, cancel := context.WithTimeout(parent, gateway.options.RequestTimeout)
	defer cancel()
	select {
	case gateway.slots <- struct{}{}:
		defer func() { <-gateway.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	client, guard, err := gateway.client(endpoint, http.MethodGet, "application/json")
	if err != nil {
		return nil, err
	}
	defer guard.transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, ErrInvalidInput
	}
	request.Header.Set("Accept", "application/json")
	if protocol == "anthropic" {
		request.Header.Set("anthropic-version", "2023-06-01")
		request.Header.Set("x-api-key", string(credential))
	} else {
		request.Header.Set("Authorization", "Bearer "+string(credential))
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if guard.failure != nil {
			return nil, guard.failure
		}
		return nil, ErrUnavailable
	}
	defer func() {
		if err := response.Body.Close(); err != nil && failure == nil {
			models, failure = nil, ErrUnavailable
		}
	}()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrDiscoveryAuthentication
	case http.StatusTooManyRequests:
		return nil, ErrDiscoveryRateLimited
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return nil, ErrDiscoveryUnsupported
	default:
		return nil, ErrUnavailable
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maxDiscoveryBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if guard.failure != nil {
			return nil, guard.failure
		}
		return nil, ErrProtocol
	}
	if len(encoded) > maxDiscoveryBytes {
		return nil, ErrResponseTooLarge
	}
	return parseDiscovered(protocol, encoded)
}

// parseDiscovered 解析受支持协议的列表结构，并将明确返回的规格规范化；未知能力不推断。
func parseDiscovered(protocol string, encoded []byte) ([]modelregistry.DiscoveredModel, error) {
	var payload struct {
		Object string `json:"object"`
		Data   []struct {
			ID               string                     `json:"id"`
			Name             string                     `json:"name"`
			DisplayName      string                     `json:"display_name"`
			ContextWindow    *int                       `json:"context_window"`
			MaxInputTokens   *int                       `json:"max_input_tokens"`
			MaxOutputTokens  *int                       `json:"max_output_tokens"`
			MaxTokens        *int                       `json:"max_tokens"`
			InputModalities  []string                   `json:"input_modalities"`
			OutputModalities []string                   `json:"output_modalities"`
			Effort           *modelregistry.ModelEffort `json:"effort"`
			SupportsTools    *bool                      `json:"supports_tools"`
			SupportsVision   *bool                      `json:"supports_vision"`
			Capabilities     struct {
				ImageInput *struct {
					Supported bool `json:"supported"`
				} `json:"image_input"`
				Effort map[string]json.RawMessage `json:"effort"`
			} `json:"capabilities"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	if decoder.Decode(&payload) != nil || decoder.Decode(new(any)) != io.EOF || payload.Object != "list" && !(protocol == "anthropic" && payload.Object == "") || payload.Data == nil || len(payload.Data) > maxDiscoveredModels {
		return nil, ErrProtocol
	}
	seen := map[string]struct{}{}
	models := make([]modelregistry.DiscoveredModel, 0, len(payload.Data))
	for _, item := range payload.Data {
		key := strings.TrimSpace(item.ID)
		if key == "" || len(key) > 256 || strings.ContainsFunc(key, unicode.IsControl) {
			return nil, ErrProtocol
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		name := strings.TrimSpace(item.DisplayName)
		if name == "" {
			name = strings.TrimSpace(item.Name)
		}
		if name == "" || len(name) > 120 || strings.ContainsFunc(name, unicode.IsControl) {
			name = key
		}
		model := modelregistry.DiscoveredModel{ModelKey: key, DisplayName: name, ContextLimit: item.ContextWindow, OutputLimit: item.MaxOutputTokens, SupportsTools: item.SupportsTools, SupportsVision: item.SupportsVision, Metadata: modelregistry.ModelMetadata{InputModalities: item.InputModalities, OutputModalities: item.OutputModalities, Effort: item.Effort}}
		if protocol == "anthropic" {
			if model.ContextLimit == nil {
				model.ContextLimit = item.MaxInputTokens
			}
			if model.OutputLimit == nil {
				model.OutputLimit = item.MaxTokens
			}
			if model.SupportsVision == nil && item.Capabilities.ImageInput != nil {
				model.SupportsVision = &item.Capabilities.ImageInput.Supported
			}
			if model.Metadata.Effort == nil && item.Capabilities.Effort != nil {
				var supported bool
				if raw, exists := item.Capabilities.Effort["supported"]; exists && json.Unmarshal(raw, &supported) != nil {
					return nil, ErrProtocol
				}
				if supported {
					levels := []string{}
					for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
						if raw, exists := item.Capabilities.Effort[level]; exists {
							var capability struct {
								Supported bool `json:"supported"`
							}
							if json.Unmarshal(raw, &capability) != nil {
								return nil, ErrProtocol
							}
							if capability.Supported {
								levels = append(levels, level)
							}
						}
					}
					if len(levels) > 0 {
						model.Metadata.Effort = &modelregistry.ModelEffort{SupportedLevels: levels}
					}
				}
			}
		}
		if model.Metadata.Validate() != nil {
			return nil, ErrProtocol
		}
		if model.ContextLimit != nil && (*model.ContextLimit < 1 || *model.ContextLimit > 100_000_000) || model.OutputLimit != nil && (*model.OutputLimit < 1 || *model.OutputLimit > 100_000_000) || model.ContextLimit != nil && model.OutputLimit != nil && *model.OutputLimit > *model.ContextLimit {
			return nil, ErrProtocol
		}
		if model.Metadata.InputModalities != nil {
			hasImage := slices.Contains(model.Metadata.InputModalities, "image")
			if model.SupportsVision != nil && *model.SupportsVision != hasImage {
				return nil, ErrProtocol
			}
			model.SupportsVision = &hasImage
		}
		models = append(models, model)
	}
	return models, nil
}
