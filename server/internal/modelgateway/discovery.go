package modelgateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"unicode"

	"io.yueyang/tom/internal/modelregistry"
)

// DiscoveredModel 是供应商模型列表接口返回的一项；规格由上游声明，可能与实际能力不符。
type DiscoveredModel struct {
	ModelKey    string `json:"model_key"`
	DisplayName string `json:"display_name"`
}

const (
	maxDiscoveredModels = 200
	maxDiscoveryBytes   = 1 << 20
)

// Discover 通过同一套出口策略读取供应商模型列表；只允许已配置来源，不跟随跳转，不保留凭据。
// 上游列表通常不含上下文与输出上限，因此这里不推断规格，登记时由管理员显式填写。
func (gateway *Gateway) Discover(parent context.Context, protocol, baseURL string, credential []byte) ([]modelregistry.DiscoveredModel, error) {
	if protocol != "openai_compat" && protocol != "anthropic" {
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
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, ErrUnavailable
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maxDiscoveryBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrProtocol
	}
	if len(encoded) > maxDiscoveryBytes {
		return nil, ErrResponseTooLarge
	}
	return parseDiscovered(encoded)
}

// parseDiscovered 只接受 {object:"list",data:[{id}]} 结构；无法识别的响应整体拒绝，不做猜测。
func parseDiscovered(encoded []byte) ([]modelregistry.DiscoveredModel, error) {
	var payload struct {
		Object string `json:"object"`
		Data   []struct {
			ID   string `json:"id"`
			Name string `json:"display_name"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	if decoder.Decode(&payload) != nil || payload.Object != "list" || len(payload.Data) == 0 || len(payload.Data) > maxDiscoveredModels {
		return nil, ErrProtocol
	}
	seen := make(map[string]struct{}, len(payload.Data))
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
		name := strings.TrimSpace(item.Name)
		if name == "" || len(name) > 120 || strings.ContainsFunc(name, unicode.IsControl) {
			name = key
		}
		models = append(models, modelregistry.DiscoveredModel{ModelKey: key, DisplayName: name})
	}
	return models, nil
}
