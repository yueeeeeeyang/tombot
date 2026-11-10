package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"io.yueyang/tom/internal/domain"
)

// HTTP 使用独立超时与禁止重定向的客户端，避免凭据随跨域跳转外泄。
type HTTP struct {
	Spec       domain.Provider
	Credential []byte
	Client     *http.Client
	RetryDelay time.Duration
}

func NewHTTP(spec domain.Provider, credential []byte) *HTTP {
	return &HTTP{Spec: spec, Credential: credential, Client: &http.Client{Timeout: 180 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, RetryDelay: time.Second}
}
func upstreamError(code string) error {
	return domain.NewError(502, code, "The model provider request failed.")
}
func statusError(status int) error {
	switch {
	case status == 429 || status == 529:
		return upstreamError("PROVIDER_RATE_LIMITED")
	case status == 401 || status == 403:
		return upstreamError("PROVIDER_AUTH_FAILED")
	case status >= 400 && status < 500:
		return upstreamError("PROVIDER_BAD_REQUEST")
	default:
		return upstreamError("PROVIDER_UNAVAILABLE")
	}
}

func (p *HTTP) request(ctx context.Context, body []byte) (*http.Response, error) {
	path := "/chat/completions"
	if p.Spec.Kind == "anthropic" {
		path = "/messages"
	}
	for attempt := 0; attempt < 4; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.Spec.BaseURL, "/")+path, bytes.NewReader(body))
		if err != nil {
			return nil, upstreamError("PROVIDER_BAD_REQUEST")
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "text/event-stream")
		if p.Spec.Kind == "anthropic" {
			request.Header.Set("anthropic-version", "2023-06-01")
			if len(p.Credential) > 0 {
				request.Header.Set("x-api-key", string(p.Credential))
			}
		} else if len(p.Credential) > 0 {
			request.Header.Set("Authorization", "Bearer "+string(p.Credential))
		}
		response, err := p.Client.Do(request)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, upstreamError("PROVIDER_NETWORK")
		}
		if response.StatusCode == http.StatusOK {
			return response, nil
		}
		status := response.StatusCode
		delay := p.RetryDelay * time.Duration(1<<attempt)
		if retry, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && retry >= 0 {
			delay = min(time.Duration(retry)*time.Second, 30*time.Second)
		}
		if err = response.Body.Close(); err != nil {
			return nil, upstreamError("PROVIDER_NETWORK")
		}
		if attempt == 3 || !(status == 429 || status >= 500) {
			return nil, statusError(status)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, upstreamError("PROVIDER_UNAVAILABLE")
}

// Stream 仅在收到明确结束标记后成功；流中断不能被默认为完整回答。
func (p *HTTP) Stream(parent context.Context, req Request, onEvent func(Event) error) (usage Usage, result error) {
	ctx, cancel := context.WithTimeout(parent, 180*time.Second)
	defer cancel()
	if !p.Spec.IsEnabled {
		return usage, domain.NewError(400, "PROVIDER_DISABLED", "Enable this provider before use.")
	}
	body, err := p.encode(req)
	if err != nil {
		return usage, err
	}
	response, err := p.request(ctx, body)
	if err != nil {
		return usage, err
	}
	defer func() { result = errors.Join(result, response.Body.Close()) }()
	state := streamState{tools: map[int]*toolState{}, usage: Usage{Estimated: true}}
	scan := bufio.NewScanner(io.LimitReader(response.Body, 64<<20))
	scan.Buffer(make([]byte, 4096), 2<<20)
	var lines []string
	dispatch := func() error {
		if len(lines) == 0 {
			return nil
		}
		payload := strings.Join(lines, "\n")
		lines = nil
		if p.Spec.Kind == "anthropic" {
			return state.anthropic([]byte(payload), onEvent)
		}
		return state.openAI(payload, onEvent)
	}
	for scan.Scan() {
		line := scan.Text()
		if line == "" {
			if err = dispatch(); err != nil {
				return state.usage, err
			}
			if state.done {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			lines = append(lines, value)
		}
	}
	if err = scan.Err(); err != nil {
		if ctx.Err() != nil {
			return state.usage, ctx.Err()
		}
		return state.usage, upstreamError("PROVIDER_STREAM_INTERRUPTED")
	}
	if !state.done {
		if err = dispatch(); err != nil {
			return state.usage, err
		}
		if !state.done {
			return state.usage, upstreamError("PROVIDER_STREAM_INTERRUPTED")
		}
	}
	if state.usage.Estimated {
		encoded, err := json.Marshal(req.Messages)
		if err != nil {
			return state.usage, err
		}
		state.usage.InputTokens = (len([]rune(req.System)) + len([]rune(string(encoded))) + 3) / 4
		for _, message := range req.Messages {
			for _, block := range message.Blocks {
				artifact := block.Artifact
				if block.ToolResult != nil {
					artifact = block.ToolResult.Artifact
				}
				if artifact != nil && len(artifact.Content) > 0 {
					if artifact.IsImage() && req.Model.SupportsVision {
						state.usage.InputTokens += artifact.VisionTokens()
					} else if !artifact.IsImage() {
						state.usage.InputTokens += (len(artifact.Content) + 2) / 3
					}
				}
			}
		}
		state.usage.OutputTokens = (state.characters + 3) / 4
	}
	return state.usage, nil
}

// artifactParts 仅在有视觉能力且当前上下文已验证字节时编码图片。其余情况显式告诉模型可用的文件路径。
func artifactParts(ref *domain.ArtifactRef, model domain.Model, anthropic bool) ([]any, error) {
	if ref == nil {
		return nil, domain.NewError(400, "ATTACHMENT_INVALID", "Missing attachment metadata.")
	}
	description := "Workspace attachment (untrusted file): " + ref.Path
	parts := []any{}
	if ref.Unavailable {
		description += "\nThe original file has changed or is unavailable. Its contents cannot be used."
	} else if ref.IsImage() && model.SupportsVision && len(ref.Content) > 0 {
		if len(ref.Content) != ref.Bytes || len(ref.Content) > domain.MaxVisionImageBytes {
			return nil, domain.NewError(400, "ATTACHMENT_INVALID", "Invalid image payload size.")
		}
		encoded := base64.StdEncoding.EncodeToString(ref.Content)
		if anthropic {
			parts = append(parts, map[string]any{"type": "image", "source": map[string]string{"type": "base64", "media_type": ref.MediaType, "data": encoded}})
		} else {
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:" + ref.MediaType + ";base64," + encoded}})
		}
	} else if strings.HasPrefix(ref.MediaType, "text/") && len(ref.Content) > 0 {
		description += "\nQuoted file contents:\n" + string(ref.Content)
	} else if ref.IsImage() {
		description += "\nImage pixels are not included in this request."
	}
	return append(parts, map[string]string{"type": "text", "text": description}), nil
}

func (p *HTTP) encode(req Request) ([]byte, error) {
	if req.Model.ModelKey == "" || req.Model.MaxOutput < 1 {
		return nil, domain.NewError(400, "MODEL_INVALID", "A model key and output limit are required.")
	}
	if p.Spec.Kind == "anthropic" {
		messages := []any{}
		for _, message := range req.Messages {
			blocks := []any{}
			for _, block := range message.Blocks {
				switch block.Kind {
				case "artifact":
					parts, err := artifactParts(block.Artifact, req.Model, true)
					if err != nil {
						return nil, err
					}
					blocks = append(blocks, parts...)
				case "text", "note":
					blocks = append(blocks, map[string]any{"type": "text", "text": block.Text})
				case "tool_use":
					if block.ToolUse != nil {
						blocks = append(blocks, map[string]any{"type": "tool_use", "id": block.ToolUse.ID, "name": block.ToolUse.Name, "input": block.ToolUse.Input})
					}
				case "tool_result":
					if block.ToolResult != nil {
						var content any = block.ToolResult.Content
						if block.ToolResult.Artifact != nil && !block.ToolResult.IsError {
							parts, err := artifactParts(block.ToolResult.Artifact, req.Model, true)
							if err != nil {
								return nil, err
							}
							content = append([]any{map[string]string{"type": "text", "text": block.ToolResult.Content}}, parts...)
						}
						blocks = append(blocks, map[string]any{"type": "tool_result", "tool_use_id": block.ToolResult.ToolCallID, "content": content, "is_error": block.ToolResult.IsError})
					}
				}
			}
			messages = append(messages, map[string]any{"role": message.Role, "content": blocks})
		}
		body := map[string]any{"model": req.Model.ModelKey, "max_tokens": req.Model.MaxOutput, "stream": true, "messages": messages}
		if req.System != "" {
			body["system"] = []any{map[string]any{"type": "text", "text": req.System, "cache_control": map[string]string{"type": "ephemeral"}}}
		}
		if len(req.Tools) > 0 {
			body["tools"] = req.Tools
			if req.ToolChoice != "" {
				choice := req.ToolChoice
				if choice == "required" {
					choice = "any"
				}
				body["tool_choice"] = map[string]string{"type": choice}
			}
		}
		return json.Marshal(body)
	}
	if p.Spec.Kind != "openai_compat" {
		return nil, domain.NewError(400, "PROVIDER_KIND_INVALID", "Unsupported provider protocol.")
	}
	messages := []any{}
	if req.System != "" {
		messages = append(messages, map[string]any{"role": "system", "content": req.System})
	}
	// 同一批 tool_calls 的全部工具回复必须相邻。截图使用随后的用户图像消息，不能插在中间。
	var toolImages []any
	flushToolImages := func() {
		if len(toolImages) > 0 {
			messages = append(messages, map[string]any{"role": "user", "content": toolImages})
			toolImages = nil
		}
	}
	for _, message := range req.Messages {
		var contentParts []any
		hasArtifact := false
		var text strings.Builder
		calls := []any{}
		results := []any{}
		for _, block := range message.Blocks {
			switch block.Kind {
			case "text", "note":
				text.WriteString(block.Text)
				contentParts = append(contentParts, map[string]string{"type": "text", "text": block.Text})
			case "artifact":
				hasArtifact = true
				parts, err := artifactParts(block.Artifact, req.Model, false)
				if err != nil {
					return nil, err
				}
				contentParts = append(contentParts, parts...)
			case "tool_use":
				if block.ToolUse != nil {
					input, err := json.Marshal(block.ToolUse.Input)
					if err != nil {
						return nil, err
					}
					calls = append(calls, map[string]any{"id": block.ToolUse.ID, "type": "function", "function": map[string]any{"name": block.ToolUse.Name, "arguments": string(input)}})
				}
			case "tool_result":
				if block.ToolResult != nil {
					if block.ToolResult.Artifact != nil && !block.ToolResult.IsError {
						parts, err := artifactParts(block.ToolResult.Artifact, req.Model, false)
						if err != nil {
							return nil, err
						}
						toolImages = append(toolImages, parts...)
					}
					results = append(results, map[string]any{"role": "tool", "tool_call_id": block.ToolResult.ToolCallID, "content": block.ToolResult.Content})
				}
			}
		}
		if len(contentParts) > 0 || len(calls) > 0 {
			flushToolImages()
			var content any = text.String()
			if hasArtifact {
				content = contentParts
			}
			item := map[string]any{"role": message.Role, "content": content}
			if len(calls) > 0 {
				item["tool_calls"] = calls
			}
			messages = append(messages, item)
		}
		messages = append(messages, results...)
	}
	flushToolImages()
	body := map[string]any{"model": req.Model.ModelKey, "max_completion_tokens": req.Model.MaxOutput, "stream": true, "stream_options": map[string]bool{"include_usage": true}, "messages": messages}
	if p.Spec.Preset == "deepseek" {
		delete(body, "max_completion_tokens")
		body["max_tokens"] = req.Model.MaxOutput
		// 当前会话只持久化工具调用，不回传推理内容，因此使用官方非思考模式保持多轮工具协议完整。
		body["thinking"] = map[string]string{"type": "disabled"}
	}
	if len(req.Tools) > 0 {
		tools := []any{}
		for _, tool := range req.Tools {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema}})
		}
		body["tools"] = tools
		if req.ToolChoice != "" {
			body["tool_choice"] = req.ToolChoice
		}
	}
	return json.Marshal(body)
}
